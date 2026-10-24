// Package erp is the replaceable HTTP adapter for the legacy ERP port consumed
// by the exceptions module. The wire protocol is documented in protocol.md and
// is a sample contract: no vendor protocol is implied. A real ERP integration
// implements the same behavior behind the same port.
package erp

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
	"strings"
	"time"

	"example.invalid/inventory/internal/exceptions"
	"example.invalid/inventory/internal/tenancy"
)

// Credentials returns the ERP credential scoped to one application tenant. A
// missing credential is an error: the adapter never falls back to another
// tenant's or a shared credential.
type Credentials interface {
	Token(tenant tenancy.TenantID) (string, error)
}

// Config configures the adapter.
type Config struct {
	BaseURL     string
	Credentials Credentials
	Timeout     time.Duration
	// AllowInsecure permits http on a non-loopback host (closed test networks).
	AllowInsecure bool
	Client        *http.Client
}

// HTTPAdapter implements exceptions.ERP.
type HTTPAdapter struct {
	base   *url.URL
	creds  Credentials
	client *http.Client
}

var _ exceptions.ERP = (*HTTPAdapter)(nil)

// ErrUnreachable marks an outcome that could not be established. It is the
// only class of error a caller may treat as "the write may have happened".
var ErrUnreachable = errors.New("erp: outcome could not be established")

// NewHTTPAdapter validates its configuration and fails closed.
func NewHTTPAdapter(cfg Config) (*HTTPAdapter, error) {
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("erp: base URL must be absolute, without credentials, query or fragment")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && (cfg.AllowInsecure || isLoopback(u.Hostname()))) {
		return nil, errors.New("erp: base URL must be https (http only on loopback or with AllowInsecure)")
	}
	if cfg.Credentials == nil {
		return nil, errors.New("erp: tenant-scoped credentials are required")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{}
	}
	c := *client
	c.Timeout = cfg.Timeout
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &HTTPAdapter{base: u, creds: cfg.Credentials, client: &c}, nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

type applyBody struct {
	ItemRef     string `json:"itemRef"`
	Quantity    int64  `json:"quantity"`
	Reason      string `json:"reason"`
	PayloadHash string `json:"payloadHash"`
}

type outcomeBody struct {
	Status    string `json:"status"`
	Reference string `json:"reference"`
	Reason    string `json:"reason"`
}

func (a *HTTPAdapter) endpoint(tenant tenancy.TenantID, key string) string {
	return a.base.JoinPath("v1", "tenants", string(tenant), "adjustments", key).String()
}

// Apply implements exceptions.ERP.
func (a *HTTPAdapter) Apply(ctx context.Context, tenant tenancy.TenantID, op exceptions.Operation) (exceptions.Outcome, error) {
	body, _ := json.Marshal(applyBody{ItemRef: op.ERPRef, Quantity: op.AdjustedQuantity, Reason: op.Reason, PayloadHash: op.PayloadHash})
	return a.call(ctx, http.MethodPut, tenant, op.Key, body)
}

// Lookup implements exceptions.ERP.
func (a *HTTPAdapter) Lookup(ctx context.Context, tenant tenancy.TenantID, key string) (exceptions.Outcome, error) {
	return a.call(ctx, http.MethodGet, tenant, key, nil)
}

func (a *HTTPAdapter) call(ctx context.Context, method string, tenant tenancy.TenantID, key string, body []byte) (exceptions.Outcome, error) {
	token, err := a.creds.Token(tenant)
	if err != nil {
		return exceptions.Outcome{}, fmt.Errorf("erp: no credential for this tenant: %w", err)
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.endpoint(tenant, key), rd)
	if err != nil {
		return exceptions.Outcome{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return exceptions.Outcome{}, fmt.Errorf("%w: %v", ErrUnreachable, scrub(err))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return exceptions.Outcome{}, fmt.Errorf("%w: reading the response: %v", ErrUnreachable, err)
	}
	var out outcomeBody
	_ = json.Unmarshal(raw, &out)

	switch {
	case resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated:
		switch out.Status {
		case "acknowledged":
			if out.Reference == "" {
				return exceptions.Outcome{}, fmt.Errorf("%w: acknowledgment without a reference", ErrUnreachable)
			}
			return exceptions.Outcome{Status: exceptions.OutcomeAcknowledged, Reference: out.Reference}, nil
		case "rejected":
			return exceptions.Outcome{Status: exceptions.OutcomeRejected, Reason: reasonOr(out.Reason, "rejected by the ERP")}, nil
		}
		return exceptions.Outcome{}, fmt.Errorf("%w: unrecognized status %q", ErrUnreachable, out.Status)
	case resp.StatusCode == http.StatusNotFound && method == http.MethodGet:
		return exceptions.Outcome{Status: exceptions.OutcomeNotFound}, nil
	case resp.StatusCode == http.StatusUnprocessableEntity && method == http.MethodPut:
		return exceptions.Outcome{Status: exceptions.OutcomeRejected, Reason: reasonOr(out.Reason, "rejected by the ERP")}, nil
	case resp.StatusCode == http.StatusConflict && method == http.MethodPut:
		// The key exists with a different payload: a definite refusal, never a retry.
		return exceptions.Outcome{Status: exceptions.OutcomeRejected, Reason: "erp_key_conflict: the operation key exists with a different payload"}, nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return exceptions.Outcome{}, fmt.Errorf("erp: credential refused (HTTP %d): check the tenant-scoped ERP credential and scopes", resp.StatusCode)
	}
	return exceptions.Outcome{}, fmt.Errorf("%w: HTTP %d", ErrUnreachable, resp.StatusCode)
}

func reasonOr(s, def string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

// scrub removes the request URL from transport errors so nothing sensitive is logged.
func scrub(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// FileCredentials reads tenant-scoped bearer tokens from protected files,
// re-reading them on use so rotation does not need a restart.
type FileCredentials struct {
	files map[tenancy.TenantID]string
	read  func(string) ([]byte, error)
}

// NewFileCredentials maps tenants to token files.
func NewFileCredentials(files map[tenancy.TenantID]string, read func(string) ([]byte, error)) (*FileCredentials, error) {
	if len(files) == 0 {
		return nil, errors.New("erp: at least one tenant credential file is required")
	}
	return &FileCredentials{files: files, read: read}, nil
}

// Token implements Credentials.
func (f *FileCredentials) Token(t tenancy.TenantID) (string, error) {
	path, ok := f.files[t]
	if !ok {
		return "", errors.New("tenant has no ERP credential")
	}
	raw, err := f.read(path)
	if err != nil {
		return "", fmt.Errorf("reading the credential file: %w", err)
	}
	tok := strings.TrimSpace(string(raw))
	if tok == "" {
		return "", errors.New("credential file is empty")
	}
	return tok, nil
}
