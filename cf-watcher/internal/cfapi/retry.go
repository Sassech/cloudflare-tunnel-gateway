// Package cfapi is a client for the Cloudflare Tunnel Configurations API
// (GET/PUT tunnel ingress config, config_src assertion). It also provides
// the shared retry/backoff helper reused by internal/dns, since both
// packages talk to the same Cloudflare API and must honor identical
// error-handling semantics.
package cfapi

import (
	"context"
	"errors"
	"time"
)

// MaxAttempts sets the maximum number of API call attempts. With 6 total
// attempts there are 5 backoff waits between them.
const MaxAttempts = 6

// BackoffDelay returns the wait duration before making attempt number
// `attempt` (1-indexed, where attempt 1 is the first try and has no wait).
// It mirrors Python's tenacity wait_exponential(multiplier=1, min=1, max=60):
//
//	delay = min(2^(attempt-2), 60) seconds, for attempt >= 2
//
// which produces the sequence 1s, 2s, 4s, 8s, 16s, ... capped at 60s.
func BackoffDelay(attempt int) time.Duration {
	if attempt < 2 {
		return 0
	}
	d := time.Duration(1<<uint(attempt-2)) * time.Second
	const maxDelay = 60 * time.Second
	if d > maxDelay {
		d = maxDelay
	}
	return d
}

// RetryableError wraps a transient failure (network error or 5xx response)
// that WithRetry should retry. Any error NOT wrapped as a RetryableError is
// treated as permanent (e.g. a 4xx response) and returned immediately
// without retrying.
type RetryableError struct {
	Err error
}

func (e *RetryableError) Error() string { return e.Err.Error() }
func (e *RetryableError) Unwrap() error { return e.Err }

// Retryable wraps err so WithRetry treats it as a transient failure that
// should be retried — network errors and 5xx responses.
// Retryable(nil) returns nil.
func Retryable(err error) error {
	if err == nil {
		return nil
	}
	return &RetryableError{Err: err}
}

// WithRetry runs fn up to MaxAttempts times.
//
// fn must return an error produced by Retryable() for transient failures
// (network errors, 5xx responses) so WithRetry knows to retry it — any
// other error (e.g. a 4xx response) is returned immediately without
// retrying. Between attempts WithRetry sleeps according to
// BackoffDelay, honoring ctx cancellation.
func WithRetry(ctx context.Context, fn func() error) error {
	var lastErr error

	for attempt := 1; attempt <= MaxAttempts; attempt++ {
		err := fn()
		if err == nil {
			return nil
		}

		var re *RetryableError
		if !errors.As(err, &re) {
			// Permanent failure (e.g. 4xx) — fail fast, do not retry.
			return err
		}
		lastErr = re.Err

		if attempt == MaxAttempts {
			break
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(BackoffDelay(attempt + 1)):
		}
	}

	return lastErr
}
