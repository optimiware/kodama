// Package engine implements the provider-neutral deployment lifecycle used by
// the deploy plugin images. Provider work is delegated to a driver; the engine
// owns reservations, durable intent, history and recovery.
package engine

import (
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
	ErrUnavailable = errors.New("deploy: state service unavailable")
)

// State is the narrow state client used by the engine.
type State interface {
	Read(ctx context.Context, key string) (statev1.Entry, string, error)
	Create(ctx context.Context, key string, req statev1.WriteRequest) (statev1.Entry, string, error)
	Update(ctx context.Context, key, etag string, req statev1.WriteRequest) (statev1.Entry, string, error)
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
	Now            func() time.Time
}

// Engine executes deployment operations.
type Engine struct {
	State  State
	Driver driver.Driver
	Clock  func() time.Time
}

type resourceDoc struct {
	SchemaVersion      int                         `json:"schemaVersion"`
	Identity           driver.Identity             `json:"identity"`
	DesiredRelease     string                      `json:"desiredRelease,omitempty"`
	ObservedRelease    string                      `json:"observedRelease,omitempty"`
	ProviderRelease    string                      `json:"providerRelease,omitempty"`
	ProviderVersion    string                      `json:"providerVersion,omitempty"`
	ActiveOperation    string                      `json:"activeOperation,omitempty"`
	LastOperation      string                      `json:"lastOperation,omitempty"`
	LastHealthyRelease string                      `json:"lastHealthyRelease,omitempty"`
	Resources          []driver.Resource           `json:"resources,omitempty"`
	ArtifactReferences []statev1.ArtifactReference `json:"artifactReferences,omitempty"`
	ExpiresAt          *time.Time                  `json:"expiresAt,omitempty"`
	DeletedAt          *time.Time                  `json:"deletedAt,omitempty"`
	UpdatedAt          time.Time                   `json:"updatedAt"`
}

type operationDoc struct {
	SchemaVersion int                        `json:"schemaVersion"`
	ID            string                     `json:"id"`
	Kind          string                     `json:"kind"`
	Fingerprint   string                     `json:"fingerprint"`
	Status        string                     `json:"status"`
	Effect        string                     `json:"effect,omitempty"`
	Error         string                     `json:"error,omitempty"`
	CreatedAt     time.Time                  `json:"createdAt"`
	UpdatedAt     time.Time                  `json:"updatedAt"`
	Checkpoints   map[string]json.RawMessage `json:"checkpoints,omitempty"`
	Observation   *driver.Observation        `json:"observation,omitempty"`
	Result        *driver.Result             `json:"result,omitempty"`
}

type releaseDoc struct {
	SchemaVersion      int                         `json:"schemaVersion"`
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
	if existing, readErr := e.readOperation(ctx, opKey); readErr == nil {
		if existing.Fingerprint != fingerprint {
			return Outcome{}, ErrConflict
		}
		switch existing.Status {
		case "succeeded":
			return Outcome{Resource: resource, Operation: existing}, nil
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
			return Outcome{Resource: resource, Operation: op}, nil
		}
		if op.Status == "unknown" {
			return e.reconcileOperation(ctx, identity, resourceKey, resource, resourceETag, opKey, op, cfg)
		}
		return Outcome{}, fmt.Errorf("deploy: operation %s is %s", opID, op.Status)
	}

	now := e.now()
	op := operationDoc{SchemaVersion: schemaVersion, ID: opID, Kind: "apply", Fingerprint: fingerprint,
		Status: "pending", CreatedAt: now, UpdatedAt: now, Checkpoints: map[string]json.RawMessage{}}
	opEntry, err := encode(op)
	if err != nil {
		return Outcome{}, err
	}
	if _, _, err := e.State.Create(ctx, opKey, statev1.WriteRequest{Value: opEntry}); err != nil {
		if !isExists(err) {
			return Outcome{}, err
		}
		existing, _, readErr := e.State.Read(ctx, opKey)
		if readErr != nil {
			return Outcome{}, readErr
		}
		if err := json.Unmarshal(existing.Value, &op); err != nil {
			return Outcome{}, err
		}
		if op.Fingerprint != fingerprint {
			return Outcome{}, ErrConflict
		}
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

	checkpoint := func(cctx context.Context, name string, data json.RawMessage) error {
		if len(data) > 4096 {
			return fmt.Errorf("deploy: checkpoint %s is too large", name)
		}
		entry, etag, err := e.State.Read(cctx, opKey)
		if err != nil {
			return err
		}
		var current operationDoc
		if err := json.Unmarshal(entry.Value, &current); err != nil {
			return err
		}
		if current.Checkpoints == nil {
			current.Checkpoints = map[string]json.RawMessage{}
		}
		current.Checkpoints[name] = data
		current.UpdatedAt = e.now()
		raw, err := encode(current)
		if err != nil {
			return err
		}
		_, _, err = e.State.Update(cctx, opKey, etag, statev1.WriteRequest{Value: raw})
		return err
	}
	req := driver.Request{Protocol: "kodama.deploy-driver/v1", RequestID: opID, Method: "apply",
		Deadline: e.deadline(cfg), Identity: identity, OperationID: opID, DesiredRelease: cfg.DesiredRelease,
		Configuration: cfg.Configuration, Artifacts: cfg.Artifacts, Checkpoints: op.Checkpoints,
		Previous: resourceEntry.Value}
	result, err := e.Driver.Apply(ctx, req, checkpoint)
	if err != nil {
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
	if resource.ActiveOperation == "" {
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
	resourceKey, _ := keyFor("resources", identity.ResourceKey)
	_, etag, err := e.State.Read(ctx, resourceKey)
	if err != nil {
		return Outcome{}, err
	}
	return e.reconcileOperation(ctx, identity, resourceKey, resource, etag, opKey, op, cfg)
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
		_ = e.saveOperation(ctx, opKey, op)
		return Outcome{Resource: resource, Operation: op}, err
	}
	if obs.State == driver.Absent || obs.State == driver.Unhealthy || obs.State == driver.Conflict {
		op.Status = "failed"
		op.Effect = "possible"
		op.Error = "provider observation: " + string(obs.State)
		op.Observation = &obs
		op.UpdatedAt = e.now()
		_ = e.saveOperation(ctx, opKey, op)
		resource.ActiveOperation = ""
		resource.LastOperation = op.ID
		resource.UpdatedAt = e.now()
		resourceRaw, _ := encode(resource)
		_, _, _ = e.State.Update(ctx, resourceKey, resourceETag, statev1.WriteRequest{Value: resourceRaw})
		return Outcome{Resource: resource, Operation: op}, fmt.Errorf("deploy: operation failed by observation")
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
	opKey, _ := keyFor("operations", identity.ResourceKey+"/"+opID)
	fingerprint, _ := fingerprint(Config{OperationID: opID, Recipe: cfg.Recipe, Configuration: cfg.Configuration, Artifacts: cfg.Artifacts, DesiredRelease: release}, identity)
	now := e.now()
	op := operationDoc{SchemaVersion: schemaVersion, ID: opID, Kind: "rollback", Fingerprint: fingerprint, Status: "pending", CreatedAt: now, UpdatedAt: now}
	raw, _ := encode(op)
	if _, _, err := e.State.Create(ctx, opKey, statev1.WriteRequest{Value: raw}); err != nil && !isExists(err) {
		return Outcome{}, err
	}
	resource.ActiveOperation = opID
	resource.UpdatedAt = now
	resourceRaw, _ := encode(resource)
	resourceEntry, resourceETag, err := e.State.Update(ctx, resourceKeyFor(identity), resourceETag, statev1.WriteRequest{Value: resourceRaw})
	if err != nil {
		return Outcome{}, err
	}
	_ = resourceEntry
	req := driver.Request{Protocol: "kodama.deploy-driver/v1", Method: "rollback", RequestID: opID,
		Deadline: e.deadline(cfg), Identity: identity, OperationID: opID, DesiredRelease: release,
		Configuration: cfg.Configuration, Artifacts: cfg.Artifacts, Previous: mustJSON(resource)}
	result, err := e.Driver.Rollback(ctx, req, nil)
	if err != nil {
		op.Status = "unknown"
		op.Effect = result.Effect
		op.Error = err.Error()
		op.UpdatedAt = e.now()
		_ = e.saveOperation(ctx, opKey, op)
		return Outcome{Resource: resource, Operation: op}, err
	}
	obs, err := e.Driver.Observe(ctx, req)
	if err != nil || obs.State != driver.Ready {
		if err == nil {
			err = fmt.Errorf("deploy: provider is %s", obs.State)
		}
		op.Status = "unknown"
		op.Error = err.Error()
		op.UpdatedAt = e.now()
		_ = e.saveOperation(ctx, opKey, op)
		return Outcome{Resource: resource, Operation: op}, err
	}
	cfg.DesiredRelease = release
	return e.settle(ctx, opKey, resourceKeyFor(identity), resourceETag, op, result, &obs, cfg)
}

// Cleanup marks a resource deleted after the driver confirms deletion.
func (e *Engine) Cleanup(ctx context.Context, cfg Config) (Outcome, error) {
	identity, resource, resourceETag, err := e.identifyResource(ctx, cfg)
	if err != nil {
		return Outcome{}, err
	}
	resourceKey, _ := keyFor("resources", identity.ResourceKey)
	if resource.DeletedAt != nil {
		return Outcome{Resource: resource}, nil
	}
	opID := cfg.OperationID
	if opID == "" {
		opID = cfg.ActionKey + ":cleanup"
	}
	opKey, _ := keyFor("operations", identity.ResourceKey+"/"+opID)
	fingerprint, _ := fingerprint(cfg, identity)
	now := e.now()
	op := operationDoc{SchemaVersion: schemaVersion, ID: opID, Kind: "cleanup", Fingerprint: fingerprint, Status: "pending", CreatedAt: now, UpdatedAt: now}
	raw, _ := encode(op)
	opEntry, _, err := e.State.Create(ctx, opKey, statev1.WriteRequest{Value: raw})
	if err != nil && !isExists(err) {
		return Outcome{}, err
	}
	if err == nil {
		op.Status = "applying"
		op.UpdatedAt = now
		raw, _ = encode(op)
		_, _, _ = e.State.Update(ctx, opKey, opEntry.ETag(), statev1.WriteRequest{Value: raw})
	}
	req := driver.Request{Protocol: "kodama.deploy-driver/v1", Method: "destroy", RequestID: opID,
		Deadline: e.deadline(cfg), Identity: identity, OperationID: opID, Configuration: cfg.Configuration,
		Artifacts: cfg.Artifacts, Previous: mustJSON(resource)}
	result, err := e.Driver.Destroy(ctx, req, nil)
	if err != nil {
		op.Status = "unknown"
		op.Effect = result.Effect
		op.Error = err.Error()
		op.UpdatedAt = e.now()
		_ = e.saveOperation(ctx, opKey, op)
		return Outcome{Resource: resource, Operation: op}, err
	}
	op.Status = "succeeded"
	op.Effect = result.Effect
	op.Result = &result
	op.UpdatedAt = e.now()
	_ = e.saveOperation(ctx, opKey, op)
	resource.ActiveOperation = ""
	resource.LastOperation = opID
	resource.DeletedAt = &now
	resource.UpdatedAt = now
	resource.ArtifactReferences = nil
	resourceRaw, _ := encode(resource)
	_, _, err = e.State.Update(ctx, resourceKey, resourceETag, statev1.WriteRequest{Value: resourceRaw})
	if err != nil {
		return Outcome{}, err
	}
	return Outcome{Resource: resource, Operation: op}, nil
}

func (e *Engine) settle(ctx context.Context, opKey, resourceKey, resourceETag string, op operationDoc, result driver.Result, obs *driver.Observation, cfg Config) (Outcome, error) {
	now := e.now()
	op.Status = "succeeded"
	op.Effect = result.Effect
	op.Result = &result
	op.Observation = obs
	op.UpdatedAt = now
	entry, etag, err := e.State.Read(ctx, opKey)
	if err != nil {
		return Outcome{}, err
	}
	_ = entry
	opRaw, _ := encode(op)
	if _, _, err := e.State.Update(ctx, opKey, etag, statev1.WriteRequest{Value: opRaw}); err != nil {
		return Outcome{}, err
	}
	resourceRaw, resourceReadETag, err := e.State.Read(ctx, resourceKey)
	if err != nil {
		return Outcome{}, err
	}
	var resource resourceDoc
	if err := json.Unmarshal(resourceRaw.Value, &resource); err != nil {
		return Outcome{}, err
	}
	resource.ActiveOperation = ""
	resource.LastOperation = op.ID
	resource.DesiredRelease = cfg.DesiredRelease
	resource.ObservedRelease = obs.ObservedRelease
	resource.ProviderRelease = result.ProviderRelease
	resource.ProviderVersion = result.ProviderVersion
	resource.Resources = result.Resources
	resource.UpdatedAt = now
	resource.LastHealthyRelease = cfg.DesiredRelease
	resource.ArtifactReferences = cfg.references()
	raw, err := encode(resource)
	if err != nil {
		return Outcome{}, err
	}
	updated, _, err := e.State.Update(ctx, resourceKey, resourceReadETag, statev1.WriteRequest{Value: raw, ArtifactReferences: cfg.references()})
	if err != nil {
		return Outcome{}, err
	}
	if cfg.DesiredRelease != "" {
		releaseKey, _ := keyFor("releases", resource.Identity.ResourceKey+"/"+cfg.DesiredRelease)
		release := releaseDoc{SchemaVersion: schemaVersion, Release: cfg.DesiredRelease, Artifacts: cfg.Artifacts,
			ProviderRelease: result.ProviderRelease, ProviderVersion: result.ProviderVersion, CreatedAt: now,
			Resources: result.Resources, ArtifactReferences: cfg.references()}
		releaseRaw, _ := encode(release)
		_, _, _ = e.State.Create(ctx, releaseKey, statev1.WriteRequest{Value: releaseRaw, ArtifactReferences: cfg.references()})
	}
	resource = resourceDoc{}
	_ = json.Unmarshal(updated.Value, &resource)
	return Outcome{Resource: resource, Operation: op}, nil
}

func (e *Engine) markUnknown(ctx context.Context, opKey, resourceKey, resourceETag string, op operationDoc, result driver.Result, cause error) (Outcome, error) {
	op.Status = "unknown"
	op.Effect = result.Effect
	op.Error = cause.Error()
	op.UpdatedAt = e.now()
	op.Result = &result
	raw, _ := encode(op)
	_, etag, err := e.State.Read(ctx, opKey)
	if err == nil {
		_, _, _ = e.State.Update(ctx, opKey, etag, statev1.WriteRequest{Value: raw})
	}
	resourceEntry, resourceETag, err := e.State.Read(ctx, resourceKey)
	if err == nil {
		var resource resourceDoc
		if json.Unmarshal(resourceEntry.Value, &resource) == nil {
			resource.ActiveOperation = op.ID
			resource.UpdatedAt = e.now()
			raw, _ := encode(resource)
			_, _, _ = e.State.Update(ctx, resourceKey, resourceETag, statev1.WriteRequest{Value: raw})
		}
	}
	return Outcome{Operation: op}, cause
}

func (e *Engine) failOperation(ctx context.Context, opKey, resourceKey, resourceETag string, op operationDoc, cause error) (Outcome, error) {
	op.Status = "failed"
	op.Effect = "none"
	op.Error = cause.Error()
	op.UpdatedAt = e.now()
	raw, _ := encode(op)
	_, etag, err := e.State.Read(ctx, opKey)
	if err == nil {
		_, _, _ = e.State.Update(ctx, opKey, etag, statev1.WriteRequest{Value: raw})
	}
	resourceEntry, resourceETag, err := e.State.Read(ctx, resourceKey)
	if err == nil {
		var resource resourceDoc
		if json.Unmarshal(resourceEntry.Value, &resource) == nil {
			resource.ActiveOperation = ""
			resource.LastOperation = op.ID
			resource.UpdatedAt = e.now()
			raw, _ := encode(resource)
			_, _, _ = e.State.Update(ctx, resourceKey, resourceETag, statev1.WriteRequest{Value: raw})
		}
	}
	return Outcome{Operation: op}, cause
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
	resource := resourceDoc{SchemaVersion: schemaVersion, Identity: identity, UpdatedAt: e.now()}
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
	return op, nil
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
	refs := make([]statev1.ArtifactReference, 0, len(cfg.Artifacts))
	for _, a := range cfg.Artifacts {
		refs = append(refs, statev1.ArtifactReference{ArtifactID: a.ID, Digest: a.Digest})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].ArtifactID < refs[j].ArtifactID })
	return refs
}

func fingerprint(cfg Config, identity driver.Identity) (string, error) {
	payload := struct {
		Kind          string            `json:"kind"`
		Resource      string            `json:"resource"`
		Desired       string            `json:"desired"`
		Artifacts     []driver.Artifact `json:"artifacts"`
		Configuration json.RawMessage   `json:"configuration"`
	}{Kind: "apply", Resource: identity.ResourceKey, Desired: cfg.DesiredRelease, Artifacts: cfg.Artifacts, Configuration: cfg.Configuration}
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

func mustJSON(v any) json.RawMessage {
	raw, _ := json.Marshal(v)
	return raw
}
