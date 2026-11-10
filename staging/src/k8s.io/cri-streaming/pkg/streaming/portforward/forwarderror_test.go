/*
Copyright The Kubernetes Authors.

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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"syscall"
	"testing"

	"k8s.io/streaming/pkg/portforward/pferrors"
)

type errForwarder struct{ err error }

func (f errForwarder) PortForward(context.Context, string, string, int32, io.ReadWriteCloser) error {
	return f.err
}

type captureStream struct {
	fakeHTTPStream
	buf bytes.Buffer
}

func (s *captureStream) Write(p []byte) (int, error) { return s.buf.Write(p) }

type nopRWC struct{ io.ReadWriter }

func (nopRWC) Close() error { return nil }

type nopWC struct{ io.Writer }

func (nopWC) Close() error { return nil }

var forwardErrorCases = map[string]struct {
	err  error
	want pferrors.Reason
}{
	"typed sandbox loss":      {pferrors.NewSandboxNotAvailable(errors.New("netns closed")), pferrors.ReasonSandboxNotAvailable},
	"typed refused":           {pferrors.NewTargetConnectionRefused(errors.New("dial")), pferrors.ReasonTargetConnectionRefused},
	"typed reset":             {pferrors.NewTargetConnectionReset(errors.New("copy")), pferrors.ReasonTargetConnectionReset},
	"untyped wrapped reset":   {fmt.Errorf("copy: %w", syscall.ECONNRESET), pferrors.ReasonTargetConnectionReset},
	"untyped wrapped refused": {fmt.Errorf("dial: %w", syscall.ECONNREFUSED), pferrors.ReasonTargetConnectionRefused},
	"unclassified":            {errors.New("something"), pferrors.ReasonUnknown},
}

func TestNewForwardError(t *testing.T) {
	for name, tc := range forwardErrorCases {
		t.Run(name, func(t *testing.T) {
			err := newForwardError(80, "pod", "uid", tc.err)
			if !errors.Is(err, tc.err) {
				t.Error("original error is not wrapped")
			}
			if got := pferrors.Classify(err); got != tc.want {
				t.Errorf("Classify = %q, want %q", got, tc.want)
			}
			if got := pferrors.ClassifyMessage(err.Error()); got != tc.want {
				t.Errorf("ClassifyMessage(%q) = %q, want %q", err.Error(), got, tc.want)
			}
		})
	}
}

func TestHTTPStreamPortForwardWritesClassifiedError(t *testing.T) {
	for name, tc := range forwardErrorCases {
		t.Run(name, func(t *testing.T) {
			data := &captureStream{fakeHTTPStream: *newFakeHTTPStream()}
			errStream := &captureStream{fakeHTTPStream: *newFakeHTTPStream()}
			data.headers.Set(PortHeader, "80")
			h := &httpStreamHandler{pod: "pod", uid: "uid", forwarder: errForwarder{tc.err}}

			h.portForward(&httpStreamPair{dataStream: data, errorStream: errStream})

			if got := pferrors.ClassifyMessage(errStream.buf.String()); got != tc.want {
				t.Errorf("error stream %q classified as %q, want %q", errStream.buf.String(), got, tc.want)
			}
		})
	}
}

func TestWebSocketPortForwardWritesClassifiedError(t *testing.T) {
	for name, tc := range forwardErrorCases {
		t.Run(name, func(t *testing.T) {
			var errStream bytes.Buffer
			h := &websocketStreamHandler{pod: "pod", uid: "uid", forwarder: errForwarder{tc.err}}

			h.portForward(&websocketStreamPair{
				port:        80,
				dataStream:  nopRWC{&bytes.Buffer{}},
				errorStream: nopWC{&errStream},
			})

			if got := pferrors.ClassifyMessage(errStream.String()); got != tc.want {
				t.Errorf("error stream %q classified as %q, want %q", errStream.String(), got, tc.want)
			}
		})
	}
}
