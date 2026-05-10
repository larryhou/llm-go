// Package llm defines the Provider interface and core streaming client.
package llm

import (
	"context"
	"time"
)

// Provider is the abstraction over a specific LLM provider (Anthropic, OpenAI, etc.).
// Implementations live in provider/anthropic and provider/openai.
type Provider interface {
	// ID returns the provider identifier (e.g. "anthropic", "openai").
	ID() string

	// Stream sends a request and returns a channel of events.
	// The channel is closed when the stream ends or an error occurs.
	// Callers must drain the channel until it's closed.
	// Cancelling ctx aborts the stream.
	Stream(ctx context.Context, req Request) (<-chan Event, error)
}

// Client wraps a Provider and adds retry logic.
type Client struct {
	provider   Provider
	maxRetries int
}

// NewClient creates a new Client wrapping the given Provider.
func NewClient(p Provider) *Client {
	return &Client{provider: p, maxRetries: 5}
}

// Stream starts an LLM stream with automatic retry on retryable errors.
// Returns a channel of events; closed when the stream finishes or fatally errors.
func (c *Client) Stream(ctx context.Context, req Request) <-chan Event {
	ch := make(chan Event, 32)
	go func() {
		defer close(ch)
		c.streamWithRetry(ctx, req, ch)
	}()
	return ch
}

func (c *Client) streamWithRetry(ctx context.Context, req Request, out chan<- Event) {
	for attempt := 1; attempt <= c.maxRetries; attempt++ {
		retry, done := c.doStream(ctx, req, out, attempt)
		if done {
			return
		}
		if !retry {
			return
		}
		// retry — caller provided a delay already via retryErr.ResponseHeaders
	}
}

// doStream runs one streaming attempt.
// Returns (shouldRetry, isDone).
func (c *Client) doStream(ctx context.Context, req Request, out chan<- Event, attempt int) (bool, bool) {
	events, err := c.provider.Stream(ctx, req)
	if err != nil {
		if llmErr, ok := AsLLMError(err); ok && ShouldRetry(llmErr) && attempt < c.maxRetries {
			if !c.sleep(ctx, RetryDelay(attempt, llmErr.ResponseHeaders)) {
				out <- Event{Type: EventError, Err: ctx.Err()}
				return false, true
			}
			return true, false
		}
		out <- Event{Type: EventError, Err: err}
		return false, true
	}

	var retryErr *LLMError
	for ev := range events {
		if ev.Type == EventError {
			if llmErr, ok := AsLLMError(ev.Err); ok && ShouldRetry(llmErr) && attempt < c.maxRetries {
				retryErr = llmErr
				// drain remaining events from provider
				for range events {
				}
				break
			}
			out <- ev
			return false, true
		}
		out <- ev
		if ev.Type == EventRequestFinish {
			return false, true
		}
	}

	if retryErr != nil {
		if !c.sleep(ctx, RetryDelay(attempt, retryErr.ResponseHeaders)) {
			out <- Event{Type: EventError, Err: ctx.Err()}
			return false, true
		}
		return true, false
	}
	return false, true
}

// sleep waits for d or until ctx is cancelled. Returns false if ctx was cancelled.
func (c *Client) sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}
