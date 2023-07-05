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
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"syscall"
	"os/signal"
	"strconv"
	"strings"
	"time"
	"io"
	"encoding/json"

	"github.com/spf13/cobra"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/client-go/kubernetes/scheme"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/polymorphichelpers"
	"k8s.io/kubectl/pkg/util"
	"k8s.io/kubectl/pkg/util/completion"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"
	"k8s.io/klog/v2"
)

// PortForwardOptions contains all the options for running the port-forward cli command.
type PortForwardOptions struct {
	Namespace     string
	PodName       string
	RESTClient    *restclient.RESTClient
	Config        *restclient.Config
	PodClient     corev1client.PodsGetter
	Address       []string
	Ports         []string
	PortNames     []string
	PortForwarder portForwarder
	StopChannel   chan struct{}
	ReadyChannel  chan struct{}
	PortsFile     string
	genericiooptions.IOStreams
}

var (
	portforwardLong = templates.LongDesc(i18n.T(`
                Forward one or more local ports to a pod.

                Use resource type/name such as deployment/mydeployment to select a pod. Resource type defaults to 'pod' if omitted.

                If there are multiple pods matching the criteria, a pod will be selected automatically. The
                forwarding session ends when the selected pod terminates, and a rerun of the command is needed
                to resume forwarding.`))

	portforwardExample = templates.Examples(i18n.T(`
		# Listen on ports 5000 and 6000 locally, forwarding data to/from ports 5000 and 6000 in the pod
		kubectl port-forward pod/mypod 5000 6000

		# Listen on ports 5000 and 6000 locally, forwarding data to/from ports 5000 and 6000 in a pod selected by the deployment
		kubectl port-forward deployment/mydeployment 5000 6000

		# Listen on port 8443 locally, forwarding to the targetPort of the service's port named "https" in a pod selected by the service
		kubectl port-forward service/myservice 8443:https

		# Listen on port 8888 locally, forwarding to 5000 in the pod
		kubectl port-forward pod/mypod 8888:5000

		# Listen on port 8888 on all addresses, forwarding to 5000 in the pod
		kubectl port-forward --address 0.0.0.0 pod/mypod 8888:5000

		# Listen on port 8888 on localhost and selected IP, forwarding to 5000 in the pod
		kubectl port-forward --address localhost,10.19.21.23 pod/mypod 8888:5000

		# Listen on a random port locally, forwarding to 5000 in the pod
		kubectl port-forward pod/mypod :5000`))
)

const (
	// Amount of time to wait until at least one pod is running
	defaultPodPortForwardWaitTimeout = 60 * time.Second
)

func NewCmdPortForward(f cmdutil.Factory, streams genericiooptions.IOStreams) *cobra.Command {
	return NewCmdPortForwardWithOpts(f, streams, &PortForwardOptions{
			PortForwarder: &defaultPortForwarder{
				IOStreams: streams,
			},
			IOStreams: streams,
		});
}

func NewCmdPortForwardWithOpts(f cmdutil.Factory, streams genericiooptions.IOStreams, opts *PortForwardOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:                   "port-forward TYPE/NAME [options] [LOCAL_PORT:]REMOTE_PORT [...[LOCAL_PORT_N:]REMOTE_PORT_N]",
		DisableFlagsInUseLine: true,
		Short:                 i18n.T("Forward one or more local ports to a pod"),
		Long:                  portforwardLong,
		Example:               portforwardExample,
		ValidArgsFunction:     completion.PodResourceNameCompletionFunc(f),
		Run: func(cmd *cobra.Command, args []string) {
			cmdutil.CheckErr(opts.Complete(f, cmd, args))
			cmdutil.CheckErr(opts.Validate())
			cmdutil.CheckErr(opts.RunPortForward())
		},
	}
	cmdutil.AddPodRunningTimeoutFlag(cmd, defaultPodPortForwardWaitTimeout)
	cmd.Flags().StringSliceVar(&opts.Address, "address", []string{"localhost"}, "Addresses to listen on (comma separated). Only accepts IP addresses or localhost as a value. When localhost is supplied, kubectl will try to bind on both 127.0.0.1 and ::1 and will fail if neither of these addresses are available to bind.")
	cmd.Flags().StringVar(&opts.PortsFile, "ports-file", "", "Write a json-format list of mapped ports to this file. Use - for stdout. Only written once all requested ports are mapped.")
	cmd.SetOut(streams.Out)
	cmd.SetErr(streams.ErrOut)
	// TODO support UID
	return cmd
}

// adapter for client-go tools.portforward.ForwardedPort
type forwardedPort struct {
	LocalPort  uint16
	LocalProtocol string
	LocalAddress string
	RemotePort uint16
	// To be filled locally?
	RemotePortName string
}

type portForwarder interface {
	ForwardPorts(method string, url *url.URL, opts PortForwardOptions) error
	GetPortMappings() ([]forwardedPort, error)
}

type defaultPortForwarder struct {
	fw *portforward.PortForwarder
	genericiooptions.IOStreams
}

func (f *defaultPortForwarder) ForwardPorts(method string, url *url.URL, opts PortForwardOptions) error {
	transport, upgrader, err := spdy.RoundTripperFor(opts.Config)
	if err != nil {
		return err
	}
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, method, url)
	f.fw, err = portforward.NewOnAddresses(dialer, opts.Address, opts.Ports, opts.StopChannel, opts.ReadyChannel, f.Out, f.ErrOut)
	if err != nil {
		return err
	}
	return f.fw.ForwardPorts()
}

// wrap portforward.GetPortMappings for the mockable portforwarder abstraction
func (f *defaultPortForwarder) GetPortMappings() ([]forwardedPort, error) {
	ports, err := f.fw.GetPortMappings()
	if err != nil {
		return []forwardedPort{}, err
	}
	r := make([]forwardedPort, len(ports))
	for i := range ports {
		r[i].LocalPort = ports[i].LocalPort
		r[i].LocalProtocol = ports[i].LocalProtocol
		r[i].LocalAddress = ports[i].LocalAddress
		r[i].RemotePort = ports[i].RemotePort
	}
	return r, nil
}

// splitPort splits port string which is in form of [LOCAL PORT]:REMOTE PORT
// and returns local and remote ports separately
func splitPort(port string) (local, remote string) {
	parts := strings.Split(port, ":")
	if len(parts) == 2 {
		return parts[0], parts[1]
	}

	return parts[0], parts[0]
}

// Translates service port to target port
// It rewrites ports as needed if the Service port declares targetPort.
// It returns an error when a named targetPort can't find a match in the pod, or the Service did not declare
// the port.
func translateServicePortToTargetPort(ports []string, svc corev1.Service, pod corev1.Pod) ([]string, error) {
	var translated []string
	for _, port := range ports {
		localPort, remotePort := splitPort(port)

		portnum, err := strconv.Atoi(remotePort)
		if err != nil {
			svcPort, err := util.LookupServicePortNumberByName(svc, remotePort)
			if err != nil {
				return nil, err
			}
			portnum = int(svcPort)

			if localPort == remotePort {
				localPort = strconv.Itoa(portnum)
			}
		}
		containerPort, err := util.LookupContainerPortNumberByServicePort(svc, pod, int32(portnum))
		if err != nil {
			// can't resolve a named port, or Service did not declare this port, return an error
			return nil, err
		}

		// convert the resolved target port back to a string
		remotePort = strconv.Itoa(int(containerPort))

		if localPort != remotePort {
			translated = append(translated, fmt.Sprintf("%s:%s", localPort, remotePort))
		} else {
			translated = append(translated, remotePort)
		}
	}
	return translated, nil
}

// convertPodNamedPortToNumber converts named ports into port numbers
// It returns an error when a named port can't be found in the pod containers
func convertPodNamedPortToNumber(ports []string, pod corev1.Pod) ([]string, error) {
	var converted []string
	for _, port := range ports {
		localPort, remotePort := splitPort(port)

		containerPortStr := remotePort
		_, err := strconv.Atoi(remotePort)
		if err != nil {
			containerPort, err := util.LookupContainerPortNumberByName(pod, remotePort)
			if err != nil {
				return nil, err
			}

			containerPortStr = strconv.Itoa(int(containerPort))
		}

		if localPort != remotePort {
			converted = append(converted, fmt.Sprintf("%s:%s", localPort, containerPortStr))
		} else {
			converted = append(converted, containerPortStr)
		}
	}

	return converted, nil
}

func checkUDPPorts(udpOnlyPorts sets.Int, ports []string, obj metav1.Object) error {
	for _, port := range ports {
		_, remotePort := splitPort(port)
		portNum, err := strconv.Atoi(remotePort)
		if err != nil {
			switch v := obj.(type) {
			case *corev1.Service:
				svcPort, err := util.LookupServicePortNumberByName(*v, remotePort)
				if err != nil {
					return err
				}
				portNum = int(svcPort)

			case *corev1.Pod:
				ctPort, err := util.LookupContainerPortNumberByName(*v, remotePort)
				if err != nil {
					return err
				}
				portNum = int(ctPort)

			default:
				return fmt.Errorf("unknown object: %v", obj)
			}
		}
		if udpOnlyPorts.Has(portNum) {
			return fmt.Errorf("UDP protocol is not supported for %s", remotePort)
		}
	}
	return nil
}

// checkUDPPortInService returns an error if remote port in Service is a UDP port
// TODO: remove this check after #47862 is solved
func checkUDPPortInService(ports []string, svc *corev1.Service) error {
	udpPorts := sets.NewInt()
	tcpPorts := sets.NewInt()
	for _, port := range svc.Spec.Ports {
		portNum := int(port.Port)
		switch port.Protocol {
		case corev1.ProtocolUDP:
			udpPorts.Insert(portNum)
		case corev1.ProtocolTCP:
			tcpPorts.Insert(portNum)
		}
	}
	return checkUDPPorts(udpPorts.Difference(tcpPorts), ports, svc)
}

// checkUDPPortInPod returns an error if remote port in Pod is a UDP port
// TODO: remove this check after #47862 is solved
func checkUDPPortInPod(ports []string, pod *corev1.Pod) error {
	udpPorts := sets.NewInt()
	tcpPorts := sets.NewInt()
	for _, ct := range pod.Spec.Containers {
		for _, ctPort := range ct.Ports {
			portNum := int(ctPort.ContainerPort)
			switch ctPort.Protocol {
			case corev1.ProtocolUDP:
				udpPorts.Insert(portNum)
			case corev1.ProtocolTCP:
				tcpPorts.Insert(portNum)
			}
		}
	}
	return checkUDPPorts(udpPorts.Difference(tcpPorts), ports, pod)
}

// Complete completes all the required options for port-forward cmd.
func (o *PortForwardOptions) Complete(f cmdutil.Factory, cmd *cobra.Command, args []string) error {
	var err error
	if len(args) < 2 {
		return cmdutil.UsageErrorf(cmd, "TYPE/NAME and list of ports are required for port-forward")
	}

	o.Namespace, _, err = f.ToRawKubeConfigLoader().Namespace()
	if err != nil {
		return err
	}

	builder := f.NewBuilder().
		WithScheme(scheme.Scheme, scheme.Scheme.PrioritizedVersionsAllGroups()...).
		ContinueOnError().
		NamespaceParam(o.Namespace).DefaultNamespace()

	getPodTimeout, err := cmdutil.GetPodRunningTimeoutFlag(cmd)
	if err != nil {
		return cmdutil.UsageErrorf(cmd, err.Error())
	}

	resourceName := args[0]
	builder.ResourceNames("pods", resourceName)

	obj, err := builder.Do().Object()
	if err != nil {
		return err
	}

	forwardablePod, err := polymorphichelpers.AttachablePodForObjectFn(f, obj, getPodTimeout)
	if err != nil {
		return err
	}

	o.PodName = forwardablePod.Name
	// Record original ports before service and named-to-numeric translation
	o.PortNames = args[1:]

	// handle service port mapping to target port if needed
	switch t := obj.(type) {
	case *corev1.Service:
		err = checkUDPPortInService(args[1:], t)
		if err != nil {
			return err
		}
		o.Ports, err = translateServicePortToTargetPort(args[1:], *t, *forwardablePod)
		if err != nil {
			return err
		}
	default:
		err = checkUDPPortInPod(args[1:], forwardablePod)
		if err != nil {
			return err
		}
		o.Ports, err = convertPodNamedPortToNumber(args[1:], *forwardablePod)
		if err != nil {
			return err
		}
	}

	clientset, err := f.KubernetesClientSet()
	if err != nil {
		return err
	}

	o.PodClient = clientset.CoreV1()

	o.Config, err = f.ToRESTConfig()
	if err != nil {
		return err
	}
	o.RESTClient, err = f.RESTClient()
	if err != nil {
		return err
	}

	o.StopChannel = make(chan struct{}, 1)
	o.ReadyChannel = make(chan struct{})
	return nil
}

// Validate validates all the required options for port-forward cmd.
func (o PortForwardOptions) Validate() error {
	if len(o.PodName) == 0 {
		return fmt.Errorf("pod name or resource type/name must be specified")
	}

	if len(o.Ports) < 1 {
		return fmt.Errorf("at least 1 PORT is required for port-forward")
	}

	if o.PortForwarder == nil || o.PodClient == nil || o.RESTClient == nil || o.Config == nil {
		return fmt.Errorf("client, client config, restClient, and portforwarder must be provided")
	}
	return nil
}

// Wait for the port forwards to be established then write a port-forward
// mapping to the specified --ports-file (possibly stdout). Returns a channel
// that, if non-null, should be waited on until closed to ensure the ports-file
// is written.
func (o *PortForwardOptions) outputPortForwardsMapping() (chan struct{}, error) {
	var ow io.Writer
	var closer func()
	if o.PortsFile == "" {
		return nil, nil
	} else if o.PortsFile == "-" {
		ow = o.Out
		closer = func(){}
	} else if strings.HasPrefix(o.PortsFile, "/dev/fd/") {
		// There's no widespread convention for specifying a file descriptor
		// like there is "-" for stdout. So linux-style /dev/fd/{n}
		// strings are special-cased and opened as a direct file
		// descriptor. This ensures that kubectl will close the
		// inherited file descriptor when it finishes writing the port
		// mapping file, so the caller can read until EoF to detect
		// when the port mapping is up and fully available. If we used
		// os.OpenFile instead, a new fd would be allocated and only
		// the new fd, not the inherited fd, would be closed when the
		// write was complete.
		// This syntax will be accepted on all unix-like systems that
		// support file descriptors, whether or not they actually
		// support /dev/fd/{n} at the file system level.
		fdstr, _ := strings.CutPrefix(o.PortsFile, "/dev/fd/")
		fdno, err := strconv.Atoi(fdstr)
		if fdno < 3 {
			// Refuse to use stdin, stdout or stderr, since we close the fd
			// once the write completes.
			return nil, fmt.Errorf("refusing to open standard file descriptor \"%s\" for ports-file", fdstr)
		}
		if err != nil {
			return nil, fmt.Errorf("could not parse fd number \"%s\" from path \"%s\": %v", fdstr, o.PortsFile, err)
		}
		f := os.NewFile(uintptr(fdno), o.PortsFile)
		if f == nil {
			return nil, fmt.Errorf("could not open mapped ports file descriptor \"%s\": invalid file descriptor")
		}
		ow = f
		closer = func() {
			if err := f.Close(); err != nil {
				klog.V(2).Infof("error closing port mapping file descriptor: %v", err)
			}
		}
	} else {
		f, err := os.OpenFile(o.PortsFile, os.O_WRONLY|syscall.O_CREAT|os.O_TRUNC, 0777)
		if err != nil {
			return nil, fmt.Errorf("could not open mapped ports output file \"%s\"", o.PortsFile)
		}
		ow = f
		closer = func() {
			if err := f.Close(); err != nil {
				klog.V(2).Infof("error closing port mapping file: %v", err)
			}
		}
	}
	portsFileDoneChan := make(chan struct{})
	// Wait for port-forward readiness then print the ports info to the file
	go func() {
		defer close(portsFileDoneChan)
		defer closer()
		_ = <- o.ReadyChannel
		ports, err := o.PortForwarder.GetPortMappings()
		if err != nil {
			klog.V(2).Infof("error waiting for port-forward to be ready: %v\n", err)
			return
		}
		// The PortForwarder only knows about numeric port mappings
		// after port name resolution and service port remapping.
		// Find the originally requested port name or service port
		// number from the client so the client can easily determine
		// which output port mappings correspond to each port it
		// requested.
		for mappingIndex := range ports {
			// o.Ports and o.PortNames correspond 1:1 so find the
			// o.Port corresponding to the resulting mapping, and
			// look up the original name or port number.
			for i, p := range o.Ports {
				_, rs := splitPort(p)
				rp, _ := strconv.Atoi(rs)
				if int(ports[mappingIndex].RemotePort) == rp {
					_, rn := splitPort(o.PortNames[i])
					ports[mappingIndex].RemotePortName = rn
					break;
				}
			}
		}
		encoder := json.NewEncoder(ow)
		if err := encoder.Encode(ports); err != nil {
			klog.V(2).Infof("error writing port mapping: %v", err)
		}
	}();
	return portsFileDoneChan, nil
}

// RunPortForward implements all the necessary functionality for port-forward cmd.
func (o PortForwardOptions) RunPortForward() error {
	pod, err := o.PodClient.Pods(o.Namespace).Get(context.TODO(), o.PodName, metav1.GetOptions{})
	if err != nil {
		return err
	}

	if pod.Status.Phase != corev1.PodRunning {
		return fmt.Errorf("unable to forward port because pod is not running. Current status=%v", pod.Status.Phase)
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt)
	defer signal.Stop(signals)

	go func() {
		<-signals
		if o.StopChannel != nil {
			close(o.StopChannel)
		}
	}()

	req := o.RESTClient.Post().
		Resource("pods").
		Namespace(o.Namespace).
		Name(pod.Name).
		SubResource("portforward")

	// Wait for the mapping to be completed then write mapping to the
	// ports-file if port mapping output was requested.
	portsFileDoneChan, err := o.outputPortForwardsMapping()
	if err != nil {
		return err
	}

	// Blocks until signalled to cancel or error
	err = o.PortForwarder.ForwardPorts("POST", req.URL(), o)
	// Ensure ports-file was actually written. Mainly for tests where
	// the forwarder establishes a connection then immediately exits.
	if portsFileDoneChan != nil {
		_ = <- portsFileDoneChan
	}
	return err
}
