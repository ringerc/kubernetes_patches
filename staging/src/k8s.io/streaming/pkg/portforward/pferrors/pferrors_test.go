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

package pferrors

import (
	"errors"
	"fmt"
	"io"
	"syscall"
	"testing"
)

var allReasons = []struct {
	reason   Reason
	sentinel error
	prefix   string
	newErr   func(error) error
}{
	{ReasonSandboxNotAvailable, ErrSandboxNotAvailable, MessageSandboxNotAvailable, NewSandboxNotAvailable},
	{ReasonTargetConnectionRefused, ErrTargetConnectionRefused, MessageTargetConnectionRefused, NewTargetConnectionRefused},
	{ReasonTargetConnectionReset, ErrTargetConnectionReset, MessageTargetConnectionReset, NewTargetConnectionReset},
}

func TestTypedErrorWrapsCauseAndMatchesSentinel(t *testing.T) {
	cause := errors.New("boom")
	for _, tc := range allReasons {
		t.Run(string(tc.reason), func(t *testing.T) {
			err := tc.newErr(cause)
			if !errors.Is(err, tc.sentinel) {
				t.Errorf("errors.Is(err, sentinel) = false")
			}
			if !errors.Is(err, cause) {
				t.Errorf("errors.Is(err, cause) = false: cause not wrapped")
			}
			for _, other := range allReasons {
				if other.reason != tc.reason && errors.Is(err, other.sentinel) {
					t.Errorf("matches sentinel of %s", other.reason)
				}
			}
			var typed *Error
			if !errors.As(err, &typed) || typed.Reason != tc.reason {
				t.Errorf("errors.As: got %v, want reason %s", typed, tc.reason)
			}
		})
	}
}

func TestTypedErrorKeepsWrappedSyscallError(t *testing.T) {
	err := NewTargetConnectionReset(fmt.Errorf("copy: %w", syscall.ECONNRESET))
	if !errors.Is(err, syscall.ECONNRESET) {
		t.Error("lost syscall.ECONNRESET")
	}
	if !errors.Is(err, ErrTargetConnectionReset) {
		t.Error("lost sentinel")
	}
}

func TestTypedErrorSurvivesFurtherWrapping(t *testing.T) {
	err := fmt.Errorf("error forwarding port 80: %w", NewSandboxNotAvailable(io.EOF))
	if got := Classify(err); got != ReasonSandboxNotAvailable {
		t.Errorf("Classify = %q", got)
	}
}

func TestErrorStringStartsWithStablePrefixAndIncludesCause(t *testing.T) {
	for _, tc := range allReasons {
		got := tc.newErr(errors.New("boom")).Error()
		if want := tc.prefix + ": boom"; got != want {
			t.Errorf("Error() = %q, want %q", got, want)
		}
	}
}

func TestClassify(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want Reason
	}{
		"nil":                {nil, ReasonUnknown},
		"plain":              {errors.New("x"), ReasonUnknown},
		"typed":              {NewSandboxNotAvailable(errors.New("x")), ReasonSandboxNotAvailable},
		"typed beats cause":  {NewSandboxNotAvailable(syscall.ECONNRESET), ReasonSandboxNotAvailable},
		"bare ECONNREFUSED":  {fmt.Errorf("dial: %w", syscall.ECONNREFUSED), ReasonTargetConnectionRefused},
		"bare ECONNRESET":    {fmt.Errorf("copy: %w", syscall.ECONNRESET), ReasonTargetConnectionReset},
		"EPIPE is not reset": {syscall.EPIPE, ReasonUnknown},
	} {
		t.Run(name, func(t *testing.T) {
			if got := Classify(tc.err); got != tc.want {
				t.Errorf("Classify = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestClassifyMessage(t *testing.T) {
	for _, tc := range allReasons {
		// As flattened by the streaming server.
		msg := fmt.Sprintf("error forwarding port 80 to pod p, uid u: %v", tc.newErr(errors.New("boom")))
		if got := ClassifyMessage(msg); got != tc.reason {
			t.Errorf("ClassifyMessage(%q) = %q, want %q", msg, got, tc.reason)
		}
	}
	for _, msg := range []string{"", "something else", "error forwarding port 80: connection reset by peer"} {
		if got := ClassifyMessage(msg); got != ReasonUnknown {
			t.Errorf("ClassifyMessage(%q) = %q, want unknown", msg, got)
		}
	}
}

func TestSessionFatal(t *testing.T) {
	for reason, want := range map[Reason]bool{
		ReasonSandboxNotAvailable:     true,
		ReasonTargetConnectionRefused: false,
		ReasonTargetConnectionReset:   false,
		ReasonUnknown:                 true, // cannot tell: keep the conservative behaviour
	} {
		if got := reason.SessionFatal(); got != want {
			t.Errorf("%q.SessionFatal() = %v, want %v", reason, got, want)
		}
	}
}

func TestWrap(t *testing.T) {
	plain := errors.New("x")
	if got := Wrap(nil); got != nil {
		t.Errorf("Wrap(nil) = %v", got)
	}
	if got := Wrap(plain); got != plain {
		t.Errorf("unclassifiable error was changed: %v", got)
	}
	typed := NewSandboxNotAvailable(syscall.ECONNRESET)
	if got := Wrap(typed); got != typed {
		t.Errorf("classified error was rewrapped: %v", got)
	}
	wrapped := Wrap(fmt.Errorf("copy: %w", syscall.ECONNRESET))
	if !errors.Is(wrapped, ErrTargetConnectionReset) || !errors.Is(wrapped, syscall.ECONNRESET) {
		t.Errorf("Wrap lost sentinel or cause: %v", wrapped)
	}
}

func TestPredicates(t *testing.T) {
	preds := map[Reason]func(error) bool{
		ReasonSandboxNotAvailable:     IsSandboxNotAvailable,
		ReasonTargetConnectionRefused: IsTargetConnectionRefused,
		ReasonTargetConnectionReset:   IsTargetConnectionReset,
	}
	for _, tc := range allReasons {
		err := fmt.Errorf("outer: %w", tc.newErr(errors.New("x")))
		for reason, pred := range preds {
			if got, want := pred(err), reason == tc.reason; got != want {
				t.Errorf("predicate for %s on %s error = %v, want %v", reason, tc.reason, got, want)
			}
		}
	}
	if IsTargetConnectionReset(nil) || IsTargetConnectionReset(errors.New("x")) {
		t.Error("predicate matched an unclassified error")
	}
}
