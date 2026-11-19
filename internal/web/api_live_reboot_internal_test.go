package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
)

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestRebootDroppedCountsOnlyWhatWasSent(t *testing.T) {
	dial := &net.OpError{Op: "dial", Net: "tcp", Err: timeoutErr{}}
	for name, tc := range map[string]struct {
		err  error
		sent bool
	}{
		"connect timeout":         {fmt.Errorf("post: %w", dial), false},
		"connection refused":      {errors.New("dial tcp 10.99.0.2:443: connect: connection refused"), false},
		"no route":                {errors.New("dial tcp 10.99.0.2:443: connect: no route to host"), false},
		"deadline while dialing":  {fmt.Errorf("x: %w", &net.OpError{Op: "dial", Err: context.DeadlineExceeded}), false},
		"timeout after the write": {&net.OpError{Op: "read", Net: "tcp", Err: timeoutErr{}}, true},
		"context deadline":        {context.DeadlineExceeded, true},
		"eof":                     {io.EOF, true},
		"reset":                   {errors.New("read tcp: connection reset by peer"), true},
		"plain refusal":           {errors.New("HTTP 400"), false},
	} {
		if got := rebootDropped(tc.err); got != tc.sent {
			t.Errorf("%s: sent=%v, want %v", name, got, tc.sent)
		}
	}
}
