package work

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/shell/pipeline"
	"github.com/optimiware/kodema/internal/shell/resource"
	"github.com/optimiware/kodema/internal/shell/service"
	"github.com/optimiware/kodema/internal/shell/store"
	"github.com/optimiware/kodema/internal/shell/tenancy"
	"github.com/optimiware/kodema/internal/shell/trigger"
)

// Change commits one complete pipeline-management unit. The receipt is consulted
// first by the store (an equal retry replays the original result before any
// version guard), and every versioned write carries its expected version so a
// stale definition change fails with version_conflict and leaves no row change.
//
// The validated on.push projection the capability built is persisted to
// pipelines.push_triggers in the same unit as the exact authored bytes; the
// adapter never parses YAML.
func (adapter *Pipelines) Change(ctx context.Context, unit pipeline.ChangeUnit) (pipeline.ChangeResult, error) {
	if err := validProjectScope(unit.Owner); err != nil {
		return pipeline.ChangeResult{}, err
	}
	if err := requireNonEmpty("pipeline name", unit.Name); err != nil {
		return pipeline.ChangeResult{}, err
	}
	if err := validateReceipt(unit.Receipt); err != nil {
		return pipeline.ChangeResult{}, err
	}
	triggers, err := encodePushTriggers(unit.PushTriggers)
	if err != nil {
		return pipeline.ChangeResult{}, err
	}
	var (
		result pipeline.ChangeResult
		writes []store.Write
		locks  []store.Lock
	)
	switch unit.Kind {
	case pipeline.ChangeApply:
		if len(unit.YAML) == 0 {
			return pipeline.ChangeResult{}, fault.New(fault.InvalidInput, "pipeline yaml is empty")
		}
		digest := sha256.Sum256(unit.YAML)
		if unit.PipelineID == "" {
			identity, err := adapter.newID()
			if err != nil {
				return pipeline.ChangeResult{}, err
			}
			result = pipeline.ChangeResult{PipelineID: identity, Version: 1}
			writes = []store.Write{{
				Table:  "pipelines",
				ID:     string(identity),
				Insert: true,
				Values: map[string]any{
					"name":          unit.Name,
					"yaml":          unit.YAML,
					"yaml_sha256":   digest[:],
					"push_triggers": triggers,
					"version":       int64(1),
					"created_at":    unit.Receipt.At,
				},
			}}
			break
		}
		result = pipeline.ChangeResult{PipelineID: unit.PipelineID, Version: unit.ExpectedVersion + 1}
		writes = []store.Write{{
			Table:         "pipelines",
			ID:            string(unit.PipelineID),
			ExpectVersion: unit.ExpectedVersion,
			Values: map[string]any{
				"name":          unit.Name,
				"yaml":          unit.YAML,
				"yaml_sha256":   digest[:],
				"push_triggers": triggers,
			},
		}}
		locks = []store.Lock{pipelineLock(unit.PipelineID, unit.ExpectedVersion)}
	case pipeline.ChangeUpdate:
		if unit.PipelineID == "" || unit.ExpectedVersion == 0 {
			return pipeline.ChangeResult{}, fault.New(fault.InvalidInput, "pipeline update requires an identity and an expected version")
		}
		if len(unit.YAML) == 0 {
			return pipeline.ChangeResult{}, fault.New(fault.InvalidInput, "pipeline yaml is empty")
		}
		digest := sha256.Sum256(unit.YAML)
		result = pipeline.ChangeResult{PipelineID: unit.PipelineID, Version: unit.ExpectedVersion + 1}
		writes = []store.Write{{
			Table:         "pipelines",
			ID:            string(unit.PipelineID),
			ExpectVersion: unit.ExpectedVersion,
			Values: map[string]any{
				"name":          unit.Name,
				"yaml":          unit.YAML,
				"yaml_sha256":   digest[:],
				"push_triggers": triggers,
			},
		}}
		locks = []store.Lock{pipelineLock(unit.PipelineID, unit.ExpectedVersion)}
	case pipeline.ChangeDelete:
		if unit.PipelineID == "" || unit.ExpectedVersion == 0 {
			return pipeline.ChangeResult{}, fault.New(fault.InvalidInput, "pipeline delete requires an identity and an expected version")
		}
		result = pipeline.ChangeResult{PipelineID: unit.PipelineID, Version: unit.ExpectedVersion + 1}
		writes = []store.Write{{
			Table:         "pipelines",
			ID:            string(unit.PipelineID),
			Delete:        true,
			ExpectVersion: unit.ExpectedVersion,
		}}
		locks = []store.Lock{pipelineLock(unit.PipelineID, unit.ExpectedVersion)}
	default:
		return pipeline.ChangeResult{}, fault.New(fault.InvalidInput, "unknown pipeline change kind")
	}
	record, err := withResult(unit.Receipt, result)
	if err != nil {
		return pipeline.ChangeResult{}, err
	}
	// A ChangeApply unit with no PipelineID creates the row it describes, so
	// its pipeline-version authority read names no prior row. The insert itself
	// is the version guard for that shape.
	authority, err := authorityLocks(ctx, adapter.executor, adapter.scanner, unit.Authority, authorityContext{
		NewPipeline: unit.Kind == pipeline.ChangeApply && unit.PipelineID == "",
	})
	if err != nil {
		return pipeline.ChangeResult{}, err
	}
	locks = append(locks, authority...)
	if (unit.Kind == pipeline.ChangeApply || unit.Kind == pipeline.ChangeUpdate) && len(unit.PushTriggers) > 0 {
		// A durable receipt with this idempotency identity means the invocation
		// already committed (or reused its key with changed content). Skip the
		// fresh-commit on.push service validation so a lost-response retry still
		// replays even if the referenced service was later disabled or modified;
		// the executor still decides replay versus changed content atomically.
		replaying, err := committedReceipt(ctx, adapter.scanner, unit.Owner, unit.Receipt.Key)
		if err != nil {
			return pipeline.ChangeResult{}, err
		}
		if !replaying {
			pushLocks, err := pushServiceLocks(ctx, adapter.executor, adapter.scanner, unit.Owner, unit.PushTriggers)
			if err != nil {
				// A concurrent identical retry may have committed between the
				// pre-flight lookup and the service read, after which the
				// service was disabled or modified. Re-check the receipt so the
				// retry still converges on the stored result; a genuinely fresh
				// commit keeps the refusal.
				committed, lookupErr := committedReceipt(ctx, adapter.scanner, unit.Owner, unit.Receipt.Key)
				if lookupErr != nil {
					return pipeline.ChangeResult{}, lookupErr
				}
				if !committed {
					return pipeline.ChangeResult{}, err
				}
			} else {
				locks = append(locks, pushLocks...)
			}
		}
	}
	committed, err := execute(ctx, adapter.executor, unit.Owner, store.Unit{
		Locks:   locks,
		Writes:  writes,
		Receipt: &record,
		Audit:   unit.Audit,
		Events:  unit.Events,
	})
	if err != nil {
		return pipeline.ChangeResult{}, err
	}
	replayed, isReplay, err := replay[pipeline.ChangeResult](committed)
	if err != nil {
		return pipeline.ChangeResult{}, err
	}
	if isReplay {
		replayed.Replayed = true
		return replayed, nil
	}
	return result, nil
}

// pushServiceLocks verifies that every on.push automatic-start service named by
// the pipeline exists in project or tenant scope, is enabled and holds the
// engineer role, and returns guarded locks pinning each observed services row
// (version, enabled state and tenant-wide scope flag) inside the same atomic
// commit unit so concurrent service.disable or role removal cannot race past
// validation.
func pushServiceLocks(
	ctx context.Context,
	executor store.Executor,
	scanner store.Scanner,
	scope tenancy.Scope,
	triggers []pipeline.PushTrigger,
) ([]store.Lock, error) {
	resolver := authorityResolver{executor: executor, scanner: scanner}
	seen := make(map[service.ServiceID]struct{}, len(triggers))
	locks := make([]store.Lock, 0, len(triggers))
	for _, trigger := range triggers {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if trigger.Service == "" {
			return nil, fault.New(fault.InvalidInput, "on.push service is empty")
		}
		if _, ok := seen[trigger.Service]; ok {
			continue
		}
		seen[trigger.Service] = struct{}{}
		row, found, tenantWide, err := resolver.readNullableScope(ctx, scope, "services", string(trigger.Service))
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, fault.New(fault.InvalidInput, fmt.Sprintf("on.push service %q does not exist in scope", trigger.Service))
		}
		if textValue(row, "state") != serviceStateEnabled {
			return nil, fault.New(fault.InvalidInput, fmt.Sprintf("on.push service %q is disabled; enable it before binding an on.push trigger", trigger.Service))
		}
		roles, err := decodeAuthorityRoles(bytesValue(row, "roles"))
		if err != nil {
			return nil, err
		}
		if !slices.Contains(tenancy.AssignableServiceRoles(roles), tenancy.RoleEngineer) {
			return nil, fault.New(fault.InvalidInput, fmt.Sprintf("on.push service %q does not hold the %q role required to start executions on push", trigger.Service, tenancy.RoleEngineer))
		}
		version := versionValue(row, "version")
		if version == 0 {
			return nil, fault.New(fault.InternalFailure, fmt.Sprintf("on.push service %q has no version", trigger.Service))
		}
		locks = append(locks, store.Lock{
			Table:         "services",
			IDs:           []string{string(trigger.Service)},
			ExpectVersion: version,
			Expect:        map[string]any{"state": serviceStateEnabled},
			TenantWide:    tenantWide,
		})
	}
	return locks, nil
}

// Get reads one scoped pipeline definition and its persisted push projection.
func (adapter *Pipelines) Get(ctx context.Context, scope tenancy.Scope, identity pipeline.PipelineID) (pipeline.Definition, error) {
	if err := validProjectScope(scope); err != nil {
		return pipeline.Definition{}, err
	}
	if err := requireNonEmpty("pipeline id", string(identity)); err != nil {
		return pipeline.Definition{}, err
	}
	row, err := readRow(ctx, adapter.executor, scope, "pipelines", string(identity))
	if err != nil {
		return pipeline.Definition{}, err
	}
	return adapter.definition(row)
}

// ByName reads one scoped pipeline definition by its project-unique name.
func (adapter *Pipelines) ByName(ctx context.Context, scope tenancy.Scope, name string) (pipeline.Definition, error) {
	if err := validProjectScope(scope); err != nil {
		return pipeline.Definition{}, err
	}
	if err := requireNonEmpty("pipeline name", name); err != nil {
		return pipeline.Definition{}, err
	}
	page, err := adapter.scanner.Query(ctx, scope, store.ScanQuery{
		Table:   "pipelines",
		Equals:  map[string]any{"name": name},
		OrderBy: []store.Order{{Column: "id"}},
		Limit:   1,
	})
	if err != nil {
		return pipeline.Definition{}, err
	}
	if len(page.Rows) == 0 {
		return pipeline.Definition{}, fault.New(fault.NotFound, "pipeline name was not found in this scope")
	}
	return adapter.definition(page.Rows[0])
}

// List reads every scoped pipeline definition in (name, id) order.
func (adapter *Pipelines) List(ctx context.Context, scope tenancy.Scope) ([]pipeline.Definition, error) {
	if err := validProjectScope(scope); err != nil {
		return nil, err
	}
	rows, err := scanAll(ctx, adapter.scanner, scope, store.ScanQuery{
		Table:   "pipelines",
		OrderBy: []store.Order{{Column: "name"}, {Column: "id"}},
		Limit:   store.MaxQueryLimit,
	})
	if err != nil {
		return nil, err
	}
	definitions := make([]pipeline.Definition, 0, len(rows))
	for _, row := range rows {
		definition, err := adapter.definition(row)
		if err != nil {
			return nil, err
		}
		definitions = append(definitions, definition)
	}
	return definitions, nil
}

// Candidates returns the current pipeline definitions with a persisted push
// trigger together with the matching resource binding. A definition whose push
// trigger has no binding is omitted, so the processor refuses its persisted
// selections as orphans instead of starting work without a binding.
func (adapter *Pipelines) Candidates(ctx context.Context, scope tenancy.Scope) ([]trigger.Candidate, error) {
	definitions, err := adapter.List(ctx, scope)
	if err != nil {
		return nil, err
	}
	candidates := make([]trigger.Candidate, 0, len(definitions))
	for _, definition := range definitions {
		triggers := definition.PushTriggers
		if len(triggers) == 0 && definition.Push != nil {
			triggers = []pipeline.PushTrigger{*definition.Push}
		}
		seen := make(map[resource.BindingID]struct{})
		for _, push := range triggers {
			if push.Repository == "" || push.Service == "" {
				continue
			}
			bindings, err := adapter.bindings(ctx, scope, push.Repository)
			if err != nil {
				return nil, err
			}
			for _, binding := range bindings {
				if _, ok := seen[binding.ID]; ok {
					continue
				}
				seen[binding.ID] = struct{}{}
				candidates = append(candidates, trigger.Candidate{Definition: definition, Binding: binding})
			}
		}
	}
	return candidates, nil
}

// bindings reads every current resource binding of one push repository. The
// trigger's service is the automatic-start principal, not the enforcing
// (reporting) service, so the binding is selected by the repository alone; the
// processor then requires the binding's enforcing service to be the report
// source.
func (adapter *Pipelines) bindings(ctx context.Context, scope tenancy.Scope, repository resource.ResourceID) ([]resource.Binding, error) {
	rows, err := scanAll(ctx, adapter.scanner, scope, store.ScanQuery{
		Table: "resource_bindings",
		Equals: map[string]any{
			"resource_id": string(repository),
		},
		OrderBy: []store.Order{{Column: "id"}},
		Limit:   store.MaxQueryLimit,
	})
	if err != nil {
		return nil, err
	}
	bindings := make([]resource.Binding, 0, len(rows))
	for _, row := range rows {
		binding, err := decodeBinding(scope, row)
		if err != nil {
			return nil, err
		}
		bindings = append(bindings, binding)
	}
	return bindings, nil
}

// definition decodes one pipelines row, including the persisted push projection.
func (adapter *Pipelines) definition(row store.Row) (pipeline.Definition, error) {
	triggers, err := decodePushTriggers(bytesValue(row, "push_triggers"))
	if err != nil {
		return pipeline.Definition{}, err
	}
	return pipeline.Definition{
		ID:           pipeline.PipelineID(textValue(row, "id")),
		Name:         textValue(row, "name"),
		YAML:         bytesValue(row, "yaml"),
		Version:      versionValue(row, "version"),
		PushTriggers: triggers,
	}, nil
}

var (
	_ pipeline.Reader         = (*Pipelines)(nil)
	_ trigger.CandidateReader = (*Pipelines)(nil)
)

// encodePushTriggers renders the push_triggers projection. The column is NOT
// NULL with a default of [], so an absent projection is stored as an empty
// array.
func encodePushTriggers(triggers []pipeline.PushTrigger) ([]byte, error) {
	if triggers == nil {
		triggers = []pipeline.PushTrigger{}
	}
	encoded, err := json.Marshal(triggers)
	if err != nil {
		return nil, fault.Wrap(fault.InvalidInput, "pipeline push trigger projection cannot be encoded", err)
	}
	return encoded, nil
}

// decodePushTriggers reads the push_triggers projection. A missing or null
// column is an empty projection.
func decodePushTriggers(raw []byte) ([]pipeline.PushTrigger, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var triggers []pipeline.PushTrigger
	if err := json.Unmarshal(raw, &triggers); err != nil {
		return nil, fault.Wrap(fault.InternalFailure, "a stored pipeline push projection is not readable", err)
	}
	return triggers, nil
}

// decodeBinding decodes one resource_bindings row for a push candidate.
func decodeBinding(scope tenancy.Scope, row store.Row) (resource.Binding, error) {
	origins, err := decodeJSON[[]string](bytesValue(row, "origins"))
	if err != nil {
		return resource.Binding{}, err
	}
	operations, err := decodeJSON[[]resource.Operation](bytesValue(row, "operations"))
	if err != nil {
		return resource.Binding{}, err
	}
	reportable, err := decodeJSON[[]string](bytesValue(row, "reportable_events"))
	if err != nil {
		return resource.Binding{}, err
	}
	metadata, err := decodeJSON[map[string]string](bytesValue(row, "metadata"))
	if err != nil {
		return resource.Binding{}, err
	}
	return resource.Binding{
		ID:               resource.BindingID(textValue(row, "id")),
		ResourceID:       resource.ResourceID(textValue(row, "resource_id")),
		Scope:            scope,
		EnforcingService: service.ServiceID(textValue(row, "service_id")),
		TrustedOrigins:   origins,
		Operations:       operations,
		ReportableEvents: reportable,
		Version:          versionValue(row, "version"),
		Metadata:         metadata,
	}, nil
}
