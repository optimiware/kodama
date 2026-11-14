package app

// Activation materialization and development workload authentication.
//
// The Kernel commits one activation before it calls the runtime Prepare port.
// This composition wrapper resolves the declared secret names through the
// trusted workload name policy and the store-backed resolver, then delivers the
// committed execution credential directly into the same PrepareRequest. No
// runtime adapter call happens before both are decided, so a denied or missing
// secret, an unusable tenant key or a stale execution fails preparation instead
// of starting a workload with partial activation data.
//
// The initial credential delivery is the PrepareRequest itself. The token
// manager's post-commit delivery bridge is intercepted while a preparation is
// in flight (through a request-local context sink) and delegates unchanged for
// every other delivery, so an initial issue never calls RefreshCredential before
// the preparation exists. A still-current committed credential is reconstructed
// from its committed nonce/expiry and the tenant derivation key instead of
// minting a second token; that reconstruction is what makes consecutive
// containers and a controller restart reuse the same credential.
//
// Secret values and token plaintext exist only in the PrepareRequest handed to
// the adapter. Nothing here logs, publishes, receipts or renders either value.

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	kerneldefinition "github.com/optimiware/kodama/internal/kernel/definition"
	kernelengine "github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/shell/app/bridges"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/token"
)

// activationCredentialOperation is the receipt operation identity of one
// activation-time execution credential. It is not a public registry operation.
const activationCredentialOperation = "execution.credential.activate"

// Reserved LLM proxy convenience environment variables injected into container
// workloads when configured models/providers are enabled in the execution's
// scope.
const (
	EnvLLMOpenAIBaseURL    = "KODAMA_LLM_OPENAI_BASE_URL"
	EnvLLMAnthropicBaseURL = "KODAMA_LLM_ANTHROPIC_BASE_URL"
)

// activationMaterializer builds the secret values and the current execution
// credential of one committed activation.
type activationMaterializer struct {
	resolver    secret.Resolver
	lookup      store.CredentialLookup
	credentials token.CredentialReader
	tenantKeys  token.TenantKeyReader
	derivation  token.DerivationKeyResolver
	manager     *token.Manager
	controller  store.Controller
	ids         *bridges.IDs
	clock       Clock
	models      *ModelsConfig
	workloadURL string

	// issuedMu guards issuedVersions, the execution-row version produced by this
	// materializer's own credential commit. The preparation-failure rebase
	// consults it so only that one commit is moved onto the post-credential
	// revision; it is never a general version override.
	issuedMu       sync.Mutex
	issuedVersions map[id.ExecutionID]uint64
}

// newActivationMaterializer refuses a partially wired materializer so a
// composition cannot silently skip secret resolution or credential delivery.
func newActivationMaterializer(config activationMaterializerConfig) (*activationMaterializer, error) {
	switch {
	case config.Resolver == nil:
		return nil, fault.New(fault.InvalidInput, "activation materializer requires a secret resolver")
	case config.Lookup == nil:
		return nil, fault.New(fault.InvalidInput, "activation materializer requires a credential lookup")
	case config.Credentials == nil:
		return nil, fault.New(fault.InvalidInput, "activation materializer requires a credential reader")
	case config.TenantKeys == nil:
		return nil, fault.New(fault.InvalidInput, "activation materializer requires a tenant key reader")
	case config.Derivation == nil:
		return nil, fault.New(fault.InvalidInput, "activation materializer requires a derivation key resolver")
	case config.Manager == nil:
		return nil, fault.New(fault.InvalidInput, "activation materializer requires a credential manager")
	case config.Controller == nil:
		return nil, fault.New(fault.InvalidInput, "activation materializer requires a controller reader")
	case config.IDs == nil:
		return nil, fault.New(fault.InvalidInput, "activation materializer requires an identity source")
	case config.Clock == nil:
		return nil, fault.New(fault.InvalidInput, "activation materializer requires a clock")
	}
	return &activationMaterializer{
		resolver:       config.Resolver,
		lookup:         config.Lookup,
		credentials:    config.Credentials,
		tenantKeys:     config.TenantKeys,
		derivation:     config.Derivation,
		manager:        config.Manager,
		controller:     config.Controller,
		ids:            config.IDs,
		clock:          config.Clock,
		models:         config.Models,
		workloadURL:    strings.TrimRight(strings.TrimSpace(config.WorkloadURL), "/"),
		issuedVersions: make(map[id.ExecutionID]uint64),
	}, nil
}

// activationMaterializerConfig binds every activation dependency.
type activationMaterializerConfig struct {
	Resolver    secret.Resolver
	Lookup      store.CredentialLookup
	Credentials token.CredentialReader
	TenantKeys  token.TenantKeyReader
	Derivation  token.DerivationKeyResolver
	Manager     *token.Manager
	Controller  store.Controller
	IDs         *bridges.IDs
	Clock       Clock
	Models      *ModelsConfig
	WorkloadURL string
}

// prepare resolves the declared secret values and the committed execution
// credential into one PrepareRequest. Every refusal happens before the runtime
// adapter is called.
func (materializer *activationMaterializer) prepare(ctx context.Context, request kernelengine.PrepareRequest) (kernelengine.PrepareRequest, error) {
	if err := ctx.Err(); err != nil {
		return request, err
	}
	executionID := request.Identity.ExecutionID
	if executionID == "" {
		return request, fault.New(fault.InvalidInput, "activation requires an execution identity")
	}
	candidate, err := materializer.lookup.LookupCredential(ctx, store.CredentialExecution, string(executionID))
	if err != nil {
		return request, fault.Wrap(fault.DependencyUnavailable, "activation execution scope is unavailable", err)
	}
	if err := candidate.Validate(); err != nil {
		return request, fault.Wrap(fault.DependencyUnavailable, "activation execution scope is invalid", err)
	}
	scope := tenancy.Scope{TenantID: candidate.TenantID, ProjectID: candidate.ProjectID}
	if !scope.ProjectScoped() {
		return request, fault.New(fault.DependencyUnavailable, "activation execution is not project scoped")
	}

	// Resolve every declared name before any credential mutation. A denied,
	// missing or unreadable value fails the whole preparation, so a workload is
	// never started with a partial secret set.
	if len(request.Container.Secrets) > 0 {
		resolved, err := materializer.resolver.Resolve(ctx, secret.ResolveRequest{
			Scope: scope,
			Names: request.Container.Secrets,
		})
		if err != nil {
			return request, err
		}
		values := make([]kernelengine.SecretValue, len(resolved))
		for index, value := range resolved {
			values[index] = kernelengine.SecretValue{Name: value.Name, Value: value.Value}
		}
		request.Secrets = values
	}

	credential, err := materializer.credential(ctx, scope, candidate.PrincipalID, request.Identity)
	if err != nil {
		return request, err
	}
	request.Credential = credential
	if materializer.models != nil && materializer.models.enabledForScope(scope) && materializer.workloadURL != "" {
		request.Container.Env = injectModelProxyEnv(request.Container.Env, materializer.workloadURL)
	}
	return request, nil
}

func injectModelProxyEnv(existing []kerneldefinition.EnvVar, baseURL string) []kerneldefinition.EnvVar {
	filtered := make([]kerneldefinition.EnvVar, 0, len(existing)+2)
	for _, entry := range existing {
		if string(entry.Name) == EnvLLMOpenAIBaseURL || string(entry.Name) == EnvLLMAnthropicBaseURL {
			continue
		}
		filtered = append(filtered, entry)
	}
	return append(filtered,
		kerneldefinition.EnvVar{
			Name:  id.EnvName(EnvLLMOpenAIBaseURL),
			Value: baseURL + "/llm/openai/v1",
		},
		kerneldefinition.EnvVar{
			Name:  id.EnvName(EnvLLMAnthropicBaseURL),
			Value: baseURL + "/llm/anthropic",
		},
	)
}

// credential returns the committed execution credential, reconstructing it
// from committed state when it is still current and issuing a first credential
// only when the pair is absent or expired. A concurrent execution-version move
// is retried from the newly committed fact; every other failure fails
// preparation before any workload exists.
func (materializer *activationMaterializer) credential(
	ctx context.Context,
	scope tenancy.Scope,
	principalID id.PrincipalID,
	effect kernelengine.EffectIdentity,
) (string, error) {
	const attempts = 3
	var conflict error
	for range attempts {
		now := materializer.clock.Now()
		if now.IsZero() {
			return "", fault.New(fault.InternalFailure, "activation clock returned zero time")
		}
		fact, err := materializer.readCredential(ctx, scope, principalID, effect.ExecutionID)
		if err != nil {
			return "", err
		}
		if !fact.Enabled {
			return "", fault.New(fault.Unauthenticated, "activation execution is not running")
		}
		if fact.Credential.State(now) == token.StateCurrent {
			return materializer.reconstruct(ctx, scope.TenantID, effect.ExecutionID, fact.Credential)
		}
		credential, err := materializer.issue(ctx, scope, principalID, effect, fact)
		if err == nil {
			return credential, nil
		}
		if class, ok := fault.ClassOf(err); !ok || class != fault.VersionConflict {
			return "", err
		}
		conflict = err
	}
	return "", fault.Wrap(fault.VersionConflict, "activation credential did not converge", conflict)
}

// readCredential reads and confirms the committed credential fact of exactly
// this execution. A fact for another scope, principal or execution fails
// closed rather than being delivered.
func (materializer *activationMaterializer) readCredential(
	ctx context.Context,
	scope tenancy.Scope,
	principalID id.PrincipalID,
	executionID id.ExecutionID,
) (token.CredentialFact, error) {
	fact, err := materializer.credentials.ReadCredential(ctx, token.CredentialRequest{
		Scope:       scope,
		PrincipalID: principalID,
		ExecutionID: executionID,
	})
	if err != nil {
		return token.CredentialFact{}, fault.Wrap(fault.DependencyUnavailable, "activation credential state is unavailable", err)
	}
	if fact.Scope != scope || fact.PrincipalID != principalID ||
		fact.ExecutionID != executionID || fact.ServiceID != "" {
		return token.CredentialFact{}, fault.New(fault.DependencyUnavailable, "activation credential state does not name the execution")
	}
	return fact, nil
}

// reconstruct re-derives the committed token from its committed nonce/expiry
// and the tenant derivation key. It mints nothing and delivers nothing.
func (materializer *activationMaterializer) reconstruct(
	ctx context.Context,
	tenantID id.TenantID,
	executionID id.ExecutionID,
	credential token.Credential,
) (string, error) {
	key, err := materializer.tenantKeys.ReadTenantKey(ctx, token.TenantKeyRequest{TenantID: tenantID})
	if err != nil {
		return "", fault.Wrap(fault.DependencyUnavailable, "tenant derivation key is unavailable", err)
	}
	if key.TenantID != tenantID {
		return "", fault.New(fault.DependencyUnavailable, "tenant derivation key is outside the execution tenant")
	}
	plaintext, err := materializer.derivation.ResolveDerivationKey(ctx, key)
	if err != nil {
		return "", fault.Wrap(fault.DependencyUnavailable, "tenant derivation key cannot be resolved", err)
	}
	presented, err := token.DeriveExecutionToken(plaintext, token.ExecutionClaims{
		TenantID:    tenantID,
		ExecutionID: executionID,
		Nonce:       credential.Nonce,
		Expiry:      credential.Expiry,
	})
	if err != nil {
		return "", fault.Wrap(fault.InternalFailure, "execution credential cannot be reconstructed", err)
	}
	return presented, nil
}

// issue mints a first (or replacement) execution credential. The credential
// TTL is bounded by the current action deadline and the eight-hour
// maximum. The post-commit delivery is captured for this preparation instead of
// refreshing a workload that does not exist yet.
func (materializer *activationMaterializer) issue(
	ctx context.Context,
	scope tenancy.Scope,
	principalID id.PrincipalID,
	effect kernelengine.EffectIdentity,
	fact token.CredentialFact,
) (string, error) {
	deadline, err := materializer.actionDeadline(ctx, effect.ActionID)
	if err != nil {
		return "", err
	}
	record, err := materializer.activationReceipt(ctx, scope, principalID, effect, fact)
	if err != nil {
		return "", err
	}
	sink := &activationCredentialSink{}
	result, err := materializer.manager.EnsureExecution(withActivationSink(ctx, sink), token.IssueRequest{
		Scope:           scope,
		PrincipalID:     principalID,
		ExecutionID:     effect.ExecutionID,
		TTL:             token.DefaultBounds.MaxTTL,
		Deadline:        deadline,
		ExpectedVersion: fact.Version,
		Receipt:         record,
	})
	if err != nil {
		return "", err
	}
	if !result.Reused && result.Version != 0 {
		// This call committed a new credential pair and advanced the shared
		// execution row to result.Version. Remember exactly that revision so the
		// immediately following preparation-failure commit can be rebased onto
		// it; a reused/replayed credential changed nothing and is not recorded.
		materializer.recordIssuedVersion(effect.ExecutionID, result.Version)
	}
	if result.Token != "" {
		return result.Token, nil
	}
	// A reused or replayed commit returns no plaintext. The delivered token is
	// the committed nonce/expiry derived again; never mint a second one.
	if captured, ok := sink.single(); ok {
		return captured, nil
	}
	committed, err := materializer.readCredential(ctx, scope, principalID, effect.ExecutionID)
	if err != nil {
		return "", err
	}
	if committed.Credential.State(materializer.clock.Now()) != token.StateCurrent {
		return "", fault.New(fault.DependencyUnavailable, "activation credential is not current after issuance")
	}
	return materializer.reconstruct(ctx, scope.TenantID, effect.ExecutionID, committed.Credential)
}

// recordIssuedVersion remembers the execution-row version produced by this
// materializer's own credential commit for one execution.
func (materializer *activationMaterializer) recordIssuedVersion(executionID id.ExecutionID, version uint64) {
	materializer.issuedMu.Lock()
	materializer.issuedVersions[executionID] = version
	materializer.issuedMu.Unlock()
}

// issuedVersion reports the version produced by this materializer's own
// credential commit for one execution, if one is still tracked.
func (materializer *activationMaterializer) issuedVersion(executionID id.ExecutionID) (uint64, bool) {
	materializer.issuedMu.Lock()
	defer materializer.issuedMu.Unlock()
	version, ok := materializer.issuedVersions[executionID]
	return version, ok
}

// forgetIssuedVersion drops the tracked credential-commit version after the
// preparation-failure rebase has consumed it.
func (materializer *activationMaterializer) forgetIssuedVersion(executionID id.ExecutionID) {
	materializer.issuedMu.Lock()
	delete(materializer.issuedVersions, executionID)
	materializer.issuedMu.Unlock()
}

// actionDeadline reads the persisted deadline of the activation's action. The
// deadline is an upper bound on the credential lifetime; an unreadable or
// malformed deadline fails closed rather than issuing an unbounded token.
func (materializer *activationMaterializer) actionDeadline(ctx context.Context, actionID id.ActionID) (time.Time, error) {
	if actionID == "" {
		return time.Time{}, fault.New(fault.InvalidInput, "activation requires an action identity")
	}
	row, err := materializer.controller.Read(ctx, "actions", string(actionID))
	if err != nil {
		return time.Time{}, fault.Wrap(fault.DependencyUnavailable, "activation action deadline is unavailable", err)
	}
	value, ok := row.Values["deadline"]
	if !ok || value == nil {
		return time.Time{}, nil
	}
	moment, ok := value.(time.Time)
	if !ok {
		return time.Time{}, fault.New(fault.DependencyUnavailable, "activation action deadline is invalid")
	}
	return moment, nil
}

// activationReceipt builds the durable receipt of one activation credential
// commit. The content is stable for the execution/action pair, so a lost
// response replays the original commit instead of minting again; the receipt
// body is the secret-free issue result and never carries token material.
func (materializer *activationMaterializer) activationReceipt(
	ctx context.Context,
	scope tenancy.Scope,
	principalID id.PrincipalID,
	effect kernelengine.EffectIdentity,
	fact token.CredentialFact,
) (receipt.Record, error) {
	fingerprint, err := receipt.Fingerprint(receipt.Content{
		MediaType: "application/octet-stream",
		Bytes:     []byte(string(effect.ExecutionID) + "\x00" + string(effect.ActionID)),
	})
	if err != nil {
		return receipt.Record{}, fault.Wrap(fault.InternalFailure, "activation receipt content is invalid", err)
	}
	receiptID, err := materializer.ids.NewReceiptID()
	if err != nil {
		return receipt.Record{}, err
	}
	result, err := json.Marshal(token.IssueResult{State: token.StateCurrent, Version: fact.Version + 1})
	if err != nil {
		return receipt.Record{}, fault.Wrap(fault.InternalFailure, "activation receipt result could not be encoded", err)
	}
	return receipt.Record{
		ID: receiptID,
		Key: receipt.Key{
			TenantID:    scope.TenantID,
			ProjectID:   scope.ProjectID,
			PrincipalID: principalID,
			Operation:   activationCredentialOperation,
			CallerKey:   activationCallerKey(effect, fact.Credential),
		},
		ContentHash: fingerprint,
		Result:      result,
		ExecutionID: effect.ExecutionID,
		At:          materializer.clock.Now(),
	}, nil
}

// activationCallerKey is the idempotency key of one activation credential
// generation. The committed nonce/expiry pair is the generation: an absent or
// expired pair may issue a fresh credential under a new key, while a retry of
// the same generation replays the original receipt instead of minting again.
func activationCallerKey(effect kernelengine.EffectIdentity, credential token.Credential) string {
	generation := "absent"
	if credential.Present() {
		generation = strconv.FormatInt(credential.Expiry.UnixNano(), 10)
	}
	return "activation:" + string(effect.ExecutionID) + ":" + string(effect.ActionID) + ":" + generation
}

// activationSinkKey is the context key of one in-flight preparation delivery
// sink.
type activationSinkKey struct{}

// activationCredentialSink captures the post-commit token of one preparation.
type activationCredentialSink struct {
	mu     sync.Mutex
	tokens []string
}

func withActivationSink(ctx context.Context, sink *activationCredentialSink) context.Context {
	return context.WithValue(ctx, activationSinkKey{}, sink)
}

func activationSinkFrom(ctx context.Context) (*activationCredentialSink, bool) {
	sink, ok := ctx.Value(activationSinkKey{}).(*activationCredentialSink)
	return sink, ok && sink != nil
}

func (sink *activationCredentialSink) record(delivery token.Delivery) {
	sink.mu.Lock()
	sink.tokens = append(sink.tokens, delivery.Token)
	sink.mu.Unlock()
}

func (sink *activationCredentialSink) single() (string, bool) {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.tokens) != 1 || sink.tokens[0] == "" {
		return "", false
	}
	return sink.tokens[0], true
}

// activationDeliverer intercepts the token manager's post-commit delivery while
// an activation preparation is in flight and delegates every other delivery to
// the runtime bridge. It never renders the token.
type activationDeliverer struct {
	inner token.Deliverer
}

var _ token.Deliverer = (*activationDeliverer)(nil)

func (deliverer *activationDeliverer) Deliver(ctx context.Context, delivery token.Delivery) error {
	if sink, ok := activationSinkFrom(ctx); ok {
		sink.record(delivery)
		return nil
	}
	if deliverer == nil || deliverer.inner == nil {
		return fault.New(fault.InternalFailure, "credential deliverer is not configured")
	}
	return deliverer.inner.Deliver(ctx, delivery)
}

// derivedTokenAuthenticator is the derived-credential authentication boundary
// the development resolver drives.
type derivedTokenAuthenticator interface {
	Authenticate(context.Context, token.AuthenticateRequest) (token.Authenticated, error)
}

// combinedPrincipalResolver authenticates development static tokens and
// derived execution/service credentials. A token with a derived prefix is
// decided only by the derived path, so a forged or failed derived credential
// can never fall through to the static development authenticator.
type combinedPrincipalResolver struct {
	static      authorize.CurrentPrincipal
	credentials derivedTokenAuthenticator
	lookup      store.CredentialLookup
	clock       Clock
}

var _ authorize.CurrentPrincipal = (*combinedPrincipalResolver)(nil)

func newCombinedPrincipalResolver(
	static authorize.CurrentPrincipal,
	credentials derivedTokenAuthenticator,
	lookup store.CredentialLookup,
	clock Clock,
) (*combinedPrincipalResolver, error) {
	if credentials == nil {
		return nil, fault.New(fault.InvalidInput, "combined principal resolver requires a credential authenticator")
	}
	if lookup == nil {
		return nil, fault.New(fault.InvalidInput, "combined principal resolver requires a credential lookup")
	}
	if clock == nil {
		return nil, fault.New(fault.InvalidInput, "combined principal resolver requires a clock")
	}
	return &combinedPrincipalResolver{
		static:      static,
		credentials: credentials,
		lookup:      lookup,
		clock:       clock,
	}, nil
}

// Resolve decides one presented credential. The two derived prefixes are
// never delegated to the static development authenticator.
func (resolver *combinedPrincipalResolver) Resolve(ctx context.Context, presented string) (authorize.Principal, error) {
	switch {
	case strings.HasPrefix(presented, token.ExecutionPrefix):
		return resolver.resolveExecution(ctx, presented)
	case strings.HasPrefix(presented, token.ServicePrefix):
		return resolver.resolveService(ctx, presented)
	}
	if resolver.static == nil {
		return authorize.Principal{}, derivedRefusal()
	}
	return resolver.static.Resolve(ctx, presented)
}

// resolveExecution verifies one kodex credential against its candidate
// execution scope and the manager's current nonce/key/expiry/principal state.
func (resolver *combinedPrincipalResolver) resolveExecution(ctx context.Context, presented string) (authorize.Principal, error) {
	credentialID, ok := derivedCredentialID(strings.TrimPrefix(presented, token.ExecutionPrefix))
	if !ok {
		return authorize.Principal{}, derivedRefusal()
	}
	candidate, err := resolver.lookup.LookupCredential(ctx, store.CredentialExecution, credentialID)
	if err != nil {
		return authorize.Principal{}, derivedRefusal()
	}
	scope, ok := derivedScope(candidate)
	if !ok {
		return authorize.Principal{}, derivedRefusal()
	}
	now := resolver.clock.Now()
	if now.IsZero() {
		return authorize.Principal{}, derivedRefusal()
	}
	authenticated, err := resolver.credentials.Authenticate(ctx, token.AuthenticateRequest{
		Scope:       scope,
		Presented:   presented,
		PrincipalID: candidate.PrincipalID,
		ExecutionID: id.ExecutionID(credentialID),
		Now:         now,
	})
	if err != nil {
		return authorize.Principal{}, derivedRefusal()
	}
	if !authenticated.Enabled || authenticated.Scope != scope ||
		authenticated.PrincipalID != candidate.PrincipalID ||
		authenticated.Kind != identity.KindExecution ||
		authenticated.ExecutionID != id.ExecutionID(credentialID) ||
		authenticated.ServiceID != "" {
		return authorize.Principal{}, derivedRefusal()
	}
	return derivedPrincipal(authenticated)
}

// resolveService verifies one kodsvc credential against its candidate service
// scope. It exists so a service-kind derived token is also decided only by the
// derived path.
func (resolver *combinedPrincipalResolver) resolveService(ctx context.Context, presented string) (authorize.Principal, error) {
	credentialID, ok := derivedCredentialID(strings.TrimPrefix(presented, token.ServicePrefix))
	if !ok {
		return authorize.Principal{}, derivedRefusal()
	}
	candidate, err := resolver.lookup.LookupCredential(ctx, store.CredentialService, credentialID)
	if err != nil {
		return authorize.Principal{}, derivedRefusal()
	}
	scope, ok := derivedScope(candidate)
	if !ok {
		return authorize.Principal{}, derivedRefusal()
	}
	now := resolver.clock.Now()
	if now.IsZero() {
		return authorize.Principal{}, derivedRefusal()
	}
	authenticated, err := resolver.credentials.Authenticate(ctx, token.AuthenticateRequest{
		Scope:       scope,
		Presented:   presented,
		PrincipalID: candidate.PrincipalID,
		ServiceID:   service.ServiceID(credentialID),
		Now:         now,
	})
	if err != nil {
		return authorize.Principal{}, derivedRefusal()
	}
	if !authenticated.Enabled || authenticated.Scope != scope ||
		authenticated.PrincipalID != candidate.PrincipalID ||
		authenticated.Kind != identity.KindService ||
		authenticated.ServiceID != service.ServiceID(credentialID) ||
		authenticated.ExecutionID != "" ||
		authenticated.PrincipalID != id.PrincipalID(credentialID) {
		return authorize.Principal{}, derivedRefusal()
	}
	return derivedPrincipal(authenticated)
}

// derivedCredentialID extracts the identity hint of one derived credential. The
// MAC segment is opaque here; the manager verifies it.
func derivedCredentialID(body string) (string, bool) {
	segments := strings.Split(body, ".")
	if len(segments) != 2 || segments[0] == "" || segments[1] == "" {
		return "", false
	}
	return segments[0], true
}

func derivedScope(candidate store.CandidateScope) (tenancy.Scope, bool) {
	if err := candidate.Validate(); err != nil {
		return tenancy.Scope{}, false
	}
	scope := tenancy.Scope{TenantID: candidate.TenantID, ProjectID: candidate.ProjectID}
	if err := scope.Validate(); err != nil {
		return tenancy.Scope{}, false
	}
	return scope, true
}

// derivedPrincipal converts one verified derived identity into the current
// principal value. Current roles and version come from the manager's read, not
// from the presented token.
func derivedPrincipal(authenticated token.Authenticated) (authorize.Principal, error) {
	principal := authorize.Principal{
		ID:          authenticated.PrincipalID,
		Kind:        authenticated.Kind,
		Scope:       authenticated.Scope,
		Roles:       append([]tenancy.Role(nil), authenticated.Roles...),
		Enabled:     authenticated.Enabled,
		Version:     authenticated.Version,
		ExecutionID: authenticated.ExecutionID,
	}
	if err := principal.Validate(); err != nil {
		return authorize.Principal{}, derivedRefusal()
	}
	return principal, nil
}

// derivedRefusal is the single uniform derived-credential refusal. It never
// renders the presented token or the private verification cause.
func derivedRefusal() error {
	return fault.New(fault.Unauthenticated, "presented credential is invalid")
}
