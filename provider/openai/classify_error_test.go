package openai

// classify_error_test.go — white-box tests for isRetryableTransportError.
// Tests live in package openai (not openai_test) because the function is unexported.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
)

// mockNetError satisfies net.Error for Timeout/Temporary tests.
type mockNetError struct {
	timeout bool
	msg     string
}

func (e *mockNetError) Error() string   { return e.msg }
func (e *mockNetError) Timeout() bool   { return e.timeout }
func (e *mockNetError) Temporary() bool { return false }

// makeOpError builds a *net.OpError with the given Op.
func makeOpError(op string, wrapped error) *net.OpError {
	return &net.OpError{Op: op, Net: "tcp", Err: wrapped}
}

func TestIsRetryableTransportError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		// nil is never retryable.
		{"nil", nil, false},

		// io errors indicating connection reset mid-stream.
		{"io.EOF", io.EOF, true},
		{"io.ErrUnexpectedEOF", io.ErrUnexpectedEOF, true},

		// net.Error (not OpError) with Timeout() == true.
		{"net.Error timeout=true", &mockNetError{timeout: true, msg: "timeout"}, true},
		// net.Error with Timeout() == false → not retryable.
		{"net.Error timeout=false", &mockNetError{timeout: false, msg: "other"}, false},

		// net.OpError with read/write ops (ECONNRESET, EPIPE) → retryable.
		{"net.OpError op=read", makeOpError("read", errors.New("connection reset")), true},
		{"net.OpError op=write", makeOpError("write", errors.New("broken pipe")), true},

		// dial op (ECONNREFUSED) is intentionally NOT retryable.
		{"net.OpError op=dial", makeOpError("dial", errors.New("connection refused")), false},

		// Arbitrary non-network error is not retryable.
		{"generic error", errors.New("some error"), false},

		// Wrapped io.EOF should also be retryable.
		{"wrapped io.EOF", fmt.Errorf("stream: %w", io.EOF), true},

		// *json.SyntaxError "unexpected end of JSON input" from SSE mid-frame drop.
		{"json.SyntaxError unexpected end", &json.SyntaxError{}, true},
		{"wrapped json.SyntaxError", fmt.Errorf("decode: %w", &json.SyntaxError{}), true},
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

// TestIsRetryableTransportError_DialTimeout verifies that a dial OpError with a
// timeout inner error is still NOT retryable — dial means endpoint unreachable.
func TestIsRetryableTransportError_DialTimeout(t *testing.T) {
	dialTimeout := makeOpError("dial", &mockNetError{timeout: true, msg: "i/o timeout"})
	if isRetryableTransportError(dialTimeout) {
		t.Error("dial op with timeout should not be retryable (endpoint unreachable)")
	}
}

// TestIsRetryableTransportError_ReadTimeout verifies that a read OpError wrapping
// a timeout IS retryable (mid-stream timeout from a live connection).
func TestIsRetryableTransportError_ReadTimeout(t *testing.T) {
	readTimeout := makeOpError("read", &mockNetError{timeout: true, msg: "i/o timeout"})
	if !isRetryableTransportError(readTimeout) {
		t.Error("read op with timeout should be retryable (mid-stream timeout)")
	}
}

