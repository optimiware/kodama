package entrypoint

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
	runtimev3 "github.com/optimiware/kodama/contracts/runtime/v3"
	"github.com/optimiware/kodama/internal/domain"
)

// NewClientV3 is NewClient for the v3 wire (a run with a source set): routes are
// appended under /runtime/v3/attempts/{attemptID}. The shared bodies (renewal,
// secrets, artifacts) are the v1 DTOs, unchanged; admission, context, begin and
// result are the v3 messages, and the two Git grant routes exist only here.
func NewClientV3(serverURL string, attemptID domain.AttemptID, caFile string, retry RetryPolicy, log *slog.Logger) (*Client, error) {
	if err := attemptID.Validate(); err != nil {
		return nil, err
	}
	c, err := newClientAt(strings.TrimRight(serverURL, "/")+"/runtime/v3/attempts/"+string(attemptID), caFile, retry, log, true)
	if err != nil {
		return nil, err
	}
	c.wire3 = true
	return c, nil
}

func (c *Client) requireV3(op string) error {
	if !c.wire3 {
		return &ProtocolError{Op: op, Err: errors.New("the client does not speak the v3 wire")}
	}
	return nil
}

// AdmitV3 exchanges the one-time admission credential on the v3 wire and stores
// the attempt token. A context of another version never validates here.
func (c *Client) AdmitV3(ctx context.Context, req runtimev1.AdmitRequest) (runtimev3.AdmitResponseV3, error) {
	if err := c.requireV3("admit"); err != nil {
		return runtimev3.AdmitResponseV3{}, err
	}
	if err := req.Validate(); err != nil {
		return runtimev3.AdmitResponseV3{}, &ProtocolError{Op: "admit", Err: err}
	}
	r, err := call[runtimev3.AdmitResponseV3](ctx, c, "admit", http.MethodPost, "/admit", req, false, http.StatusOK)
	if err != nil {
		return runtimev3.AdmitResponseV3{}, err
	}
	c.setToken(r.value.Token.Token)
	return r.value, nil
}

// ContextV3 fetches the immutable ContextV3.
func (c *Client) ContextV3(ctx context.Context) (runtimev3.ContextV3, error) {
	if err := c.requireV3("getContext"); err != nil {
		return runtimev3.ContextV3{}, err
	}
	r, err := call[runtimev3.ContextV3](ctx, c, "getContext", http.MethodGet, "/context", nil, true, http.StatusOK)
	return r.value, err
}

// BeginV3 records the command-begin fence over every source.
func (c *Client) BeginV3(ctx context.Context, req runtimev3.BeginRequestV3) (runtimev3.BeginResponseV3, error) {
	if err := c.requireV3("begin"); err != nil {
		return runtimev3.BeginResponseV3{}, err
	}
	if err := req.Validate(); err != nil {
		return runtimev3.BeginResponseV3{}, &ProtocolError{Op: "begin", Err: err}
	}
	r, err := call[runtimev3.BeginResponseV3](ctx, c, "begin", http.MethodPost, "/begin", req, true, http.StatusOK)
	return r.value, err
}

// SubmitResultV3 submits the normalized v3 result (commits instead of a source
// commit).
func (c *Client) SubmitResultV3(ctx context.Context, req runtimev3.ResultRequestV3) (runtimev1.ResultResponse, error) {
	if err := c.requireV3("submitResult"); err != nil {
		return runtimev1.ResultResponse{}, err
	}
	if err := req.Validate(); err != nil {
		return runtimev1.ResultResponse{}, &ProtocolError{Op: "submitResult", Err: err}
	}
	r, err := call[runtimev1.ResultResponse](ctx, c, "submitResult", http.MethodPost, "/result", req, true, http.StatusOK)
	return r.value, err
}

// SeedGrant asks for the single-use seed grant of a repository this run created.
// The response carries a credential: the caller writes it to the advertised file
// and never logs it.
func (c *Client) SeedGrant(ctx context.Context, req runtimev3.GitSeedRequest) (runtimev3.GitGrantResponse, error) {
	if err := c.requireV3("seedGitGrant"); err != nil {
		return runtimev3.GitGrantResponse{}, err
	}
	if err := req.Validate(); err != nil {
		return runtimev3.GitGrantResponse{}, &ProtocolError{Op: "seedGitGrant", Err: err}
	}
	r, err := call[runtimev3.GitGrantResponse](ctx, c, "seedGitGrant", http.MethodPost, "/git/seed", req, true, http.StatusOK)
	return r.value, err
}

// IntegrationGrant asks for the integration grant of an approved gate.
func (c *Client) IntegrationGrant(ctx context.Context, req runtimev3.GitIntegrationRequest) (runtimev3.GitGrantResponse, error) {
	if err := c.requireV3("integrationGitGrant"); err != nil {
		return runtimev3.GitGrantResponse{}, err
	}
	if err := req.Validate(); err != nil {
		return runtimev3.GitGrantResponse{}, &ProtocolError{Op: "integrationGitGrant", Err: err}
	}
	r, err := call[runtimev3.GitGrantResponse](ctx, c, "integrationGitGrant", http.MethodPost, "/git/integration", req, true, http.StatusOK)
	return r.value, err
}

// ---- resource subjects on the v3 wire ----

// NewResourceClientV3 builds the client of /runtime/v3/runs/{runId}/resources/{key}.
func NewResourceClientV3(serverURL string, run domain.RunID, key, caFile string, retry RetryPolicy, log *slog.Logger) (*ResourceClientV3, error) {
	if err := run.Validate(); err != nil {
		return nil, err
	}
	if _, ok := resourceKindOfKey(key); !ok {
		return nil, fmt.Errorf("resource key %q is not preparation or service.NAME", key)
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	base := strings.TrimRight(serverURL, "/") + "/runtime/v3/runs/" + string(run) + "/resources/" + key
	c, err := newClientAt(base, caFile, retry, log, true)
	if err != nil {
		return nil, err
	}
	c.wire3 = true
	return &ResourceClientV3{c: c}, nil
}

// ResourceClientV3 is the HTTP adapter of one resource's v3 routes.
type ResourceClientV3 struct{ c *Client }

// resourceRuntimeV3 is the consumer-owned port of the v3 preparation: the v2
// resource exchanges with the v3 context and receipt.
type resourceRuntimeV3 interface {
	AdmitResourceV3(ctx context.Context, req runtimev3.ResourceAdmitRequest) (runtimev3.ResourceAdmitResponseV3, error)
	RenewResourceToken(ctx context.Context) (runtimev3.ResourceTokenResponse, error)
	ResourceContextV3(ctx context.Context) (runtimev3.ResourceContextV3, error)
	CompletePreparationV3(ctx context.Context, req runtimev3.PreparationReceiptRequestV3) (runtimev3.PreparationReceiptResponse, error)
	// Dispose drops every credential the port holds. Idempotent and irreversible.
	Dispose()
}

var _ resourceRuntimeV3 = (*ResourceClientV3)(nil)

// Dispose clears the bearer token; the client then sends nothing.
func (r *ResourceClientV3) Dispose() { r.c.dispose() }

// AdmitResourceV3 exchanges the one-time admission credential and stores the token.
func (r *ResourceClientV3) AdmitResourceV3(ctx context.Context, req runtimev3.ResourceAdmitRequest) (runtimev3.ResourceAdmitResponseV3, error) {
	if err := req.Validate(); err != nil {
		return runtimev3.ResourceAdmitResponseV3{}, &ProtocolError{Op: "admitResource", Err: err}
	}
	res, err := call[runtimev3.ResourceAdmitResponseV3](ctx, r.c, "admitResource", http.MethodPost, "/admit", req, false, http.StatusOK)
	if err != nil {
		return runtimev3.ResourceAdmitResponseV3{}, err
	}
	r.c.setToken(res.value.Token.Token)
	return res.value, nil
}

// RenewResourceToken renews the bearer token and stores the new one.
func (r *ResourceClientV3) RenewResourceToken(ctx context.Context) (runtimev3.ResourceTokenResponse, error) {
	res, err := call[runtimev3.ResourceTokenResponse](ctx, r.c, "renewResourceToken", http.MethodPost, "/token", runtimev3.ResourceTokenRequest{}, true, http.StatusOK)
	if err != nil {
		return runtimev3.ResourceTokenResponse{}, err
	}
	r.c.setToken(res.value.Token.Token)
	return res.value, nil
}

// ResourceContextV3 fetches the resource's committed context.
func (r *ResourceClientV3) ResourceContextV3(ctx context.Context) (runtimev3.ResourceContextV3, error) {
	res, err := call[runtimev3.ResourceContextV3](ctx, r.c, "getResourceContext", http.MethodGet, "/context", nil, true, http.StatusOK)
	return res.value, err
}

// CompletePreparationV3 records the per-source checkout receipt (idempotent).
func (r *ResourceClientV3) CompletePreparationV3(ctx context.Context, req runtimev3.PreparationReceiptRequestV3) (runtimev3.PreparationReceiptResponse, error) {
	if err := req.Validate(); err != nil {
		return runtimev3.PreparationReceiptResponse{}, &ProtocolError{Op: "completePreparation", Err: err}
	}
	res, err := call[runtimev3.PreparationReceiptResponse](ctx, r.c, "completePreparation", http.MethodPost, "/preparation/complete", req, true, http.StatusOK)
	return res.value, err
}

// ResolveSecret resolves one pinned binding of a service (the v2 body, unchanged).
func (r *ResourceClientV3) ResolveSecret(ctx context.Context, req runtimev1.SecretResolveRequest) (runtimev1.SecretResolveResponse, error) {
	if err := req.Validate(); err != nil {
		return runtimev1.SecretResolveResponse{}, &ProtocolError{Op: "resolveResourceSecret", Err: err}
	}
	res, err := call[runtimev1.SecretResolveResponse](ctx, r.c, "resolveResourceSecret", http.MethodPost, "/secrets/resolve", req, true, http.StatusOK)
	return res.value, err
}

// serviceAdapterV3 presents the v3 resource routes of a SERVICE to the supervisor
// that speaks the v2 resource port: the service message is the unchanged v2
// ServiceContext and only the context version differs (docs/contracts/runtime-v3.md,
// "Services are unchanged"), so the adapter re-wraps the committed context and
// keeps the v3 routes, token and error envelope. A v3 context that is not a
// service never reaches the supervisor.
type serviceAdapterV3 struct{ c *ResourceClientV3 }

var _ resourceRuntime = serviceAdapterV3{}

func (a serviceAdapterV3) Dispose() { a.c.Dispose() }

func wrapService(c runtimev3.ResourceContextV3) (runtimev2.ResourceContext, error) {
	if c.ResourceKind != runtimev3.ResourceService || c.Service == nil {
		return runtimev2.ResourceContext{}, errors.New("the committed context is not a service's")
	}
	return runtimev2.ResourceContext{
		ContextVersion: runtimev2.ContextVersion, ResourceKind: runtimev2.ResourceService, TenantID: c.TenantID, ProjectID: c.ProjectID,
		RunID: c.RunID, ResourceKey: c.ResourceKey, Service: c.Service, Policy: c.Policy,
	}, nil
}

func (a serviceAdapterV3) AdmitResource(ctx context.Context, req runtimev2.ResourceAdmitRequest) (runtimev2.ResourceAdmitResponse, error) {
	r, err := a.c.AdmitResourceV3(ctx, req)
	if err != nil {
		return runtimev2.ResourceAdmitResponse{}, err
	}
	c, err := wrapService(r.Context)
	if err != nil {
		return runtimev2.ResourceAdmitResponse{}, &ProtocolError{Op: "admitResource", Err: err}
	}
	return runtimev2.ResourceAdmitResponse{Token: r.Token, Context: c}, nil
}

func (a serviceAdapterV3) RenewResourceToken(ctx context.Context) (runtimev2.ResourceTokenResponse, error) {
	return a.c.RenewResourceToken(ctx)
}

func (a serviceAdapterV3) ResourceContext(ctx context.Context) (runtimev2.ResourceContext, error) {
	r, err := a.c.ResourceContextV3(ctx)
	if err != nil {
		return runtimev2.ResourceContext{}, err
	}
	c, err := wrapService(r)
	if err != nil {
		return runtimev2.ResourceContext{}, &ProtocolError{Op: "getResourceContext", Err: err}
	}
	return c, nil
}

func (a serviceAdapterV3) ResolveSecret(ctx context.Context, req runtimev1.SecretResolveRequest) (runtimev1.SecretResolveResponse, error) {
	return a.c.ResolveSecret(ctx, req)
}

func (a serviceAdapterV3) CompletePreparation(context.Context, runtimev2.PreparationReceiptRequest) (runtimev2.PreparationReceiptResponse, error) {
	return runtimev2.PreparationReceiptResponse{}, &ProtocolError{Op: "completePreparation", Err: errors.New("a service records no preparation")}
}
