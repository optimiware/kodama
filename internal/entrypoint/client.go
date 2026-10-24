package entrypoint

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

const maxResponseBytes = 8 << 20

// RetryPolicy bounds retries of transient failures (transport errors, 5xx,
// 429). Contract-terminal 4xx responses are never retried.
type RetryPolicy struct {
	MaxAttempts int           // total tries per request, at least 1
	BaseDelay   time.Duration // first backoff
	MaxDelay    time.Duration // backoff and Retry-After cap
}

// DefaultRetry is used when a Config leaves the policy zero.
var DefaultRetry = RetryPolicy{MaxAttempts: 5, BaseDelay: 250 * time.Millisecond, MaxDelay: 5 * time.Second}

// Client is the HTTP adapter for Runtime and Uploader. The bearer token is held
// in memory and never logged.
type Client struct {
	base    string
	http    *http.Client
	retry   RetryPolicy
	log     *slog.Logger
	mu      sync.Mutex
	token   string
	timeout time.Duration
}

// NewClient builds a client for one attempt. serverURL is the origin (and any
// prefix) of the runtime service; routes are appended under
// /runtime/v1/attempts/{attemptID}. caFile, when set, adds PEM roots to the
// system pool for the runtime API and upload targets.
func NewClient(serverURL string, attemptID domain.AttemptID, caFile string, retry RetryPolicy, log *slog.Logger) (*Client, error) {
	if err := attemptID.Validate(); err != nil {
		return nil, err
	}
	if retry.MaxAttempts < 1 {
		retry = DefaultRetry
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read CA bundle: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("CA bundle contains no certificates")
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	c := &Client{
		base:  strings.TrimRight(serverURL, "/") + "/runtime/v1/attempts/" + string(attemptID),
		http:  &http.Client{Transport: transport, Timeout: 0, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		retry: retry,
		log:   log,
	}
	return c, nil
}

func (c *Client) setToken(t string) {
	c.mu.Lock()
	c.token = t
	c.mu.Unlock()
}

func (c *Client) bearer() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.token
}

type result[T any] struct {
	value  T
	status int
}

// call performs one contract operation with bounded retries.
func call[Resp any, P interface {
	*Resp
	Validate() error
}](ctx context.Context, c *Client, op, method, path string, body any, auth bool, okStatuses ...int) (result[Resp], error) {
	var zero result[Resp]
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return zero, &ProtocolError{Op: op, Err: err}
		}
	}
	var last error
	for attempt := 0; attempt < c.retry.MaxAttempts; attempt++ {
		if attempt > 0 {
			if err := c.backoff(ctx, attempt, last); err != nil {
				return zero, err
			}
		}
		res, retryAfter, err := doOnce[Resp, P](ctx, c, op, method, path, payload, auth, okStatuses)
		if err == nil {
			return res, nil
		}
		if ctx.Err() != nil {
			return zero, ctx.Err()
		}
		if !retryable(err) {
			return zero, err
		}
		last = err
		if retryAfter > 0 {
			last = &retryAfterError{err: err, after: retryAfter}
		}
		c.log.Debug("runtime request failed; will retry", "op", op, "attempt", attempt+1, "error", err.Error())
	}
	return zero, &TransportError{Op: op, Err: unwrapRetryAfter(last)}
}

type retryAfterError struct {
	err   error
	after time.Duration
}

func (e *retryAfterError) Error() string { return e.err.Error() }
func (e *retryAfterError) Unwrap() error { return e.err }

func unwrapRetryAfter(err error) error {
	var r *retryAfterError
	if errors.As(err, &r) {
		return r.err
	}
	return err
}

// transient marks an error as retryable inside doOnce.
type transient struct{ err error }

func (t *transient) Error() string { return t.err.Error() }
func (t *transient) Unwrap() error { return t.err }

func retryable(err error) bool {
	var t *transient
	return errors.As(err, &t)
}

func (c *Client) backoff(ctx context.Context, attempt int, last error) error {
	d := c.retry.BaseDelay << (attempt - 1)
	if d <= 0 || d > c.retry.MaxDelay {
		d = c.retry.MaxDelay
	}
	if d > 0 {
		d = d/2 + time.Duration(rand.Int64N(int64(d/2)+1))
	}
	var r *retryAfterError
	if errors.As(last, &r) && r.after > d {
		d = min(r.after, c.retry.MaxDelay)
	}
	return sleep(ctx, d)
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func doOnce[Resp any, P interface {
	*Resp
	Validate() error
}](ctx context.Context, c *Client, op, method, path string, payload []byte, auth bool, okStatuses []int) (result[Resp], time.Duration, error) {
	var zero result[Resp]
	var rdr io.Reader
	if payload != nil {
		rdr = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return zero, 0, &ProtocolError{Op: op, Err: err}
	}
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth {
		tok := c.bearer()
		if tok == "" {
			return zero, 0, &ProtocolError{Op: op, Err: errors.New("no runtime token; admit first")}
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return zero, 0, &transient{err: scrubURLError(op, err)}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return zero, 0, &transient{err: fmt.Errorf("%s: read response: %w", op, err)}
	}
	if len(data) > maxResponseBytes {
		return zero, 0, &ProtocolError{Op: op, Err: errors.New("response too large")}
	}
	for _, s := range okStatuses {
		if resp.StatusCode == s {
			v, err := runtimev1.DecodeResponse[Resp, P](data)
			if err != nil {
				return zero, 0, &ProtocolError{Op: op, Err: err}
			}
			return result[Resp]{value: v, status: resp.StatusCode}, 0, nil
		}
	}
	apiErr := &APIError{Op: op, Status: resp.StatusCode}
	if e, derr := runtimev1.DecodeResponse[runtimev1.ErrorResponse](data); derr == nil {
		apiErr.Code, apiErr.Message, apiErr.RequestID, apiErr.Details = e.Error.Code, e.Error.Message, e.Error.RequestID, e.Error.Details
	}
	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
		var after time.Duration
		if secs, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && secs > 0 {
			after = time.Duration(secs) * time.Second
		}
		return zero, after, &transient{err: apiErr}
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return zero, 0, &ProtocolError{Op: op, Err: fmt.Errorf("unexpected success status %d", resp.StatusCode)}
	}
	return zero, 0, apiErr
}

// scrubURLError drops the request URL from transport errors: upload URLs carry
// signatures.
func scrubURLError(op string, err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s: %w", op, ue.Err)
	}
	return fmt.Errorf("%s: %w", op, err)
}

// Admit exchanges the one-time admission credential and stores the token.
func (c *Client) Admit(ctx context.Context, req runtimev1.AdmitRequest) (runtimev1.AdmitResponse, error) {
	if err := req.Validate(); err != nil {
		return runtimev1.AdmitResponse{}, &ProtocolError{Op: "admit", Err: err}
	}
	r, err := call[runtimev1.AdmitResponse](ctx, c, "admit", http.MethodPost, "/admit", req, false, http.StatusOK)
	if err != nil {
		return runtimev1.AdmitResponse{}, err
	}
	c.setToken(r.value.Token.Token)
	return r.value, nil
}

// RenewToken renews the bearer token and stores the new one.
func (c *Client) RenewToken(ctx context.Context) (runtimev1.TokenResponse, error) {
	r, err := call[runtimev1.TokenResponse](ctx, c, "renewToken", http.MethodPost, "/token", runtimev1.TokenRequest{}, true, http.StatusOK)
	if err != nil {
		return runtimev1.TokenResponse{}, err
	}
	c.setToken(r.value.Token.Token)
	return r.value, nil
}

func (c *Client) Context(ctx context.Context) (runtimev1.Context, error) {
	r, err := call[runtimev1.Context](ctx, c, "getContext", http.MethodGet, "/context", nil, true, http.StatusOK)
	return r.value, err
}

func (c *Client) ResolveSecret(ctx context.Context, req runtimev1.SecretResolveRequest) (runtimev1.SecretResolveResponse, error) {
	if err := req.Validate(); err != nil {
		return runtimev1.SecretResolveResponse{}, &ProtocolError{Op: "resolveSecret", Err: err}
	}
	r, err := call[runtimev1.SecretResolveResponse](ctx, c, "resolveSecret", http.MethodPost, "/secrets/resolve", req, true, http.StatusOK)
	return r.value, err
}

func (c *Client) ReserveArtifact(ctx context.Context, req runtimev1.ArtifactReserveRequest) (runtimev1.ArtifactReserveResponse, error) {
	if err := req.Validate(); err != nil {
		return runtimev1.ArtifactReserveResponse{}, &ProtocolError{Op: "reserveArtifact", Err: err}
	}
	r, err := call[runtimev1.ArtifactReserveResponse](ctx, c, "reserveArtifact", http.MethodPost, "/artifacts", req, true, http.StatusOK, http.StatusCreated)
	return r.value, err
}

func (c *Client) CompleteArtifact(ctx context.Context, id domain.ArtifactID, req runtimev1.ArtifactCompleteRequest) (runtimev1.ArtifactCompleteResponse, error) {
	if err := id.Validate(); err != nil {
		return runtimev1.ArtifactCompleteResponse{}, &ProtocolError{Op: "completeArtifact", Err: err}
	}
	if err := req.Validate(); err != nil {
		return runtimev1.ArtifactCompleteResponse{}, &ProtocolError{Op: "completeArtifact", Err: err}
	}
	r, err := call[runtimev1.ArtifactCompleteResponse](ctx, c, "completeArtifact", http.MethodPost, "/artifacts/"+string(id)+"/complete", req, true, http.StatusOK)
	return r.value, err
}

// ProposeSource returns the proposal outcome; HTTP 202 yields status pending.
func (c *Client) ProposeSource(ctx context.Context, req runtimev1.SourceProposalRequest) (runtimev1.SourceProposalResponse, error) {
	if err := req.Validate(); err != nil {
		return runtimev1.SourceProposalResponse{}, &ProtocolError{Op: "proposeSource", Err: err}
	}
	r, err := call[runtimev1.SourceProposalResponse](ctx, c, "proposeSource", http.MethodPost, "/source", req, true, http.StatusOK, http.StatusAccepted)
	if err != nil {
		return r.value, err
	}
	if (r.status == http.StatusOK) != (r.value.Status == runtimev1.PublishAccepted) {
		return runtimev1.SourceProposalResponse{}, &ProtocolError{Op: "proposeSource", Err: fmt.Errorf("HTTP %d does not match status %q", r.status, r.value.Status)}
	}
	return r.value, nil
}

func (c *Client) SubmitResult(ctx context.Context, req runtimev1.ResultRequest) (runtimev1.ResultResponse, error) {
	if err := req.Validate(); err != nil {
		return runtimev1.ResultResponse{}, &ProtocolError{Op: "submitResult", Err: err}
	}
	r, err := call[runtimev1.ResultResponse](ctx, c, "submitResult", http.MethodPost, "/result", req, true, http.StatusOK)
	return r.value, err
}

// Upload PUTs the object to the instructions of a reservation. No bearer token
// is sent: the URL is the capability.
func (c *Client) Upload(ctx context.Context, target runtimev1.Upload, body io.ReaderAt, size int64) error {
	const op = "upload"
	var last error
	for attempt := 0; attempt < c.retry.MaxAttempts; attempt++ {
		if attempt > 0 {
			if err := c.backoff(ctx, attempt, last); err != nil {
				return err
			}
		}
		err := c.uploadOnce(ctx, target, body, size)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !retryable(err) {
			return err
		}
		last = err
	}
	return &TransportError{Op: op, Err: unwrapRetryAfter(last)}
}

func (c *Client) uploadOnce(ctx context.Context, target runtimev1.Upload, body io.ReaderAt, size int64) error {
	const op = "upload"
	req, err := http.NewRequestWithContext(ctx, target.Method, target.URL, io.NewSectionReader(body, 0, size))
	if err != nil {
		return &ProtocolError{Op: op, Err: errors.New("invalid upload instructions")}
	}
	req.ContentLength = size
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(io.NewSectionReader(body, 0, size)), nil }
	for k, v := range target.Headers {
		if strings.EqualFold(k, "Content-Length") || strings.EqualFold(k, "Host") {
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return &transient{err: scrubURLError(op, err)}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests:
		return &transient{err: &APIError{Op: op, Status: resp.StatusCode}}
	}
	return &APIError{Op: op, Status: resp.StatusCode}
}
