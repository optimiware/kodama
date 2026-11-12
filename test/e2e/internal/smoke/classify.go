//go:build e2e

package smoke

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/test/e2e/internal/harness"
)

// CredentialExpiredError reports a 401 on a release-smoke read. Installed
// OAuthSonas tokens live about 899 seconds, so a 401 during a scenario is a
// credential-lifetime failure, never a scenario assertion.
type CredentialExpiredError struct {
	Operation string
	Status    int
}

// Error names the operation and the ~899s token lifetime without echoing a
// credential.
func (err *CredentialExpiredError) Error() string {
	return fmt.Sprintf("%s was refused as unauthenticated (HTTP %d): the persona token has expired or been revoked; installed OAuthSonas tokens live about 899 seconds", err.Operation, err.Status)
}

// ForbiddenError reports a 403 on a release-smoke read. The caller's persona
// role does not hold the operation, which retrying cannot fix.
type ForbiddenError struct {
	Operation string
	Status    int
}

// Error names the operation and the public forbidden refusal.
func (err *ForbiddenError) Error() string {
	return fmt.Sprintf("%s was refused as forbidden (HTTP %d): the persona role is not permitted this operation", err.Operation, err.Status)
}

// Classify wraps every public fetch failure. A 401 or 403 is terminal: it is
// returned as a loud credential/authority error and terminal=true. Every other
// failure is returned with the operation named and terminal=false, so a poll
// may retry a transient dependency failure but never an authentication one.
func Classify(operation string, err error) (error, bool) {
	if err == nil {
		return nil, false
	}
	if _, ok := errors.AsType[*CredentialExpiredError](err); ok {
		return err, true
	}
	if _, ok := errors.AsType[*ForbiddenError](err); ok {
		return err, true
	}
	if refusal, ok := errors.AsType[*harness.APIError](err); ok {
		switch refusal.Code {
		case api.CodeUnauthenticated:
			return &CredentialExpiredError{Operation: operation, Status: refusal.Status}, true
		case api.CodeForbidden:
			return &ForbiddenError{Operation: operation, Status: refusal.Status}, true
		}
	}
	return fmt.Errorf("%s: %w", operation, err), false
}

// Wait fetches one observation until stop is true, the context ends or the
// bounded window expires. Every fetch failure is classified: a 401/403 aborts
// immediately with the loud credential/authority error, while a transient
// failure is retried inside the bound. It returns the last value, every value
// observed in order, and an error.
func Wait[T any](ctx context.Context, interval, timeout time.Duration, operation string, fetch func(context.Context) (T, error), stop func(T) bool) (T, []T, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if fetch == nil {
		var zero T
		return zero, nil, fmt.Errorf("%s wait has no fetch function", operation)
	}
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	deadline := time.Now().Add(timeout)
	var last T
	observations := make([]T, 0, 16)
	for {
		if err := ctx.Err(); err != nil {
			return last, observations, err
		}
		value, err := fetch(ctx)
		if err != nil {
			classified, terminal := Classify(operation, err)
			if terminal {
				return last, observations, classified
			}
			if !time.Now().Add(interval).Before(deadline) {
				return last, observations, fmt.Errorf("%s did not become observable within %s: %w", operation, timeout, classified)
			}
			time.Sleep(interval)
			continue
		}
		last = value
		observations = append(observations, value)
		if stop == nil || stop(value) {
			return last, observations, nil
		}
		if !time.Now().Add(interval).Before(deadline) {
			return last, observations, fmt.Errorf("%s did not reach its stop condition within %s", operation, timeout)
		}
		time.Sleep(interval)
	}
}
