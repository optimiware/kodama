package app

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/optimiware/kodama/internal/artifacts"
	artifactshttp "github.com/optimiware/kodama/internal/artifacts/http"
	"github.com/optimiware/kodama/internal/execution"
	executionhttp "github.com/optimiware/kodama/internal/execution/http"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/identity"
	identityhttp "github.com/optimiware/kodama/internal/identity/http"
	"github.com/optimiware/kodama/internal/observation"
	obshttp "github.com/optimiware/kodama/internal/observation/http"
	"github.com/optimiware/kodama/internal/pipeline"
	pipelinehttp "github.com/optimiware/kodama/internal/pipeline/http"
	"github.com/optimiware/kodama/internal/secrets"
	secretshttp "github.com/optimiware/kodama/internal/secrets/http"
	"github.com/optimiware/kodama/internal/work"
	workhttp "github.com/optimiware/kodama/internal/work/http"
	"github.com/optimiware/kodama/internal/workspace"
	workspacehttp "github.com/optimiware/kodama/internal/workspace/http"
)

// PublicInputs are the module services behind the public API and the identity
// seams that guard it. NewPublicRouter is the only place the routes of the
// supported subset are registered: the PostgreSQL composition (NewCore) and the
// in-memory development composition both call it, so a route cannot be served
// by one and missing from the other, and every route passes the same
// authenticate → throttle → authorize chain with the same rule table.
type PublicInputs struct {
	Authenticator httpapi.Authenticator
	Authority     identity.Authority
	Log           *slog.Logger
	Tuning        Tuning
	Cursors       *httpapi.CursorCodec
	// IdempotencyHashKey keys the fingerprint of secret-carrying requests.
	IdempotencyHashKey []byte

	Identity     *identity.Service
	Work         *work.Service
	Secrets      *secrets.Service
	SecretsKeyed secretshttp.Keyed
	Pipelines    *pipeline.Service
	Runs         *execution.Service
	RunCreator   executionhttp.Creator
	Observation  *observation.Service
	Artifacts    *artifacts.Service
	Workspaces   *workspace.Service
	Slice        *ExecutionSlice
	// Delivery is the manual agent/gate/delivery slice (T-072): agent profiles,
	// gates, environments and deployment records.
	Delivery *DeliverySlice
	// ModelTools is the model gateway and MCP proxy (T-039): the management
	// routes of model settings and MCP registrations. Nil when the installation
	// selected neither, which leaves them unregistered (recorded as disabled).
	ModelTools *ModelToolSlice
	// BusinessLoop is the business-loop slice (T-075): the dependency routes and,
	// when selected, the ChatOps destination routes. Nil leaves them unregistered
	// (recorded as disabled), as for the M1 and M2 compositions.
	BusinessLoop *BusinessLoopSlice
}

type routeRegistrar interface {
	RegisterRoutes(*httpapi.Router) error
}

type workspaceRegistrar struct{ h *workspacehttp.Handler }

func (w workspaceRegistrar) RegisterRoutes(rt *httpapi.Router) error { return w.h.Register(rt) }

// NewPublicRouter builds the deny-by-default router and registers the public
// routes of the supported subset. It returns the router and the names of the
// route groups it registered (recorded in Routes). A missing service is a
// construction error: a group is never silently skipped.
func NewPublicRouter(in PublicInputs) (*httpapi.Router, []string, error) {
	switch {
	case in.Authenticator == nil, in.Authority == nil, in.Cursors == nil:
		return nil, nil, errors.New("app: an authenticator, an authority and a cursor codec are required")
	case in.Identity == nil, in.Work == nil, in.Secrets == nil, in.SecretsKeyed == nil, in.Pipelines == nil, in.Runs == nil,
		in.RunCreator == nil, in.Observation == nil, in.Artifacts == nil, in.Workspaces == nil, in.Slice == nil, in.Delivery == nil:
		return nil, nil, errors.New("app: every module service of the public slice is required")
	}
	authz, err := identityhttp.NewAuthorizer(in.Authority, coreRules())
	if err != nil {
		return nil, nil, err
	}
	router, err := httpapi.NewRouter(httpapi.RouterConfig{
		Authenticator: in.Authenticator, Authorizer: authz, RateLimiter: newRateLimiter(in.Tuning.RateLimitPerMinute, nil),
		Prefix: APIPrefix, Logger: in.Log,
	})
	if err != nil {
		return nil, nil, err
	}
	idHandler, err := identityhttp.NewHandler(in.Identity, in.Cursors)
	if err != nil {
		return nil, nil, err
	}
	workHandler, err := workhttp.NewHandler(in.Work, in.Cursors)
	if err != nil {
		return nil, nil, err
	}
	secretHandler, err := secretshttp.NewHandler(secretshttp.Config{Service: in.Secrets, Keyed: in.SecretsKeyed, Cursors: in.Cursors, HashKey: in.IdempotencyHashKey})
	if err != nil {
		return nil, nil, err
	}
	pipeHandler, err := pipelinehttp.NewHandler(in.Pipelines, in.Cursors)
	if err != nil {
		return nil, nil, err
	}
	runHandler, err := executionhttp.NewHandler(executionhttp.Config{Service: in.Runs, Creator: in.RunCreator, Cursors: in.Cursors})
	if err != nil {
		return nil, nil, err
	}
	obsHandler, err := obshttp.NewHandler(in.Observation, in.Cursors, obshttp.HandlerOptions{
		WriteTimeout: in.Tuning.ObservationWriteTimeout, MaxBufferedBytes: in.Tuning.ObservationBufferBytes, Logger: in.Log,
	})
	if err != nil {
		return nil, nil, err
	}
	artHandler, err := artifactshttp.NewPublic(in.Artifacts, artifactshttp.DefaultActions())
	if err != nil {
		return nil, nil, err
	}
	wsHandler, err := workspacehttp.NewHandler(in.Workspaces)
	if err != nil {
		return nil, nil, err
	}
	for _, h := range []routeRegistrar{idHandler, workHandler, secretHandler, pipeHandler, runHandler, obsHandler, artHandler, workspaceRegistrar{wsHandler}} {
		if err := h.RegisterRoutes(router); err != nil {
			return nil, nil, fmt.Errorf("register routes: %w", err)
		}
	}
	if err := in.Slice.RegisterPublic(router); err != nil {
		return nil, nil, fmt.Errorf("register routes: %w", err)
	}
	if err := in.Delivery.RegisterPublic(router); err != nil {
		return nil, nil, fmt.Errorf("register routes: %w", err)
	}
	if in.ModelTools != nil {
		if err := in.ModelTools.RegisterPublic(router); err != nil {
			return nil, nil, fmt.Errorf("register routes: %w", err)
		}
	}
	if in.BusinessLoop != nil {
		if err := in.BusinessLoop.RegisterPublic(router); err != nil {
			return nil, nil, fmt.Errorf("register routes: %w", err)
		}
	}
	groups := append([]string{"identity", "work", "variables", "secrets", "pipeline-definitions", "runs", "events", "artifacts", "workspaces", "executor-dispatch"}, in.Slice.Routes...)
	groups = append(groups, in.Delivery.Routes...)
	if in.ModelTools != nil {
		groups = append(groups, in.ModelTools.Routes...)
	}
	if in.BusinessLoop != nil {
		groups = append(groups, in.BusinessLoop.Routes...)
	}
	return router, groups, nil
}

// activated are the capabilities the execution slice records as not wired until
// the manual delivery slice (T-072) serves them.
var activated = map[string]bool{"gates": true, "agents": true, "delivery": true, "cleanup-runs": true}

// M2Slices are the optional M2 contributions (T-039) that activate capabilities
// the earlier slices record as not wired.
type M2Slices struct {
	ModelTools *ModelToolSlice
	Triggers   *TriggerSlice
}

// DisabledCapabilities merges what the execution slice and the delivery slice
// record as deliberately not wired: the delivery slice activates the capabilities
// the execution slice listed pending it and adds its own refusals.
func DisabledCapabilities(slice *ExecutionSlice, d *DeliverySlice) []string {
	return DisabledCapabilitiesM2(slice, d, M2Slices{})
}

// DisabledCapabilitiesM2 is DisabledCapabilities after the M2 composition: the
// Git trigger slice activates `git-webhooks`, and the delivery slice's own
// `model-gateway`, `mcp-proxy` and `agent-steps` refusals are dropped when the
// model/tool slice is wired (agent steps still need a qualified wrapper
// catalogue, which the delivery slice's caller reports itself).
func DisabledCapabilitiesM2(slice *ExecutionSlice, d *DeliverySlice, m M2Slices) []string {
	var out []string
	for _, name := range slice.Disabled {
		if d != nil && activated[name] {
			continue
		}
		if name == "git-webhooks" && m.Triggers != nil {
			continue
		}
		out = append(out, name)
	}
	if d != nil {
		out = append(out, d.Disabled...)
	}
	return out
}
