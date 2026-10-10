package controlplane

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Error is safe to persist and show in the conversation. Unwrap retains the
// original transport error for classification and server-side diagnostics.
type operatorModelError struct {
	kind       string
	message    string
	cause      error
	retryable  bool
	retryAfter time.Duration
}

func (e *operatorModelError) Error() string { return e.message }
func (e *operatorModelError) Unwrap() error { return e.cause }

func operatorModelConnectionError(cause error) *operatorModelError {
	var networkError net.Error
	if errors.Is(cause, context.DeadlineExceeded) || (errors.As(cause, &networkError) && networkError.Timeout()) {
		return &operatorModelError{kind: "timeout", message: "The model response timed out. Your conversation and completed operations are saved.", cause: cause, retryable: true}
	}
	return &operatorModelError{kind: "connection", message: "The connection to the model provider was interrupted. Your conversation and completed operations are saved.", cause: cause, retryable: true}
}

func operatorModelRetryableStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func operatorModelHTTPError(status int, retryAfter string) *operatorModelError {
	message := fmt.Sprintf("model provider returned HTTP %d; check the configured model and provider account", status)
	if operatorModelRetryableStatus(status) {
		message = "The model provider is temporarily unavailable. Your conversation and completed operations are saved."
	}
	if status == http.StatusRequestTimeout || status == http.StatusGatewayTimeout {
		message = "The model response timed out. Your conversation and completed operations are saved."
	}
	if status == http.StatusTooManyRequests {
		message = "The model provider is busy. Please try again shortly."
	}
	return &operatorModelError{
		kind: "http", message: message, cause: fmt.Errorf("HTTP %d", status),
		retryable: operatorModelRetryableStatus(status), retryAfter: operatorModelRetryAfter(retryAfter, time.Now()),
	}
}

func operatorModelRetryAfter(value string, now time.Time) time.Duration {
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		// A longer provider cooldown must stop retries, not overflow a duration.
		if seconds > 30 {
			return 31 * time.Second
		}
		return time.Duration(seconds) * time.Second
	}
	if date, err := http.ParseTime(value); err == nil && date.After(now) {
		return date.Sub(now)
	}
	return 0
}

// Durable conversation replies retry within their worker budget. Temporary
// welcomes and titles keep their existing single-attempt, shorter deadlines.
func (c OperatorConfig) completeWithRetries(ctx context.Context, messages []modelMessage, tools []mcpTool, maxTokens int, onText func(string) error) (modelMessage, error) {
	const attempts = 3
	delay := c.modelRetryDelay
	if delay <= 0 {
		delay = time.Second
	}
	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return modelMessage{}, err
		}
		emitted := false
		answer, err := c.completeWithLimit(ctx, messages, tools, maxTokens, func(text string) error {
			emitted = true
			return onText(text)
		})
		if err == nil {
			return answer, nil
		}
		if ctx.Err() != nil {
			return answer, ctx.Err()
		}
		var failure *operatorModelError
		if !errors.As(err, &failure) {
			// Persistence/callback and validation failures are not provider retries.
			return answer, err
		}
		// Tool requests are only executed after a complete, successful response.
		// Retry this model step, never an earlier checkpoint or completed tool.
		// Once text exists, preserve the partial answer instead of duplicating it.
		retry := failure.retryable && !emitted && answer.Content == "" && attempt < attempts && failure.retryAfter <= 30*time.Second
		cause := failure.cause
		// url.Error includes the complete request URL, possibly with credentials
		// or query parameters. Log the transport cause without that wrapper.
		var requestError *url.Error
		for errors.As(cause, &requestError) {
			cause = requestError.Err
		}
		log.Printf("workspace model request failed: model=%s attempt=%d kind=%s retry=%t cause=%q", c.Model, attempt, failure.kind, retry, cause)
		if !retry {
			return answer, err
		}
		wait := max(delay, failure.retryAfter)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return answer, ctx.Err()
		case <-timer.C:
		}
		delay *= 2
	}
}
