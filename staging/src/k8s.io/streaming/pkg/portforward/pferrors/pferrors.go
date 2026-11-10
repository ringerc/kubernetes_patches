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

// Package pferrors defines classified errors for port forwarding.
//
// A runtime's PortForward implementation wraps failures in these types so the
// streaming server can tell a failure of one forwarded connection from the
// loss of the pod sandbox. The wire protocol carries only the error text, so
// the text of each type starts with a stable message that clients can match
// with ClassifyMessage.
package pferrors

import (
	"errors"
	"strings"
	"syscall"
)

// Reason classifies a port forwarding failure.
type Reason string

const (
	// ReasonUnknown means the failure is not classified, for example because
	// the runtime or server predates this package.
	ReasonUnknown Reason = ""
	// ReasonSandboxNotAvailable means the pod sandbox is gone, not ready, or
	// its network namespace is closed. No connection to the pod can work.
	ReasonSandboxNotAvailable Reason = "SandboxNotAvailable"
	// ReasonTargetConnectionRefused means the pod refused the connection to
	// the target port. Other connections may still work.
	ReasonTargetConnectionRefused Reason = "TargetConnectionRefused"
	// ReasonTargetConnectionReset means the target reset or dropped an
	// established connection. Other connections may still work.
	ReasonTargetConnectionReset Reason = "TargetConnectionReset"
)

// Messages that start the text of the corresponding errors. They are part of
// the API: clients match them in error stream text.
const (
	MessageSandboxNotAvailable     = "pod sandbox not available"
	MessageTargetConnectionRefused = "target connection refused"
	MessageTargetConnectionReset   = "target connection reset"
)

var messages = map[Reason]string{
	ReasonSandboxNotAvailable:     MessageSandboxNotAvailable,
	ReasonTargetConnectionRefused: MessageTargetConnectionRefused,
	ReasonTargetConnectionReset:   MessageTargetConnectionReset,
}

// Sentinels for use with errors.Is. They match any Error with the same Reason.
var (
	ErrSandboxNotAvailable     error = &Error{Reason: ReasonSandboxNotAvailable}
	ErrTargetConnectionRefused error = &Error{Reason: ReasonTargetConnectionRefused}
	ErrTargetConnectionReset   error = &Error{Reason: ReasonTargetConnectionReset}
)

// Error is a classified port forwarding failure. It wraps the underlying
// cause, so errors.Is and errors.As keep working on it.
type Error struct {
	Reason Reason
	// Err is the underlying cause, if any.
	Err error
}

func (e *Error) Error() string {
	msg := messages[e.Reason]
	if e.Err == nil {
		return msg
	}
	return msg + ": " + e.Err.Error()
}

func (e *Error) Unwrap() error { return e.Err }

// Is reports whether target is an *Error with the same Reason, so that
// errors.Is(err, ErrTargetConnectionReset) works regardless of the cause.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Reason == e.Reason
}

// NewSandboxNotAvailable wraps cause as a ReasonSandboxNotAvailable error.
func NewSandboxNotAvailable(cause error) error {
	return &Error{Reason: ReasonSandboxNotAvailable, Err: cause}
}

// NewTargetConnectionRefused wraps cause as a ReasonTargetConnectionRefused error.
func NewTargetConnectionRefused(cause error) error {
	return &Error{Reason: ReasonTargetConnectionRefused, Err: cause}
}

// NewTargetConnectionReset wraps cause as a ReasonTargetConnectionReset error.
func NewTargetConnectionReset(cause error) error {
	return &Error{Reason: ReasonTargetConnectionReset, Err: cause}
}

// Classify returns the Reason for err. A classified error anywhere in the
// chain wins. Otherwise ECONNREFUSED and ECONNRESET in the chain are mapped,
// which classifies errors from runtimes that wrap with %w but do not use this
// package. Sandbox loss cannot be detected that way.
func Classify(err error) Reason {
	if err == nil {
		return ReasonUnknown
	}
	var typed *Error
	if errors.As(err, &typed) {
		return typed.Reason
	}
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return ReasonTargetConnectionRefused
	case errors.Is(err, syscall.ECONNRESET):
		return ReasonTargetConnectionReset
	}
	return ReasonUnknown
}

// Wrap returns err as a classified error if Classify finds a reason for it
// and it is not one already. Other errors are returned unchanged.
func Wrap(err error) error {
	if err == nil {
		return nil
	}
	var typed *Error
	if errors.As(err, &typed) {
		return err
	}
	if reason := Classify(err); reason != ReasonUnknown {
		return &Error{Reason: reason, Err: err}
	}
	return err
}

// ClassifyMessage returns the Reason whose message appears in msg, the text
// read from a port forward error stream, or ReasonUnknown.
func ClassifyMessage(msg string) Reason {
	for _, reason := range []Reason{ReasonSandboxNotAvailable, ReasonTargetConnectionRefused, ReasonTargetConnectionReset} {
		if strings.Contains(msg, messages[reason]) {
			return reason
		}
	}
	return ReasonUnknown
}

// SessionFatal reports whether a failure with this reason means the whole
// forwarding session is unusable. Unknown reasons are fatal, as they were
// before failures were classified.
func (r Reason) SessionFatal() bool {
	switch r {
	case ReasonTargetConnectionRefused, ReasonTargetConnectionReset:
		return false
	}
	return true
}

// IsSandboxNotAvailable reports whether err is classified as ReasonSandboxNotAvailable.
func IsSandboxNotAvailable(err error) bool { return Classify(err) == ReasonSandboxNotAvailable }

// IsTargetConnectionRefused reports whether err is classified as ReasonTargetConnectionRefused.
func IsTargetConnectionRefused(err error) bool {
	return Classify(err) == ReasonTargetConnectionRefused
}

// IsTargetConnectionReset reports whether err is classified as ReasonTargetConnectionReset.
func IsTargetConnectionReset(err error) bool { return Classify(err) == ReasonTargetConnectionReset }
