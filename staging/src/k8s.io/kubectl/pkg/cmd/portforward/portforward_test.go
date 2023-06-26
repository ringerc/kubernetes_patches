/*
Copyright 2014 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package portforward

import (
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"testing"
	"strconv"
	"os"
	"io"
	"errors"

	"github.com/spf13/cobra"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/client-go/rest/fake"
	cmdtesting "k8s.io/kubectl/pkg/cmd/testing"
	"k8s.io/kubectl/pkg/scheme"
)

const LOCAL_PORT_OFFSET = 5

type fakePortForwarder struct {
	method string
	url    *url.URL
	pfErr  error
	ports []string
	mappedPorts []forwardedPort
}

func (f *fakePortForwarder) ForwardPorts(method string, url *url.URL, opts PortForwardOptions) error {
	f.method = method
	f.url = url
	close(opts.ReadyChannel)
	if f.pfErr == nil {
		var err error
		f.mappedPorts, err = fakePortMapping(f.ports)
		if err != nil {
			return err
		}
	}
	return f.pfErr
}

func (f *fakePortForwarder) GetPorts() ([]forwardedPort, error) {
	if f.pfErr != nil {
		return []forwardedPort{}, fmt.Errorf("not Ready")
	}
	return f.mappedPorts, nil
}

// fake up a mapping of allocated ports, since we're not really forwarding
// anything at all. This doesn't have to handle string port mappings and
// doesn't have to realistically emulate local random port assignment, it just
// has to return something that could be a realistic result from the input port
// mappings.
func fakePortMapping(ports []string) ([]forwardedPort, error) {
	fp := make([]forwardedPort, len(ports))
	for i, p := range ports {
		lps, rps := splitPort(p)
		var err error
		rp, err := strconv.Atoi(rps)
		fp[i].Remote = uint16(rp)
		if err != nil {
			return []forwardedPort{}, err
		}
		if lps == "" {
			// To avoid random local port allocation in tests just
			// assume it's local port with arbitrary offset
			fp[i].Local = fp[i].Remote + LOCAL_PORT_OFFSET
		} else {
			lp, err := strconv.Atoi(lps)
			if err != nil {
				return []forwardedPort{}, err
			}
			fp[i].Local = uint16(lp)
		}
	}
	return fp, nil
}

type testchecks func (t *testing.T, opts *PortForwardOptions, fakeForwarder *fakePortForwarder)

func testPortForward(t *testing.T, args []string, allerr bool, expectPorts []forwardedPort, extrachecks testchecks) {
	version := "v1"

	podPath := "/api/" + version + "/namespaces/test/pods/foo"
	pfPath := "/api/" + version + "/namespaces/test/pods/foo/portforward"
	tests := []struct {
		name            string
		podPath, pfPath string
		pod             *corev1.Pod
		pfErr           bool
	}{
		{
			name:    "pod portforward",
			podPath: podPath,
			pfPath:  pfPath,
			pod:     execPod(),
			pfErr:   allerr,
		},
		{
			name:    "pod portforward error",
			podPath: podPath,
			pfPath:  pfPath,
			pod:     execPod(),
			pfErr:   true,
		},
	}
	origDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := os.Chdir(t.TempDir()); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func () {
				if err = os.Chdir(origDir); err != nil {
					t.Fatal(err)
				}
			})
			var err error
			tf := cmdtesting.NewTestFactory().WithNamespace("test")
			defer tf.Cleanup()

			codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)
			ns := scheme.Codecs.WithoutConversion()

			tf.Client = &fake.RESTClient{
				VersionedAPIPath:     "/api/v1",
				GroupVersion:         schema.GroupVersion{Group: "", Version: "v1"},
				NegotiatedSerializer: ns,
				Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
					switch p, m := req.URL.Path, req.Method; {
					case p == test.podPath && m == "GET":
						body := cmdtesting.ObjBody(codec, test.pod)
						return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: body}, nil
					default:
						t.Errorf("%s: unexpected request: %#v\n%#v", test.name, req.URL, req)
						return nil, nil
					}
				}),
			}
			tf.ClientConfigVal = cmdtesting.DefaultClientConfig()

			discardStreams := genericiooptions.NewTestIOStreamsDiscard()
			opts := &PortForwardOptions{IOStreams: discardStreams}
			cmd := NewCmdPortForwardWithOpts(tf, discardStreams, opts)
			var ff *fakePortForwarder
			cmd.Run = func(cmd *cobra.Command, args []string) {
				if err = opts.Complete(tf, cmd, args); err != nil {
					return
				}
				ff = &fakePortForwarder{
					ports: opts.Ports,
				}
				if test.pfErr {
					ff.pfErr = fmt.Errorf("pf error")
				}
				opts.PortForwarder = ff
				if err = opts.Validate(); err != nil {
					return
				}
				err = opts.RunPortForward()
			}
			cmd.SetArgs(args)
			cmd.Execute()

			if test.pfErr && ff != nil && err != ff.pfErr {
				t.Errorf("%s: Unexpected port-forward error: %v", test.name, err)
			}
			if !test.pfErr && err != nil {
				t.Errorf("%s: Unexpected error: %v", test.name, err)
			}
			if test.pfErr {
				return
			}

			if ff.url == nil || ff.url.Path != test.pfPath {
				t.Errorf("%s: Did not get expected path for portforward request", test.name)
			}
			if ff.method != "POST" {
				t.Errorf("%s: Did not get method for attach request: %s", test.name, ff.method)
			}
			if !reflect.DeepEqual(ff.mappedPorts, expectPorts) {
				t.Errorf("%s: mapped ports %v did not match expected %v", test.name, ff.mappedPorts, expectPorts)
			}
			if extrachecks != nil {
				extrachecks(t, opts, ff)
			}
		})
	}
}

func TestPortForward(t *testing.T) {
	tests := []struct {
		name            string
		// if all sub-tests will fail due to invalid args etc
		allerr          bool
		args            []string
		expectPorts     []forwardedPort
		extrachecks     testchecks
	}{
		{
			name: "basic-localmapped",
			args: []string{"foo", ":5000", ":1000"},
			expectPorts: []forwardedPort{
					forwardedPort{Local: 5000 + LOCAL_PORT_OFFSET, Remote: 5000},
					forwardedPort{Local: 1000 + LOCAL_PORT_OFFSET, Remote: 1000},
				},
		},
		{
			name: "basic-localexplicit",
			args: []string{"foo", "5000:5000", "2000:1000"},
			expectPorts: []forwardedPort{
					forwardedPort{Local: 5000, Remote: 5000},
					forwardedPort{Local: 2000, Remote: 1000},
				},
		},
		{
			name: "noports",
			allerr: true,
			args: []string{},
			expectPorts: []forwardedPort{},
		},
		{
			name: "noports",
			allerr: true,
			args: []string{"--invalid-argument"},
			expectPorts: []forwardedPort{},
		},
		{
			name: "ports-file-stdout",
			args: []string{"--ports-file=-", "foo", ":5000", "2000:1000"},
			expectPorts: []forwardedPort{
					forwardedPort{Local: 5000 + LOCAL_PORT_OFFSET, Remote: 5000},
					forwardedPort{Local: 2000, Remote: 1000},
				},
		},
		{
			name: "ports-file-path",
			args: []string{"--ports-file=ports-file", "foo", ":5000", "2000:1000"},
			expectPorts: []forwardedPort{
					forwardedPort{Local: 5000 + LOCAL_PORT_OFFSET, Remote: 5000},
					forwardedPort{Local: 2000, Remote: 1000},
				},
			extrachecks: func (t *testing.T, opts *PortForwardOptions, fakeForwarder *fakePortForwarder) {
				f, err := os.Open("ports-file")
				if err != nil {
					if errors.Is(err, os.ErrNotExist) {
						t.Errorf("expected file \"ports-file\" not found: %v", err)
						return
					}
					t.Fatal(err)
				}
				defer f.Close()
				portsFileContents, err := io.ReadAll(f)
				if err != nil {
					t.Fatal(err)
				}
				expectedPortsFile := `[{"Local":5005,"Remote":5000},{"Local":2000,"Remote":1000}]`+"\n"
				if string(portsFileContents) != expectedPortsFile {
					t.Errorf("contents of ports-file \"%s\" does not match expected value \"%s\"", string(portsFileContents), expectedPortsFile)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			testPortForward(t, test.args, test.allerr, test.expectPorts, test.extrachecks)
		})
	}
}

func TestTranslateServicePortToTargetPort(t *testing.T) {
	cases := []struct {
		name       string
		svc        corev1.Service
		pod        corev1.Pod
		ports      []string
		translated []string
		err        bool
	}{
		{
			name: "test success 1 (int port)",
			svc: corev1.Service{
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{
						{
							Port:       80,
							TargetPort: intstr.FromInt32(8080),
						},
					},
				},
			},
			pod: corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Ports: []corev1.ContainerPort{
								{
									Name:          "http",
									ContainerPort: int32(8080)},
							},
						},
					},
				},
			},
			ports:      []string{"80"},
			translated: []string{"80:8080"},
			err:        false,
		},
		{
			name: "test success 1 (int port with random local port)",
			svc: corev1.Service{
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{
						{
							Port:       80,
							TargetPort: intstr.FromInt32(8080),
						},
					},
				},
			},
			pod: corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Ports: []corev1.ContainerPort{
								{
									Name:          "http",
									ContainerPort: int32(8080)},
							},
						},
					},
				},
			},
			ports:      []string{":80"},
			translated: []string{":8080"},
			err:        false,
		},
		{
			name: "test success 1 (int port with explicit local port)",
			svc: corev1.Service{
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{
						{
							Port:       8080,
							TargetPort: intstr.FromInt32(8080),
						},
					},
				},
			},
			pod: corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Ports: []corev1.ContainerPort{
								{
									Name:          "http",
									ContainerPort: int32(8080)},
							},
						},
					},
				},
			},
			ports:      []string{"8000:8080"},
			translated: []string{"8000:8080"},
			err:        false,
		},
		{
			name: "test success 2 (clusterIP: None)",
			svc: corev1.Service{
				Spec: corev1.ServiceSpec{
					ClusterIP: "None",
					Ports: []corev1.ServicePort{
						{
							Port:       80,
							TargetPort: intstr.FromInt32(8080),
						},
					},
				},
			},
			pod: corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Ports: []corev1.ContainerPort{
								{
									Name:          "http",
									ContainerPort: int32(8080)},
							},
						},
					},
				},
			},
			ports:      []string{"80"},
			translated: []string{"80"},
			err:        false,
		},
		{
			name: "test success 2 (clusterIP: None with random local port)",
			svc: corev1.Service{
				Spec: corev1.ServiceSpec{
					ClusterIP: "None",
					Ports: []corev1.ServicePort{
						{
							Port:       80,
							TargetPort: intstr.FromInt32(8080),
						},
					},
				},
			},
			pod: corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Ports: []corev1.ContainerPort{
								{
									Name:          "http",
									ContainerPort: int32(8080)},
							},
						},
					},
				},
			},
			ports:      []string{":80"},
			translated: []string{":80"},
			err:        false,
		},
		{
			name: "test success 3 (named target port)",
			svc: corev1.Service{
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{
						{
							Port:       80,
							TargetPort: intstr.FromString("http"),
						},
						{
							Port:       443,
							TargetPort: intstr.FromString("https"),
						},
					},
				},
			},
			pod: corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Ports: []corev1.ContainerPort{
								{
									Name:          "http",
									ContainerPort: int32(8080)},
								{
									Name:          "https",
									ContainerPort: int32(8443)},
							},
						},
					},
				},
			},
			ports:      []string{"80", "443"},
			translated: []string{"80:8080", "443:8443"},
			err:        false,
		},
		{
			name: "test success 3 (named target port with random local port)",
			svc: corev1.Service{
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{
						{
							Port:       80,
							TargetPort: intstr.FromString("http"),
						},
						{
							Port:       443,
							TargetPort: intstr.FromString("https"),
						},
					},
				},
			},
			pod: corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Ports: []corev1.ContainerPort{
								{
									Name:          "http",
									ContainerPort: int32(8080)},
								{
									Name:          "https",
									ContainerPort: int32(8443)},
							},
						},
					},
				},
			},
			ports:      []string{":80", ":443"},
			translated: []string{":8080", ":8443"},
			err:        false,
		},
		{
			name: "test success 4 (named service port)",
			svc: corev1.Service{
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{
						{
							Port:       80,
							Name:       "http",
							TargetPort: intstr.FromInt32(8080),
						},
						{
							Port:       443,
							Name:       "https",
							TargetPort: intstr.FromInt32(8443),
						},
					},
				},
			},
			pod: corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Ports: []corev1.ContainerPort{
								{
									ContainerPort: int32(8080)},
								{
									ContainerPort: int32(8443)},
							},
						},
					},
				},
			},
			ports:      []string{"http", "https"},
			translated: []string{"80:8080", "443:8443"},
			err:        false,
		},
		{
			name: "test success 4 (named service port with random local port)",
			svc: corev1.Service{
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{
						{
							Port:       80,
							Name:       "http",
							TargetPort: intstr.FromInt32(8080),
						},
						{
							Port:       443,
							Name:       "https",
							TargetPort: intstr.FromInt32(8443),
						},
					},
				},
			},
			pod: corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Ports: []corev1.ContainerPort{
								{
									ContainerPort: int32(8080)},
								{
									ContainerPort: int32(8443)},
							},
						},
					},
				},
			},
			ports:      []string{":http", ":https"},
			translated: []string{":8080", ":8443"},
			err:        false,
		},
		{
			name: "test success 4 (named service port and named pod container port)",
			svc: corev1.Service{
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{
						{
							Port:       80,
							Name:       "http",
							TargetPort: intstr.FromString("http"),
						},
					},
				},
			},
			pod: corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Ports: []corev1.ContainerPort{
								{
									Name:          "http",
									ContainerPort: int32(80)},
							},
						},
					},
				},
			},
			ports:      []string{"http"},
			translated: []string{"80"},
			err:        false,
		},
		{
			name: "test success (targetPort omitted)",
			svc: corev1.Service{
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{
						{
							Port: 80,
						},
					},
				},
			},
			pod: corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Ports: []corev1.ContainerPort{
								{
									Name:          "http",
									ContainerPort: int32(80)},
							},
						},
					},
				},
			},
			ports:      []string{"80"},
			translated: []string{"80"},
			err:        false,
		},
		{
			name: "test success (targetPort omitted with random local port)",
			svc: corev1.Service{
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{
						{
							Port: 80,
						},
					},
				},
			},
			pod: corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Ports: []corev1.ContainerPort{
								{
									Name:          "http",
									ContainerPort: int32(80)},
							},
						},
					},
				},
			},
			ports:      []string{":80"},
			translated: []string{":80"},
			err:        false,
		},
		{
			name: "test failure 1 (named target port lookup failure)",
			svc: corev1.Service{
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{
						{
							Port:       80,
							TargetPort: intstr.FromString("http"),
						},
					},
				},
			},
			pod: corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Ports: []corev1.ContainerPort{
								{
									Name:          "https",
									ContainerPort: int32(443)},
							},
						},
					},
				},
			},
			ports:      []string{"80"},
			translated: []string{},
			err:        true,
		},
		{
			name: "test failure 1 (named service port lookup failure)",
			svc: corev1.Service{
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{
						{
							Port:       80,
							TargetPort: intstr.FromString("http"),
						},
					},
				},
			},
			pod: corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Ports: []corev1.ContainerPort{
								{
									Name:          "http",
									ContainerPort: int32(8080)},
							},
						},
					},
				},
			},
			ports:      []string{"https"},
			translated: []string{},
			err:        true,
		},
		{
			name: "test failure 2 (service port not declared)",
			svc: corev1.Service{
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{
						{
							Port:       80,
							TargetPort: intstr.FromString("http"),
						},
					},
				},
			},
			pod: corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Ports: []corev1.ContainerPort{
								{
									Name:          "https",
									ContainerPort: int32(443)},
							},
						},
					},
				},
			},
			ports:      []string{"443"},
			translated: []string{},
			err:        true,
		},
	}

	for _, tc := range cases {
		translated, err := translateServicePortToTargetPort(tc.ports, tc.svc, tc.pod)
		if err != nil {
			if tc.err {
				continue
			}

			t.Errorf("%v: unexpected error: %v", tc.name, err)
			continue
		}

		if tc.err {
			t.Errorf("%v: unexpected success", tc.name)
			continue
		}

		if !reflect.DeepEqual(translated, tc.translated) {
			t.Errorf("%v: expected %v; got %v", tc.name, tc.translated, translated)
		}
	}
}

func execPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "test", ResourceVersion: "10"},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyAlways,
			DNSPolicy:     corev1.DNSClusterFirst,
			Containers: []corev1.Container{
				{
					Name: "bar",
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
	}
}

func TestConvertPodNamedPortToNumber(t *testing.T) {
	cases := []struct {
		name      string
		pod       corev1.Pod
		ports     []string
		converted []string
		err       bool
	}{
		{
			name: "port number without local port",
			pod: corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Ports: []corev1.ContainerPort{
								{
									Name:          "http",
									ContainerPort: int32(80)},
							},
						},
					},
				},
			},
			ports:     []string{"80"},
			converted: []string{"80"},
			err:       false,
		},
		{
			name: "port number with local port",
			pod: corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Ports: []corev1.ContainerPort{
								{
									Name:          "http",
									ContainerPort: int32(80)},
							},
						},
					},
				},
			},
			ports:     []string{"8000:80"},
			converted: []string{"8000:80"},
			err:       false,
		},
		{
			name: "port number with random local port",
			pod: corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Ports: []corev1.ContainerPort{
								{
									Name:          "http",
									ContainerPort: int32(80)},
							},
						},
					},
				},
			},
			ports:     []string{":80"},
			converted: []string{":80"},
			err:       false,
		},
		{
			name: "named port without local port",
			pod: corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Ports: []corev1.ContainerPort{
								{
									Name:          "http",
									ContainerPort: int32(80)},
							},
						},
					},
				},
			},
			ports:     []string{"http"},
			converted: []string{"80"},
			err:       false,
		},
		{
			name: "named port with local port",
			pod: corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Ports: []corev1.ContainerPort{
								{
									Name:          "http",
									ContainerPort: int32(80)},
							},
						},
					},
				},
			},
			ports:     []string{"8000:http"},
			converted: []string{"8000:80"},
			err:       false,
		},
		{
			name: "named port with random local port",
			pod: corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Ports: []corev1.ContainerPort{
								{
									Name:          "http",
									ContainerPort: int32(80)},
							},
						},
					},
				},
			},
			ports:     []string{":http"},
			converted: []string{":80"},
			err:       false,
		},
		{
			name: "named port can not be found",
			pod: corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Ports: []corev1.ContainerPort{
								{
									Name:          "https",
									ContainerPort: int32(443)},
							},
						},
					},
				},
			},
			ports: []string{"http"},
			err:   true,
		},
		{
			name: "one of the requested named ports can not be found",
			pod: corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Ports: []corev1.ContainerPort{
								{
									Name:          "https",
									ContainerPort: int32(443)},
							},
						},
					},
				},
			},
			ports: []string{"https", "http"},
			err:   true,
		},
	}

	for _, tc := range cases {
		converted, err := convertPodNamedPortToNumber(tc.ports, tc.pod)
		if err != nil {
			if tc.err {
				continue
			}

			t.Errorf("%v: unexpected error: %v", tc.name, err)
			continue
		}

		if tc.err {
			t.Errorf("%v: unexpected success", tc.name)
			continue
		}

		if !reflect.DeepEqual(converted, tc.converted) {
			t.Errorf("%v: expected %v; got %v", tc.name, tc.converted, converted)
		}
	}
}

func TestCheckUDPPort(t *testing.T) {
	tests := []struct {
		name        string
		pod         *corev1.Pod
		service     *corev1.Service
		ports       []string
		expectError bool
	}{
		{
			name: "forward to a UDP port in a Pod",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Ports: []corev1.ContainerPort{
								{Protocol: corev1.ProtocolUDP, ContainerPort: 53},
							},
						},
					},
				},
			},
			ports:       []string{"53"},
			expectError: true,
		},
		{
			name: "forward to a named UDP port in a Pod",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Ports: []corev1.ContainerPort{
								{Protocol: corev1.ProtocolUDP, ContainerPort: 53, Name: "dns"},
							},
						},
					},
				},
			},
			ports:       []string{"dns"},
			expectError: true,
		},
		{
			name: "Pod has ports with both TCP and UDP protocol (UDP first)",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Ports: []corev1.ContainerPort{
								{Protocol: corev1.ProtocolUDP, ContainerPort: 53},
								{Protocol: corev1.ProtocolTCP, ContainerPort: 53},
							},
						},
					},
				},
			},
			ports: []string{":53"},
		},
		{
			name: "Pod has ports with both TCP and UDP protocol (TCP first)",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Ports: []corev1.ContainerPort{
								{Protocol: corev1.ProtocolTCP, ContainerPort: 53},
								{Protocol: corev1.ProtocolUDP, ContainerPort: 53},
							},
						},
					},
				},
			},
			ports: []string{":53"},
		},

		{
			name: "forward to a UDP port in a Service",
			service: &corev1.Service{
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{
						{Protocol: corev1.ProtocolUDP, Port: 53},
					},
				},
			},
			ports:       []string{"53"},
			expectError: true,
		},
		{
			name: "forward to a named UDP port in a Service",
			service: &corev1.Service{
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{
						{Protocol: corev1.ProtocolUDP, Port: 53, Name: "dns"},
					},
				},
			},
			ports:       []string{"10053:dns"},
			expectError: true,
		},
		{
			name: "Service has ports with both TCP and UDP protocol (UDP first)",
			service: &corev1.Service{
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{
						{Protocol: corev1.ProtocolUDP, Port: 53},
						{Protocol: corev1.ProtocolTCP, Port: 53},
					},
				},
			},
			ports: []string{"53"},
		},
		{
			name: "Service has ports with both TCP and UDP protocol (TCP first)",
			service: &corev1.Service{
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{
						{Protocol: corev1.ProtocolTCP, Port: 53},
						{Protocol: corev1.ProtocolUDP, Port: 53},
					},
				},
			},
			ports: []string{"53"},
		},
	}
	for _, tc := range tests {
		var err error
		if tc.pod != nil {
			err = checkUDPPortInPod(tc.ports, tc.pod)
		} else if tc.service != nil {
			err = checkUDPPortInService(tc.ports, tc.service)
		}
		if err != nil {
			if tc.expectError {
				continue
			}
			t.Errorf("%v: unexpected error: %v", tc.name, err)
			continue
		}
		if tc.expectError {
			t.Errorf("%v: unexpected success", tc.name)
		}
	}
}
