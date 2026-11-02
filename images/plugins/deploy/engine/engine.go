// Package engine implements the provider-neutral deployment lifecycle used by
// the deploy plugin images. Provider work is delegated to a driver; the engine
// owns reservations, durable intent, history and recovery.
package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	statev1 "github.com/optimiware/kodama/contracts/state/v1"
	"github.com/optimiware/kodama/images/plugins/deploy/driver"
	pluginstate "github.com/optimiware/kodama/pkg/pluginruntime/state"
)

const schemaVersion = 1

var (
	ErrBusy        = errors.New("deploy: another operation is active")
	ErrConflict    = errors.New("deploy: operation input conflicts with the recorded operation")
	ErrCheckpoint  = errors.New("deploy: durable checkpoint failed")
	ErrUnavailable = errors.New("deploy: state service unavailable")
)

// State is the narrow state client used by the engine.
type State interface {
	Read(ctx context.Context, key string) (statev1.Entry, string, error)
	Create(ctx context.Context, key string, req statev1.WriteRequest) (statev1.Entry, string, error)
	Update(ctx context.Context, key, etag string, req statev1.WriteRequest) (statev1.Entry, string, error)
	Delete(ctx context.Context, key, etag string) error
	List(ctx context.Context, prefix, cursor string, limit int) ([]statev1.Entry, string, error)
}

// Config is one engine invocation. Configuration is provider configuration
// (namespace, release, manifest path); Artifacts are verified descriptors.
type Config struct {
	ActionKey      string
	OperationID    string
	Resource       string
	Recipe         string
	RecipeVersion  int64
	Configuration  json.RawMessage
	Artifacts      []driver.Artifact
	DesiredRelease string
	Timeout        time.Duration
	ExpiresIn      time.Duration
	PlanDigest     string
	Now            func() time.Time
}

// Engine executes deployment operations.
type Engine struct {
	State  State
	Driver driver.Driver
	Clock  func() time.Time
}

// PlanDoc is the immutable, provider-neutral intent produced by `plan`. Its
// fingerprint is the apply fingerprint of the exact artifacts and configuration,
// so an approved plan cannot be silently changed before apply.
type PlanDoc struct {
	SchemaVersion int               `json:"schemaVersion"`
	Resource      string            `json:"resource"`
	Release       string            `json:"release"`
	Artifacts     []driver.Artifact `json:"artifacts,omitempty"`
	Configuration json.RawMessage   `json:"configuration"`
	Fingerprint   string            `json:"fingerprint"`
	CreatedAt     time.Time         `json:"createdAt"`
}

// Plan validates the inputs and returns the immutable intent without touching
// provider state.
func (e *Engine) Plan(ctx context.Context, cfg Config) (PlanDoc, error) {
	if err := validateConfig(cfg); err != nil {
		return PlanDoc{}, err
	}
	identity, err := e.Driver.Identify(ctx, driver.IdentityRequest{Recipe: cfg.Recipe, RecipeVersion: cfg.RecipeVersion, Configuration: cfg.Configuration})
	if err != nil {
		return PlanDoc{}, err
	}
	if cfg.DesiredRelease == "" {
		cfg.DesiredRelease, err = ReleaseID(identity, cfg.Artifacts, cfg.Configuration)
		if err != nil {
			return PlanDoc{}, err
		}
	}
	fingerprint, err := fingerprint(cfg, identity)
	if err != nil {
		return PlanDoc{}, err
	}
	return PlanDoc{SchemaVersion: schemaVersion, Resource: identity.ResourceKey, Release: cfg.DesiredRelease,
		Artifacts: append([]driver.Artifact(nil), cfg.Artifacts...), Configuration: cfg.Configuration,
		Fingerprint: fingerprint, CreatedAt: e.now()}, nil
}

type resourceDoc struct {
	SchemaVersion      int                         `json:"schemaVersion"`
	Identity           driver.Identity             `json:"identity"`
	Recipe             string                      `json:"recipe,omitempty"`
	RecipeVersion      int64                       `json:"recipeVersion,omitempty"`
	Configuration      json.RawMessage             `json:"configuration,omitempty"`
	DesiredRelease     string                      `json:"desiredRelease,omitempty"`
	ObservedRelease    string                      `json:"observedRelease,omitempty"`
	ProviderRelease    string                      `json:"providerRelease,omitempty"`
	ProviderVersion    string                      `json:"providerVersion,omitempty"`
	ActiveOperation    string                      `json:"activeOperation,omitempty"`
	LastOperation      string                      `json:"lastOperation,omitempty"`
	LastHealthyRelease string                      `json:"lastHealthyRelease,omitempty"`
	Resources          []driver.Resource           `json:"resources,omitempty"`
	ArtifactReferences []statev1.ArtifactReference `json:"artifactReferences,omitempty"`
	// PendingRelease is the observed outcome waiting for secondary history
	// publication. It lets reconciliation finish publication from the
	// authoritative document instead of guessing from a new invocation.
	PendingRelease *releaseDoc `json:"pendingRelease,omitempty"`
	ExpiresAt      *time.Time  `json:"expiresAt,omitempty"`
	DeletedAt      *time.Time  `json:"deletedAt,omitempty"`
	UpdatedAt      time.Time   `json:"updatedAt"`
}

type operationDoc struct {
	SchemaVersion int       `json:"schemaVersion"`
	ID            string    `json:"id"`
	Kind          string    `json:"kind"`
	Fingerprint   string    `json:"fingerprint"`
	Status        string    `json:"status"`
	Effect        string    `json:"effect,omitempty"`
	Error         string    `json:"error,omitempty"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
	// The immutable invocation input is retained with the operation so a
	// reconciliation run can finish the original intent even when its own CLI
	// arguments differ.
	DesiredRelease string                     `json:"desiredRelease,omitempty"`
	Artifacts      []driver.Artifact          `json:"artifacts,omitempty"`
	Configuration  json.RawMessage            `json:"configuration,omitempty"`
	Checkpoints    map[string]json.RawMessage `json:"checkpoints,omitempty"`
	Observation    *driver.Observation        `json:"observation,omitempty"`
	Result         *driver.Result             `json:"result,omitempty"`
}

type releaseDoc struct {
	SchemaVersion      int                         `json:"schemaVersion"`
	ResourceKey        string                      `json:"resourceKey,omitempty"`
	Release            string                      `json:"release"`
	Artifacts          []driver.Artifact           `json:"artifacts,omitempty"`
	ProviderRelease    string                      `json:"providerRelease,omitempty"`
	ProviderVersion    string                      `json:"providerVersion,omitempty"`
	CreatedAt          time.Time                   `json:"createdAt"`
	Resources          []driver.Resource           `json:"resources,omitempty"`
	ArtifactReferences []statev1.ArtifactReference `json:"artifactReferences,omitempty"`
}

type Outcome struct {
	Resource  resourceDoc  `json:"resource"`
	Operation operationDoc `json:"operation"`
}

func (e *Engine) now() time.Time {
	if e.Clock != nil {
		return e.Clock().UTC()
	}
	return time.Now().UTC()
}

// Apply reserves the resource, persists intent, invokes the driver and settles
// the operation from an observation. It never blindly retries a mutation.
func (e *Engine) Apply(ctx context.Context, cfg Config) (Outcome, error) {
	if e.State == nil || e.Driver == nil {
		return Outcome{}, fmt.Errorf("deploy: state and driver are required")
	}
	if err := validateConfig(cfg); err != nil {
		return Outcome{}, err
	}
	identityReq := driver.IdentityRequest{Recipe: cfg.Recipe, RecipeVersion: cfg.RecipeVersion, Configuration: cfg.Configuration}
	identity, err := e.Driver.Identify(ctx, identityReq)
	if err != nil {
		return Outcome{}, err
	}
	if cfg.DesiredRelease == "" {
		cfg.DesiredRelease, err = ReleaseID(identity, cfg.Artifacts, cfg.Configuration)
		if err != nil {
			return Outcome{}, err
		}
	}
	resourceKey, err := keyFor("resources", identity.ResourceKey)
	if err != nil {
		return Outcome{}, err
	}
	resourceEntry, resourceETag, err := e.loadOrCreateResource(ctx, resourceKey, identity, cfg)
	if err != nil {
		return Outcome{}, err
	}
	var resource resourceDoc
	if err := json.Unmarshal(resourceEntry.Value, &resource); err != nil {
		return Outcome{}, fmt.Errorf("deploy: decode resource state: %w", err)
	}
	if err := checkSchema("resource", resource.SchemaVersion); err != nil {
		return Outcome{}, err
	}
	if resource.DeletedAt != nil {
		return Outcome{}, fmt.Errorf("deploy: resource is deleted")
	}
	opID := cfg.OperationID
	if opID == "" {
		opID = cfg.ActionKey
	}
	if opID == "" {
		return Outcome{}, fmt.Errorf("deploy: operation id or action key is required")
	}
	opKey, err := keyFor("operations", identity.ResourceKey+"/"+opID)
	if err != nil {
		return Outcome{}, err
	}
	fingerprint, err := fingerprint(cfg, identity)
	if err != nil {
		return Outcome{}, err
	}
	if cfg.PlanDigest != "" && cfg.PlanDigest != fingerprint {
		return Outcome{}, ErrConflict
	}
	if existing, readErr := e.readOperation(ctx, opKey); readErr == nil {
		if existing.Fingerprint != fingerprint {
			return Outcome{}, ErrConflict
		}
		switch existing.Status {
		case "succeeded":
			return e.finishExisting(ctx, resourceKey, resourceETag, resource, existing)
		case "unknown":
			return e.reconcileOperation(ctx, identity, resourceKey, resource, resourceETag, opKey, existing, cfg)
		default:
			return Outcome{}, fmt.Errorf("deploy: operation %s is %s", opID, existing.Status)
		}
	} else if !isNotFound(readErr) {
		return Outcome{}, readErr
	}
	if resource.ActiveOperation != "" {
		if resource.ActiveOperation != opID {
			return Outcome{}, ErrBusy
		}
		op, err := e.readOperation(ctx, opKey)
		if err != nil {
			return Outcome{}, err
		}
		if op.Fingerprint != fingerprint {
			return Outcome{}, ErrConflict
		}
		if op.Status == "succeeded" {
			return e.finishExisting(ctx, resourceKey, resourceETag, resource, op)
		}
		if op.Status == "unknown" {
			return e.reconcileOperation(ctx, identity, resourceKey, resource, resourceETag, opKey, op, cfg)
		}
		return Outcome{}, fmt.Errorf("deploy: operation %s is %s", opID, op.Status)
	}

	now := e.now()
	resource.Recipe = cfg.Recipe
	resource.RecipeVersion = cfg.RecipeVersion
	resource.Configuration = cfg.Configuration
	if cfg.ExpiresIn > 0 {
		expires := now.Add(cfg.ExpiresIn)
		resource.ExpiresAt = &expires
	}
	op := operationDoc{SchemaVersion: schemaVersion, ID: opID, Kind: "apply", Fingerprint: fingerprint,
		Status: "pending", CreatedAt: now, UpdatedAt: now, DesiredRelease: cfg.DesiredRelease,
		Artifacts: append([]driver.Artifact(nil), cfg.Artifacts...), Configuration: cfg.Configuration,
		Checkpoints: map[string]json.RawMessage{}}
	opEntry, etag, err := e.ensureOperation(ctx, opKey, op)
	if err != nil {
		return Outcome{}, err
	}
	// Persist the active operation before any provider mutation.
	resource.ActiveOperation = opID
	resource.UpdatedAt = now
	resourceRaw, err := encode(resource)
	if err != nil {
		return Outcome{}, err
	}
	resourceEntry, resourceETag, err = e.State.Update(ctx, resourceKey, resourceETag, statev1.WriteRequest{Value: resourceRaw, ArtifactReferences: cfg.references()})
	if err != nil {
		return Outcome{}, fmt.Errorf("deploy: reserve resource: %w", err)
	}
	if err := json.Unmarshal(resourceEntry.Value, &resource); err != nil {
		return Outcome{}, err
	}
	op.Status = "applying"
	op.UpdatedAt = e.now()
	opRaw, err := encode(op)
	if err != nil {
		return Outcome{}, err
	}
	opEntry, etag, err = e.State.Update(ctx, opKey, etag, statev1.WriteRequest{Value: opRaw})
	if err != nil {
		return Outcome{}, fmt.Errorf("deploy: record applying operation: %w", err)
	}
	_ = opEntry
	checkpoint := e.checkpointFunc(opKey)
	req := driver.Request{Protocol: "kodama.deploy-driver/v1", RequestID: opID, Method: "apply",
		Deadline: e.deadline(cfg), Identity: identity, OperationID: opID, DesiredRelease: cfg.DesiredRelease,
		Configuration: cfg.Configuration, Artifacts: cfg.Artifacts, Checkpoints: op.Checkpoints,
		Previous: resourceEntry.Value}
	result, err := e.Driver.Apply(ctx, req, checkpoint)
	if err != nil {
		if errors.Is(err, ErrCheckpoint) {
			return e.failOperation(ctx, opKey, resourceKey, resourceETag, op, err)
		}
		effect := result.Effect
		if effect == "" {
			effect = "possible"
		}
		if effect == "none" {
			return e.failOperation(ctx, opKey, resourceKey, resourceETag, op, err)
		}
		return e.markUnknown(ctx, opKey, resourceKey, resourceETag, op, result, err)
	}
	if result.Effect == "" {
		result.Effect = "confirmed"
	}
	obs, obsErr := e.Driver.Observe(ctx, req)
	if obsErr != nil || obs.State != driver.Ready {
		if obsErr == nil {
			err = fmt.Errorf("deploy: provider is %s", obs.State)
		} else {
			err = obsErr
		}
		return e.markUnknown(ctx, opKey, resourceKey, resourceETag, op, result, err)
	}
	return e.settle(ctx, opKey, resourceKey, resourceETag, op, result, &obs, cfg)
}

// Status observes a resource without mutating it.
func (e *Engine) Status(ctx context.Context, cfg Config) (resourceDoc, driver.Observation, error) {
	identity, resource, _, err := e.identifyResource(ctx, cfg)
	if err != nil {
		return resourceDoc{}, driver.Observation{}, err
	}
	req := driver.Request{Protocol: "kodama.deploy-driver/v1", Method: "observe",
		Deadline: e.deadline(cfg), Identity: identity, OperationID: cfg.OperationID,
		Configuration: cfg.Configuration, Artifacts: cfg.Artifacts}
	obs, err := e.Driver.Observe(ctx, req)
	return resource, obs, err
}

// Reconcile observes an operation that is already active. It never invokes
// Apply again.
func (e *Engine) Reconcile(ctx context.Context, cfg Config) (Outcome, error) {
	identity, resource, _, err := e.identifyResource(ctx, cfg)
	if err != nil {
		return Outcome{}, err
	}
	resourceKey, err := keyFor("resources", identity.ResourceKey)
	if err != nil {
		return Outcome{}, err
	}
	entry, etag, err := e.State.Read(ctx, resourceKey)
	if err != nil {
		return Outcome{}, err
	}
	if err := json.Unmarshal(entry.Value, &resource); err != nil {
		return Outcome{}, err
	}
	if resource.ActiveOperation == "" {
		if resource.PendingRelease != nil {
			finished, err := e.finishPendingPublication(ctx, resourceKey)
			return Outcome{Resource: finished}, err
		}
		return Outcome{Resource: resource}, nil
	}
	opKey, err := keyFor("operations", identity.ResourceKey+"/"+resource.ActiveOperation)
	if err != nil {
		return Outcome{}, err
	}
	op, err := e.readOperation(ctx, opKey)
	if err != nil {
		return Outcome{}, err
	}
	switch op.Status {
	case "succeeded":
		if op.Kind == "cleanup" {
			finished, err := e.finishCleanup(ctx, resourceKey, etag, resource, op)
			return Outcome{Resource: finished, Operation: op}, err
		}
		return e.finishExisting(ctx, resourceKey, etag, resource, op)
	case "applying", "unknown":
		if op.Kind == "cleanup" {
			return e.reconcileCleanup(ctx, identity, resourceKey, resource, etag, opKey, op, cfg)
		}
		return e.reconcileOperation(ctx, identity, resourceKey, resource, etag, opKey, op, cfg)
	case "pending":
		return Outcome{Resource: resource, Operation: op}, fmt.Errorf("deploy: operation %s has not started", op.ID)
	default:
		return Outcome{Resource: resource, Operation: op}, fmt.Errorf("deploy: operation %s is %s", op.ID, op.Status)
	}
}

func (e *Engine) reconcileOperation(ctx context.Context, identity driver.Identity, resourceKey string, resource resourceDoc, resourceETag, opKey string, op operationDoc, cfg Config) (Outcome, error) {
	req := driver.Request{Protocol: "kodama.deploy-driver/v1", Method: "observe", RequestID: op.ID,
		Deadline: e.deadline(cfg), Identity: identity, OperationID: op.ID,
		DesiredRelease: resource.DesiredRelease, Configuration: cfg.Configuration,
		Artifacts: cfg.Artifacts, Previous: mustJSON(resource)}
	obs, err := e.Driver.Observe(ctx, req)
	if err != nil || obs.State == driver.Unknown || obs.State == driver.Progressing {
		if err == nil {
			err = fmt.Errorf("deploy: provider is %s", obs.State)
		}
		op.UpdatedAt = e.now()
		op.Observation = &obs
		if saveErr := e.saveOperation(ctx, opKey, op); saveErr != nil {
			err = errors.Join(err, saveErr)
		}
		return Outcome{Resource: resource, Operation: op}, err
	}
	if obs.State == driver.Absent || obs.State == driver.Unhealthy || obs.State == driver.Conflict {
		op.Status = "failed"
		op.Effect = "possible"
		op.Error = "provider observation: " + string(obs.State)
		op.Observation = &obs
		op.UpdatedAt = e.now()
		saveErr := e.saveOperation(ctx, opKey, op)
		resource.ActiveOperation = ""
		resource.LastOperation = op.ID
		resource.PendingRelease = nil
		resource.UpdatedAt = e.now()
		resourceRaw, encErr := encode(resource)
		if encErr != nil {
			return Outcome{Resource: resource, Operation: op}, encErr
		}
		if _, _, err := e.State.Update(ctx, resourceKey, resourceETag, statev1.WriteRequest{Value: resourceRaw}); err != nil {
			saveErr = errors.Join(saveErr, err)
		}
		return Outcome{Resource: resource, Operation: op}, errors.Join(fmt.Errorf("deploy: operation failed by observation"), saveErr)
	}
	return e.settle(ctx, opKey, resourceKey, resourceETag, op, driver.Result{Effect: "confirmed", Resources: obs.Resources}, &obs, cfg)
}

// Rollback asks the driver to activate a recorded provider release or reapply a
// retained artifact. It is a separate operation and never rewrites history.
func (e *Engine) Rollback(ctx context.Context, cfg Config, release string) (Outcome, error) {
	if release == "" {
		return Outcome{}, fmt.Errorf("deploy: rollback release is required")
	}
	identity, resource, resourceETag, err := e.identifyResource(ctx, cfg)
	if err != nil {
		return Outcome{}, err
	}
	if resource.DeletedAt != nil {
		return Outcome{}, fmt.Errorf("deploy: resource is deleted")
	}
	opID := cfg.OperationID
	if opID == "" {
		opID = cfg.ActionKey + ":rollback:" + release
	}
	if resource.ActiveOperation != "" && resource.ActiveOperation != opID {
		return Outcome{}, ErrBusy
	}
	opKey, err := keyFor("operations", identity.ResourceKey+"/"+opID)
	if err != nil {
		return Outcome{}, err
	}
	cfg.DesiredRelease = release
	var providerRevision string
	if stored, err := e.readRelease(ctx, identity, release); err == nil {
		// Rollback reuses the retained descriptors. A caller cannot substitute a
		// different bundle for a recorded release.
		cfg.Artifacts = stored.Artifacts
		providerRevision = stored.ProviderVersion
	} else if !isNotFound(err) {
		return Outcome{}, err
	}
	fingerprint, err := fingerprintFor("rollback", cfg, identity)
	if err != nil {
		return Outcome{}, err
	}
	if existing, err := e.readOperation(ctx, opKey); err == nil {
		if existing.Fingerprint != fingerprint {
			return Outcome{}, ErrConflict
		}
		switch existing.Status {
		case "succeeded":
			return e.finishExisting(ctx, resourceKeyFor(identity), resourceETag, resource, existing)
		case "unknown":
			return e.reconcileOperation(ctx, identity, resourceKeyFor(identity), resource, resourceETag, opKey, existing, cfg)
		default:
			return Outcome{}, fmt.Errorf("deploy: operation %s is %s", opID, existing.Status)
		}
	} else if !isNotFound(err) {
		return Outcome{}, err
	}
	if resource.ActiveOperation != "" && resource.ActiveOperation != opID {
		return Outcome{}, ErrBusy
	}
	now := e.now()
	op := operationDoc{SchemaVersion: schemaVersion, ID: opID, Kind: "rollback", Fingerprint: fingerprint,
		Status: "pending", CreatedAt: now, UpdatedAt: now, DesiredRelease: release,
		Artifacts: append([]driver.Artifact(nil), cfg.Artifacts...), Configuration: cfg.Configuration,
		Checkpoints: map[string]json.RawMessage{}}
	if _, _, err := e.ensureOperation(ctx, opKey, op); err != nil {
		return Outcome{}, err
	}
	resource.ActiveOperation = opID
	resource.UpdatedAt = now
	resourceRaw, err := encode(resource)
	if err != nil {
		return Outcome{}, err
	}
	if _, resourceETag, err = e.State.Update(ctx, resourceKeyFor(identity), resourceETag, statev1.WriteRequest{Value: resourceRaw, ArtifactReferences: cfg.references()}); err != nil {
		return Outcome{}, fmt.Errorf("deploy: reserve resource: %w", err)
	}
	if err := e.setOperationApplying(ctx, opKey, &op); err != nil {
		return Outcome{}, err
	}
	req := driver.Request{Protocol: "kodama.deploy-driver/v1", Method: "rollback", RequestID: opID,
		Deadline: e.deadline(cfg), Identity: identity, OperationID: opID, DesiredRelease: release,
		ProviderRevision: providerRevision,
		Configuration:    cfg.Configuration, Artifacts: cfg.Artifacts, Previous: mustJSON(resource)}
	result, err := e.Driver.Rollback(ctx, req, e.checkpointFunc(opKey))
	if err != nil {
		if errors.Is(err, ErrCheckpoint) || result.Effect == "none" {
			return e.failOperation(ctx, opKey, resourceKeyFor(identity), resourceETag, op, err)
		}
		return e.markUnknown(ctx, opKey, resourceKeyFor(identity), resourceETag, op, result, err)
	}
	obs, err := e.Driver.Observe(ctx, req)
	if err != nil || obs.State != driver.Ready {
		if err == nil {
			err = fmt.Errorf("deploy: provider is %s", obs.State)
		}
		return e.markUnknown(ctx, opKey, resourceKeyFor(identity), resourceETag, op, result, err)
	}
	return e.settle(ctx, opKey, resourceKeyFor(identity), resourceETag, op, result, &obs, cfg)
}

// Cleanup marks a resource deleted after the driver confirms deletion.
func (e *Engine) Cleanup(ctx context.Context, cfg Config) (Outcome, error) {
	identity, resource, resourceETag, err := e.identifyResource(ctx, cfg)
	if err != nil {
		return Outcome{}, err
	}
	resourceKey, err := keyFor("resources", identity.ResourceKey)
	if err != nil {
		return Outcome{}, err
	}
	if resource.DeletedAt != nil {
		return Outcome{Resource: resource}, nil
	}
	opID := cfg.OperationID
	if opID == "" {
		opID = cfg.ActionKey + ":cleanup"
	}
	if resource.ActiveOperation != "" && resource.ActiveOperation != opID {
		return Outcome{}, ErrBusy
	}
	opKey, err := keyFor("operations", identity.ResourceKey+"/"+opID)
	if err != nil {
		return Outcome{}, err
	}
	fingerprint, err := fingerprintFor("cleanup", cfg, identity)
	if err != nil {
		return Outcome{}, err
	}
	if existing, err := e.readOperation(ctx, opKey); err == nil {
		if existing.Fingerprint != fingerprint {
			return Outcome{}, ErrConflict
		}
		switch existing.Status {
		case "succeeded":
			resource, err = e.finishCleanup(ctx, resourceKey, resourceETag, resource, existing)
			return Outcome{Resource: resource, Operation: existing}, err
		case "unknown":
			return e.reconcileCleanup(ctx, identity, resourceKey, resource, resourceETag, opKey, existing, cfg)
		default:
			return Outcome{}, fmt.Errorf("deploy: operation %s is %s", opID, existing.Status)
		}
	} else if !isNotFound(err) {
		return Outcome{}, err
	}
	now := e.now()
	op := operationDoc{SchemaVersion: schemaVersion, ID: opID, Kind: "cleanup", Fingerprint: fingerprint,
		Status: "pending", CreatedAt: now, UpdatedAt: now, DesiredRelease: cfg.DesiredRelease,
		Artifacts: append([]driver.Artifact(nil), cfg.Artifacts...), Configuration: cfg.Configuration,
		Checkpoints: map[string]json.RawMessage{}}
	if _, _, err := e.ensureOperation(ctx, opKey, op); err != nil {
		return Outcome{}, err
	}
	resource.ActiveOperation = opID
	resource.UpdatedAt = now
	resourceRaw, err := encode(resource)
	if err != nil {
		return Outcome{}, err
	}
	if _, resourceETag, err = e.State.Update(ctx, resourceKey, resourceETag, statev1.WriteRequest{Value: resourceRaw, ArtifactReferences: cfg.references()}); err != nil {
		return Outcome{}, fmt.Errorf("deploy: reserve cleanup: %w", err)
	}
	if err := e.setOperationApplying(ctx, opKey, &op); err != nil {
		return Outcome{}, err
	}
	req := driver.Request{Protocol: "kodama.deploy-driver/v1", Method: "destroy", RequestID: opID,
		Deadline: e.deadline(cfg), Identity: identity, OperationID: opID, Configuration: cfg.Configuration,
		Artifacts: cfg.Artifacts, Previous: mustJSON(resource), Checkpoints: op.Checkpoints}
	checkpoint := e.checkpointFunc(opKey)
	result, err := e.Driver.Destroy(ctx, req, checkpoint)
	if err != nil {
		if errors.Is(err, ErrCheckpoint) || result.Effect == "none" {
			return e.failOperation(ctx, opKey, resourceKey, resourceETag, op, err)
		}
		return e.markUnknown(ctx, opKey, resourceKey, resourceETag, op, result, err)
	}
	obs, err := e.Driver.Observe(ctx, withMethod(req, "observe"))
	if err != nil {
		return e.markUnknown(ctx, opKey, resourceKey, resourceETag, op, result, err)
	}
	if obs.State != driver.Absent {
		if obs.State == driver.Unknown || obs.State == driver.Progressing {
			return e.markUnknown(ctx, opKey, resourceKey, resourceETag, op, result, fmt.Errorf("deploy: deletion is %s", obs.State))
		}
		return e.markUnknown(ctx, opKey, resourceKey, resourceETag, op, result, fmt.Errorf("deploy: resource still observed as %s", obs.State))
	}
	op.Status = "succeeded"
	op.Effect = result.Effect
	op.Result = &result
	op.Observation = &obs
	op.UpdatedAt = e.now()
	if err := e.saveOperation(ctx, opKey, op); err != nil {
		return Outcome{Resource: resource, Operation: op}, err
	}
	resource, err = e.finishCleanup(ctx, resourceKey, resourceETag, resource, op)
	return Outcome{Resource: resource, Operation: op}, err
}

func (e *Engine) settle(ctx context.Context, opKey, resourceKey, _ string, op operationDoc, result driver.Result, obs *driver.Observation, cfg Config) (Outcome, error) {
	now := e.now()
	resourceEntry, currentETag, err := e.State.Read(ctx, resourceKey)
	if err != nil {
		return Outcome{}, err
	}
	var resource resourceDoc
	if err := json.Unmarshal(resourceEntry.Value, &resource); err != nil {
		return Outcome{}, err
	}
	if resource.ActiveOperation != "" && resource.ActiveOperation != op.ID {
		return Outcome{}, ErrBusy
	}
	// Reconciliation uses the immutable intent retained with the operation;
	// only an operation written before these fields existed falls back to the
	// current invocation.
	desired := op.DesiredRelease
	if desired == "" {
		desired = resource.DesiredRelease
	}
	if desired == "" {
		desired = cfg.DesiredRelease
	}
	artifacts := op.Artifacts
	if len(artifacts) == 0 {
		artifacts = cfg.Artifacts
	}
	refs := referencesFor(artifacts)
	release := releaseDoc{SchemaVersion: schemaVersion, ResourceKey: resource.Identity.ResourceKey, Release: desired, Artifacts: artifacts,
		ProviderRelease: result.ProviderRelease, ProviderVersion: result.ProviderVersion, CreatedAt: now,
		Resources: result.Resources, ArtifactReferences: refs}
	// The authoritative resource keeps the reservation and the pending history
	// while secondary history is published. It also carries the exact retained
	// descriptors, so reconciliation never substitutes a new invocation.
	resource.LastOperation = op.ID
	resource.DesiredRelease = desired
	if op.Configuration != nil {
		resource.Configuration = op.Configuration
	}
	resource.ObservedRelease = obs.ObservedRelease
	resource.ProviderRelease = result.ProviderRelease
	resource.ProviderVersion = result.ProviderVersion
	resource.Resources = result.Resources
	resource.UpdatedAt = now
	resource.LastHealthyRelease = desired
	resource.ArtifactReferences = refs
	if desired != "" {
		resource.PendingRelease = &release
	} else {
		resource.PendingRelease = nil
	}
	raw, err := encode(resource)
	if err != nil {
		return Outcome{}, err
	}
	if _, currentETag, err = e.State.Update(ctx, resourceKey, currentETag, statev1.WriteRequest{Value: raw, ArtifactReferences: refs}); err != nil {
		return Outcome{}, fmt.Errorf("deploy: record observed resource: %w", err)
	}
	if resource.PendingRelease != nil {
		if err := e.publishRelease(ctx, resource.Identity.ResourceKey, release, refs); err != nil {
			return Outcome{Resource: resource, Operation: op}, fmt.Errorf("deploy: publish release history: %w", err)
		}
	}
	op.Status = "succeeded"
	op.Effect = result.Effect
	op.Result = &result
	op.Observation = obs
	op.UpdatedAt = e.now()
	if err := e.saveOperation(ctx, opKey, op); err != nil {
		return Outcome{Resource: resource, Operation: op}, fmt.Errorf("deploy: publish operation history: %w", err)
	}
	resource.PendingRelease = nil
	resource.ActiveOperation = ""
	resource.UpdatedAt = e.now()
	raw, err = encode(resource)
	if err != nil {
		return Outcome{}, err
	}
	updated, _, err := e.State.Update(ctx, resourceKey, currentETag, statev1.WriteRequest{Value: raw, ArtifactReferences: refs})
	if err != nil {
		return Outcome{Resource: resource, Operation: op}, fmt.Errorf("deploy: clear operation reservation: %w", err)
	}
	resource = resourceDoc{}
	if err := json.Unmarshal(updated.Value, &resource); err != nil {
		return Outcome{}, err
	}
	return Outcome{Resource: resource, Operation: op}, nil
}

// publishRelease writes one immutable release record idempotently. A retry
// after a lost response compares the existing record instead of overwriting it.
func (e *Engine) publishRelease(ctx context.Context, resourceKey string, release releaseDoc, refs []statev1.ArtifactReference) error {
	if release.ResourceKey == "" {
		release.ResourceKey = resourceKey
	}
	releaseKey, err := keyFor("releases", resourceKey+"/"+release.Release)
	if err != nil {
		return err
	}
	releaseRaw, err := encode(release)
	if err != nil {
		return err
	}
	if _, _, err := e.State.Create(ctx, releaseKey, statev1.WriteRequest{Value: releaseRaw, ArtifactReferences: refs}); err != nil {
		if !isExists(err) {
			return err
		}
		existing, _, err := e.State.Read(ctx, releaseKey)
		if err != nil {
			return err
		}
		var prior releaseDoc
		if err := json.Unmarshal(existing.Value, &prior); err != nil {
			return err
		}
		if !sameRelease(prior, release) {
			return ErrConflict
		}
	}
	return nil
}

// finishPendingPublication completes a resource whose provider outcome was
// observed but whose secondary history was not confirmed before the failure.
func (e *Engine) finishPendingPublication(ctx context.Context, resourceKey string) (resourceDoc, error) {
	entry, etag, err := e.State.Read(ctx, resourceKey)
	if err != nil {
		return resourceDoc{}, err
	}
	var resource resourceDoc
	if err := json.Unmarshal(entry.Value, &resource); err != nil {
		return resourceDoc{}, err
	}
	refs := resource.ArtifactReferences
	if len(refs) == 0 && resource.PendingRelease != nil {
		refs = resource.PendingRelease.ArtifactReferences
	}
	if resource.PendingRelease != nil {
		if err := e.publishRelease(ctx, resource.Identity.ResourceKey, *resource.PendingRelease, refs); err != nil {
			return resource, err
		}
	}
	resource.PendingRelease = nil
	resource.ActiveOperation = ""
	resource.UpdatedAt = e.now()
	raw, err := encode(resource)
	if err != nil {
		return resourceDoc{}, err
	}
	updated, _, err := e.State.Update(ctx, resourceKey, etag, statev1.WriteRequest{Value: raw, ArtifactReferences: refs})
	if err != nil {
		return resourceDoc{}, err
	}
	resource = resourceDoc{}
	if err := json.Unmarshal(updated.Value, &resource); err != nil {
		return resourceDoc{}, err
	}
	return resource, nil
}

func (e *Engine) markUnknown(ctx context.Context, opKey, resourceKey, resourceETag string, op operationDoc, result driver.Result, cause error) (Outcome, error) {
	op.Status = "unknown"
	op.Effect = result.Effect
	if op.Effect == "" {
		op.Effect = "possible"
	}
	op.Error = cause.Error()
	op.UpdatedAt = e.now()
	op.Result = &result
	saveErr := e.saveOperation(ctx, opKey, op)
	resource := resourceDoc{}
	resourceEntry, _, readErr := e.State.Read(ctx, resourceKey)
	if readErr == nil {
		if err := json.Unmarshal(resourceEntry.Value, &resource); err == nil {
			resource.ActiveOperation = op.ID
			resource.UpdatedAt = e.now()
			raw, err := encode(resource)
			if err == nil {
				_, _, err = e.State.Update(ctx, resourceKey, resourceEntry.ETag(), statev1.WriteRequest{Value: raw})
			}
			readErr = err
		}
	}
	return Outcome{Resource: resource, Operation: op}, errors.Join(cause, saveErr, readErr)
}

func (e *Engine) failOperation(ctx context.Context, opKey, resourceKey, resourceETag string, op operationDoc, cause error) (Outcome, error) {
	op.Status = "failed"
	op.Effect = "none"
	op.Error = cause.Error()
	op.UpdatedAt = e.now()
	saveErr := e.saveOperation(ctx, opKey, op)
	resource := resourceDoc{}
	resourceEntry, _, readErr := e.State.Read(ctx, resourceKey)
	if readErr == nil {
		if err := json.Unmarshal(resourceEntry.Value, &resource); err == nil {
			resource.ActiveOperation = ""
			resource.LastOperation = op.ID
			resource.PendingRelease = nil
			resource.UpdatedAt = e.now()
			raw, err := encode(resource)
			if err == nil {
				_, _, err = e.State.Update(ctx, resourceKey, resourceEntry.ETag(), statev1.WriteRequest{Value: raw})
			}
			readErr = err
		}
	}
	return Outcome{Resource: resource, Operation: op}, errors.Join(cause, saveErr, readErr)
}

func (e *Engine) identifyResource(ctx context.Context, cfg Config) (driver.Identity, resourceDoc, string, error) {
	if err := validateConfig(cfg); err != nil {
		return driver.Identity{}, resourceDoc{}, "", err
	}
	identity, err := e.Driver.Identify(ctx, driver.IdentityRequest{Recipe: cfg.Recipe, RecipeVersion: cfg.RecipeVersion, Configuration: cfg.Configuration})
	if err != nil {
		return driver.Identity{}, resourceDoc{}, "", err
	}
	key, _ := keyFor("resources", identity.ResourceKey)
	entry, etag, err := e.State.Read(ctx, key)
	if err != nil {
		return driver.Identity{}, resourceDoc{}, "", err
	}
	var resource resourceDoc
	if err := json.Unmarshal(entry.Value, &resource); err != nil {
		return driver.Identity{}, resourceDoc{}, "", err
	}
	if err := checkSchema("resource", resource.SchemaVersion); err != nil {
		return driver.Identity{}, resourceDoc{}, "", err
	}
	return identity, resource, etag, nil
}

func (e *Engine) loadOrCreateResource(ctx context.Context, key string, identity driver.Identity, cfg Config) (statev1.Entry, string, error) {
	entry, etag, err := e.State.Read(ctx, key)
	if err == nil {
		return entry, etag, nil
	}
	if !isNotFound(err) {
		return statev1.Entry{}, "", err
	}
	resource := resourceDoc{SchemaVersion: schemaVersion, Identity: identity, Recipe: cfg.Recipe,
		RecipeVersion: cfg.RecipeVersion, Configuration: cfg.Configuration, UpdatedAt: e.now()}
	raw, err := encode(resource)
	if err != nil {
		return statev1.Entry{}, "", err
	}
	entry, etag, err = e.State.Create(ctx, key, statev1.WriteRequest{Value: raw, ArtifactReferences: cfg.references()})
	if err != nil {
		if isExists(err) {
			return e.State.Read(ctx, key)
		}
		return statev1.Entry{}, "", err
	}
	return entry, etag, nil
}

func (e *Engine) readOperation(ctx context.Context, key string) (operationDoc, error) {
	entry, _, err := e.State.Read(ctx, key)
	if err != nil {
		return operationDoc{}, err
	}
	var op operationDoc
	if err := json.Unmarshal(entry.Value, &op); err != nil {
		return operationDoc{}, err
	}
	if err := checkSchema("operation", op.SchemaVersion); err != nil {
		return operationDoc{}, err
	}
	return op, nil
}

func checkSchema(kind string, got int) error {
	if got > schemaVersion {
		return fmt.Errorf("deploy: %s schema %d is newer than this plugin supports (%d)", kind, got, schemaVersion)
	}
	return nil
}

func (e *Engine) deadline(cfg Config) time.Time {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	return e.now().Add(timeout)
}

func validateConfig(cfg Config) error {
	if cfg.Recipe == "" {
		return fmt.Errorf("deploy: recipe is required")
	}
	if len(cfg.Configuration) == 0 {
		return fmt.Errorf("deploy: configuration is required")
	}
	return nil
}

func (cfg Config) references() []statev1.ArtifactReference {
	return referencesFor(cfg.Artifacts)
}

func referencesFor(artifacts []driver.Artifact) []statev1.ArtifactReference {
	refs := make([]statev1.ArtifactReference, 0, len(artifacts))
	for _, a := range artifacts {
		if a.ID == "" || a.Digest == "" {
			continue
		}
		refs = append(refs, statev1.ArtifactReference{ArtifactID: a.ID, Digest: a.Digest})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].ArtifactID < refs[j].ArtifactID })
	return refs
}

// Prune removes release records outside the retained rollback window. It never
// removes the desired or last healthy release, and refuses while an operation is
// active.
func (e *Engine) Prune(ctx context.Context, cfg Config, keep int) ([]string, error) {
	if keep < 1 {
		keep = 1
	}
	identity, resource, _, err := e.identifyResource(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if resource.ActiveOperation != "" {
		return nil, ErrBusy
	}
	prefix := "releases/"
	var entries []statev1.Entry
	cursor := ""
	for {
		page, next, err := e.State.List(ctx, prefix, cursor, statev1.MaxListLimit)
		if err != nil {
			return nil, err
		}
		entries = append(entries, page...)
		if next == "" {
			break
		}
		cursor = next
	}
	type releaseEntry struct {
		entry   statev1.Entry
		release releaseDoc
	}
	docs := make([]releaseEntry, 0, len(entries))
	for _, entry := range entries {
		var doc releaseDoc
		if err := json.Unmarshal(entry.Value, &doc); err != nil {
			return nil, err
		}
		if err := checkSchema("release", doc.SchemaVersion); err != nil {
			return nil, err
		}
		if doc.ResourceKey != "" && doc.ResourceKey != identity.ResourceKey {
			continue
		}
		if doc.ResourceKey == "" {
			// Older records have no resource key; their release key is not
			// enough to identify the resource, so they are never pruned here.
			continue
		}
		docs = append(docs, releaseEntry{entry: entry, release: doc})
	}
	sort.Slice(docs, func(i, j int) bool {
		if !docs[i].release.CreatedAt.Equal(docs[j].release.CreatedAt) {
			return docs[i].release.CreatedAt.After(docs[j].release.CreatedAt)
		}
		return docs[i].entry.Key > docs[j].entry.Key
	})
	protected := map[string]bool{resource.DesiredRelease: true, resource.LastHealthyRelease: true}
	kept := 0
	var removed []string
	for _, doc := range docs {
		if protected[doc.release.Release] || kept < keep {
			kept++
			continue
		}
		if err := e.State.Delete(ctx, doc.entry.Key, doc.entry.ETag()); err != nil {
			return removed, err
		}
		removed = append(removed, doc.release.Release)
	}
	return removed, nil
}

// CleanupExpired runs an authorized cleanup for every resource whose recorded
// expiry has passed. It lists only the binding and revalidates each record.
func (e *Engine) CleanupExpired(ctx context.Context, cfg Config) ([]Outcome, error) {
	var out []Outcome
	var errs []error
	cursor := ""
	for {
		page, next, err := e.State.List(ctx, "resources/", cursor, statev1.MaxListLimit)
		if err != nil {
			return out, err
		}
		for _, entry := range page {
			var resource resourceDoc
			if err := json.Unmarshal(entry.Value, &resource); err != nil {
				errs = append(errs, err)
				continue
			}
			if err := checkSchema("resource", resource.SchemaVersion); err != nil {
				errs = append(errs, err)
				continue
			}
			if resource.DeletedAt != nil || resource.ExpiresAt == nil || resource.ExpiresAt.After(e.now()) {
				continue
			}
			cleanup := cfg
			cleanup.Recipe = resource.Recipe
			cleanup.RecipeVersion = resource.RecipeVersion
			cleanup.Configuration = resource.Configuration
			cleanup.DesiredRelease = resource.DesiredRelease
			cleanup.ActionKey = cfg.ActionKey + ":expired"
			cleanup.OperationID = "expired:" + resource.Identity.ResourceKey
			outcome, err := e.Cleanup(ctx, cleanup)
			out = append(out, outcome)
			if err != nil {
				errs = append(errs, err)
			}
		}
		if next == "" {
			break
		}
		cursor = next
	}
	return out, errors.Join(errs...)
}

// LegacyImport is the bounded, provider-free import of one previously owned
// resource inventory. It is the transition seam for P8; it never redeploys or
// claims an uncertain legacy effect.
type LegacyImport struct {
	ResourceKey     string            `json:"resourceKey"`
	Release         string            `json:"release"`
	ObservedRelease string            `json:"observedRelease,omitempty"`
	ProviderRelease string            `json:"providerRelease,omitempty"`
	ProviderVersion string            `json:"providerVersion,omitempty"`
	Resources       []driver.Resource `json:"resources"`
	Artifacts       []driver.Artifact `json:"artifacts,omitempty"`
	Configuration   json.RawMessage   `json:"configuration,omitempty"`
	OperationID     string            `json:"operationId,omitempty"`
}

// ImportLegacy creates the shared-engine resource and release records for an
// already verified legacy inventory. The caller must have verified the live
// provider identity and ownership before invoking it.
func (e *Engine) ImportLegacy(ctx context.Context, cfg Config, doc LegacyImport) (Outcome, error) {
	if err := validateConfig(cfg); err != nil {
		return Outcome{}, err
	}
	identity, err := e.Driver.Identify(ctx, driver.IdentityRequest{Recipe: cfg.Recipe, RecipeVersion: cfg.RecipeVersion, Configuration: cfg.Configuration})
	if err != nil {
		return Outcome{}, err
	}
	if doc.ResourceKey != "" && doc.ResourceKey != identity.ResourceKey {
		return Outcome{}, fmt.Errorf("deploy: legacy resource key %q does not match %q", doc.ResourceKey, identity.ResourceKey)
	}
	if doc.Release == "" {
		return Outcome{}, fmt.Errorf("deploy: legacy release is required")
	}
	config := doc.Configuration
	if len(config) == 0 {
		config = cfg.Configuration
	}
	now := e.now()
	refs := referencesFor(doc.Artifacts)
	resource := resourceDoc{
		SchemaVersion: schemaVersion, Identity: identity, Recipe: cfg.Recipe, RecipeVersion: cfg.RecipeVersion,
		Configuration: config, DesiredRelease: doc.Release, ObservedRelease: doc.ObservedRelease,
		ProviderRelease: doc.ProviderRelease, ProviderVersion: doc.ProviderVersion, Resources: doc.Resources,
		LastHealthyRelease: doc.Release, ArtifactReferences: refs, UpdatedAt: now,
	}
	if doc.OperationID != "" {
		resource.LastOperation = doc.OperationID
	}
	raw, err := encode(resource)
	if err != nil {
		return Outcome{}, err
	}
	key, err := keyFor("resources", identity.ResourceKey)
	if err != nil {
		return Outcome{}, err
	}
	if _, _, err := e.State.Create(ctx, key, statev1.WriteRequest{Value: raw, ArtifactReferences: refs}); err != nil {
		if isExists(err) {
			return Outcome{}, fmt.Errorf("deploy: legacy resource already imported: %w", ErrConflict)
		}
		return Outcome{}, err
	}
	release := releaseDoc{SchemaVersion: schemaVersion, ResourceKey: identity.ResourceKey, Release: doc.Release, Artifacts: doc.Artifacts,
		ProviderRelease: doc.ProviderRelease, ProviderVersion: doc.ProviderVersion, CreatedAt: now,
		Resources: doc.Resources, ArtifactReferences: refs}
	if err := e.publishRelease(ctx, identity.ResourceKey, release, refs); err != nil {
		return Outcome{Resource: resource}, err
	}
	return Outcome{Resource: resource}, nil
}

// ReleaseID is the immutable identity of one deployable input: the normalized
// resource identity, the verified artifacts and the canonical effective
// configuration. Local paths and provider credentials are deliberately absent.
func ReleaseID(identity driver.Identity, artifacts []driver.Artifact, configuration json.RawMessage) (string, error) {
	type artifactID struct {
		ID       string `json:"id"`
		Digest   string `json:"digest"`
		Kind     string `json:"kind"`
		Registry string `json:"registry,omitempty"`
	}
	ids := make([]artifactID, 0, len(artifacts))
	for _, a := range artifacts {
		ids = append(ids, artifactID{ID: a.ID, Digest: a.Digest, Kind: string(a.Kind), Registry: a.Registry})
	}
	sort.Slice(ids, func(i, j int) bool {
		if ids[i].ID != ids[j].ID {
			return ids[i].ID < ids[j].ID
		}
		return ids[i].Digest < ids[j].Digest
	})
	payload := struct {
		Resource      string          `json:"resource"`
		Artifacts     []artifactID    `json:"artifacts"`
		Configuration json.RawMessage `json:"configuration"`
	}{Resource: identity.ResourceKey, Artifacts: ids, Configuration: configuration}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func fingerprint(cfg Config, identity driver.Identity) (string, error) {
	return fingerprintFor("apply", cfg, identity)
}

func fingerprintFor(kind string, cfg Config, identity driver.Identity) (string, error) {
	payload := struct {
		Kind          string            `json:"kind"`
		Resource      string            `json:"resource"`
		Desired       string            `json:"desired"`
		Artifacts     []driver.Artifact `json:"artifacts"`
		Configuration json.RawMessage   `json:"configuration"`
	}{Kind: kind, Resource: identity.ResourceKey, Desired: cfg.DesiredRelease, Artifacts: cfg.Artifacts, Configuration: cfg.Configuration}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func keyFor(prefix, value string) (string, error) {
	sum := sha256.Sum256([]byte(value))
	return prefix + "/" + hex.EncodeToString(sum[:]), nil
}

func resourceKeyFor(identity driver.Identity) string {
	key, _ := keyFor("resources", identity.ResourceKey)
	return key
}

func encode(v any) (json.RawMessage, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if len(raw) > statev1.MaxValueBytes {
		return nil, fmt.Errorf("deploy: state value exceeds %d bytes", statev1.MaxValueBytes)
	}
	return raw, nil
}

func isNotFound(err error) bool {
	return errors.Is(err, pluginstate.ErrNotFound)
}

func isExists(err error) bool {
	var httpErr *pluginstate.HTTPError
	return errors.As(err, &httpErr) && httpErr.Status == 412
}

func (e *Engine) saveOperation(ctx context.Context, key string, op operationDoc) error {
	raw, err := encode(op)
	if err != nil {
		return err
	}
	_, etag, err := e.State.Read(ctx, key)
	if err != nil {
		return err
	}
	_, _, err = e.State.Update(ctx, key, etag, statev1.WriteRequest{Value: raw})
	return err
}

func (e *Engine) checkpointFunc(opKey string) driver.Checkpoint {
	return func(ctx context.Context, name string, data json.RawMessage) error {
		if len(data) > 4096 {
			return checkpointFailure(fmt.Errorf("checkpoint %s is too large", name))
		}
		entry, etag, err := e.State.Read(ctx, opKey)
		if err != nil {
			return checkpointFailure(err)
		}
		var current operationDoc
		if err := json.Unmarshal(entry.Value, &current); err != nil {
			return checkpointFailure(err)
		}
		if current.Checkpoints == nil {
			current.Checkpoints = map[string]json.RawMessage{}
		}
		if prior, ok := current.Checkpoints[name]; ok {
			if !bytes.Equal(prior, data) {
				return checkpointFailure(ErrConflict)
			}
			return nil
		}
		current.Checkpoints[name] = data
		current.UpdatedAt = e.now()
		raw, err := encode(current)
		if err != nil {
			return checkpointFailure(err)
		}
		if _, _, err = e.State.Update(ctx, opKey, etag, statev1.WriteRequest{Value: raw}); err != nil {
			return checkpointFailure(err)
		}
		return nil
	}
}

func checkpointFailure(err error) error { return errors.Join(ErrCheckpoint, err) }

func (e *Engine) ensureOperation(ctx context.Context, key string, op operationDoc) (statev1.Entry, string, error) {
	raw, err := encode(op)
	if err != nil {
		return statev1.Entry{}, "", err
	}
	entry, etag, err := e.State.Create(ctx, key, statev1.WriteRequest{Value: raw})
	if err == nil {
		return entry, etag, nil
	}
	if !isExists(err) {
		return statev1.Entry{}, "", err
	}
	entry, etag, err = e.State.Read(ctx, key)
	if err != nil {
		return statev1.Entry{}, "", err
	}
	var existing operationDoc
	if err := json.Unmarshal(entry.Value, &existing); err != nil {
		return statev1.Entry{}, "", err
	}
	if existing.Fingerprint != op.Fingerprint {
		return statev1.Entry{}, "", ErrConflict
	}
	return entry, etag, nil
}

func (e *Engine) setOperationApplying(ctx context.Context, key string, op *operationDoc) error {
	entry, etag, err := e.State.Read(ctx, key)
	if err != nil {
		return err
	}
	var current operationDoc
	if err := json.Unmarshal(entry.Value, &current); err != nil {
		return err
	}
	if current.Fingerprint != op.Fingerprint {
		return ErrConflict
	}
	if current.Status != "pending" {
		// Only the attempt that CASes pending -> applying may issue the
		// mutation. A concurrent replay observes the operation rather than
		// starting a second one.
		return fmt.Errorf("deploy: operation %s is %s", op.ID, current.Status)
	}
	current.Status = "applying"
	current.UpdatedAt = e.now()
	raw, err := encode(current)
	if err != nil {
		return err
	}
	if _, _, err := e.State.Update(ctx, key, etag, statev1.WriteRequest{Value: raw}); err != nil {
		return err
	}
	*op = current
	return nil
}

func (e *Engine) finishExisting(ctx context.Context, resourceKey, _ string, resource resourceDoc, op operationDoc) (Outcome, error) {
	if resource.ActiveOperation != "" && resource.ActiveOperation != op.ID {
		return Outcome{}, ErrBusy
	}
	if resource.ActiveOperation == op.ID || resource.PendingRelease != nil {
		finished, err := e.finishPendingPublication(ctx, resourceKey)
		if err != nil {
			return Outcome{Resource: resource, Operation: op}, err
		}
		resource = finished
	}
	return Outcome{Resource: resource, Operation: op}, nil
}

func (e *Engine) finishCleanup(ctx context.Context, resourceKey, _ string, resource resourceDoc, op operationDoc) (resourceDoc, error) {
	entry, etag, err := e.State.Read(ctx, resourceKey)
	if err != nil {
		return resourceDoc{}, err
	}
	if err := json.Unmarshal(entry.Value, &resource); err != nil {
		return resourceDoc{}, err
	}
	if resource.ActiveOperation != "" && resource.ActiveOperation != op.ID {
		return resourceDoc{}, ErrBusy
	}
	if resource.DeletedAt == nil {
		now := e.now()
		resource.DeletedAt = &now
	}
	resource.ActiveOperation = ""
	resource.LastOperation = op.ID
	resource.UpdatedAt = e.now()
	resource.ArtifactReferences = nil
	resource.PendingRelease = nil
	raw, err := encode(resource)
	if err != nil {
		return resourceDoc{}, err
	}
	updated, _, err := e.State.Update(ctx, resourceKey, etag, statev1.WriteRequest{Value: raw})
	if err != nil {
		return resourceDoc{}, err
	}
	resource = resourceDoc{}
	if err := json.Unmarshal(updated.Value, &resource); err != nil {
		return resourceDoc{}, err
	}
	return resource, nil
}

func (e *Engine) reconcileCleanup(ctx context.Context, identity driver.Identity, resourceKey string, resource resourceDoc, resourceETag, opKey string, op operationDoc, cfg Config) (Outcome, error) {
	req := driver.Request{Protocol: "kodama.deploy-driver/v1", Method: "observe", RequestID: op.ID,
		Deadline: e.deadline(cfg), Identity: identity, OperationID: op.ID,
		Configuration: cfg.Configuration, Artifacts: cfg.Artifacts, Previous: mustJSON(resource)}
	obs, err := e.Driver.Observe(ctx, req)
	if err != nil {
		op.UpdatedAt = e.now()
		op.Observation = &obs
		if saveErr := e.saveOperation(ctx, opKey, op); saveErr != nil {
			err = errors.Join(err, saveErr)
		}
		return Outcome{Resource: resource, Operation: op}, err
	}
	if obs.State != driver.Absent {
		op.UpdatedAt = e.now()
		op.Observation = &obs
		if saveErr := e.saveOperation(ctx, opKey, op); saveErr != nil {
			err = errors.Join(err, saveErr)
		}
		return Outcome{Resource: resource, Operation: op}, fmt.Errorf("deploy: deletion is %s", obs.State)
	}
	op.Status = "succeeded"
	op.Result = &driver.Result{Effect: "confirmed", Resources: obs.Resources}
	op.Observation = &obs
	op.UpdatedAt = e.now()
	if err := e.saveOperation(ctx, opKey, op); err != nil {
		return Outcome{Resource: resource, Operation: op}, err
	}
	resource, err = e.finishCleanup(ctx, resourceKey, resourceETag, resource, op)
	return Outcome{Resource: resource, Operation: op}, err
}

func (e *Engine) readRelease(ctx context.Context, identity driver.Identity, release string) (releaseDoc, error) {
	key, err := keyFor("releases", identity.ResourceKey+"/"+release)
	if err != nil {
		return releaseDoc{}, err
	}
	entry, _, err := e.State.Read(ctx, key)
	if err != nil {
		return releaseDoc{}, err
	}
	var doc releaseDoc
	if err := json.Unmarshal(entry.Value, &doc); err != nil {
		return releaseDoc{}, err
	}
	if err := checkSchema("release", doc.SchemaVersion); err != nil {
		return releaseDoc{}, err
	}
	return doc, nil
}

func sameRelease(a, b releaseDoc) bool {
	a.CreatedAt = time.Time{}
	b.CreatedAt = time.Time{}
	left, err := json.Marshal(a)
	if err != nil {
		return false
	}
	right, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return bytes.Equal(left, right)
}

func withMethod(req driver.Request, method string) driver.Request {
	req.Method = method
	return req
}

func mustJSON(v any) json.RawMessage {
	raw, _ := json.Marshal(v)
	return raw
}
