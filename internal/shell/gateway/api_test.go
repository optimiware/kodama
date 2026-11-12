// Optional model-gateway API pinned by these tests.
//
// This file pins the gateway orchestration declared in
// internal/shell/gateway/{types.go,ports.go}; the tests below never redefine
// api.ChatCompletionRequest/Response, the authorize registry,
// secret.ResolveRequest/Resolved or store.Unit.
//
//	type Clock interface{ Now() time.Time }
//
//	type UsageID string
//	type UsageOutcome string
//	const (
//	    UsageSucceeded UsageOutcome = "succeeded"
//	    UsageFailed    UsageOutcome = "failed"
//	    UsageUncertain UsageOutcome = "uncertain"
//	)
//	const (
//	    UsageTable              = "model_usage"
//	    MaxTimeout              = 5 * time.Minute
//	    DefaultContentRetention = 7 * 24 * time.Hour
//	)
//
//	type ModelConfig struct {
//	    Scope               tenancy.Scope
//	    Model               string
//	    UpstreamModel       string
//	    Endpoint            string
//	    SecretName          id.SecretName
//	    MaxRequestBytes     int
//	    MaxResponseBytes    int
//	    MaxCompletionTokens int
//	    Timeout             time.Duration
//	    RecordContent       bool
//	    ContentRetention    time.Duration
//	}
//
//	type Config struct {
//	    Enabled bool
//	    Models  []ModelConfig
//	}
//
//	type ContentRecord struct {
//	    Scope       tenancy.Scope
//	    PrincipalID id.PrincipalID
//	    ExecutionID id.ExecutionID
//	    Model       string
//	    Request     []byte
//	    Response    []byte
//	    At          time.Time
//	    RetainUntil time.Time
//	}
//
//	type ContentRecorder interface {
//	    Record(context.Context, ContentRecord) error
//	}
//
//	type UsageRecord struct {
//	    ID            UsageID
//	    Scope         tenancy.Scope
//	    PrincipalID   id.PrincipalID
//	    ExecutionID   id.ExecutionID
//	    Model         string
//	    UpstreamModel string
//	    Outcome       UsageOutcome
//	    UsageKnown    bool
//	    Usage         api.ChatUsage
//	    At            time.Time
//	}
//	func (UsageRecord) Unit() (store.Unit, error)
//
//	type ExecutionReader interface {
//	    ReadExecution(context.Context, id.ExecutionID) (authorize.AdmittedExecution, error)
//	}
//
//	type Option func(*options) error
//	func WithExecutionReader(ExecutionReader) Option
//
//	type Dependencies struct {
//	    Principals authorize.CurrentPrincipal
//	    Authorizer authorize.Authorizer
//	    Authority  authorize.AuthorityReader
//	    Secrets    secret.Resolver
//	    Store      store.Executor
//	    Content    ContentRecorder
//	    Transport  http.RoundTripper
//	    Clock      Clock
//	    NewID      func() (UsageID, error)
//	}
//
//	type Invocation struct {
//	    Credential string
//	    ProjectID  id.ProjectID
//	    Request    api.ChatCompletionRequest
//	    Permission authorize.ExecutionPermission
//	}
//
//	func New(Config, Dependencies, ...Option) (*Gateway, error)
//	func (*Gateway) Invoke(context.Context, Invocation) (api.ChatCompletionResponse, error)
//
// Behavior pinned by these tests (S05 §1, S09 §5, S10 §5, the model.invoke
// registry row):
//
//   - A disabled gateway constructs with no dependencies and performs no
//     dependency initialization; Invoke refuses with not_found and calls
//     nothing. An enabled gateway refuses an incomplete composition
//     (internal_failure), a missing execution reader option (invalid_input) and
//     an invalid configuration (invalid_input).
//   - Invoke validates the declared non-streaming subset, resolves the
//     presented credential currently, derives the target scope as {resolved
//     principal tenant, Invocation.ProjectID}, authorizes model.invoke (the
//     current admitted execution read for execution principals, Authorizer for
//     human/service principals), matches a ModelConfig whose scope is exactly
//     that project, re-reads the registry row's current authority facts
//     (membership for human/service; membership, execution_running and
//     action_deadline bound to the admitted execution/action for an execution
//     caller) and only then resolves
//     the one configured secret name and forwards. Every refusal before
//     forwarding makes zero upstream calls, zero secret resolves and zero usage
//     writes. A denied membership or execution_running fact is forbidden; a
//     denied action_deadline fact is deadline_expired; an unavailable secret is
//     dependency_unavailable.
//   - The forwarded body uses ModelConfig.UpstreamModel, an omitted max_tokens
//     becomes ModelConfig.MaxCompletionTokens, an explicit larger value is
//     refused, and the encoded request must fit ModelConfig.MaxRequestBytes.
//     The response is read within ModelConfig.MaxResponseBytes.
//   - Provider auth/rate-limit/outage failures, non-2xx responses, malformed
//     JSON, an invalid completion and event-stream bodies are redacted
//     dependency_unavailable. A lost response after headers (a body read
//     failure) is uncertain_external_effect. A transport error that is not
//     provably pre-write (a reset, an EOF waiting for headers, a GOAWAY after
//     the body, or no response at all) is also uncertain_external_effect,
//     because the provider may already have processed the call; only a
//     connection that could not be dialled is a definitive
//     dependency_unavailable. Caller/config/action-deadline cancellation is
//     deadline_expired. Every response body is closed and exactly one RoundTrip
//     happens per dispatched call: the gateway never replays an uncertain
//     attempt.
//   - Every dispatched call persists exactly one attributable usage row through
//     store.Executor on a settlement context that survives caller cancellation:
//     succeeded/known with the validated counts, failed/unknown for a definitive
//     provider or protocol refusal, uncertain/unknown for a lost response,
//     ambiguous transport failure or cancellation. A dispatched call whose row
//     cannot be persisted is reported as uncertain_external_effect, never as a
//     replay-safe internal_failure and never as a silent success.
//     UsageRecord.Unit() writes one insert into UsageTable with
//     principal_id, execution_id, model, upstream_model, outcome, usage_known,
//     prompt_tokens, completion_tokens, total_tokens, recorded_at and version=1
//     (integers are int64, recorded_at is time.Time), and carries no receipt,
//     lock or event because model.invoke has receipt policy none. An empty
//     ExecutionID is written as SQL NULL (a present nil execution_id), never
//     as an empty string; a set ExecutionID is written as its text.
//   - Raw request/response bytes reach ContentRecorder only when the configured
//     ModelConfig opts in, with ContentRetention or the seven-day default.
//   - Provider credentials and the caller credential never appear in errors,
//     usage rows, content records or the returned response. Only the configured
//     secret name is resolved, and every call resolves it anew so a rotation
//     takes effect without a restart.
package gateway_test

import (
	"context"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/gateway"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// TestNewDisabledGatewayNeedsNoDependencies pins the optional-capability
// property: a disabled gateway is constructed without any dependency and never
// reaches a secret, authorizer or provider seam.
func TestNewDisabledGatewayNeedsNoDependencies(t *testing.T) {
	instance, err := gateway.New(gateway.Config{}, gateway.Dependencies{})
	if err != nil {
		t.Fatalf("New(disabled): %v", err)
	}
	if instance == nil {
		t.Fatal("New(disabled) returned a nil gateway")
	}
	_, err = instance.Invoke(context.Background(), invocation())
	requireClass(t, err, fault.NotFound)
}

// TestNewRejectsIncompleteComposition pins that an enabled gateway refuses a
// partially wired composition instead of guessing a substitute.
func TestNewRejectsIncompleteComposition(t *testing.T) {
	full := newFakes().dependencies()
	cases := []struct {
		name   string
		mutate func(*gateway.Dependencies)
	}{
		{"no dependencies at all", func(d *gateway.Dependencies) { *d = gateway.Dependencies{} }},
		{"no principal resolver", func(d *gateway.Dependencies) { d.Principals = nil }},
		{"no authorizer", func(d *gateway.Dependencies) { d.Authorizer = nil }},
		{"no authority reader", func(d *gateway.Dependencies) { d.Authority = nil }},
		{"no secret resolver", func(d *gateway.Dependencies) { d.Secrets = nil }},
		{"no store", func(d *gateway.Dependencies) { d.Store = nil }},
		{"no content recorder", func(d *gateway.Dependencies) { d.Content = nil }},
		{"no transport", func(d *gateway.Dependencies) { d.Transport = nil }},
		{"no clock", func(d *gateway.Dependencies) { d.Clock = nil }},
		{"no usage identity source", func(d *gateway.Dependencies) { d.NewID = nil }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			dependencies := full
			testCase.mutate(&dependencies)
			_, err := gateway.New(validConfig(), dependencies)
			requireClass(t, err, fault.InternalFailure)
		})
	}
}

// TestNewRejectsMissingExecutionReader pins that the execution-authority seam
// is required for an enabled gateway even though it is injected outside the
// declared Dependencies record: a missing or nil option is invalid_input, never
// a silently disabled execution path.
func TestNewRejectsMissingExecutionReader(t *testing.T) {
	if _, err := gateway.New(validConfig(), newFakes().dependencies()); err == nil {
		t.Fatal("New accepted an enabled gateway with no execution reader option")
	} else {
		requireClass(t, err, fault.InvalidInput)
	}
	if _, err := gateway.New(validConfig(), newFakes().dependencies(), gateway.WithExecutionReader(nil)); err == nil {
		t.Fatal("New accepted a nil execution reader option")
	} else {
		requireClass(t, err, fault.InvalidInput)
	}
}

// TestNewRejectsInvalidConfiguration pins the finite configuration contract:
// every configured model names a project scope, an upstream model, a provider
// endpoint, exactly one secret reference, positive bounded request/response
// sizes, a positive completion cap and a finite timeout.
func TestNewRejectsInvalidConfiguration(t *testing.T) {
	cases := []struct {
		name   string
		config gateway.Config
	}{
		{"enabled without configured models", gateway.Config{Enabled: true}},
		{"empty configured model name", gateway.Config{Enabled: true, Models: []gateway.ModelConfig{modelWith(func(m *gateway.ModelConfig) { m.Model = "" })}}},
		{"empty upstream model", gateway.Config{Enabled: true, Models: []gateway.ModelConfig{modelWith(func(m *gateway.ModelConfig) { m.UpstreamModel = "" })}}},
		{"empty endpoint", gateway.Config{Enabled: true, Models: []gateway.ModelConfig{modelWith(func(m *gateway.ModelConfig) { m.Endpoint = "" })}}},
		{"non-http endpoint", gateway.Config{Enabled: true, Models: []gateway.ModelConfig{modelWith(func(m *gateway.ModelConfig) { m.Endpoint = "ftp://models.example.test/v1" })}}},
		{"missing secret reference", gateway.Config{Enabled: true, Models: []gateway.ModelConfig{modelWith(func(m *gateway.ModelConfig) { m.SecretName = "" })}}},
		{"invalid secret reference", gateway.Config{Enabled: true, Models: []gateway.ModelConfig{modelWith(func(m *gateway.ModelConfig) { m.SecretName = id.SecretName("../key") })}}},
		{"lowercase secret reference", gateway.Config{Enabled: true, Models: []gateway.ModelConfig{modelWith(func(m *gateway.ModelConfig) { m.SecretName = id.SecretName("model-api-key") })}}},
		{"reserved secret reference", gateway.Config{Enabled: true, Models: []gateway.ModelConfig{modelWith(func(m *gateway.ModelConfig) { m.SecretName = id.SecretName(id.SecretTokenName) })}}},
		{"tenant-wide model scope", gateway.Config{Enabled: true, Models: []gateway.ModelConfig{modelWith(func(m *gateway.ModelConfig) { m.Scope = tenancy.Scope{TenantID: testTenant} })}}},
		{"invalid scope", gateway.Config{Enabled: true, Models: []gateway.ModelConfig{modelWith(func(m *gateway.ModelConfig) { m.Scope = tenancy.Scope{} })}}},
		{"zero request bound", gateway.Config{Enabled: true, Models: []gateway.ModelConfig{modelWith(func(m *gateway.ModelConfig) { m.MaxRequestBytes = 0 })}}},
		{"request bound above the declared bound", gateway.Config{Enabled: true, Models: []gateway.ModelConfig{modelWith(func(m *gateway.ModelConfig) { m.MaxRequestBytes = gateway.DefaultBounds.MaxRequestBytes + 1 })}}},
		{"zero response bound", gateway.Config{Enabled: true, Models: []gateway.ModelConfig{modelWith(func(m *gateway.ModelConfig) { m.MaxResponseBytes = 0 })}}},
		{"response bound above the declared bound", gateway.Config{Enabled: true, Models: []gateway.ModelConfig{modelWith(func(m *gateway.ModelConfig) { m.MaxResponseBytes = gateway.DefaultBounds.MaxResponseBytes + 1 })}}},
		{"zero completion cap", gateway.Config{Enabled: true, Models: []gateway.ModelConfig{modelWith(func(m *gateway.ModelConfig) { m.MaxCompletionTokens = 0 })}}},
		{"completion cap above the declared subset", gateway.Config{Enabled: true, Models: []gateway.ModelConfig{modelWith(func(m *gateway.ModelConfig) { m.MaxCompletionTokens = api.MaxCompletionTokens + 1 })}}},
		{"zero timeout", gateway.Config{Enabled: true, Models: []gateway.ModelConfig{modelWith(func(m *gateway.ModelConfig) { m.Timeout = 0 })}}},
		{"timeout above the declared maximum", gateway.Config{Enabled: true, Models: []gateway.ModelConfig{modelWith(func(m *gateway.ModelConfig) { m.Timeout = gateway.MaxTimeout + time.Second })}}},
		{"negative content retention", gateway.Config{Enabled: true, Models: []gateway.ModelConfig{modelWith(func(m *gateway.ModelConfig) { m.ContentRetention = -time.Hour })}}},
		{"duplicate model in one scope", gateway.Config{Enabled: true, Models: []gateway.ModelConfig{
			validModelConfig(),
			modelWith(func(m *gateway.ModelConfig) { m.UpstreamModel = "another-upstream" }),
		}}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fake := newFakes()
			_, err := gateway.New(testCase.config, fake.dependencies(), gateway.WithExecutionReader(fake.executions))
			requireClass(t, err, fault.InvalidInput)
		})
	}
}

// TestUsageRecordUnitCarriesAttribution pins the one usage commit unit: exact
// attribution columns, no receipt/lock/event, and honest unknown counts.
func TestUsageRecordUnitCarriesAttribution(t *testing.T) {
	known := gateway.UsageRecord{
		ID:            gateway.UsageID("usage-1"),
		Scope:         testScope(),
		PrincipalID:   testPrincipal,
		ExecutionID:   testExecution,
		Model:         testModel,
		UpstreamModel: testUpstream,
		Outcome:       gateway.UsageSucceeded,
		UsageKnown:    true,
		Usage:         api.ChatUsage{PromptTokens: 7, CompletionTokens: 5, TotalTokens: 12},
		At:            fixedNow,
	}
	unit, err := known.Unit()
	if err != nil {
		t.Fatalf("UsageRecord.Unit: %v", err)
	}
	if unit.Receipt != nil {
		t.Error("usage unit carries a receipt; model.invoke has receipt policy none")
	}
	if len(unit.Locks) != 0 {
		t.Errorf("usage unit locks = %d; want 0", len(unit.Locks))
	}
	if len(unit.Events) != 0 {
		t.Errorf("usage unit events = %d; want 0", len(unit.Events))
	}
	write := singleInsert(t, unit)
	if write.Table != gateway.UsageTable {
		t.Errorf("usage table = %q; want %q", write.Table, gateway.UsageTable)
	}
	if write.ID != string(known.ID) {
		t.Errorf("usage row id = %q; want %q", write.ID, known.ID)
	}
	values := write.Values
	if values["principal_id"] != string(testPrincipal) {
		t.Errorf("principal_id = %v; want %q", values["principal_id"], testPrincipal)
	}
	if values["execution_id"] != string(testExecution) {
		t.Errorf("execution_id = %v; want %q", values["execution_id"], testExecution)
	}
	if values["model"] != testModel {
		t.Errorf("model = %v; want %q", values["model"], testModel)
	}
	if values["upstream_model"] != testUpstream {
		t.Errorf("upstream_model = %v; want %q", values["upstream_model"], testUpstream)
	}
	if values["outcome"] != string(gateway.UsageSucceeded) {
		t.Errorf("outcome = %v; want %q", values["outcome"], gateway.UsageSucceeded)
	}
	if values["usage_known"] != true {
		t.Errorf("usage_known = %v; want true", values["usage_known"])
	}
	if values["prompt_tokens"] != int64(7) || values["completion_tokens"] != int64(5) || values["total_tokens"] != int64(12) {
		t.Errorf("token counts = %v/%v/%v; want 7/5/12 as int64",
			values["prompt_tokens"], values["completion_tokens"], values["total_tokens"])
	}
	if recorded, ok := values["recorded_at"].(time.Time); !ok || !recorded.Equal(fixedNow) {
		t.Errorf("recorded_at = %v; want %v", values["recorded_at"], fixedNow)
	}
	if values["version"] != uint64(1) {
		t.Errorf("version = %v; want uint64(1)", values["version"])
	}

	unknown := known
	unknown.ID = gateway.UsageID("usage-2")
	unknown.Outcome = gateway.UsageUncertain
	unknown.UsageKnown = false
	unknown.Usage = api.ChatUsage{}
	unit, err = unknown.Unit()
	if err != nil {
		t.Fatalf("UsageRecord.Unit(unknown): %v", err)
	}
	values = singleInsert(t, unit).Values
	if values["outcome"] != string(gateway.UsageUncertain) {
		t.Errorf("unknown outcome = %v; want %q", values["outcome"], gateway.UsageUncertain)
	}
	if values["usage_known"] != false {
		t.Errorf("unknown usage_known = %v; want false", values["usage_known"])
	}
	for _, column := range []string{"prompt_tokens", "completion_tokens", "total_tokens"} {
		if values[column] != int64(0) {
			t.Errorf("unknown %s = %v; want an honest int64(0)", column, values[column])
		}
	}
}

// TestUsageRecordUnitWritesSQLNullForAbsentExecution pins the declared builder
// itself, with no gateway in between: a record without an execution identity
// carries a present nil execution_id (SQL NULL, so the nullable foreign key is
// not evaluated), and a record with one carries it as text.
func TestUsageRecordUnitWritesSQLNullForAbsentExecution(t *testing.T) {
	record := gateway.UsageRecord{
		ID:            gateway.UsageID("usage-1"),
		Scope:         testScope(),
		PrincipalID:   testPrincipal,
		Model:         testModel,
		UpstreamModel: testUpstream,
		Outcome:       gateway.UsageSucceeded,
		UsageKnown:    true,
		Usage:         api.ChatUsage{PromptTokens: 7, CompletionTokens: 5, TotalTokens: 12},
		At:            fixedNow,
	}
	unit, err := record.Unit()
	if err != nil {
		t.Fatalf("UsageRecord.Unit(no execution): %v", err)
	}
	assertUnitConformsToSchema(t, unit, gateway.UsageTable)
	value, present := singleInsert(t, unit).Values["execution_id"]
	if !present {
		t.Fatal("usage unit omits execution_id; the nullable column must be present as NULL")
	}
	if value != nil {
		t.Errorf("execution_id = %#v (%T); want an untyped nil for a record without an execution", value, value)
	}

	record.ExecutionID = testExecution
	unit, err = record.Unit()
	if err != nil {
		t.Fatalf("UsageRecord.Unit(execution): %v", err)
	}
	assertUnitConformsToSchema(t, unit, gateway.UsageTable)
	value, present = singleInsert(t, unit).Values["execution_id"]
	if text, ok := value.(string); !present || !ok || text != string(testExecution) {
		t.Errorf("execution_id = %#v (%T, present=%v); want the text %q", value, value, present, testExecution)
	}
}

// TestUsageRecordUnitRejectsUnattributableRecords pins that an incomplete or
// dishonest usage record cannot become a row.
func TestUsageRecordUnitRejectsUnattributableRecords(t *testing.T) {
	valid := gateway.UsageRecord{
		ID:            gateway.UsageID("usage-1"),
		Scope:         testScope(),
		PrincipalID:   testPrincipal,
		ExecutionID:   testExecution,
		Model:         testModel,
		UpstreamModel: testUpstream,
		Outcome:       gateway.UsageSucceeded,
		UsageKnown:    true,
		Usage:         api.ChatUsage{PromptTokens: 7, CompletionTokens: 5, TotalTokens: 12},
		At:            fixedNow,
	}
	cases := []struct {
		name   string
		mutate func(*gateway.UsageRecord)
	}{
		{"empty id", func(r *gateway.UsageRecord) { r.ID = "" }},
		{"tenant-wide scope", func(r *gateway.UsageRecord) { r.Scope = tenancy.Scope{TenantID: testTenant} }},
		{"invalid scope", func(r *gateway.UsageRecord) { r.Scope = tenancy.Scope{} }},
		{"empty principal", func(r *gateway.UsageRecord) { r.PrincipalID = "" }},
		{"empty model", func(r *gateway.UsageRecord) { r.Model = "" }},
		{"empty upstream model", func(r *gateway.UsageRecord) { r.UpstreamModel = "" }},
		{"unknown outcome", func(r *gateway.UsageRecord) { r.Outcome = gateway.UsageOutcome("maybe") }},
		{"known usage with inconsistent counts", func(r *gateway.UsageRecord) {
			r.Usage = api.ChatUsage{PromptTokens: 3, CompletionTokens: 4, TotalTokens: 9}
		}},
		{"unknown usage with fabricated counts", func(r *gateway.UsageRecord) {
			r.UsageKnown = false
			r.Usage = api.ChatUsage{PromptTokens: 3, CompletionTokens: 4, TotalTokens: 7}
		}},
		{"zero recorded time", func(r *gateway.UsageRecord) { r.At = time.Time{} }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			record := valid
			testCase.mutate(&record)
			if _, err := record.Unit(); err == nil {
				t.Fatal("UsageRecord.Unit accepted an unattributable record")
			} else {
				requireClass(t, err, fault.InvalidInput)
			}
		})
	}
}
