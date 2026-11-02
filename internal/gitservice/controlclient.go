package gitservice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// TokenSource returns the worker identity token (a signed gitv1.WorkerIdentity,
// audience kodama-git-control) to present to the control plane. T-136 provides
// the production source.
type TokenSource func(ctx context.Context) (string, error)

// ControlClient implements ControlPlane over HTTPS to the control plane's
// internal route family (gitv1.ControlBasePath) with the worker identity as
// bearer credential. It can call no other route.
type ControlClient struct {
	base   string
	tokens TokenSource
	http   *http.Client
}

var _ ControlPlane = (*ControlClient)(nil)

// NewControlPlaneClient returns a client for tenant's routes under baseURL (the
// control plane's scheme and host). A nil httpClient uses a client with the
// default transport.
func NewControlPlaneClient(baseURL string, tenant domain.TenantID, tokens TokenSource, httpClient *http.Client) (*ControlClient, error) {
	if err := tenant.Validate(); err != nil {
		return nil, err
	}
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("gitservice: the control plane base URL must be an http(s) URL without query")
	}
	if tokens == nil {
		return nil, errors.New("gitservice: a worker identity token source is required")
	}
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	path := strings.ReplaceAll(gitv1.ControlBasePath, "{tenantId}", string(tenant))
	return &ControlClient{base: strings.TrimSuffix(u.String(), "/") + path, tokens: tokens, http: httpClient}, nil
}

func (c *ControlClient) do(ctx context.Context, method, path string, in any) ([]byte, error) {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(data)
	}
	token, err := c.tokens(ctx)
	if err != nil {
		return nil, fmt.Errorf("worker identity: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusOK {
		return data, nil
	}
	var e gitv1.ErrorResponse
	if json.Unmarshal(data, &e) == nil {
		if status, known := gitv1.Status(e.Error.Code); known && status == resp.StatusCode && e.Error.Message != "" {
			return nil, &gitv1.ContractError{Code: e.Error.Code, Message: e.Error.Message}
		}
	}
	return nil, &gitv1.ContractError{Code: domain.CodeUnavailable, Message: fmt.Sprintf("the control plane answered %d", resp.StatusCode)}
}

func call[T any, P interface {
	*T
	Validate() error
}](ctx context.Context, c *ControlClient, method, path string, in any) (T, error) {
	var zero T
	data, err := c.do(ctx, method, path, in)
	if err != nil {
		return zero, err
	}
	return gitv1.DecodeResponse[T, P](data)
}

func (c *ControlClient) Authorize(ctx context.Context, req gitv1.PushAuthorizeRequest) (gitv1.PushAuthorizeResponse, error) {
	return call[gitv1.PushAuthorizeResponse](ctx, c, http.MethodPost, "/pushes", req)
}

func (c *ControlClient) CommitIntent(ctx context.Context, pushID string, req gitv1.CommitIntentRequest) (gitv1.CommitIntentResponse, error) {
	if !repositoryIDPattern.MatchString(pushID) {
		return gitv1.CommitIntentResponse{}, contractError(domain.CodeInvalidRequest, "pushId is not a valid ID")
	}
	return call[gitv1.CommitIntentResponse](ctx, c, http.MethodPost, "/pushes/"+url.PathEscape(pushID)+"/commit-intent", req)
}

func (c *ControlClient) Outcome(ctx context.Context, pushID string, req gitv1.OutcomeRequest) (gitv1.OutcomeResponse, error) {
	if !repositoryIDPattern.MatchString(pushID) {
		return gitv1.OutcomeResponse{}, contractError(domain.CodeInvalidRequest, "pushId is not a valid ID")
	}
	return call[gitv1.OutcomeResponse](ctx, c, http.MethodPost, "/pushes/"+url.PathEscape(pushID)+"/outcome", req)
}

func (c *ControlClient) Initialized(ctx context.Context, req gitv1.InitializedRequest) (gitv1.InitializedResponse, error) {
	if !repositoryIDPattern.MatchString(req.RepositoryID) {
		return gitv1.InitializedResponse{}, contractError(domain.CodeInvalidRequest, "repositoryId is not a valid ID")
	}
	return call[gitv1.InitializedResponse](ctx, c, http.MethodPost, "/repositories/"+url.PathEscape(req.RepositoryID)+"/initialized", req)
}

func (c *ControlClient) Assignments(ctx context.Context) (gitv1.AssignmentsResponse, error) {
	return call[gitv1.AssignmentsResponse](ctx, c, http.MethodGet, "/assignments", nil)
}
