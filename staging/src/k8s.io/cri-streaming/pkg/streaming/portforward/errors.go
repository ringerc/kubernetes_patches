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

import "k8s.io/streaming/pkg/portforward/pferrors"

// Classified errors that a PortForwarder should return, re-exported so that
// runtime implementations need only this package.
type (
	Reason = pferrors.Reason
	Error  = pferrors.Error
)

var (
	NewSandboxNotAvailable     = pferrors.NewSandboxNotAvailable
	NewTargetConnectionRefused = pferrors.NewTargetConnectionRefused
	NewTargetConnectionReset   = pferrors.NewTargetConnectionReset

	ErrSandboxNotAvailable     = pferrors.ErrSandboxNotAvailable
	ErrTargetConnectionRefused = pferrors.ErrTargetConnectionRefused
	ErrTargetConnectionReset   = pferrors.ErrTargetConnectionReset
)
