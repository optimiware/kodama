package app

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"

	artifactsv1 "github.com/optimiware/kodama/contracts/artifacts/v1"
	deliveryv1 "github.com/optimiware/kodama/contracts/delivery/v1"
	"github.com/optimiware/kodama/internal/artifacts"
	artifactspg "github.com/optimiware/kodama/internal/artifacts/postgres"
	"github.com/optimiware/kodama/internal/cli/agent"
	"github.com/optimiware/kodama/internal/cli/auth"
	"github.com/optimiware/kodama/internal/cli/configuration"
	"github.com/optimiware/kodama/internal/cli/deployment"
	"github.com/optimiware/kodama/internal/cli/environment"
	"github.com/optimiware/kodama/internal/cli/gate"
	clipipeline "github.com/optimiware/kodama/internal/cli/pipeline"
	clirelease "github.com/optimiware/kodama/internal/cli/release"
	"github.com/optimiware/kodama/internal/cli/root"
	clirun "github.com/optimiware/kodama/internal/cli/run"
	"github.com/optimiware/kodama/internal/cli/work"
	"github.com/optimiware/kodama/internal/cli/workspace"
	"github.com/optimiware/kodama/internal/delivery"
	deliveryadapters "github.com/optimiware/kodama/internal/delivery/adapters"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/pipeline"
	"github.com/optimiware/kodama/internal/storage"
)

// LegacyDeliveryMode controls the bounded migration and retirement of the
// pre-ADR-0012 Kubernetes-specific delivery and ToolQualification surface.
type LegacyDeliveryMode string

const (
	LegacyDeliveryOpen    LegacyDeliveryMode = "open"
	LegacyDeliveryDrain   LegacyDeliveryMode = "drain"
	LegacyDeliveryRetired LegacyDeliveryMode = "retired"
)

func (m LegacyDeliveryMode) Valid() bool {
	return m == LegacyDeliveryOpen || m == LegacyDeliveryDrain || m == LegacyDeliveryRetired
}

// SDLCContributions returns the core schema contributions followed by the
// additive ADR-0012 artifact release and publication migration (005),
// preserving the exact ledger prefix of earlier releases.
func SDLCContributions() []storage.Contribution {
	return append(Contributions(), artifactspg.ReleaseMigrations())
}

// SDLCManifest validates SDLCContributions.
func SDLCManifest() (storage.Manifest, error) {
	return storage.NewManifest(SDLCContributions()...)
}

// ContainerSDLCInputs wires the ADR-0012 container-first SDLC slice:
// artifact catalogue, immutable releases, verified publications, admitted input
// fetch, and tool-neutral deployments without requiring a delivery-tool image
// catalogue or managed-agent model/MCP configuration.
type ContainerSDLCInputs struct {
	Env               Env
	Artifacts         *artifacts.Service
	ReleaseStore      artifacts.ReleaseStore
	ReleaseAuthority  artifacts.ReleaseAuthority
	PublicationVerify artifacts.PublicationVerifier
	GenericStore      delivery.GenericStore
	GenericDelivery   *delivery.GenericService
	LegacyMode        LegacyDeliveryMode
}

// ContainerSDLCSlice is the assembled ADR-0012 capability slice.
type ContainerSDLCSlice struct {
	Handler           http.Handler
	Effects           execution.EffectReconciler
	GenericDelivery   *delivery.GenericService
	LegacyMode        LegacyDeliveryMode
	mu                sync.Mutex
	legacyActiveCount int
}

// AssembleContainerSDLC validates inputs, enforces the production guard against
// development-only adapters on both the release store and generic deployment
// store, and wires the ADR-0012 public and runtime routes plus the conservative
// generic effect reconciler.
func AssembleContainerSDLC(in ContainerSDLCInputs) (*ContainerSDLCSlice, error) {
	if !in.Env.Valid() {
		return nil, ErrEnvUnknown
	}
	if in.Artifacts == nil || in.ReleaseStore == nil || in.ReleaseAuthority == nil || in.PublicationVerify == nil || in.GenericStore == nil || in.GenericDelivery == nil {
		return nil, errors.New("app/sdlc: artifact service, release store, release authority, publication verifier, generic store and generic delivery service are required")
	}
	if _, err := Admit(in.Env, PortStore, in.ReleaseStore); err != nil {
		return nil, err
	}
	if _, err := Admit(in.Env, PortStore, in.GenericStore); err != nil {
		return nil, err
	}
	mode := in.LegacyMode
	if mode == "" {
		mode = LegacyDeliveryDrain
	}
	if !mode.Valid() {
		return nil, errors.New("app/sdlc: invalid LegacyDeliveryMode")
	}

	effects, err := deliveryadapters.NewGenericEffects(in.GenericStore)
	if err != nil {
		return nil, err
	}
	s := &ContainerSDLCSlice{
		Effects:         effects,
		GenericDelivery: in.GenericDelivery,
		LegacyMode:      mode,
	}
	s.Handler = s.routes(in)
	return s, nil
}

// AdmitLegacyDelivery checks whether a legacy single-image/Kubernetes delivery
// admission is allowed under the current LegacyDeliveryMode. Under drain or
// retired, new legacy admissions are refused while existing records drain.
func (s *ContainerSDLCSlice) AdmitLegacyDelivery() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.LegacyMode != LegacyDeliveryOpen {
		return errors.New("legacy_delivery_drained: register a tool-neutral v2 deployment")
	}
	s.legacyActiveCount++
	return nil
}

func bearerOf(r *http.Request) (string, bool) {
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	if !strings.HasPrefix(h, "Bearer ") {
		return "", false
	}
	tok := strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	return tok, tok != ""
}

func (s *ContainerSDLCSlice) routes(in ContainerSDLCInputs) http.Handler {
	mux := http.NewServeMux()

	callerOf := func(r *http.Request) artifacts.Caller {
		tok, _ := bearerOf(r)
		return artifacts.Caller{Subject: tok}
	}

	mux.HandleFunc("GET /api/v1/tenants/{tenantId}/projects/{projectId}/artifacts", func(w http.ResponseWriter, r *http.Request) {
		scope := domain.Scope{TenantID: domain.TenantID(r.PathValue("tenantId")), ProjectID: domain.ProjectID(r.PathValue("projectId"))}
		items, err := in.Artifacts.ListArtifacts(r.Context(), callerOf(r), scope, "", 50, in.ReleaseStore, in.ReleaseAuthority)
		if err != nil {
			writeSDLCError(w, err)
			return
		}
		type itemDTO struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Kind   string `json:"kind"`
			Digest string `json:"digest"`
		}
		out := make([]itemDTO, 0, len(items))
		for _, a := range items {
			out = append(out, itemDTO{ID: string(a.ID()), Name: a.Name(), Kind: string(a.Kind()), Digest: string(a.Digest())})
		}
		writeSDLCJSON(w, http.StatusOK, map[string]any{"items": out})
	})

	mux.HandleFunc("POST /api/v1/tenants/{tenantId}/projects/{projectId}/releases", func(w http.ResponseWriter, r *http.Request) {
		scope := domain.Scope{TenantID: domain.TenantID(r.PathValue("tenantId")), ProjectID: domain.ProjectID(r.PathValue("projectId"))}
		if strings.TrimSpace(r.Header.Get("Idempotency-Key")) == "" {
			writeSDLCJSON(w, http.StatusBadRequest, map[string]any{"error": "missing Idempotency-Key"})
			return
		}
		var body struct {
			Name         string                               `json:"name"`
			VersionLabel string                               `json:"versionLabel"`
			Artifacts    map[string]artifactsv1.ManifestEntry `json:"artifacts"`
			Sources      []artifactsv1.SourceRef              `json:"sources"`
			Producers    []artifactsv1.ProducerRef            `json:"producers"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeSDLCJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
			return
		}
		rel, created, err := in.Artifacts.CreateRelease(r.Context(), callerOf(r), artifactsv1.ReleaseManifestInput{
			Scope:        scope,
			Name:         body.Name,
			VersionLabel: body.VersionLabel,
			Artifacts:    body.Artifacts,
			Sources:      body.Sources,
			Producers:    body.Producers,
		}, "req_rel_create", in.ReleaseStore, in.ReleaseAuthority)
		if err != nil {
			writeSDLCError(w, err)
			return
		}
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		writeSDLCJSON(w, status, releaseDTO(rel))
	})

	mux.HandleFunc("GET /api/v1/tenants/{tenantId}/projects/{projectId}/releases", func(w http.ResponseWriter, r *http.Request) {
		scope := domain.Scope{TenantID: domain.TenantID(r.PathValue("tenantId")), ProjectID: domain.ProjectID(r.PathValue("projectId"))}
		items, err := in.Artifacts.ListReleases(r.Context(), callerOf(r), scope, r.URL.Query().Get("name"), "", 50, in.ReleaseStore, in.ReleaseAuthority)
		if err != nil {
			writeSDLCError(w, err)
			return
		}
		out := make([]map[string]any, 0, len(items))
		for _, rel := range items {
			out = append(out, releaseDTO(rel))
		}
		writeSDLCJSON(w, http.StatusOK, map[string]any{"items": out})
	})

	mux.HandleFunc("GET /api/v1/tenants/{tenantId}/projects/{projectId}/releases/resolve", func(w http.ResponseWriter, r *http.Request) {
		scope := domain.Scope{TenantID: domain.TenantID(r.PathValue("tenantId")), ProjectID: domain.ProjectID(r.PathValue("projectId"))}
		q := r.URL.Query()
		rel, err := in.Artifacts.ResolveRelease(r.Context(), callerOf(r), scope, q.Get("name"), q.Get("version"), in.ReleaseStore, in.ReleaseAuthority)
		if err != nil {
			writeSDLCError(w, err)
			return
		}
		writeSDLCJSON(w, http.StatusOK, releaseDTO(rel))
	})

	mux.HandleFunc("GET /api/v1/tenants/{tenantId}/projects/{projectId}/releases/{id}", func(w http.ResponseWriter, r *http.Request) {
		scope := domain.Scope{TenantID: domain.TenantID(r.PathValue("tenantId")), ProjectID: domain.ProjectID(r.PathValue("projectId"))}
		rel, err := in.Artifacts.GetRelease(r.Context(), callerOf(r), scope, domain.ReleaseID(r.PathValue("id")), in.ReleaseStore, in.ReleaseAuthority)
		if err != nil {
			writeSDLCError(w, err)
			return
		}
		writeSDLCJSON(w, http.StatusOK, releaseDTO(rel))
	})

	mux.HandleFunc("POST /api/v1/tenants/{tenantId}/projects/{projectId}/releases/{id}/retire", func(w http.ResponseWriter, r *http.Request) {
		scope := domain.Scope{TenantID: domain.TenantID(r.PathValue("tenantId")), ProjectID: domain.ProjectID(r.PathValue("projectId"))}
		rawVer := strings.Trim(strings.TrimSpace(r.Header.Get("If-Match")), `"`)
		v, err := strconv.ParseInt(rawVer, 10, 64)
		if err != nil {
			writeSDLCJSON(w, http.StatusPreconditionRequired, map[string]any{"error": "valid If-Match version required"})
			return
		}
		rel, err := in.Artifacts.RetireRelease(r.Context(), callerOf(r), scope, domain.ReleaseID(r.PathValue("id")), domain.Version(v), "req_retire", in.ReleaseStore, in.ReleaseAuthority)
		if err != nil {
			writeSDLCError(w, err)
			return
		}
		writeSDLCJSON(w, http.StatusOK, releaseDTO(rel))
	})

	mux.HandleFunc("GET /api/v1/tenants/{tenantId}/projects/{projectId}/artifacts/{id}/publications", func(w http.ResponseWriter, r *http.Request) {
		scope := domain.Scope{TenantID: domain.TenantID(r.PathValue("tenantId")), ProjectID: domain.ProjectID(r.PathValue("projectId"))}
		items, err := in.Artifacts.ListPublications(r.Context(), callerOf(r), scope, domain.ArtifactID(r.PathValue("id")), "", 50, in.ReleaseStore, in.ReleaseAuthority)
		if err != nil {
			writeSDLCError(w, err)
			return
		}
		writeSDLCJSON(w, http.StatusOK, map[string]any{"items": items})
	})

	// Runtime admitted-input fetch route (authenticated by attempt runtime token).
	mux.HandleFunc("POST /runtime/v2/attempts/{attemptId}/inputs/fetch", func(w http.ResponseWriter, r *http.Request) {
		tok, ok := bearerOf(r)
		if !ok {
			writeSDLCJSON(w, http.StatusUnauthorized, map[string]any{"error": "missing runtime bearer token"})
			return
		}
		attempt := domain.AttemptID(r.PathValue("attemptId"))
		var req artifactsv1.InputFetchRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeSDLCJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
			return
		}
		resp, err := in.Artifacts.FetchInput(r.Context(), artifacts.Caller{Subject: tok}, attempt, req, in.ReleaseAuthority)
		if err != nil {
			writeSDLCError(w, err)
			return
		}
		writeSDLCJSON(w, http.StatusOK, resp)
	})

	// Runtime publication verification and receipt route (authenticated by attempt runtime token).
	mux.HandleFunc("POST /runtime/v2/attempts/{attemptId}/publications", func(w http.ResponseWriter, r *http.Request) {
		tok, ok := bearerOf(r)
		if !ok {
			writeSDLCJSON(w, http.StatusUnauthorized, map[string]any{"error": "missing runtime bearer token"})
			return
		}
		attempt := domain.AttemptID(r.PathValue("attemptId"))
		var req struct {
			ArtifactID  domain.ArtifactID          `json:"artifactId"`
			Destination artifactsv1.DestinationRef `json:"destination"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeSDLCJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
			return
		}
		receipt, created, err := in.Artifacts.RecordPublication(
			r.Context(), artifacts.Caller{Subject: tok}, attempt, req.ArtifactID, req.Destination, "req_pub",
			in.ReleaseStore, in.ReleaseAuthority, in.PublicationVerify,
		)
		if err != nil {
			writeSDLCError(w, err)
			return
		}
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		writeSDLCJSON(w, status, receipt)
	})

	// Runtime tool-neutral deployment registration route: authenticates the
	// runtime deployment.call grant, derives scope/attempt/run/actionKey/pins
	// from the grant's immutable pinned context, pins referenced artifacts, and
	// commits to the durable GenericStore.
	mux.HandleFunc("POST /runtime/v2/attempts/{attemptId}/deployments/generic", func(w http.ResponseWriter, r *http.Request) {
		tok, ok := bearerOf(r)
		if !ok {
			writeSDLCJSON(w, http.StatusUnauthorized, map[string]any{"error": "missing runtime bearer token"})
			return
		}
		attempt := domain.AttemptID(r.PathValue("attemptId"))
		var req delivery.GenericRuntimeRegisterRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeSDLCJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
			return
		}
		dep, created, err := s.GenericDelivery.RegisterRuntime(r.Context(), attempt, tok, req)
		if err != nil {
			writeDeliveryError(w, err)
			return
		}
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		writeSDLCJSON(w, status, map[string]any{
			"id":      string(dep.ID()),
			"status":  string(dep.Status()),
			"reason":  dep.Reason(),
			"version": int64(dep.Version()),
			"created": created,
		})
	})

	// Runtime tool-neutral deployment outcome route.
	mux.HandleFunc("POST /runtime/v2/attempts/{attemptId}/deployments/generic/{id}/outcome", func(w http.ResponseWriter, r *http.Request) {
		tok, ok := bearerOf(r)
		if !ok {
			writeSDLCJSON(w, http.StatusUnauthorized, map[string]any{"error": "missing runtime bearer token"})
			return
		}
		attempt := domain.AttemptID(r.PathValue("attemptId"))
		id := delivery.DeploymentID(r.PathValue("id"))
		var out deliveryv1.GenericDeploymentOutcome
		if err := json.NewDecoder(r.Body).Decode(&out); err != nil {
			writeSDLCJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
			return
		}
		dep, err := s.GenericDelivery.ReportRuntimeOutcome(r.Context(), attempt, tok, id, out)
		if err != nil {
			writeDeliveryError(w, err)
			return
		}
		writeSDLCJSON(w, http.StatusOK, map[string]any{
			"id":           string(dep.ID()),
			"status":       string(dep.Status()),
			"reason":       dep.Reason(),
			"attribution":  string(dep.Attribution()),
			"externalRefs": dep.ExternalRefs(),
			"version":      int64(dep.Version()),
		})
	})

	// Human/operator explicit unknown-outcome settlement route.
	mux.HandleFunc("POST /api/v1/tenants/{tenantId}/projects/{projectId}/deployments/generic/{id}/settle", func(w http.ResponseWriter, r *http.Request) {
		tok, ok := bearerOf(r)
		if !ok {
			writeSDLCJSON(w, http.StatusUnauthorized, map[string]any{"error": "missing bearer token"})
			return
		}
		scope := domain.Scope{TenantID: domain.TenantID(r.PathValue("tenantId")), ProjectID: domain.ProjectID(r.PathValue("projectId"))}
		id := delivery.DeploymentID(r.PathValue("id"))
		var body struct {
			Status       delivery.Status   `json:"status"`
			Reason       string            `json:"reason"`
			ExternalRefs map[string]string `json:"externalRefs"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeSDLCJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
			return
		}
		caller := delivery.Caller{Actor: delivery.Actor{Type: delivery.ActorUser, ID: tok}, RequestID: "req_settle"}
		dep, err := s.GenericDelivery.SettleUnknown(r.Context(), caller, scope, id, body.Status, body.Reason, body.ExternalRefs)
		if err != nil {
			writeDeliveryError(w, err)
			return
		}
		writeSDLCJSON(w, http.StatusOK, map[string]any{
			"id":                string(dep.ID()),
			"status":            string(dep.Status()),
			"reason":            dep.Reason(),
			"explicitlySettled": dep.ExplicitlySettled(),
			"version":           int64(dep.Version()),
		})
	})

	return mux
}

func releaseDTO(rel artifacts.Release) map[string]any {
	return map[string]any{
		"id":             string(rel.ID()),
		"name":           rel.Name(),
		"versionLabel":   rel.VersionLabel(),
		"manifestDigest": string(rel.ManifestDigest()),
		"version":        int64(rel.Version()),
		"retired":        rel.Retired(),
		"artifacts":      rel.Manifest().Entries(),
	}
}

func writeSDLCError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, artifacts.ErrInvalid):
		status = http.StatusBadRequest
	case errors.Is(err, artifacts.ErrAuthorityMissing):
		status = http.StatusUnauthorized
	case errors.Is(err, artifacts.ErrForbidden), errors.Is(err, artifacts.ErrAttemptInactive):
		status = http.StatusForbidden
	case errors.Is(err, artifacts.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, artifacts.ErrIdempotencyConflict), errors.Is(err, artifacts.ErrReleaseRetired), errors.Is(err, artifacts.ErrIncomplete):
		status = http.StatusConflict
	case errors.Is(err, artifacts.ErrVersionConflict):
		status = http.StatusPreconditionFailed
	case errors.Is(err, artifacts.ErrOutputInvalid), errors.Is(err, artifacts.ErrDigestMismatch):
		status = http.StatusUnprocessableEntity
	}
	writeSDLCJSON(w, status, map[string]any{"error": err.Error()})
}

func writeDeliveryError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, delivery.ErrInvalid):
		status = http.StatusBadRequest
	case errors.Is(err, delivery.ErrUnauthenticated):
		status = http.StatusUnauthorized
	case errors.Is(err, delivery.ErrDenied), errors.Is(err, delivery.ErrPinRequired):
		status = http.StatusForbidden
	case errors.Is(err, delivery.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, delivery.ErrConflict), errors.Is(err, delivery.ErrOutcomeUnknown):
		status = http.StatusConflict
	case errors.Is(err, delivery.ErrProvenance), errors.Is(err, delivery.ErrCredentialUnavailable):
		status = http.StatusUnprocessableEntity
	}
	writeSDLCJSON(w, status, map[string]any{"error": err.Error()})
}

func writeSDLCJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// NewSDLCCLI builds the CLI runner with the ADR-0012 `artifact`, `release` and
// `publication` command groups enabled alongside the core groups.
func NewSDLCCLI(cfg CLIConfig) (*root.Runner, error) {
	getenv := cfg.Getenv
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	ctxPath := getenv("KODAMA_CONTEXT_FILE")
	if ctxPath == "" {
		p, err := root.DefaultContextPath()
		if err != nil {
			return nil, err
		}
		ctxPath = p
	}
	insecure := getenv("KODAMA_ALLOW_INSECURE_LOOPBACK") == "true"
	authCfg := auth.ServiceConfig{HTTP: cfg.HTTP, AllowInsecureLoopbackEndpoints: insecure}
	if dir := getenv("KODAMA_CREDENTIAL_DIR"); dir != "" {
		authCfg.File = auth.NewFileStore(dir)
	}
	authSvc := auth.NewService(authCfg)
	validator, err := pipeline.NewValidator(pipeline.Config{
		Capabilities:   pipeline.EnabledKinds{pipeline.KindContainer, pipeline.KindAgent, pipeline.KindGate},
		EnableV1Alpha2: true,
	})
	if err != nil {
		return nil, err
	}
	pipelineCmds, err := clipipeline.NewCommands(clipipeline.Config{Validator: validator, Actor: pipeline.Actor{ID: "local-validation", Type: pipeline.ActorUser}})
	if err != nil {
		return nil, err
	}
	runCmds, err := clirun.NewCommands(clirun.Config{Tokens: authSvc, HTTP: cfg.HTTP, AllowInsecureLoopback: insecure})
	if err != nil {
		return nil, err
	}
	return root.NewRunner(root.RunnerConfig{
		Streams: cfg.Streams, Contexts: root.NewFileContextStore(ctxPath), Tokens: authSvc, HTTP: cfg.HTTP, AllowInsecureLoopback: insecure,
	},
		root.NewContextCommands(), auth.NewCommands(authSvc), work.NewCommands(), workspace.NewCommands(),
		pipelineCmds, runCmds, configuration.NewVariableCommands(), configuration.NewSecretCommands(),
		agent.NewCommands(), gate.NewCommands(), environment.NewCommands(), deployment.NewCommands(),
		clirelease.NewArtifactCommands(), clirelease.NewReleaseCommands(), clirelease.NewPublicationCommands(),
	)
}
