package opsapp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	opsapp "github.com/optimiware/kodama/internal/shell/app/ops"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/execution"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/inbox"
	"github.com/optimiware/kodama/internal/shell/log"
	"github.com/optimiware/kodama/internal/shell/mcpproxy"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	shellrequest "github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/resource"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/store/memory"
	storeexecution "github.com/optimiware/kodama/internal/shell/store/units/execution"
	storeidentity "github.com/optimiware/kodama/internal/shell/store/units/identity"
	"github.com/optimiware/kodama/internal/shell/store/units/platform"
	"github.com/optimiware/kodama/internal/shell/store/units/readers"
	"github.com/optimiware/kodama/internal/shell/store/units/work"
	"github.com/optimiware/kodama/internal/shell/task"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	tenancyadmin "github.com/optimiware/kodama/internal/shell/tenancy/admin"
	"github.com/optimiware/kodama/internal/shell/token"
	"github.com/optimiware/kodama/internal/shell/transport/ops"
	"github.com/optimiware/kodama/internal/shell/verify"
)

const (
	testTenant   = "tenant-a"
	testProject  = "project-a"
	testAdmin    = "admin-1"
	testEngineer = "engineer-1"
	testService  = "service-1"
	testToken    = "token-engineer"
	testNow      = "2026-11-04T12:00:00Z"
)

var fixedNow = func() time.Time {
	parsed, err := time.Parse(time.RFC3339, testNow)
	if err != nil {
		panic(err)
	}
	return parsed
}()

// fixedClock is the injected authoritative time.
type fixedClock struct{}

func (fixedClock) Now() time.Time { return fixedNow }

// sequenceIDs is the one deterministic identity source shared by every service
// and the handlers.
type sequenceIDs struct {
	mu        sync.Mutex
	next      int
	questions int
}

func (source *sequenceIDs) bump(prefix string) string {
	source.next++
	return fmt.Sprintf("%s-%d", prefix, source.next)
}

func (source *sequenceIDs) NewEventID() (id.EventID, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	return id.EventID(source.bump("event")), nil
}

func (source *sequenceIDs) NewReceiptID() (id.ReceiptID, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	return id.ReceiptID(source.bump("receipt")), nil
}

func (source *sequenceIDs) NewRowID() (string, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	return source.bump("membership"), nil
}

func (source *sequenceIDs) NewTaskID() (id.TaskID, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	return id.TaskID(source.bump("task")), nil
}

func (source *sequenceIDs) NewQuestionID() (task.QuestionID, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	source.questions++
	return task.QuestionID(fmt.Sprintf("question-%d", source.questions)), nil
}

func (source *sequenceIDs) NewExecutionID() (id.ExecutionID, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	return id.ExecutionID(source.bump("execution")), nil
}

func (source *sequenceIDs) NewPrincipalID() (id.PrincipalID, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	return id.PrincipalID(source.bump("principal")), nil
}

func (source *sequenceIDs) NewSecretID() (secret.SecretID, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	return secret.SecretID(source.bump("secret")), nil
}

func (source *sequenceIDs) NewVerificationID() (verify.VerificationID, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	return verify.VerificationID(source.bump("verification")), nil
}

// staticPrincipals resolves the configured credential to one principal.
type staticPrincipals struct {
	principals map[string]identity.Principal
}

func (resolver staticPrincipals) Resolve(_ context.Context, credential string) (identity.Principal, error) {
	principal, ok := resolver.principals[credential]
	if !ok {
		return identity.Principal{}, fmt.Errorf("unknown credential")
	}
	return principal, nil
}

// staticActorActions is the default actor-action seam. It resolves every
// credential to no execution action, the human/service shape; the execution
// shape is exercised by verify_action_test.go.
type staticActorActions struct{}

func (staticActorActions) ResolveActorAction(context.Context, string) (id.ActionID, error) {
	return "", nil
}

// fixedKeys mints deterministic encrypted tenant keys. It is the KMS boundary
// fake: no plaintext is ever produced.
type fixedKeys struct{}

func (fixedKeys) NewTenantKey(_ context.Context, tenantID id.TenantID, version uint64) (token.TenantKey, error) {
	return token.TenantKey{
		TenantID:   tenantID,
		Version:    version,
		Ciphertext: []byte("ciphertext"),
		WrappedKey: bytes.Repeat([]byte{1}, secret.WrappedKeyBytes),
		WrapNonce:  bytes.Repeat([]byte{2}, secret.WrapNonceBytes),
		KeyRef:     "kms/tenant-key",
		KeyVersion: 1,
	}, nil
}

// recordingKeys is fixedKeys with an observation counter. A refusal that must
// happen before any handler runs can assert that no tenant key was minted.
type recordingKeys struct {
	mu    sync.Mutex
	calls int
}

func (keys *recordingKeys) NewTenantKey(ctx context.Context, tenantID id.TenantID, version uint64) (token.TenantKey, error) {
	keys.mu.Lock()
	keys.calls++
	keys.mu.Unlock()
	return fixedKeys{}.NewTenantKey(ctx, tenantID, version)
}

// callsMade reports how many tenant keys were minted.
func (keys *recordingKeys) callsMade() int {
	keys.mu.Lock()
	defer keys.mu.Unlock()
	return keys.calls
}

// derivationResolver unwraps every tenant key to deterministic plaintext.
type derivationResolver struct{}

func (derivationResolver) ResolveDerivationKey(context.Context, token.TenantKey) ([]byte, error) {
	return bytes.Repeat([]byte{0x2a}, 32), nil
}

// recordingDeliverer accepts every credential delivery and keeps nothing.
type recordingDeliverer struct{}

func (deliverer *recordingDeliverer) Deliver(context.Context, token.Delivery) error {
	return nil
}

// recordingTokenInvalidator accepts every credential invalidation and keeps
// nothing.
type recordingTokenInvalidator struct{}

func (invalidator *recordingTokenInvalidator) Invalidate(context.Context, token.Invalidation) error {
	return nil
}

// recordingServiceInvalidator accepts every service invalidation and keeps
// nothing.
type recordingServiceInvalidator struct{}

func (invalidator *recordingServiceInvalidator) InvalidateService(context.Context, service.ServiceInvalidation) error {
	return nil
}

// sealedCipher is the envelope boundary fake. It is structurally valid and
// deterministic, not real cryptography.
type sealedCipher struct{}

func (sealedCipher) Seal(_ context.Context, associated secret.AssociatedData, plaintext []byte) (secret.Envelope, error) {
	return secret.Envelope{
		Suite:      secret.SuiteAES256GCM,
		KeyRef:     secret.KeyRef(fmt.Sprintf("kms/%d", associated.KeyVersion)),
		KeyVersion: associated.KeyVersion,
		Nonce:      bytes.Repeat([]byte{3}, secret.ContentNonceBytes),
		Ciphertext: append([]byte(nil), plaintext...),
		WrappedKey: bytes.Repeat([]byte{4}, secret.WrappedKeyBytes),
		WrapNonce:  bytes.Repeat([]byte{5}, secret.WrapNonceBytes),
	}, nil
}

func (sealedCipher) Open(_ context.Context, _ secret.AssociatedData, envelope secret.Envelope) ([]byte, error) {
	return append([]byte(nil), envelope.Ciphertext...), nil
}

// imageResolver answers every image with one pinned digest.
type imageResolver struct{}

func (imageResolver) Resolve(_ context.Context, request pipeline.ResolveRequest) (pipeline.ResolvedImage, error) {
	return pipeline.ResolvedImage{
		Reference: request.Reference,
		Digest:    "sha256:" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}, nil
}

// executionFacts is the router execution-authority reader. It reports no
// current execution unless a test installs one.
type executionFacts struct {
	mu   sync.Mutex
	byID map[id.ExecutionID]authorize.AdmittedExecution
}

func (reader *executionFacts) ReadExecution(_ context.Context, executionID id.ExecutionID) (authorize.AdmittedExecution, error) {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	admitted, ok := reader.byID[executionID]
	if !ok {
		return authorize.AdmittedExecution{}, fmt.Errorf("execution not found")
	}
	return admitted, nil
}

// modelRecorder records the principal-bound model invocations.
type modelRecorder struct{ calls int }

func (recorder *modelRecorder) Invoke(
	_ context.Context,
	principal authorize.Principal,
	_ id.ProjectID,
	request api.ChatCompletionRequest,
) (api.ChatCompletionResponse, error) {
	recorder.calls++
	return api.ChatCompletionResponse{
		ID:    "completion-1",
		Model: request.Model,
		Choices: []api.ChatChoice{{
			Index:        0,
			Message:      api.ChatMessage{Role: "assistant", Content: "ok"},
			FinishReason: "stop",
		}},
		Usage: api.ChatUsage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
	}, nil
}

// toolRecorder records the principal-bound tool calls.
type toolRecorder struct{ calls int }

func (recorder *toolRecorder) Call(
	_ context.Context,
	_ authorize.Principal,
	_ id.ProjectID,
	tool string,
	_ json.RawMessage,
) (mcpproxy.CallResult, error) {
	recorder.calls++
	return mcpproxy.CallResult{Content: json.RawMessage(`"ok"`)}, nil
}

// harness wires the real capability services over one memory store.
type harness struct {
	t       *testing.T
	store   *memory.Store
	ids     *sequenceIDs
	adapter *storeidentity.Adapter
	tasks   *work.Task
	inbox   *work.Inbox
	pipes   *work.Pipelines
	execs   *storeexecution.Store

	tenancy     *tenancyadmin.Service
	bindings    *resource.Service
	management  *pipeline.Management
	admission   *pipeline.AdmissionService
	executions  *execution.Service
	secrets     *secret.Manager
	services    *service.Admin
	credentials *token.CredentialAdmin
	taskService *task.Service
	inboxQuery  *inbox.Service
	requests    *shellrequest.Service
	verifier    *verify.Verifier
	reporter    *verify.Reporter
	ingress     *event.Ingress
	eventRead   *event.Reader
	subscribe   *event.Subscriber

	principals staticPrincipals
	facts      *executionFacts
	models     *modelRecorder
	tools      *toolRecorder

	ingressConfig event.IngressConfig

	deps          opsapp.Dependencies
	keys          *recordingKeys
	bindingsTable []ops.Binding
	ownership     *opsapp.Ownership
	router        *ops.Router

	// admissionSeam and executionSeam optionally wrap the real capability
	// services so a test can observe the exact request the handler built.
	admissionSeam opsapp.PipelineAdmission
	executionSeam opsapp.ExecutionOperations

	// actorActions and verifierSeam optionally replace the composed
	// actor-action resolver and verifier so a test can observe the exact
	// attempt the actor.verify handler builds.
	actorActions opsapp.ActorAction
	verifierSeam opsapp.Verification
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	instance := memory.NewWithClock(func() time.Time { return fixedNow })
	clock := fixedClock{}
	ids := &sequenceIDs{}

	adapter, err := storeidentity.New(storeidentity.Config{
		Executor: instance,
		Scanner:  instance,
		Clock:    clock,
		IDs:      ids,
	})
	if err != nil {
		t.Fatalf("identity.New: %v", err)
	}
	taskAdapter, err := work.NewTask(instance, instance)
	if err != nil {
		t.Fatalf("work.NewTask: %v", err)
	}
	inboxAdapter, err := work.NewInbox(instance, instance)
	if err != nil {
		t.Fatalf("work.NewInbox: %v", err)
	}
	pipes, err := work.NewPipelines(instance, instance, func() (pipeline.PipelineID, error) {
		return pipeline.PipelineID(ids.bump("pipeline")), nil
	})
	if err != nil {
		t.Fatalf("work.NewPipelines: %v", err)
	}
	executions := storeexecution.New(instance, instance)

	tenancyService, err := tenancyadmin.NewService(tenancyadmin.Config{
		Clock:             clock,
		IDs:               ids,
		TenantCreate:      adapter,
		TenantKeyRotation: adapter,
		ProjectCreate:     adapter,
		ProjectUpdate:     adapter,
		MembershipGrant:   adapter,
		MembershipRevoke:  adapter,
		MembershipQuery:   adapter,
	})
	if err != nil {
		t.Fatalf("tenancyadmin.NewService: %v", err)
	}
	bindingService, err := resource.NewService(resource.Config{
		Clock:    clock,
		IDs:      ids,
		Bindings: adapter,
		Deletes:  adapter,
		Reads:    adapter,
		Queries:  adapter,
	})
	if err != nil {
		t.Fatalf("resource.NewService: %v", err)
	}
	management, err := pipeline.NewManagement(pipeline.ManagementConfig{
		Clock:      clock,
		IDs:        ids,
		Authorizer: authorize.NewMatrix(),
		Authority:  adapter,
		Changes:    pipes,
		Reader:     pipes,
	})
	if err != nil {
		t.Fatalf("pipeline.NewManagement: %v", err)
	}
	admission, err := pipeline.NewAdmissionService(pipeline.AdmissionConfig{
		Resolver: imageResolver{},
		Reader:   pipes,
	})
	if err != nil {
		t.Fatalf("pipeline.NewAdmissionService: %v", err)
	}
	executionService, err := execution.NewService(execution.Config{
		Clock:       clock,
		IDs:         ids,
		Authorizer:  authorize.NewMatrix(),
		Authority:   adapter,
		Reader:      executions,
		State:       executions,
		Transitions: executions,
		Starts:      executions,
	})
	if err != nil {
		t.Fatalf("execution.NewService: %v", err)
	}
	secretManager, err := secret.NewManager(secret.ManagerConfig{
		InstallationID: "install-1",
		KeyVersion:     1,
		Clock:          clock,
		IDs:            ids,
		Executor:       instance,
		Scanner:        instance,
		Cipher:         sealedCipher{},
		Lookup:         mustSecretLookup(t, instance),
	})
	if err != nil {
		t.Fatalf("secret.NewManager: %v", err)
	}
	serviceAdmin, err := service.NewAdmin(service.AdminConfig{
		Authorizer:    authorize.NewMatrix(),
		Authority:     adapter,
		Principals:    adapter,
		Services:      adapter,
		Port:          adapter,
		Invalidations: &recordingServiceInvalidator{},
		Clock:         clock,
		IDs:           ids,
	})
	if err != nil {
		t.Fatalf("service.NewAdmin: %v", err)
	}
	credentialManager, err := token.NewManager(token.ManagerConfig{
		Keys:          adapter,
		Resolver:      derivationResolver{},
		Credentials:   adapter,
		Principals:    adapter,
		Services:      adapter,
		Mutations:     adapter,
		Receipts:      mustReceiptLookup(t, instance),
		Deliverer:     &recordingDeliverer{},
		Invalidations: &recordingTokenInvalidator{},
		Clock:         clock,
		Random:        bytes.NewReader(bytes.Repeat([]byte{7}, 4096)),
		Bounds:        token.DefaultBounds,
	})
	if err != nil {
		t.Fatalf("token.NewManager: %v", err)
	}
	credentialAdmin, err := token.NewCredentialAdmin(token.CredentialAdminConfig{
		Authorizer:  authorize.NewMatrix(),
		Authority:   adapter,
		Principals:  adapter,
		Services:    adapter,
		Credentials: adapter,
		Manager:     credentialManager,
		Receipts:    mustReceiptLookup(t, instance),
		Clock:       clock,
		IDs:         ids,
	})
	if err != nil {
		t.Fatalf("token.NewCredentialAdmin: %v", err)
	}
	taskService, err := task.NewService(task.ServiceConfig{
		Clock:          clock,
		IDs:            ids,
		Authorizer:     authorize.NewMatrix(),
		Authority:      adapter,
		Principals:     adapter,
		Pipelines:      pipes,
		Reader:         taskAdapter,
		Executions:     mustTaskExecutionReader(t, instance),
		Aggregates:     executions,
		Admission:      admission,
		Creates:        taskAdapter,
		Edits:          taskAdapter,
		Starts:         taskAdapter,
		Asks:           taskAdapter,
		Replies:        taskAdapter,
		Cancels:        taskAdapter,
		Progress:       taskAdapter,
		DefaultTimeout: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("task.NewService: %v", err)
	}
	inboxService, err := inbox.NewService(inbox.ServiceConfig{
		Authorizer: authorize.NewMatrix(),
		Authority:  adapter,
		Principals: adapter,
		Queries:    inboxAdapter,
	})
	if err != nil {
		t.Fatalf("inbox.NewService: %v", err)
	}
	requestService, err := shellrequest.NewService(shellrequest.Config{
		Clock:       clock,
		IDs:         ids,
		Authorizer:  authorize.NewMatrix(),
		Authority:   adapter,
		State:       executions,
		Activations: executions,
		Resolutions: executions,
	})
	if err != nil {
		t.Fatalf("request.NewService: %v", err)
	}
	bindingReader, err := readers.NewBindingReader(instance)
	if err != nil {
		t.Fatalf("readers.NewBindingReader: %v", err)
	}
	verifierPrincipals := staticPrincipals{principals: map[string]identity.Principal{
		"actor-engineer": {
			ID:      testEngineer,
			Kind:    identity.KindHuman,
			Scope:   tenancy.Scope{TenantID: testTenant, ProjectID: testProject},
			Roles:   []tenancy.Role{tenancy.RoleAdmin, tenancy.RoleEngineer, tenancy.RoleOperator, tenancy.RoleReviewer, tenancy.RoleViewer},
			Enabled: true,
			Version: 1,
		},
	}}
	verifier, err := verify.New(verify.Config{
		Principals: verifierPrincipals,
		Bindings:   bindingReader,
		Evidence:   mustEvidenceReader(t, instance),
		Records:    mustRecordAdapter(t, instance),
		Clock:      clock,
		NewID:      func() (verify.VerificationID, error) { return ids.NewVerificationID() },
	})
	if err != nil {
		t.Fatalf("verify.New: %v", err)
	}
	reporter, err := verify.NewReporter(verify.ReportConfig{
		Authorizer:    authorize.NewMatrix(),
		Authority:     adapter,
		Bindings:      bindingReader,
		Verifications: mustVerificationReader(t, instance),
		Reports:       mustReportAdapter(t, instance),
		Clock:         clock,
		IDs:           ids,
	})
	if err != nil {
		t.Fatalf("verify.NewReporter: %v", err)
	}
	ingressConfig := event.IngressConfig{
		Credentials:  staticPrincipals{},
		Authority:    adapter,
		Authorizer:   authorize.NewMatrix(),
		Port:         mustIngressAdapter(t, instance),
		Clock:        clock,
		NewEventID:   ids.NewEventID,
		NewReceiptID: ids.NewReceiptID,
	}
	ingress, err := event.NewIngress(ingressConfig)
	if err != nil {
		t.Fatalf("event.NewIngress: %v", err)
	}
	eventReader, err := event.NewReader(event.ReadConfig{
		Authorizer: authorize.NewMatrix(),
		Authority:  adapter,
		Principals: adapter,
		Port:       mustEventReader(t, instance),
	})
	if err != nil {
		t.Fatalf("event.NewReader: %v", err)
	}
	subscriber, err := event.NewSubscriber(event.SubscribeConfig{
		Authorizer: authorize.NewMatrix(),
		Authority:  adapter,
		Principals: adapter,
		Cursor:     mustCursorReader(t, instance),
		Validity:   fixedValidity{},
		Clock:      clock,
	})
	if err != nil {
		t.Fatalf("event.NewSubscriber: %v", err)
	}

	engineer := identity.Principal{
		ID:      testEngineer,
		Kind:    identity.KindHuman,
		Scope:   tenancy.Scope{TenantID: testTenant, ProjectID: testProject},
		Roles:   []tenancy.Role{tenancy.RoleAdmin, tenancy.RoleEngineer, tenancy.RoleOperator, tenancy.RoleReviewer, tenancy.RoleViewer},
		Enabled: true,
		Version: 1,
	}
	engineerB := identity.Principal{
		ID:      "engineer-b",
		Kind:    identity.KindHuman,
		Scope:   tenancy.Scope{TenantID: testTenant, ProjectID: "project-b"},
		Roles:   []tenancy.Role{tenancy.RoleAdmin, tenancy.RoleEngineer, tenancy.RoleOperator, tenancy.RoleReviewer, tenancy.RoleViewer},
		Enabled: true,
		Version: 1,
	}
	admin := identity.Principal{
		ID:      testAdmin,
		Kind:    identity.KindHuman,
		Scope:   tenancy.Scope{TenantID: testTenant},
		Roles:   []tenancy.Role{tenancy.RoleAdmin, tenancy.RoleEngineer, tenancy.RoleOperator, tenancy.RoleReviewer, tenancy.RoleViewer},
		Enabled: true,
		Version: 1,
	}
	h := &harness{
		t:       t,
		store:   instance,
		ids:     ids,
		adapter: adapter,
		tasks:   taskAdapter,
		inbox:   inboxAdapter,
		pipes:   pipes,
		execs:   executions,

		tenancy:       tenancyService,
		bindings:      bindingService,
		management:    management,
		admission:     admission,
		executions:    executionService,
		secrets:       secretManager,
		services:      serviceAdmin,
		credentials:   credentialAdmin,
		taskService:   taskService,
		inboxQuery:    inboxService,
		requests:      requestService,
		verifier:      verifier,
		actorActions:  staticActorActions{},
		reporter:      reporter,
		ingress:       ingress,
		ingressConfig: ingressConfig,
		eventRead:     eventReader,
		subscribe:     subscriber,

		principals: staticPrincipals{principals: map[string]identity.Principal{
			testToken:          engineer,
			"token-admin":      admin,
			"token-engineer-b": engineerB,
			"actor-engineer":   engineer,
		}},
		facts:  &executionFacts{byID: map[id.ExecutionID]authorize.AdmittedExecution{}},
		keys:   &recordingKeys{},
		models: &modelRecorder{},
		tools:  &toolRecorder{},
	}
	h.seedMembership(t, engineer.Scope, "membership-engineer", engineer.ID, 1, engineer.Roles...)
	h.seedMembership(t, admin.Scope, "membership-admin", admin.ID, 1, admin.Roles...)
	h.seedMembership(t, engineerB.Scope, "membership-engineer-b", engineerB.ID, 1, engineerB.Roles...)
	h.buildRouter(t)
	return h
}

// seedMembership seeds the bootstrap membership through the store seam. This is
// the only bootstrap path: no Admin exists before the first row.
func (h *harness) seedMembership(t *testing.T, scope tenancy.Scope, rowID string, principal id.PrincipalID, version uint64, roles ...tenancy.Role) {
	t.Helper()
	encoded, err := json.Marshal(roles)
	if err != nil {
		t.Fatalf("encode roles: %v", err)
	}
	if _, err := h.store.Execute(context.Background(), scope, store.Unit{
		Writes: []store.Write{{
			Table:  "memberships",
			ID:     rowID,
			Insert: true,
			Values: map[string]any{
				"principal_id": string(principal),
				"roles":        encoded,
				"version":      int64(version),
				"created_at":   fixedNow,
			},
		}},
	}); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
}

// buildRouter constructs the real binding table and router.
func (h *harness) buildRouter(t *testing.T) {
	t.Helper()
	ownership, err := opsapp.NewOwnership(h.facts, opsapp.RegistryPermissions{})
	if err != nil {
		t.Fatalf("NewOwnership: %v", err)
	}
	admission := opsapp.PipelineAdmission(h.admission)
	if h.admissionSeam != nil {
		admission = h.admissionSeam
	}
	executions := opsapp.ExecutionOperations(h.executions)
	if h.executionSeam != nil {
		executions = h.executionSeam
	}
	verifier := opsapp.Verification(h.verifier)
	if h.verifierSeam != nil {
		verifier = h.verifierSeam
	}
	actorActions := h.actorActions
	if actorActions == nil {
		actorActions = staticActorActions{}
	}
	deps := opsapp.Dependencies{
		Tenancy:        h.tenancy,
		Bindings:       h.bindings,
		Pipelines:      h.management,
		Admission:      admission,
		Executions:     executions,
		Receipts:       mustReceiptLookup(t, h.store),
		Secrets:        h.secrets,
		Authority:      h.adapter,
		Services:       h.services,
		Credentials:    h.credentials,
		Tasks:          h.taskService,
		Inbox:          h.inboxQuery,
		Requests:       h.requests,
		Logs:           noLogs{},
		Verifier:       verifier,
		ActorActions:   actorActions,
		Reports:        h.reporter,
		Events:         principalIngress{config: h.ingressConfig},
		EventRead:      h.eventRead,
		EventSubscribe: h.subscribe,
		Models:         h.models,
		Tools:          h.tools,
		IDs:            h.ids,
		TenantKeys:     h.keys,
		ActionTimeout:  time.Minute,
	}
	bindings, err := opsapp.Bindings(deps)
	if err != nil {
		t.Fatalf("Bindings: %v", err)
	}
	router, err := ops.NewRouter(ops.Config{
		Principals: h.principals,
		Scopes:     ownership,
		Executions: ownership,
		Bindings:   bindings,
	})
	if err != nil {
		t.Fatalf("ops.NewRouter: %v", err)
	}
	h.deps = deps
	h.bindingsTable = bindings
	h.ownership = ownership
	h.router = router
}

// invoke calls one operation through the real router with a credential.
func (h *harness) invoke(t *testing.T, credential string, operation string, input string, params map[string]string) (api.Result, error) {
	t.Helper()
	request := api.Request{Input: json.RawMessage(input)}
	if codec, ok := codecForOperation(operation); ok && codec.RequiresIdempotencyKey {
		request.IdempotencyKey = "key-" + operation
	}
	return h.router.Invoke(context.Background(), ops.Invocation{
		Operation:  authorize.OperationName(operation),
		Credential: credential,
		Request:    request,
		Params:     params,
	})
}

// invokeVersion calls one operation with an expected version.
func (h *harness) invokeVersion(t *testing.T, credential string, operation string, input string, version uint64, params map[string]string) (api.Result, error) {
	t.Helper()
	request := api.Request{Input: json.RawMessage(input), ExpectedVersion: &version}
	if codec, ok := codecForOperation(operation); ok && codec.RequiresIdempotencyKey {
		request.IdempotencyKey = "key-" + operation
	}
	return h.router.Invoke(context.Background(), ops.Invocation{
		Operation:  authorize.OperationName(operation),
		Credential: credential,
		Request:    request,
		Params:     params,
	})
}

// codecForOperation returns the public codec.
func codecForOperation(operation string) (api.OperationCodec, bool) {
	for _, codec := range api.OperationCodecs() {
		if codec.Operation == operation {
			return codec, true
		}
	}
	return api.OperationCodec{}, false
}

// noLogs is the retained-log boundary: the harness has no store-backed
// retained sink, so the log operation refuses as not_found in these tests.
type noLogs struct{}

func (noLogs) Read(context.Context, log.ReadCommand) (log.ReadPage, error) {
	return log.ReadPage{}, fault.New(fault.NotFound, "retained logs are not available")
}

// principalIngress binds the real ingress to an already authenticated principal
// for one call, because the router does not forward the credential.
type principalIngress struct {
	config event.IngressConfig
}

func (publisher principalIngress) Publish(
	ctx context.Context,
	principal authorize.Principal,
	attempt event.PublishAttempt,
) (event.IngressResult, error) {
	config := publisher.config
	config.Credentials = fixedPrincipal{principal: principal}
	ingress, err := event.NewIngress(config)
	if err != nil {
		return event.IngressResult{}, err
	}
	attempt.Credential = "authenticated"
	return ingress.Publish(ctx, attempt)
}

// fixedValidity is the transport-boundary session-expiry fact. The
// store-backed validity reader refuses humans by design; composition supplies
// the authenticated session expiry.
type fixedValidity struct{}

func (fixedValidity) ValidUntil(context.Context, identity.Principal) (time.Time, error) {
	return fixedNow.Add(time.Hour), nil
}

// fixedPrincipal resolves every presented credential to one principal.
type fixedPrincipal struct {
	principal authorize.Principal
}

func (fixed fixedPrincipal) Resolve(context.Context, string) (identity.Principal, error) {
	return fixed.principal, nil
}

func mustSecretLookup(t *testing.T, scanner store.Scanner) *platform.SecretLookup {
	t.Helper()
	lookup, err := platform.NewSecretLookup(scanner)
	if err != nil {
		t.Fatalf("platform.NewSecretLookup: %v", err)
	}
	return lookup
}

func mustEvidenceReader(t *testing.T, scanner store.Scanner) *platform.EvidenceReader {
	t.Helper()
	reader, err := platform.NewEvidenceReader(scanner)
	if err != nil {
		t.Fatalf("platform.NewEvidenceReader: %v", err)
	}
	return reader
}

func mustRecordAdapter(t *testing.T, instance *memory.Store) *platform.RecordAdapter {
	t.Helper()
	adapter, err := platform.NewRecordAdapter(instance, instance)
	if err != nil {
		t.Fatalf("platform.NewRecordAdapter: %v", err)
	}
	return adapter
}

func mustReportAdapter(t *testing.T, instance *memory.Store) *platform.ReportAdapter {
	t.Helper()
	adapter, err := platform.NewReportAdapter(instance, instance)
	if err != nil {
		t.Fatalf("platform.NewReportAdapter: %v", err)
	}
	return adapter
}

func mustIngressAdapter(t *testing.T, instance *memory.Store) *platform.IngressAdapter {
	t.Helper()
	adapter, err := platform.NewIngressAdapter(instance, instance)
	if err != nil {
		t.Fatalf("platform.NewIngressAdapter: %v", err)
	}
	return adapter
}

func mustEventReader(t *testing.T, instance *memory.Store) *platform.EventReader {
	t.Helper()
	reader, err := platform.NewEventReader(instance)
	if err != nil {
		t.Fatalf("platform.NewEventReader: %v", err)
	}
	return reader
}

func mustCursorReader(t *testing.T, instance *memory.Store) *readers.CursorReader {
	t.Helper()
	reader, err := readers.NewCursorReader(instance)
	if err != nil {
		t.Fatalf("readers.NewCursorReader: %v", err)
	}
	return reader
}

func mustReceiptLookup(t *testing.T, instance *memory.Store) *readers.ReceiptLookup {
	t.Helper()
	lookup, err := readers.NewReceiptLookup(instance)
	if err != nil {
		t.Fatalf("readers.NewReceiptLookup: %v", err)
	}
	return lookup
}

func mustVerificationReader(t *testing.T, instance *memory.Store) *readers.VerificationReader {
	t.Helper()
	reader, err := readers.NewVerificationReader(instance)
	if err != nil {
		t.Fatalf("readers.NewVerificationReader: %v", err)
	}
	return reader
}

func mustTaskExecutionReader(t *testing.T, instance *memory.Store) *readers.ExecutionReader {
	t.Helper()
	reader, err := readers.NewExecutionReader(instance, instance)
	if err != nil {
		t.Fatalf("readers.NewExecutionReader: %v", err)
	}
	return reader
}

// seedResource seeds one resource row through the store seam. Resource rows are
// created by the resource capability's own path in production; the binding
// ownership authority read requires the row to exist.
func (h *harness) seedResource(t *testing.T, scope tenancy.Scope, resourceID string, version uint64) {
	t.Helper()
	if _, err := h.store.Execute(context.Background(), scope, store.Unit{
		Writes: []store.Write{{
			Table:  "resources",
			ID:     resourceID,
			Insert: true,
			Values: map[string]any{
				"version":    int64(version),
				"created_at": fixedNow,
			},
		}},
	}); err != nil {
		t.Fatalf("seed resource: %v", err)
	}
}

// invokeKey calls one operation with an explicit idempotency key.
func (h *harness) invokeKey(t *testing.T, credential, operation, input, key string, params map[string]string) (api.Result, error) {
	t.Helper()
	return h.router.Invoke(context.Background(), ops.Invocation{
		Operation:  authorize.OperationName(operation),
		Credential: credential,
		Request:    api.Request{Input: json.RawMessage(input), IdempotencyKey: key},
		Params:     params,
	})
}
