// Package gateway declares and implements the optional configured model
// surface.
//
// The gateway is an optional in-process Shell capability that invokes the
// shared authorizer directly; it is not a fourth layer and exposes no model
// management API. A surface fixes the operation name, the configured model
// allowlist and the request-size bound so the transport adapter cannot invent
// broader authority than the registry grants.
//
// The orchestration values and ports cover one configured model allowlist per
// project, one bounded non-streaming OpenAI-compatible invocation and one
// attributable usage row per dispatched call. Provider credentials are
// resolved inside the Shell for the duration of one call and are never
// returned, persisted or logged.
package gateway

import (
	"fmt"
	"strings"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// Surface is one configured model surface.
type Surface struct {
	Operation       string   `json:"operation"`
	AllowedModels   []string `json:"allowed_models"`
	MaxRequestBytes int      `json:"max_request_bytes"`
}

// Bounds is the gateway request/response bound set.
type Bounds struct {
	MaxRequestBytes  int `json:"max_request_bytes"`
	MaxResponseBytes int `json:"max_response_bytes"`
}

// DefaultBounds is the declared gateway bound set: 1 MiB each way.
var DefaultBounds = Bounds{
	MaxRequestBytes:  1 << 20,
	MaxResponseBytes: 1 << 20,
}

// Validate checks that the surface names an operation, at least one configured
// model and a request bound inside the gateway bound.
func (surface Surface) Validate() error {
	if surface.Operation == "" {
		return fault.New(fault.InvalidInput, "model surface operation is empty")
	}
	if len(surface.AllowedModels) == 0 {
		return fault.New(fault.InvalidInput, "model surface has no allowed models")
	}
	for _, model := range surface.AllowedModels {
		if model == "" {
			return fault.New(fault.InvalidInput, "model surface has an empty allowed model")
		}
	}
	if surface.MaxRequestBytes <= 0 || surface.MaxRequestBytes > DefaultBounds.MaxRequestBytes {
		return fault.New(fault.InvalidInput, fmt.Sprintf("model surface request bound %d is outside (0, %d]", surface.MaxRequestBytes, DefaultBounds.MaxRequestBytes))
	}
	return nil
}

// UsageID identifies one persisted usage row. It is allocated through the
// injected identity source so a usage row has a stable, attributable identity.
type UsageID string

// UsageOutcome is the settled outcome of one dispatched model call.
type UsageOutcome string

const (
	// UsageSucceeded reports a completed, validated completion.
	UsageSucceeded UsageOutcome = "succeeded"
	// UsageFailed reports a definitive provider or protocol refusal.
	UsageFailed UsageOutcome = "failed"
	// UsageUncertain reports a dispatched call whose outcome is unknown.
	UsageUncertain UsageOutcome = "uncertain"
)

// usageOutcomes is the closed settled-outcome vocabulary.
var usageOutcomes = [...]UsageOutcome{UsageSucceeded, UsageFailed, UsageUncertain}

// Valid reports whether the outcome is one of the declared outcomes.
func (outcome UsageOutcome) Valid() bool {
	for _, candidate := range usageOutcomes {
		if outcome == candidate {
			return true
		}
	}
	return false
}

const (
	// UsageTable is the attributable model-usage table.
	UsageTable = "model_usage"
	// MaxTimeout is the largest configured finite provider timeout.
	MaxTimeout = 5 * time.Minute
	// DefaultContentRetention is the seven-day raw-content retention default.
	DefaultContentRetention = 7 * 24 * time.Hour
)

// ModelConfig is one configured model in exactly one project scope.
//
// Endpoint is the provider URL, UpstreamModel is the provider-side model name
// the caller's configured name maps to, and SecretName is the one declared
// secret resolved per call. RecordContent opts raw request/response bytes into
// authorized retention; ContentRetention zero uses DefaultContentRetention.
type ModelConfig struct {
	Scope               tenancy.Scope
	Model               string
	UpstreamModel       string
	Endpoint            string
	SecretName          id.SecretName
	MaxRequestBytes     int
	MaxResponseBytes    int
	MaxCompletionTokens int
	Timeout             time.Duration
	RecordContent       bool
	ContentRetention    time.Duration
}

// Validate checks one configured model: a project scope, the configured and
// upstream names, an http(s) provider endpoint, one safe declared secret
// reference, positive bounds inside the declared gateway and model subset
// bounds, a finite timeout and a non-negative content retention.
func (model ModelConfig) Validate() error {
	if err := model.Scope.Validate(); err != nil {
		return err
	}
	if !model.Scope.ProjectScoped() {
		return fault.New(fault.InvalidInput, "configured model scope must name a project")
	}
	if model.Model == "" {
		return fault.New(fault.InvalidInput, "configured model name is empty")
	}
	if model.UpstreamModel == "" {
		return fault.New(fault.InvalidInput, "configured upstream model is empty")
	}
	if err := validateEndpoint(model.Endpoint); err != nil {
		return err
	}
	if err := validateSecretReference(model.Scope, model.SecretName); err != nil {
		return err
	}
	if model.MaxRequestBytes <= 0 || model.MaxRequestBytes > DefaultBounds.MaxRequestBytes {
		return fault.New(fault.InvalidInput, fmt.Sprintf("configured model request bound %d is outside (0, %d]", model.MaxRequestBytes, DefaultBounds.MaxRequestBytes))
	}
	if model.MaxResponseBytes <= 0 || model.MaxResponseBytes > DefaultBounds.MaxResponseBytes {
		return fault.New(fault.InvalidInput, fmt.Sprintf("configured model response bound %d is outside (0, %d]", model.MaxResponseBytes, DefaultBounds.MaxResponseBytes))
	}
	if model.MaxCompletionTokens <= 0 || model.MaxCompletionTokens > api.MaxCompletionTokens {
		return fault.New(fault.InvalidInput, fmt.Sprintf("configured model completion cap %d is outside (0, %d]", model.MaxCompletionTokens, api.MaxCompletionTokens))
	}
	if model.Timeout <= 0 || model.Timeout > MaxTimeout {
		return fault.New(fault.InvalidInput, fmt.Sprintf("configured model timeout %s is outside (0, %s]", model.Timeout, MaxTimeout))
	}
	if model.ContentRetention < 0 {
		return fault.New(fault.InvalidInput, "configured model content retention is negative")
	}
	return nil
}

// validateEndpoint checks that the provider endpoint is an explicit http(s)
// URL with a host. It deliberately performs no DNS or network work.
func validateEndpoint(endpoint string) error {
	if endpoint == "" {
		return fault.New(fault.InvalidInput, "configured model endpoint is empty")
	}
	lower := strings.ToLower(endpoint)
	var rest string
	switch {
	case strings.HasPrefix(lower, "https://"):
		rest = endpoint[len("https://"):]
	case strings.HasPrefix(lower, "http://"):
		rest = endpoint[len("http://"):]
	default:
		return fault.New(fault.InvalidInput, "configured model endpoint is not an http(s) URL")
	}
	host := rest
	if index := strings.IndexAny(host, "/?#"); index >= 0 {
		host = host[:index]
	}
	if host == "" || strings.ContainsAny(host, "@ \t\r\n") {
		return fault.New(fault.InvalidInput, "configured model endpoint has no usable host")
	}
	return nil
}

// validateSecretReference checks that a configured model names exactly one
// declared secret through the shared secret-name grammar and the exact resolve
// request shape. The secret resolver remains the authoritative custody check
// for the value itself.
func validateSecretReference(scope tenancy.Scope, name id.SecretName) error {
	if _, err := id.ParseSecretName(string(name)); err != nil {
		return fault.Wrap(fault.InvalidInput, "configured model secret name is invalid", err)
	}
	request := secret.ResolveRequest{Scope: scope, Names: []id.SecretName{name}}
	if err := request.Validate(); err != nil {
		return fault.Wrap(fault.InvalidInput, "configured model secret reference is invalid", err)
	}
	return nil
}

// Config is the optional gateway configuration. A disabled gateway is inert:
// it validates nothing, touches no dependency and refuses every invocation.
type Config struct {
	Enabled bool
	Models  []ModelConfig
}

// Validate checks an enabled configuration: at least one configured model and
// no duplicate model name inside one project scope.
func (config Config) Validate() error {
	if !config.Enabled {
		return nil
	}
	if len(config.Models) == 0 {
		return fault.New(fault.InvalidInput, "enabled model gateway has no configured models")
	}
	seen := make(map[modelKey]bool, len(config.Models))
	for _, model := range config.Models {
		if err := model.Validate(); err != nil {
			return err
		}
		key := modelKey{scope: model.Scope, model: model.Model}
		if seen[key] {
			return fault.New(fault.InvalidInput, fmt.Sprintf("configured model %q is declared twice in one project", model.Model))
		}
		seen[key] = true
	}
	return nil
}

// modelKey is the exact configured-model identity: one name in one project.
type modelKey struct {
	scope tenancy.Scope
	model string
}

// ContentRecord is one authorized raw request/response capture. It is written
// only when the configured model opts in and carries no provider credential.
type ContentRecord struct {
	Scope       tenancy.Scope
	PrincipalID id.PrincipalID
	ExecutionID id.ExecutionID
	Model       string
	Request     []byte
	Response    []byte
	At          time.Time
	RetainUntil time.Time
}

// UsageRecord is one attributable model-usage row. UsageKnown distinguishes a
// validated count from an honest unknown, and Usage carries the provider's
// validated counts only when UsageKnown is true. The row targets the
// model_usage table; ExecutionID is empty for a non-execution caller, so the
// persisted execution_id is nullable.
type UsageRecord struct {
	ID            UsageID
	Scope         tenancy.Scope
	PrincipalID   id.PrincipalID
	ExecutionID   id.ExecutionID
	Model         string
	UpstreamModel string
	Outcome       UsageOutcome
	UsageKnown    bool
	Usage         api.ChatUsage
	At            time.Time
}

// Unit builds the one-row usage commit unit. It carries no receipt, lock,
// audit row or event because model.invoke has receipt policy none. An empty
// ExecutionID is written as SQL NULL, never as an empty string.
func (record UsageRecord) Unit() (store.Unit, error) {
	if err := record.validate(); err != nil {
		return store.Unit{}, err
	}
	var executionID any
	if record.ExecutionID != "" {
		executionID = string(record.ExecutionID)
	}
	return store.Unit{
		Writes: []store.Write{{
			Table:  UsageTable,
			ID:     string(record.ID),
			Insert: true,
			Values: map[string]any{
				"principal_id":      string(record.PrincipalID),
				"execution_id":      executionID,
				"model":             record.Model,
				"upstream_model":    record.UpstreamModel,
				"outcome":           string(record.Outcome),
				"usage_known":       record.UsageKnown,
				"prompt_tokens":     int64(record.Usage.PromptTokens),
				"completion_tokens": int64(record.Usage.CompletionTokens),
				"total_tokens":      int64(record.Usage.TotalTokens),
				"recorded_at":       record.At,
				"version":           uint64(1),
			},
		}},
	}, nil
}

// validate checks that the record can become an attributable row: identity,
// project scope, principal, configured/upstream names, a declared outcome, an
// honest usage count and a recorded time.
func (record UsageRecord) validate() error {
	if record.ID == "" {
		return fault.New(fault.InvalidInput, "usage record id is empty")
	}
	if len(record.ID) > id.MaxIDLength {
		return fault.New(fault.InvalidInput, "usage record id is too long")
	}
	if err := record.Scope.Validate(); err != nil {
		return err
	}
	if !record.Scope.ProjectScoped() {
		return fault.New(fault.InvalidInput, "usage record scope must name a project")
	}
	if _, err := id.ParsePrincipalID(string(record.PrincipalID)); err != nil {
		return fault.Wrap(fault.InvalidInput, "usage record principal id is invalid", err)
	}
	if record.ExecutionID != "" {
		if _, err := id.ParseExecutionID(string(record.ExecutionID)); err != nil {
			return fault.Wrap(fault.InvalidInput, "usage record execution id is invalid", err)
		}
	}
	if record.Model == "" {
		return fault.New(fault.InvalidInput, "usage record model is empty")
	}
	if record.UpstreamModel == "" {
		return fault.New(fault.InvalidInput, "usage record upstream model is empty")
	}
	if !record.Outcome.Valid() {
		return fault.New(fault.InvalidInput, fmt.Sprintf("usage record outcome %q is not frozen", record.Outcome))
	}
	if record.At.IsZero() {
		return fault.New(fault.InvalidInput, "usage record time is zero")
	}
	if record.Outcome == UsageSucceeded && !record.UsageKnown {
		return fault.New(fault.InvalidInput, "a succeeded usage record must carry validated counts")
	}
	if record.Outcome != UsageSucceeded && record.UsageKnown {
		return fault.New(fault.InvalidInput, "a non-succeeded usage record cannot claim known counts")
	}
	if record.UsageKnown {
		if err := record.Usage.Validate(); err != nil {
			return fault.Wrap(fault.InvalidInput, "usage record counts are inconsistent", err)
		}
	} else if record.Usage != (api.ChatUsage{}) {
		return fault.New(fault.InvalidInput, "usage record reports unknown usage with fabricated counts")
	}
	return nil
}
