package anthropic

// classify_error_test.go — white-box tests for isRetryableTransportError.
// Tests live in package anthropic (not anthropic_test) because the function is unexported.

import (
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
)

// mockNetErrAnthropic satisfies net.Error for Timeout tests.
type mockNetErrAnthropic struct {
	timeout bool
	msg     string
}

func (e *mockNetErrAnthropic) Error() string   { return e.msg }
func (e *mockNetErrAnthropic) Timeout() bool   { return e.timeout }
func (e *mockNetErrAnthropic) Temporary() bool { return false }

func makeOpErrAnthropic(op string, wrapped error) *net.OpError {
	return &net.OpError{Op: op, Net: "tcp", Err: wrapped}
}

func TestIsRetryableTransportError_Anthropic(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"io.EOF", io.EOF, true},
		{"io.ErrUnexpectedEOF", io.ErrUnexpectedEOF, true},
		{"net.Error timeout=true", &mockNetErrAnthropic{timeout: true, msg: "timeout"}, true},
		{"net.Error timeout=false", &mockNetErrAnthropic{timeout: false, msg: "other"}, false},
		{"net.OpError op=read", makeOpErrAnthropic("read", errors.New("connection reset")), true},
		{"net.OpError op=write", makeOpErrAnthropic("write", errors.New("broken pipe")), true},
		{"net.OpError op=dial", makeOpErrAnthropic("dial", errors.New("connection refused")), false},
		{"generic error", errors.New("some error"), false},
		{"wrapped io.EOF", fmt.Errorf("stream: %w", io.EOF), true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isRetryableTransportError(tc.err)
			if got != tc.want {
				t.Errorf("isRetryableTransportError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
