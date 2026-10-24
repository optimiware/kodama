// Package delivery implements the application's ordinary delivery scripts as a
// small Go helper: it consumes the software factory's runtime callbacks (context,
// deployment register, outcome and lookup) and applies namespace-scoped
// Kubernetes objects. It is not a deployment engine and keeps no ledger of its
// own: the factory records the deployment; this code reports to it and
// reconciles by observing the cluster.
//
// The package deliberately imports nothing from the factory. Its wire types are
// the minimal subset of the published runtime contract it needs.
package delivery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Errors.
var (
	// ErrAuthorityLost means the attempt was revoked or its pin no longer matches:
	// every external effect stops immediately.
	ErrAuthorityLost = errors.New("delivery: runtime authority lost")
	// ErrUnavailable means the callback outcome could not be established after
	// bounded retries (nothing was acknowledged, or the response was lost).
	ErrUnavailable = errors.New("delivery: factory callback unavailable")
	// ErrOutcomeUnknown is returned by the factory when an apply is refused because
	// the previous outcome is still unknown.
	ErrOutcomeUnknown = errors.New("delivery: the previous outcome is unknown")
	// ErrConflict is a replay or intent that contradicts the recorded state.
	ErrConflict = errors.New("delivery: conflicts with the recorded deployment")
)

// APIError is a structured refusal from the runtime API.
type APIError struct {
	Status int
	Code   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("runtime API refused the call: HTTP %d %s", e.Status, e.Code)
}

// Source is the exact candidate source.
type Source struct {
	RepositoryURL string `json:"repositoryUrl"`
	Commit        string `json:"commit"`
}

// Image names the verified OCI artifact.
type Image struct {
	ArtifactID string `json:"artifactId"`
	Digest     string `json:"digest"`
}

// Object is one owned external object.
type Object struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
	UID        string `json:"uid,omitempty"`
	Ready      *bool  `json:"ready,omitempty"`
}

// Record is the factory's deployment record (subset).
type Record struct {
	ID                  string   `json:"id"`
	EnvironmentID       string   `json:"environmentId"`
	ConfigurationDigest string   `json:"configurationDigest"`
	Namespace           string   `json:"namespace"`
	Source              Source   `json:"source"`
	Image               Image    `json:"image"`
	ActionKey           string   `json:"actionKey"`
	Status              string   `json:"status"`
	Reason              string   `json:"reason"`
	OutcomeUnknown      bool     `json:"outcomeUnknown"`
	Objects             []Object `json:"objects"`
	ExpiresAt           string   `json:"expiresAt,omitempty"`
}

// Receipt is the registration/lookup/outcome response.
type Receipt struct {
	Deployment  Record            `json:"deployment"`
	Annotations map[string]string `json:"annotations"`
	Created     bool              `json:"created"`
}

// Report is an outcome report.
type Report struct {
	State   string   `json:"state"`
	Reason  string   `json:"reason,omitempty"`
	Objects []Object `json:"objects,omitempty"`
}

// Binding is a pinned secret binding of the attempt.
type Binding struct {
	Ref      string `json:"ref"`
	Version  int64  `json:"version"`
	Name     string `json:"name"`
	Delivery string `json:"delivery"`
}

// Context is the subset of the immutable attempt context the scripts use.
type Context struct {
	TenantID  string `json:"tenantId"`
	ProjectID string `json:"projectId"`
	RunID     string `json:"runId"`
	StepID    string `json:"stepId"`
	AttemptID string `json:"attemptId"`
	ActionKey string `json:"actionKey"`
	Command   struct {
		Image string `json:"image"`
	} `json:"command"`
	Workspace struct {
		Access string  `json:"access"`
		Source *Source `json:"source"`
	} `json:"workspace"`
	Inputs      map[string]json.RawMessage `json:"inputs"`
	Secrets     []Binding                  `json:"secrets"`
	Environment *struct {
		ID           string `json:"id"`
		ConfigDigest string `json:"configDigest"`
	} `json:"environment"`
}

// Input returns a string input of the pinned context.
func (c Context) Input(name string) (string, bool) {
	raw, ok := c.Inputs[name]
	if !ok {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// Runtime is a client of the factory's runtime callbacks for one attempt. The
// origin and token file are explicit installation inputs; the token file is
// re-read on every call because the entrypoint renews it atomically, and the
// token is never accepted from an environment variable or logged.
type Runtime struct {
	BaseURL   string
	TokenFile string
	AttemptID string
	HTTP      *http.Client
	// Attempts and Backoff bound retries of retryable failures.
	Attempts int
	Backoff  time.Duration
}

// NewRuntime validates the explicit configuration.
func NewRuntime(baseURL, tokenFile, attemptID string) (*Runtime, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("delivery: the runtime origin must be an absolute URL without credentials")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && isLoopback(u.Hostname())) {
		return nil, errors.New("delivery: the runtime origin must be https (http only on loopback)")
	}
	if tokenFile == "" || attemptID == "" {
		return nil, errors.New("delivery: the token file and attempt id are required")
	}
	return &Runtime{BaseURL: strings.TrimRight(baseURL, "/"), TokenFile: tokenFile, AttemptID: attemptID,
		HTTP: &http.Client{Timeout: 20 * time.Second}, Attempts: 5, Backoff: 200 * time.Millisecond}, nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (r *Runtime) path(suffix string) string {
	return r.BaseURL + "/runtime/v1/attempts/" + url.PathEscape(r.AttemptID) + suffix
}

func (r *Runtime) token() (string, error) {
	raw, err := os.ReadFile(r.TokenFile)
	if err != nil {
		return "", fmt.Errorf("delivery: reading the runtime token file: %w", errors.Unwrap(err))
	}
	t := strings.TrimSpace(string(raw))
	if t == "" {
		return "", errors.New("delivery: the runtime token file is empty")
	}
	return t, nil
}

// do performs one call with bounded retries. Only transport errors, 429 and 5xx
// are retried: they mean nothing was acknowledged or the response was lost, and
// every callback is idempotent so repeating the identical call is safe.
func (r *Runtime) do(ctx context.Context, method, suffix string, body, out any) error {
	var payload []byte
	if body != nil {
		payload, _ = json.Marshal(body)
	}
	var last error
	for attempt := 0; attempt < max(r.Attempts, 1); attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(r.Backoff << (attempt - 1)):
			}
		}
		tok, err := r.token()
		if err != nil {
			return err
		}
		var rd io.Reader
		if payload != nil {
			rd = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, r.path(suffix), rd)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Accept", "application/json")
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := r.HTTP.Do(req)
		if err != nil {
			last = fmt.Errorf("%w: %v", ErrUnavailable, scrubURL(err))
			continue
		}
		raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if rerr != nil {
			last = fmt.Errorf("%w: reading the response", ErrUnavailable)
			continue
		}
		switch {
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			if out != nil {
				if err := json.Unmarshal(raw, out); err != nil {
					return fmt.Errorf("delivery: malformed response from the runtime API: %w", err)
				}
			}
			return nil
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			last = fmt.Errorf("%w: HTTP %d", ErrUnavailable, resp.StatusCode)
			continue
		}
		return classify(resp.StatusCode, raw)
	}
	return last
}

func scrubURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

func classify(status int, raw []byte) error {
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &env)
	code := env.Error.Code
	api := &APIError{Status: status, Code: code}
	switch {
	case status == http.StatusUnauthorized, status == http.StatusForbidden && (code == "attempt_revoked" || code == "pin_required" || code == "forbidden" || code == "invalid_credential"):
		return fmt.Errorf("%w: %w", ErrAuthorityLost, api)
	case status == http.StatusConflict && code == "outcome_unknown":
		return fmt.Errorf("%w: %w", ErrOutcomeUnknown, api)
	case status == http.StatusConflict:
		return fmt.Errorf("%w: %w", ErrConflict, api)
	}
	return api
}

// Context fetches the attempt's immutable pinned context.
func (r *Runtime) Context(ctx context.Context) (Context, error) {
	var c Context
	if err := r.do(ctx, http.MethodGet, "/context", nil, &c); err != nil {
		return Context{}, err
	}
	return c, nil
}

// Register records the pre-apply deployment, idempotently by logical action.
func (r *Runtime) Register(ctx context.Context, src Source, img Image) (Receipt, error) {
	var out Receipt
	err := r.do(ctx, http.MethodPost, "/deployments", map[string]any{"source": src, "image": img}, &out)
	return out, err
}

// Lookup reads the attempt's own logical action after a lost response or restart.
func (r *Runtime) Lookup(ctx context.Context, actionKey string) (Receipt, error) {
	var out Receipt
	err := r.do(ctx, http.MethodGet, "/deployments/by-action/"+url.PathEscape(actionKey), nil, &out)
	return out, err
}

// Outcome reports applying/succeeded/failed/unknown.
func (r *Runtime) Outcome(ctx context.Context, deploymentID string, rep Report) (Receipt, error) {
	var out Receipt
	err := r.do(ctx, http.MethodPost, "/deployments/"+url.PathEscape(deploymentID)+"/outcome", rep, &out)
	return out, err
}
