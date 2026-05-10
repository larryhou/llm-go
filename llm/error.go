package llm

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ErrorKind classifies LLM errors for retry/reporting decisions.
// Aligned with packages/llm/src/schema/errors.ts ErrorReason.
type ErrorKind string

const (
	ErrContextOverflow  ErrorKind = "context_overflow"
	ErrAuth             ErrorKind = "auth"
	ErrRateLimit        ErrorKind = "rate_limit"
	ErrQuotaExceeded    ErrorKind = "quota_exceeded"
	ErrInvalidRequest   ErrorKind = "invalid_request"
	ErrContentPolicy    ErrorKind = "content_policy"
	ErrProviderInternal ErrorKind = "provider_internal"
	ErrTransport        ErrorKind = "transport"
	ErrUnknown          ErrorKind = "unknown"
)

// LLMError is the canonical error type returned by LLM operations.
type LLMError struct {
	Kind            ErrorKind
	Message         string
	StatusCode      int
	IsRetryable     bool
	ResponseBody    string
	ResponseHeaders http.Header
}

func (e *LLMError) Error() string {
	if e.StatusCode != 0 {
		return fmt.Sprintf("llm error [%s] status=%d: %s", e.Kind, e.StatusCode, e.Message)
	}
	return fmt.Sprintf("llm error [%s]: %s", e.Kind, e.Message)
}

// IsContextOverflow returns true if this error indicates the context window was exceeded.
func (e *LLMError) IsContextOverflow() bool { return e.Kind == ErrContextOverflow }

// AsLLMError extracts an *LLMError from an error chain.
func AsLLMError(err error) (*LLMError, bool) {
	var e *LLMError
	return e, errors.As(err, &e)
}

// overflowPatterns is the canonical list of 17+ patterns indicating context overflow.
// Aligned with packages/opencode/src/provider/error.ts OVERFLOW_PATTERNS.
var overflowPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)prompt is too long`),                                    // Anthropic
	regexp.MustCompile(`(?i)input is too long for requested model`),                 // Bedrock
	regexp.MustCompile(`(?i)exceeds the context window`),                            // OpenAI
	regexp.MustCompile(`(?i)input token count.*exceeds the maximum`),                // Google Gemini
	regexp.MustCompile(`(?i)maximum prompt length is \d+`),                         // xAI Grok
	regexp.MustCompile(`(?i)reduce the length of the messages`),                     // Groq
	regexp.MustCompile(`(?i)maximum context length is \d+ tokens`),                 // OpenRouter/DeepSeek/vLLM
	regexp.MustCompile(`(?i)exceeds the limit of \d+`),                             // GitHub Copilot
	regexp.MustCompile(`(?i)exceeds the available context size`),                   // llama.cpp
	regexp.MustCompile(`(?i)greater than the context length`),                      // LM Studio
	regexp.MustCompile(`(?i)context window exceeds limit`),                         // MiniMax
	regexp.MustCompile(`(?i)exceeded model token limit`),                           // Kimi/Moonshot
	regexp.MustCompile(`(?i)context[_ ]length[_ ]exceeded`),                       // Generic
	regexp.MustCompile(`(?i)request entity too large`),                             // HTTP 413
	regexp.MustCompile(`(?i)context length is only \d+ tokens`),                   // vLLM
	regexp.MustCompile(`(?i)input length.*exceeds.*context length`),                // vLLM
	regexp.MustCompile(`(?i)prompt too long; exceeded (?:max )?context length`),   // Ollama
	regexp.MustCompile(`(?i)too large for model with \d+ maximum context length`), // Mistral
	regexp.MustCompile(`(?i)model_context_window_exceeded`),                       // z.ai
}

// emptyBodyPattern matches Cerebras/Mistral "400 (no body)" style errors.
var emptyBodyPattern = regexp.MustCompile(`(?i)^4(00|13)\s*(status code)?\s*\(no body\)`)

// isOverflowMessage returns true if the message indicates a context overflow.
func isOverflowMessage(msg string) bool {
	for _, p := range overflowPatterns {
		if p.MatchString(msg) {
			return true
		}
	}
	return emptyBodyPattern.MatchString(msg)
}

// ClassifyHTTPError converts an HTTP status code + response body into an LLMError.
// Aligned with packages/opencode/src/provider/error.ts parseAPICallError.
func ClassifyHTTPError(providerID string, statusCode int, body string, headers http.Header) *LLMError {
	msg := extractMessage(providerID, statusCode, body)

	// Check for context overflow first
	if isOverflowMessage(msg) || statusCode == 413 || isBodyContextOverflow(body) {
		return &LLMError{
			Kind:         ErrContextOverflow,
			Message:      msg,
			StatusCode:   statusCode,
			IsRetryable:  false,
			ResponseBody: body,
		}
	}

	kind, retryable := classifyStatus(statusCode, body)
	return &LLMError{
		Kind:            kind,
		Message:         msg,
		StatusCode:      statusCode,
		IsRetryable:     retryable,
		ResponseBody:    body,
		ResponseHeaders: headers,
	}
}

// ClassifyStreamError parses JSON error events from the stream.
// Aligned with packages/opencode/src/provider/error.ts parseStreamError.
func ClassifyStreamError(errorType, errorCode, message string) *LLMError {
	switch {
	case errorCode == "context_length_exceeded" || errorType == "context_length_exceeded":
		return &LLMError{Kind: ErrContextOverflow, Message: message, IsRetryable: false}
	case errorCode == "insufficient_quota":
		return &LLMError{Kind: ErrQuotaExceeded, Message: message, IsRetryable: false}
	case errorCode == "usage_not_included" || errorCode == "invalid_prompt":
		return &LLMError{Kind: ErrInvalidRequest, Message: message, IsRetryable: false}
	case errorCode == "server_is_overloaded" || errorType == "server_error":
		return &LLMError{Kind: ErrProviderInternal, Message: message, IsRetryable: true}
	}
	return nil
}

// classifyStatus maps HTTP status codes to ErrorKind + retryable flag.
func classifyStatus(statusCode int, body string) (ErrorKind, bool) {
	switch {
	case statusCode == 401 || statusCode == 403:
		return ErrAuth, false
	case statusCode == 429:
		// Distinguish rate limit vs quota exceeded via body keywords
		b := strings.ToLower(body)
		if strings.Contains(b, "quota") || strings.Contains(b, "billing") {
			return ErrQuotaExceeded, false
		}
		return ErrRateLimit, true
	case statusCode == 400 || statusCode == 404 || statusCode == 422:
		return ErrInvalidRequest, false
	case statusCode >= 500:
		return ErrProviderInternal, true
	case statusCode == 529: // Anthropic overloaded
		return ErrProviderInternal, true
	}
	return ErrUnknown, false
}

// isBodyContextOverflow checks JSON body for context_length_exceeded code.
func isBodyContextOverflow(body string) bool {
	// fast string check before full parse
	return strings.Contains(body, "context_length_exceeded")
}

// extractMessage builds a human-readable error message from the response.
// Aligned with packages/opencode/src/provider/error.ts message().
func extractMessage(providerID string, statusCode int, body string) string {
	if body == "" {
		return httpStatusText(statusCode)
	}
	// Reject HTML bodies (gateway error pages)
	trimmed := strings.TrimSpace(body)
	if strings.HasPrefix(trimmed, "<!") || strings.HasPrefix(trimmed, "<html") {
		switch statusCode {
		case 401:
			return "Unauthorized: check your API key"
		case 403:
			return "Forbidden: insufficient permissions"
		default:
			return fmt.Sprintf("HTTP %d from %s", statusCode, providerID)
		}
	}
	return body
}

func httpStatusText(code int) string {
	if t := http.StatusText(code); t != "" {
		return fmt.Sprintf("%d %s", code, t)
	}
	return fmt.Sprintf("HTTP %d", code)
}

// RetryDelay calculates the delay before the next retry attempt.
// Aligned with packages/opencode/src/session/retry.ts delay().
//
// Priority:
//  1. retry-after-ms header (ms float)
//  2. retry-after header (seconds float or HTTP date)
//  3. Exponential backoff: 2000ms × 2^(attempt-1), capped at 30s
func RetryDelay(attempt int, headers http.Header) time.Duration {
	if headers != nil {
		if v := headers.Get("retry-after-ms"); v != "" {
			if ms, err := strconv.ParseFloat(v, 64); err == nil && ms > 0 {
				return time.Duration(ms) * time.Millisecond
			}
		}
		if v := headers.Get("retry-after"); v != "" {
			// Try as seconds
			if secs, err := strconv.ParseFloat(v, 64); err == nil && secs > 0 {
				return time.Duration(secs*1000) * time.Millisecond
			}
			// Try as HTTP date
			if t, err := http.ParseTime(v); err == nil {
				d := time.Until(t)
				if d > 0 {
					return d
				}
			}
		}
	}

	// Exponential backoff: 2000 × 2^(attempt-1), max 30s
	const (
		initial = 2_000 * time.Millisecond
		maxDelay = 30_000 * time.Millisecond
	)
	delay := initial
	for i := 1; i < attempt; i++ {
		delay *= 2
		if delay > maxDelay {
			return maxDelay
		}
	}
	return delay
}

// ShouldRetry returns true if the error is retryable.
// Aligned with packages/opencode/src/session/retry.ts retryable().
func ShouldRetry(err *LLMError) bool {
	if err == nil {
		return false
	}
	if err.Kind == ErrContextOverflow {
		return false
	}
	if err.StatusCode >= 500 {
		return true
	}
	return err.IsRetryable
}
