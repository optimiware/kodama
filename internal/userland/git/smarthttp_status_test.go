package git_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	git "github.com/optimiware/kodama/internal/userland/git"
)

// statusActorVerifier is the smart-HTTP read verifier boundary used to inject
// one exact verifier failure per test case. A nil error admits an execution
// identity through the read path.
type statusActorVerifier struct {
	err   error
	calls int
}

func (verifier *statusActorVerifier) VerifyReadActor(context.Context, string, string) (git.Identity, error) {
	verifier.calls++
	if verifier.err != nil {
		return git.Identity{}, verifier.err
	}
	return executionIdentity(), nil
}

// statusRoute is one smart-HTTP exchange: both the read advertisement and
// service POST of upload-pack, and the write advertisement and service POST of
// receive-pack.
type statusRoute struct {
	name        string
	method      string
	target      string
	contentType string
	path        string
}

var statusRoutes = []statusRoute{
	{
		name:   "info/refs upload-pack",
		method: http.MethodGet,
		target: "/shop/web/info/refs?service=git-upload-pack",
		path:   "read",
	},
	{
		name:        "upload-pack POST",
		method:      http.MethodPost,
		target:      "/shop/web/git-upload-pack",
		contentType: "application/x-git-upload-pack-request",
		path:        "read",
	},
	{
		name:   "info/refs receive-pack",
		method: http.MethodGet,
		target: "/shop/web/info/refs?service=git-receive-pack",
		path:   "write",
	},
	{
		name:        "receive-pack POST",
		method:      http.MethodPost,
		target:      "/shop/web/git-receive-pack",
		contentType: "application/x-git-receive-pack-request",
		path:        "write",
	},
}

// newStatusHandler builds one smart-HTTP handler whose actor verifier fails
// with the exact supplied error.
func newStatusHandler(t *testing.T, verifier *statusActorVerifier) (*git.SmartHTTP, *fakeBackend) {
	t.Helper()
	return newStatusHandlerWithResourceID(t, verifier, func(string) (string, error) { return testResourceID, nil })
}

// newStatusHandlerWithResourceID builds one smart-HTTP handler with an injected
// repository-to-resource resolver.
func newStatusHandlerWithResourceID(t *testing.T, verifier *statusActorVerifier, resourceID func(string) (string, error)) (*git.SmartHTTP, *fakeBackend) {
	t.Helper()
	base := newFixture(t)
	backend := newFakeBackend()
	handler, err := git.NewSmartHTTP(git.SmartHTTPConfig{
		Service:      base.service,
		Backend:      backend,
		Actors:       verifier,
		ActorHandles: newFakeActorHandles(),
		ResourceID:   resourceID,
		SourceEvent:  newTestSourceEvents(),
	})
	if err != nil {
		t.Fatalf("NewSmartHTTP: %v", err)
	}
	return handler, backend
}

// statusRequest performs one authenticated exchange for the supplied route.
func statusRequest(t *testing.T, handler *git.SmartHTTP, route statusRoute) *httptest.ResponseRecorder {
	t.Helper()
	var body *bytes.Reader
	if route.method == http.MethodPost {
		body = bytes.NewReader([]byte("commands"))
	} else {
		body = bytes.NewReader(nil)
	}
	request := httptest.NewRequest(route.method, route.target, body)
	request.SetBasicAuth("kodama", "viewer-token")
	if route.contentType != "" {
		request.Header.Set("Content-Type", route.contentType)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// retryAfterSeconds parses and bounds one Retry-After value. Git clients honour
// a small whole-second hint; a missing, non-numeric, zero or large value is a
// defect for a transient failure.
func retryAfterSeconds(t *testing.T, value string) int {
	t.Helper()
	if value == "" {
		t.Fatal("Retry-After header is missing")
	}
	seconds, err := strconv.Atoi(value)
	if err != nil {
		t.Fatalf("Retry-After %q is not a whole number of seconds", value)
	}
	if seconds < 1 || seconds > 10 {
		t.Fatalf("Retry-After %q is outside the small 1..10 second bound", value)
	}
	return seconds
}

// TestSmartHTTPVerifierStatusTable pins the exact class-to-status table for a
// failed actor verification on every smart-HTTP route. Transient dependency
// failures carry a bounded Retry-After hint and never the 401 credential
// challenge; unauthenticated and forbidden answers keep 401/403. An
// unclassified error is a server failure, never "bad credentials".
func TestSmartHTTPVerifierStatusTable(t *testing.T) {
	const secret = "token=super-secret-verification-detail"
	cases := []struct {
		name      string
		err       error
		status    int
		retry     bool
		challenge bool
	}{
		{
			name:      "unauthenticated",
			err:       fault.Wrap(fault.Unauthenticated, "the credential could not be verified", errors.New(secret)),
			status:    http.StatusUnauthorized,
			challenge: true,
		},
		{
			// The production repository-to-resource resolver returns this error
			// for a repository with no mapping: it must be indistinguishable
			// from a credential failure, never a 500.
			name:      "unregistered repository",
			err:       git.UnregisteredRepositoryError("shop/missing"),
			status:    http.StatusUnauthorized,
			challenge: true,
		},
		{
			name:   "forbidden",
			err:    fault.Wrap(fault.Forbidden, "the principal may not read the resource", errors.New(secret)),
			status: http.StatusForbidden,
		},
		{
			name:   "not found",
			err:    fault.Wrap(fault.NotFound, "the resource is not visible", errors.New(secret)),
			status: http.StatusNotFound,
		},
		{
			name:   "invalid input",
			err:    fault.Wrap(fault.InvalidInput, "the verification request is malformed", errors.New(secret)),
			status: http.StatusBadRequest,
		},
		{
			name:   "changed idempotency content",
			err:    fault.Wrap(fault.ChangedIdempotencyContent, "the retry content changed", errors.New(secret)),
			status: http.StatusConflict,
		},
		{
			name:   "version conflict",
			err:    fault.Wrap(fault.VersionConflict, "the verification fact observed a stale version", errors.New(secret)),
			status: http.StatusServiceUnavailable,
			retry:  true,
		},
		{
			name:   "dependency unavailable",
			err:    fault.Wrap(fault.DependencyUnavailable, "the verification store is unavailable", errors.New(secret)),
			status: http.StatusServiceUnavailable,
			retry:  true,
		},
		{
			name:   "uncertain external effect",
			err:    fault.Wrap(fault.UncertainExternalEffect, "the verification effect is unknown", errors.New(secret)),
			status: http.StatusServiceUnavailable,
			retry:  true,
		},
		{
			name:   "deadline expired",
			err:    fault.Wrap(fault.DeadlineExpired, "the verification deadline expired", errors.New(secret)),
			status: http.StatusServiceUnavailable,
			retry:  true,
		},
		{
			name:   "unsupported capability",
			err:    fault.Wrap(fault.UnsupportedCapability, "the adapter cannot verify", errors.New(secret)),
			status: http.StatusInternalServerError,
		},
		{
			name:   "internal failure",
			err:    fault.Wrap(fault.InternalFailure, "an internal verification failure", errors.New(secret)),
			status: http.StatusInternalServerError,
		},
		{
			name:   "unclassified error",
			err:    fmt.Errorf("dial the verification store: %s", secret),
			status: http.StatusInternalServerError,
		},
		{
			name:   "context deadline exceeded",
			err:    context.DeadlineExceeded,
			status: http.StatusServiceUnavailable,
			retry:  true,
		},
		{
			name:   "context canceled",
			err:    context.Canceled,
			status: http.StatusRequestTimeout,
		},
	}

	for _, testCase := range cases {
		for _, route := range statusRoutes {
			t.Run(testCase.name+"/"+route.name, func(t *testing.T) {
				verifier := &statusActorVerifier{err: testCase.err}
				handler, backend := newStatusHandler(t, verifier)
				recorder := statusRequest(t, handler, route)

				if recorder.Code != testCase.status {
					t.Fatalf("status = %d; want %d (body %q)", recorder.Code, testCase.status, recorder.Body.String())
				}
				if testCase.challenge {
					if value := recorder.Header().Get("WWW-Authenticate"); !strings.Contains(value, "Basic") {
						t.Fatalf("WWW-Authenticate = %q; want a Basic challenge", value)
					}
				} else if value := recorder.Header().Get("WWW-Authenticate"); value != "" {
					t.Fatalf("WWW-Authenticate = %q; want no credential challenge", value)
				}
				if testCase.retry {
					retryAfterSeconds(t, recorder.Header().Get("Retry-After"))
				} else if value := recorder.Header().Get("Retry-After"); value != "" {
					t.Fatalf("Retry-After = %q; want no retry hint for a non-transient failure", value)
				}
				if body := recorder.Body.String(); body != http.StatusText(testCase.status)+"\n" {
					t.Fatalf("body = %q; want the generic status text", body)
				}
				if body := recorder.Body.String(); strings.Contains(body, secret) {
					t.Fatalf("body = %q; leaks the verifier detail", body)
				}
				if backend.callCount() != 0 {
					t.Fatal("a refused verification reached the backend")
				}
			})
		}
	}
}

// TestSmartHTTPUnregisteredRepositoryAnswersCredentialRefusal pins the
// repository-level resource resolution in the handler itself, independently of
// the command composition: a repository with no registered resource identity
// answers the uniform 401 challenge on every route, never a 500 that would
// confirm the repository is unmapped, and never a retry hint. A genuine
// resolver fault stays an unclassified server failure.
func TestSmartHTTPUnregisteredRepositoryAnswersCredentialRefusal(t *testing.T) {
	for _, route := range statusRoutes {
		t.Run(route.name, func(t *testing.T) {
			verifier := &statusActorVerifier{}
			handler, backend := newStatusHandlerWithResourceID(t, verifier, func(repository string) (string, error) {
				return "", git.UnregisteredRepositoryError(repository)
			})
			recorder := statusRequest(t, handler, route)

			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d; want 401 (body %q)", recorder.Code, recorder.Body.String())
			}
			if value := recorder.Header().Get("WWW-Authenticate"); !strings.Contains(value, "Basic") {
				t.Fatalf("WWW-Authenticate = %q; want a Basic challenge", value)
			}
			if value := recorder.Header().Get("Retry-After"); value != "" {
				t.Fatalf("Retry-After = %q; want no retry hint for an unregistered repository", value)
			}
			if body := recorder.Body.String(); body != "Unauthorized\n" {
				t.Fatalf("body = %q; want the generic credential refusal", body)
			}
			if backend.callCount() != 0 {
				t.Fatal("an unregistered repository reached the backend")
			}
		})
	}

	// A resolver failure that is not the deliberately hidden unregistered case
	// is a genuine server fault: 500, no challenge and no retry hint.
	verifier := &statusActorVerifier{}
	handler, backend := newStatusHandlerWithResourceID(t, verifier, func(string) (string, error) {
		return "", errors.New("resource registry is unreadable")
	})
	recorder := statusRequest(t, handler, statusRoutes[0])
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("resolver fault status = %d; want 500", recorder.Code)
	}
	if value := recorder.Header().Get("WWW-Authenticate"); value != "" {
		t.Fatalf("resolver fault WWW-Authenticate = %q; want none", value)
	}
	if value := recorder.Header().Get("Retry-After"); value != "" {
		t.Fatalf("resolver fault Retry-After = %q; want none", value)
	}
	if backend.callCount() != 0 {
		t.Fatal("a resolver fault reached the backend")
	}
}

// TestSmartHTTPTransientVerificationNeverChallenges pins the regression: a
// version conflict and an unavailable dependency on the write route are a
// retryable 503, not the 401 challenge Git renders as "could not read
// Username". Real credential failures on the same routes stay 401/403.
func TestSmartHTTPTransientVerificationNeverChallenges(t *testing.T) {
	for _, route := range statusRoutes {
		t.Run(route.name, func(t *testing.T) {
			for _, transient := range []fault.Class{
				fault.VersionConflict,
				fault.DependencyUnavailable,
				fault.UncertainExternalEffect,
			} {
				verifier := &statusActorVerifier{err: fault.New(transient, "transient verification failure")}
				handler, backend := newStatusHandler(t, verifier)
				recorder := statusRequest(t, handler, route)
				if recorder.Code != http.StatusServiceUnavailable {
					t.Fatalf("class %q status = %d; want 503", transient, recorder.Code)
				}
				if recorder.Header().Get("WWW-Authenticate") != "" {
					t.Fatalf("class %q returned a credential challenge", transient)
				}
				retryAfterSeconds(t, recorder.Header().Get("Retry-After"))
				if backend.callCount() != 0 {
					t.Fatalf("class %q reached the backend", transient)
				}
			}

			// A real credential refusal keeps the existing 401 challenge, and an
			// authenticated-but-forbidden principal keeps 403.
			verifier := &statusActorVerifier{err: fault.New(fault.Unauthenticated, "the credential is invalid or expired")}
			handler, _ := newStatusHandler(t, verifier)
			recorder := statusRequest(t, handler, route)
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("unauthenticated status = %d; want 401", recorder.Code)
			}
			if value := recorder.Header().Get("WWW-Authenticate"); !strings.Contains(value, "Basic") {
				t.Fatalf("unauthenticated WWW-Authenticate = %q; want Basic", value)
			}
			if value := recorder.Header().Get("Retry-After"); value != "" {
				t.Fatalf("unauthenticated Retry-After = %q; want none", value)
			}

			verifier = &statusActorVerifier{err: fault.New(fault.Forbidden, "the principal may not write")}
			handler, _ = newStatusHandler(t, verifier)
			recorder = statusRequest(t, handler, route)
			if recorder.Code != http.StatusForbidden {
				t.Fatalf("forbidden status = %d; want 403", recorder.Code)
			}
			if value := recorder.Header().Get("Retry-After"); value != "" {
				t.Fatalf("forbidden Retry-After = %q; want none", value)
			}
		})
	}
}
