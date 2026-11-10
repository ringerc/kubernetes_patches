/*
Copyright 2016 The Kubernetes Authors.

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
	"io"
	"net/http"
	"time"

	"k8s.io/streaming/pkg/httpstream/wsstream"
	"k8s.io/streaming/pkg/portforward/pferrors"
	"k8s.io/streaming/pkg/runtime"
)

// PortForwarder knows how to forward content from a data stream to/from a port
// in a pod.
type PortForwarder interface {
	// PortForwarder copies data between a data stream and a port in a pod.
	//
	// Implementations should wrap failures with the constructors in
	// k8s.io/streaming/pkg/portforward/pferrors, so that clients can tell the
	// loss of the pod sandbox from the failure of a single connection to the
	// target. Errors wrapping ECONNREFUSED or ECONNRESET are classified
	// automatically. Any error ends only the stream pair it occurred on.
	PortForward(ctx context.Context, name string, uid string, port int32, stream io.ReadWriteCloser) error
}

// ServePortForward handles a port forwarding request.  A single request is
// kept alive as long as the client is still alive and the connection has not
// been timed out due to idleness. This function handles multiple forwarded
// connections; i.e., multiple `curl http://localhost:8888/` requests will be
// handled by a single invocation of ServePortForward.
func ServePortForward(w http.ResponseWriter, req *http.Request, portForwarder PortForwarder, podName string, uid string, portForwardOptions *V4Options, idleTimeout time.Duration, streamCreationTimeout time.Duration, supportedProtocols []string) {
	var err error
	if wsstream.IsWebSocketRequest(req) {
		err = handleWebSocketStreams(req, w, portForwarder, podName, uid, portForwardOptions, supportedProtocols, idleTimeout, streamCreationTimeout)
	} else {
		err = handleHTTPStreams(req, w, portForwarder, podName, uid, supportedProtocols, idleTimeout, streamCreationTimeout)
	}

	if err != nil {
		runtime.HandleError(err)
		return
	}
}

// newForwardError describes a failed forward for the client's error stream.
// The text of a classified error follows the colon, so clients can match it
// with pferrors.ClassifyMessage. The result wraps err.
func newForwardError(port int32, pod, uid string, err error) error {
	return fmt.Errorf("error forwarding port %d to pod %s, uid %v: %w", port, pod, uid, pferrors.Wrap(err))
}
