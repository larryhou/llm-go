package openai

// sse_norm_test.go — white-box tests for sseNormReadCloser.
// Verifies that runs of 3+ consecutive '\n' bytes are collapsed to '\n\n',
// while normal SSE separators (\n\n) and single newlines pass through unchanged.

import (
	"io"
	"strings"
	"testing"
)

func readAll(rc io.ReadCloser) (string, error) {
	b, err := io.ReadAll(rc)
	return string(b), err
}

func norm(s string) *sseNormReadCloser {
	return &sseNormReadCloser{rc: io.NopCloser(strings.NewReader(s))}
}

func TestSseNorm_NormalTwoNewlines_Unchanged(t *testing.T) {
	input := "data: {}\n\ndata: {}\n\n"
	got, err := readAll(norm(input))
	if err != nil {
		t.Fatal(err)
	}
	if got != input {
		t.Errorf("expected unchanged, got %q", got)
	}
}

func TestSseNorm_ThreeNewlines_Collapsed(t *testing.T) {
	// Proxy emits \n\n\n between events — third \n must be dropped.
	input := "data: {\"a\":1}\n\n\ndata: {\"b\":2}\n\n"
	want := "data: {\"a\":1}\n\ndata: {\"b\":2}\n\n"
	got, err := readAll(norm(input))
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSseNorm_ManyNewlines_CollapsedToTwo(t *testing.T) {
	input := "data: {}\n\n\n\n\n\ndata: {}\n\n"
	want := "data: {}\n\ndata: {}\n\n"
	got, err := readAll(norm(input))
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSseNorm_SingleNewlineInData_Unchanged(t *testing.T) {
	// Single \n inside a data value must pass through.
	input := "data: line1\ndata: line2\n\n"
	got, err := readAll(norm(input))
	if err != nil {
		t.Fatal(err)
	}
	if got != input {
		t.Errorf("expected unchanged, got %q", got)
	}
}

func TestSseNorm_EmptyInput(t *testing.T) {
	got, err := readAll(norm(""))
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestSseNorm_SmallReadBuffer(t *testing.T) {
	// Read one byte at a time to exercise chunk-boundary behaviour.
	input := "data: {}\n\n\ndata: {}\n\n"
	want := "data: {}\n\ndata: {}\n\n"

	rc := &sseNormReadCloser{rc: io.NopCloser(strings.NewReader(input))}
	var out strings.Builder
	buf := make([]byte, 1)
	for {
		n, err := rc.Read(buf)
		if n > 0 {
			out.Write(buf[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if out.String() != want {
		t.Errorf("got %q, want %q", out.String(), want)
	}
}
