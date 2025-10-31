package runtime

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"
)

// RetryConfig controls retry behavior for transient failures.
type RetryConfig struct {
	// MaxRetries is the maximum number of retry attempts (0 = no retries).
	MaxRetries int
	// InitialBackoff is the initial delay before the first retry.
	InitialBackoff time.Duration
	// MaxBackoff is the maximum delay between retries.
	MaxBackoff time.Duration
	// Multiplier is the factor by which backoff increases with each retry (exponential backoff).
	Multiplier float64
}

// DefaultRetryConfig returns a sensible default retry configuration for API calls.
func DefaultRetryConfig() *RetryConfig {
	return &RetryConfig{
		MaxRetries:     3,
		InitialBackoff: 500 * time.Millisecond,
		MaxBackoff:     8 * time.Second,
		Multiplier:     2.0,
	}
}

// isRetryableError determines if an error should be retried.
func isRetryableError(err error) bool {
	if err == nil {
		return false
	}

	var netErr net.Error
	if errors.As(err, &netErr) {
		// Network errors are retryable
		if netErr.Timeout() || netErr.Temporary() {
			return true
		}
	}

	// Check for DNS errors
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return !dnsErr.IsNotFound && !dnsErr.IsTemporary
	}

	return false
}

// isRetryableStatusCode determines if an HTTP status code should be retried.
func isRetryableStatusCode(code int) bool {
	// Retry on 5xx server errors and 429 (rate limit)
	return code >= 500 || code == 429
}

// retryableAPIError wraps an error with retry context.
type retryableAPIError struct {
	err        error
	statusCode int
	retryable  bool
}

func (e *retryableAPIError) Error() string {
	if e.statusCode > 0 {
		return fmt.Sprintf("API error (status %d): %v", e.statusCode, e.err)
	}
	return fmt.Sprintf("API error: %v", e.err)
}

func (e *retryableAPIError) Unwrap() error {
	return e.err
}

// executeWithRetry executes a function with retry logic for transient failures.
func executeWithRetry(ctx context.Context, config *RetryConfig, fn func() error) error {
	if config == nil || config.MaxRetries <= 0 {
		// No retry config or retries disabled - execute once
		return fn()
	}

	var lastErr error
	backoff := config.InitialBackoff

	for attempt := 0; attempt <= config.MaxRetries; attempt++ {
		err := fn()
		if err == nil {
			return nil
		}

		lastErr = err

		// Check if error is retryable
		var retryErr *retryableAPIError
		if !errors.As(err, &retryErr) || !retryErr.retryable {
			// Not retryable - return immediately
			return err
		}

		// Don't retry on last attempt
		if attempt >= config.MaxRetries {
			break
		}

		// Wait before retry (with context cancellation check)
		select {
		case <-ctx.Done():
			return fmt.Errorf("context cancelled during retry: %w", ctx.Err())
		case <-time.After(backoff):
			// Continue with retry
		}

		// Exponential backoff for next iteration
		backoff = time.Duration(float64(backoff) * config.Multiplier)
		if backoff > config.MaxBackoff {
			backoff = config.MaxBackoff
		}
	}

	return fmt.Errorf("retry exhausted after %d attempts: %w", config.MaxRetries+1, lastErr)
}
