package adaptercontract

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/integration/nats"
	"github.com/optimiware/kodama/internal/shell/log"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/token"
)

// TestNATSSubjectScoping pins the scope-qualified subject layout. A subscriber
// for one tenant/project cannot receive another scope's events, logs or live
// invalidations.
func TestNATSSubjectScoping(t *testing.T) {
	var document struct {
		Subjects map[string]string `json:"subjects"`
	}
	readTestJSON(t, "integration.json", &document)
	tenantScope := tenancy.Scope{TenantID: "tenant-1"}
	projectScope := tenancy.Scope{TenantID: "tenant-1", ProjectID: "project-1"}

	cases := []struct {
		name string
		got  func() (nats.Subject, error)
		want string
	}{
		{"tenant_event", func() (nats.Subject, error) { return nats.EventSubject(tenantScope) }, document.Subjects["tenant_event"]},
		{"project_event", func() (nats.Subject, error) { return nats.EventSubject(projectScope) }, document.Subjects["project_event"]},
		{"project_log", func() (nats.Subject, error) {
			return nats.LogSubject(projectScope, id.ExecutionID("exec-1"))
		}, document.Subjects["project_log"]},
		{"tenant_invalidation", func() (nats.Subject, error) { return nats.InvalidationSubject(tenantScope) }, document.Subjects["tenant_invalidation"]},
		{"project_invalidation", func() (nats.Subject, error) { return nats.InvalidationSubject(projectScope) }, document.Subjects["project_invalidation"]},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			subject, err := tc.got()
			if err != nil {
				t.Fatalf("subject: %v", err)
			}
			if string(subject) != tc.want {
				t.Fatalf("subject = %q; want %q", subject, tc.want)
			}
			if err := subject.Validate(); err != nil {
				t.Fatalf("subject is not valid: %v", err)
			}
		})
	}

	tenantEvent, _ := nats.EventSubject(tenantScope)
	projectEvent, _ := nats.EventSubject(projectScope)
	if tenantEvent == projectEvent {
		t.Fatal("tenant and project event subjects collide")
	}
	otherTenant, _ := nats.EventSubject(tenancy.Scope{TenantID: "tenant-2", ProjectID: "project-1"})
	if otherTenant == projectEvent {
		t.Fatal("subjects from different tenants collide")
	}

	parsed, err := nats.ParseScope(projectEvent)
	if err != nil {
		t.Fatalf("ParseScope(project event): %v", err)
	}
	if parsed != projectScope {
		t.Fatalf("ParseScope = %+v; want %+v", parsed, projectScope)
	}
	parsed, err = nats.ParseScope(tenantEvent)
	if err != nil {
		t.Fatalf("ParseScope(tenant event): %v", err)
	}
	if parsed != tenantScope {
		t.Fatalf("ParseScope = %+v; want %+v", parsed, tenantScope)
	}

	invalid := []nats.Subject{
		"",
		"kodama.v1.tenant-1.*.events",
		"kodama.v1.tenant-1.>.events",
		"kodama.v1.tenant-1..events",
	}
	for _, subject := range invalid {
		if err := subject.Validate(); err == nil {
			t.Errorf("subject %q accepted; want refusal", subject)
		}
	}
}

// TestOIDCConfigAndRequest pins the access-token verifier input rules. Issuer
// and audience are explicit; a token or timestamp is required.
func TestOIDCConfigAndRequest(t *testing.T) {
	config := identity.OIDCConfig{
		Issuer:    "https://issuer.example",
		Audience:  "kodama",
		ClockSkew: 30 * time.Second,
	}
	if err := config.Validate(); err != nil {
		t.Fatalf("valid OIDC config refused: %v", err)
	}
	invalidConfigs := []identity.OIDCConfig{
		{Issuer: "http://issuer.example", Audience: "kodama"},
		{Issuer: "", Audience: "kodama"},
		{Issuer: "https://issuer.example", Audience: ""},
		{Issuer: "https://issuer.example", Audience: "kodama", ClockSkew: -time.Second},
	}
	for _, candidate := range invalidConfigs {
		if err := candidate.Validate(); err == nil {
			t.Errorf("OIDC config %+v accepted; want refusal", candidate)
		}
	}

	request := identity.OIDCRequest{AccessToken: "access-token", Now: time.Unix(100, 0).UTC()}
	if err := request.Validate(); err != nil {
		t.Fatalf("valid OIDC request refused: %v", err)
	}
	if err := (identity.OIDCRequest{Now: time.Unix(100, 0).UTC()}).Validate(); err == nil {
		t.Error("OIDC request without an access token accepted")
	}
	if err := (identity.OIDCRequest{AccessToken: "token"}).Validate(); err == nil {
		t.Error("OIDC request without an authoritative time accepted")
	}
}

// TestOAuthClientConfig pins one OAuth application's installation client
// configuration: an explicit protocol, HTTPS issuer, client id, secret
// reference, audience, unique scopes and an optional HTTPS redirect URL. The
// secret reference names stored material; no secret value is accepted here.
func TestOAuthClientConfig(t *testing.T) {
	config := identity.OAuthClientConfig{
		Protocol:        identity.OAuthProtocolOIDC,
		Issuer:          "https://tenant.eu.auth0.com/",
		ClientID:        "platform-client",
		ClientSecretRef: "secret/platform-oauth-client",
		Audience:        "https://platform.example/api",
		Scopes:          []string{"openid", "profile", "platform.read"},
		RedirectURL:     "https://platform.example/callback",
	}
	if err := config.Validate(); err != nil {
		t.Fatalf("valid OAuth client config refused: %v", err)
	}
	if err := (identity.OAuthClientConfig{
		Protocol:        identity.OAuthProtocolOAuth2,
		Issuer:          "https://tenant.eu.auth0.com/",
		ClientID:        "platform-client",
		ClientSecretRef: "secret/platform-oauth-client",
		Audience:        "https://platform.example/api",
		Scopes:          []string{"platform.read"},
	}).Validate(); err != nil {
		t.Fatalf("valid OAuth2 client config without a redirect URL refused: %v", err)
	}

	invalid := []identity.OAuthClientConfig{
		{Issuer: config.Issuer, ClientID: config.ClientID, ClientSecretRef: config.ClientSecretRef, Audience: config.Audience, Scopes: config.Scopes},
		{Protocol: "saml", Issuer: config.Issuer, ClientID: config.ClientID, ClientSecretRef: config.ClientSecretRef, Audience: config.Audience, Scopes: config.Scopes},
		{Protocol: config.Protocol, Issuer: "http://tenant.eu.auth0.com/", ClientID: config.ClientID, ClientSecretRef: config.ClientSecretRef, Audience: config.Audience, Scopes: config.Scopes},
		{Protocol: config.Protocol, Issuer: "", ClientID: config.ClientID, ClientSecretRef: config.ClientSecretRef, Audience: config.Audience, Scopes: config.Scopes},
		{Protocol: config.Protocol, Issuer: config.Issuer, ClientSecretRef: config.ClientSecretRef, Audience: config.Audience, Scopes: config.Scopes},
		{Protocol: config.Protocol, Issuer: config.Issuer, ClientID: "bad client", ClientSecretRef: config.ClientSecretRef, Audience: config.Audience, Scopes: config.Scopes},
		{Protocol: config.Protocol, Issuer: config.Issuer, ClientID: config.ClientID, Audience: config.Audience, Scopes: config.Scopes},
		{Protocol: config.Protocol, Issuer: config.Issuer, ClientID: config.ClientID, ClientSecretRef: config.ClientSecretRef, Scopes: config.Scopes},
		{Protocol: config.Protocol, Issuer: config.Issuer, ClientID: config.ClientID, ClientSecretRef: config.ClientSecretRef, Audience: config.Audience},
		{Protocol: config.Protocol, Issuer: config.Issuer, ClientID: config.ClientID, ClientSecretRef: config.ClientSecretRef, Audience: config.Audience, Scopes: []string{""}},
		{Protocol: config.Protocol, Issuer: config.Issuer, ClientID: config.ClientID, ClientSecretRef: config.ClientSecretRef, Audience: config.Audience, Scopes: []string{"openid", "openid"}},
		{Protocol: config.Protocol, Issuer: config.Issuer, ClientID: config.ClientID, ClientSecretRef: config.ClientSecretRef, Audience: config.Audience, Scopes: config.Scopes, RedirectURL: "http://platform.example/callback"},
	}
	for _, candidate := range invalid {
		if err := candidate.Validate(); err == nil {
			t.Errorf("OAuth client config %+v accepted; want refusal", candidate)
		}
	}
}

// TestOAuthClaimsMatch pins the tenant-claim and role-string match. Subject is
// optional audit context and never substitutes for the two-claim match.
func TestOAuthClaimsMatch(t *testing.T) {
	claims := identity.OAuthClaims{
		TenantClaim: "https://kodama/tenant",
		Tenant:      "tenant-1",
		RoleClaim:   "https://kodama/roles",
		Role:        "operator",
		Subject:     "auth0|subject-1",
	}
	if err := claims.Validate(); err != nil {
		t.Fatalf("valid OAuth claims refused: %v", err)
	}
	values := map[string][]string{
		claims.TenantClaim: {"tenant-1"},
		claims.RoleClaim:   {"viewer", "operator"},
	}
	if !claims.Matches(values) {
		t.Error("claims with the configured tenant and role did not match")
	}
	missingRole := map[string][]string{claims.TenantClaim: {"tenant-1"}, claims.RoleClaim: {"viewer"}}
	if claims.Matches(missingRole) {
		t.Error("claims without the configured role matched")
	}
	wrongTenant := map[string][]string{claims.TenantClaim: {"tenant-2"}, claims.RoleClaim: {"operator"}}
	if claims.Matches(wrongTenant) {
		t.Error("claims with a different tenant matched")
	}
	if claims.Matches(nil) {
		t.Error("nil claims matched")
	}

	invalid := []identity.OAuthClaims{
		{Tenant: claims.Tenant, RoleClaim: claims.RoleClaim, Role: claims.Role},
		{TenantClaim: claims.TenantClaim, RoleClaim: claims.RoleClaim, Role: claims.Role},
		{TenantClaim: claims.TenantClaim, Tenant: claims.Tenant, Role: claims.Role},
		{TenantClaim: claims.TenantClaim, Tenant: claims.Tenant, RoleClaim: claims.RoleClaim},
		{TenantClaim: claims.TenantClaim, Tenant: claims.Tenant, RoleClaim: claims.RoleClaim, Role: claims.Role, Subject: "bad subject"},
	}
	for _, candidate := range invalid {
		if err := candidate.Validate(); err == nil {
			t.Errorf("OAuth claims %+v accepted; want refusal", candidate)
		}
	}
}

// TestRBACTable proves each OAuth application has its own flexible
// role -> access table with validation, copy-safe lookup and membership
// checks. It is data, not a shared policy engine.
func TestRBACTable(t *testing.T) {
	table := authorize.RBACTable{
		"operator": {"platform.read", "platform.invoke"},
		"admin":    {"platform.admin"},
	}
	if err := table.Validate(); err != nil {
		t.Fatalf("valid RBAC table refused: %v", err)
	}
	access, ok := table.Lookup("operator")
	if !ok {
		t.Fatal("RBAC lookup did not find the operator role")
	}
	if !reflect.DeepEqual(access, []string{"platform.read", "platform.invoke"}) {
		t.Errorf("operator access = %v; want [platform.read platform.invoke]", access)
	}
	access[0] = "mutated"
	if !table.Allows("operator", "platform.read") {
		t.Error("mutating a lookup copy changed the RBAC table")
	}
	if _, ok := table.Lookup("missing"); ok {
		t.Error("RBAC lookup found an unknown role")
	}
	if !table.Allows("operator", "platform.invoke") {
		t.Error("RBAC table did not grant a declared access string")
	}
	if table.Allows("operator", "platform.admin") {
		t.Error("RBAC table granted another role's access string")
	}

	invalid := []authorize.RBACTable{
		{},
		{"": {"platform.read"}},
		{"operator": nil},
		{"operator": {}},
		{"operator": {""}},
		{"operator": {"platform.read", "platform.read"}},
		{"operator": {"bad access"}},
		{"bad role": {"platform.read"}},
	}
	for _, candidate := range invalid {
		if err := candidate.Validate(); err == nil {
			t.Errorf("RBAC table %+v accepted; want refusal", candidate)
		}
	}
}

// TestHostedAuthorityOAuth proves the Platform seam is one OAuth-shaped
// context-first Check: the request carries the protocol, scope, principal,
// operation, tenant/role claim match and required access, and the response
// carries the same protocol plus the application's RBAC decision.
func TestHostedAuthorityOAuth(t *testing.T) {
	claims := identity.OAuthClaims{
		TenantClaim: "https://kodama/tenant",
		Tenant:      "tenant-1",
		RoleClaim:   "https://kodama/roles",
		Role:        "operator",
	}
	request := authorize.HostedAuthorityRequest{
		Protocol:       identity.OAuthProtocolOIDC,
		Scope:          tenancy.Scope{TenantID: "tenant-1", ProjectID: "project-1"},
		PrincipalID:    "principal-1",
		Operation:      "model.invoke",
		Claims:         claims,
		RequiredAccess: []string{"platform.invoke"},
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("valid hosted request refused: %v", err)
	}
	invalidRequests := []authorize.HostedAuthorityRequest{
		{Scope: request.Scope, PrincipalID: request.PrincipalID, Operation: request.Operation, Claims: request.Claims, RequiredAccess: request.RequiredAccess},
		{Protocol: "saml", Scope: request.Scope, PrincipalID: request.PrincipalID, Operation: request.Operation, Claims: request.Claims, RequiredAccess: request.RequiredAccess},
		{Protocol: request.Protocol, PrincipalID: request.PrincipalID, Operation: request.Operation, Claims: request.Claims, RequiredAccess: request.RequiredAccess},
		{Protocol: request.Protocol, Scope: request.Scope, Operation: request.Operation, Claims: request.Claims, RequiredAccess: request.RequiredAccess},
		{Protocol: request.Protocol, Scope: request.Scope, PrincipalID: request.PrincipalID, Claims: request.Claims, RequiredAccess: request.RequiredAccess},
		{Protocol: request.Protocol, Scope: request.Scope, PrincipalID: request.PrincipalID, Operation: request.Operation, RequiredAccess: request.RequiredAccess},
		{Protocol: request.Protocol, Scope: request.Scope, PrincipalID: request.PrincipalID, Operation: request.Operation, Claims: request.Claims},
		{Protocol: request.Protocol, Scope: request.Scope, PrincipalID: request.PrincipalID, Operation: request.Operation, Claims: request.Claims, RequiredAccess: []string{"platform.invoke", "platform.invoke"}},
		{Protocol: request.Protocol, Scope: request.Scope, PrincipalID: request.PrincipalID, Operation: request.Operation, Claims: request.Claims, RequiredAccess: []string{"bad access"}},
	}
	for _, candidate := range invalidRequests {
		if err := candidate.Validate(); err == nil {
			t.Errorf("hosted request %+v accepted; want refusal", candidate)
		}
	}

	response, err := fakeHostedAuthority{}.Check(context.Background(), request)
	if err != nil {
		t.Fatalf("hosted Check: %v", err)
	}
	if response.Protocol != request.Protocol {
		t.Errorf("response protocol = %q; want %q", response.Protocol, request.Protocol)
	}
	if !response.Decision.Allowed || response.Decision.Role != claims.Role {
		t.Errorf("decision = %+v; want allowed role %q", response.Decision, claims.Role)
	}
	if !reflect.DeepEqual(response.Decision.Access, request.RequiredAccess) {
		t.Errorf("decision access = %v; want %v", response.Decision.Access, request.RequiredAccess)
	}
	if err := response.Validate(); err != nil {
		t.Fatalf("valid hosted response refused: %v", err)
	}
	if err := (authorize.HostedAuthorityResponse{
		Protocol: identity.OAuthProtocolOIDC,
		Decision: authorize.HostedAuthorityDecision{Allowed: false},
	}).Validate(); err != nil {
		t.Errorf("valid refused decision rejected: %v", err)
	}

	invalidResponses := []authorize.HostedAuthorityResponse{
		{Decision: authorize.HostedAuthorityDecision{Allowed: true, Role: claims.Role, Access: request.RequiredAccess}},
		{Protocol: "saml", Decision: authorize.HostedAuthorityDecision{Allowed: true, Role: claims.Role, Access: request.RequiredAccess}},
		{Protocol: request.Protocol, Decision: authorize.HostedAuthorityDecision{Allowed: true, Access: request.RequiredAccess}},
		{Protocol: request.Protocol, Decision: authorize.HostedAuthorityDecision{Allowed: true, Role: claims.Role}},
		{Protocol: request.Protocol, Decision: authorize.HostedAuthorityDecision{Allowed: true, Role: claims.Role, Access: []string{"platform.invoke", "platform.invoke"}}},
	}
	for _, candidate := range invalidResponses {
		if err := candidate.Validate(); err == nil {
			t.Errorf("hosted response %+v accepted; want refusal", candidate)
		}
	}
}

// TestImageReferencePinning proves registry resolution returns an immutable
// sha256 digest; mutable tags and short digests are refused.
func TestImageReferencePinning(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	reference, err := pipeline.ParseImageReference("registry.example/repo@" + digest)
	if err != nil {
		t.Fatalf("ParseImageReference: %v", err)
	}
	if reference.Digest != digest {
		t.Errorf("digest = %q; want %q", reference.Digest, digest)
	}
	if err := reference.Validate(); err != nil {
		t.Fatalf("valid image reference refused: %v", err)
	}
	invalidReferences := []string{
		"registry.example/repo:latest",
		"registry.example/repo:1.0",
		"registry.example/repo",
		"registry.example/repo@sha256:short",
		"registry.example/repo@md5:" + strings.Repeat("a", 32),
	}
	for _, value := range invalidReferences {
		if _, err := pipeline.ParseImageReference(value); err == nil {
			t.Errorf("ParseImageReference(%q) accepted a mutable reference", value)
		}
	}

	resolved := pipeline.ResolvedImage{Reference: "registry.example/repo", Digest: digest}
	if err := resolved.Validate(); err != nil {
		t.Fatalf("valid resolved image refused: %v", err)
	}
	for _, candidate := range []pipeline.ResolvedImage{
		{Reference: "registry.example/repo"},
		{Reference: "registry.example/repo", Digest: "sha256:short"},
		{Reference: "registry.example/repo", Digest: "md5:" + strings.Repeat("a", 32)},
	} {
		if err := candidate.Validate(); err == nil {
			t.Errorf("resolved image %+v accepted; want refusal", candidate)
		}
	}
}

// TestTokenInvalidation pins live credential invalidation: one execution or
// service principal, a declared reason and an authoritative time.
func TestTokenInvalidation(t *testing.T) {
	wantReasons := []token.InvalidationReason{
		token.InvalidationRotated,
		token.InvalidationRevoked,
		token.InvalidationDisabled,
	}
	if got := token.InvalidationReasons(); !reflect.DeepEqual(got, wantReasons) {
		t.Fatalf("invalidation reasons = %v; want %v", got, wantReasons)
	}

	scope := tenancy.Scope{TenantID: "tenant-1", ProjectID: "project-1"}
	execution := token.Invalidation{
		Scope:       scope,
		PrincipalID: "exec-1",
		ExecutionID: "exec-1",
		Reason:      token.InvalidationRotated,
		At:          time.Unix(100, 0).UTC(),
	}
	if err := execution.Validate(); err != nil {
		t.Fatalf("valid execution invalidation refused: %v", err)
	}
	service := token.Invalidation{
		Scope:       scope,
		PrincipalID: "service-1",
		ServiceID:   "service-1",
		Reason:      token.InvalidationRevoked,
		At:          time.Unix(100, 0).UTC(),
	}
	if err := service.Validate(); err != nil {
		t.Fatalf("valid service invalidation refused: %v", err)
	}

	invalid := []token.Invalidation{
		{Scope: scope, PrincipalID: "exec-1", Reason: token.InvalidationRotated, At: time.Unix(100, 0)},
		{Scope: scope, PrincipalID: "exec-1", ExecutionID: "exec-1", ServiceID: "service-1", Reason: token.InvalidationRotated, At: time.Unix(100, 0)},
		{Scope: scope, PrincipalID: "exec-1", ExecutionID: "exec-1", At: time.Unix(100, 0)},
		{Scope: scope, PrincipalID: "exec-1", ExecutionID: "exec-1", Reason: token.InvalidationReason("unknown"), At: time.Unix(100, 0)},
		{Scope: scope, PrincipalID: "exec-1", ExecutionID: "exec-1", Reason: token.InvalidationRotated},
		{PrincipalID: "exec-1", ExecutionID: "exec-1", Reason: token.InvalidationRotated, At: time.Unix(100, 0)},
	}
	for _, candidate := range invalid {
		if err := candidate.Validate(); err == nil {
			t.Errorf("invalidation %+v accepted; want refusal", candidate)
		}
	}
}

// recordingNATSPublisher is a compile-only NATS publisher fake that derives the
// live log subject from the supplied scope, proving the seam cannot fall back
// to chunk-only identity.
type recordingNATSPublisher struct {
	scope   tenancy.Scope
	subject nats.Subject
}

func (*recordingNATSPublisher) PublishEvent(context.Context, event.Envelope) error { return nil }

func (publisher *recordingNATSPublisher) PublishLive(_ context.Context, scope tenancy.Scope, chunk log.Chunk) error {
	subject, err := nats.LiveLogSubject(scope, chunk)
	if err != nil {
		return err
	}
	publisher.scope = scope
	publisher.subject = subject
	return nil
}

var _ nats.Publisher = (*recordingNATSPublisher)(nil)

// scopeBoundLiveSubscriber refuses a subscription whose explicit scope does not
// match the execution's recorded scope.
type scopeBoundLiveSubscriber struct {
	allowed tenancy.Scope
}

func (subscriber scopeBoundLiveSubscriber) SubscribeLive(_ context.Context, scope tenancy.Scope, _ id.ExecutionID) (log.LiveSubscription, error) {
	if err := log.ValidateScope(scope); err != nil {
		return nil, err
	}
	if scope != subscriber.allowed {
		return nil, fmt.Errorf("subscription scope %+v does not match the execution scope", scope)
	}
	return fakeLiveSubscription{}, nil
}

// TestLogScopeRequired proves every retained/live log operation carries an
// explicit valid scope and that a publisher derives its live subject only from
// that scope. A scope-less or mismatched request is refused.
func TestLogScopeRequired(t *testing.T) {
	valid := tenancy.Scope{TenantID: "tenant-1", ProjectID: "project-1"}
	chunk := log.Chunk{
		ExecutionID: "exec-1",
		ActionID:    "action-1",
		Stream:      log.StreamStdout,
		Offset:      0,
		Sequence:    1,
		Data:        []byte("line"),
	}

	if err := log.ValidateScope(tenancy.Scope{}); err == nil {
		t.Error("scope-less log operation accepted")
	}
	if err := log.ValidateScope(valid); err != nil {
		t.Fatalf("valid log scope refused: %v", err)
	}

	read := log.ReadRequest{
		Scope:       valid,
		ExecutionID: chunk.ExecutionID,
		ActionID:    chunk.ActionID,
		Stream:      chunk.Stream,
		Limit:       10,
	}
	if err := read.Validate(); err != nil {
		t.Fatalf("valid scoped read refused: %v", err)
	}
	scopeLess := read
	scopeLess.Scope = tenancy.Scope{}
	if err := scopeLess.Validate(); err == nil {
		t.Error("scope-less retained read accepted")
	}
	badLimit := read
	badLimit.Limit = 0
	if err := badLimit.Validate(); err == nil {
		t.Error("retained read without a positive limit accepted")
	}

	publisher := &recordingNATSPublisher{}
	if err := publisher.PublishLive(context.Background(), valid, chunk); err != nil {
		t.Fatalf("valid live publication refused: %v", err)
	}
	want, err := nats.LogSubject(valid, chunk.ExecutionID)
	if err != nil {
		t.Fatalf("LogSubject: %v", err)
	}
	if publisher.subject != want {
		t.Fatalf("live subject = %q; want %q derived from the supplied scope", publisher.subject, want)
	}
	if publisher.scope != valid {
		t.Fatalf("recorded scope = %+v; want %+v", publisher.scope, valid)
	}
	if err := publisher.PublishLive(context.Background(), tenancy.Scope{}, chunk); err == nil {
		t.Error("scope-less live publication accepted")
	}

	subscriber := scopeBoundLiveSubscriber{allowed: valid}
	if _, err := subscriber.SubscribeLive(context.Background(), valid, chunk.ExecutionID); err != nil {
		t.Fatalf("valid scoped subscription refused: %v", err)
	}
	if _, err := subscriber.SubscribeLive(context.Background(), tenancy.Scope{TenantID: "tenant-1", ProjectID: "project-2"}, chunk.ExecutionID); err == nil {
		t.Error("mismatched subscription scope accepted")
	}

	sink := fakeRetainedSink{}
	if _, err := sink.Append(context.Background(), valid, chunk); err != nil {
		t.Fatalf("valid scoped append refused: %v", err)
	}
	if _, err := sink.Append(context.Background(), tenancy.Scope{}, chunk); err == nil {
		t.Error("scope-less retained append accepted")
	}
	if _, err := sink.Read(context.Background(), read); err != nil {
		t.Fatalf("valid scoped retained read refused: %v", err)
	}
	if _, err := sink.Read(context.Background(), scopeLess); err == nil {
		t.Error("scope-less retained read accepted by the sink")
	}
}

// TestNATSSubjectEncodingInjective proves a project id "_" cannot collide with
// the tenant-wide placeholder: the builders refuse it, so ParseScope can never
// map a project subject to a tenant-wide scope.
func TestNATSSubjectEncodingInjective(t *testing.T) {
	placeholderProject := tenancy.Scope{TenantID: "tenant-1", ProjectID: "_"}
	builders := []struct {
		name  string
		build func() (nats.Subject, error)
	}{
		{"event", func() (nats.Subject, error) { return nats.EventSubject(placeholderProject) }},
		{"invalidation", func() (nats.Subject, error) { return nats.InvalidationSubject(placeholderProject) }},
		{"log", func() (nats.Subject, error) { return nats.LogSubject(placeholderProject, id.ExecutionID("exec-1")) }},
	}
	for _, builder := range builders {
		t.Run(builder.name, func(t *testing.T) {
			if _, err := builder.build(); err == nil {
				t.Fatalf("project id %q accepted; want refusal so the encoding stays injective", nats.TenantWideProject)
			}
		})
	}

	tenantWide, err := nats.EventSubject(tenancy.Scope{TenantID: "tenant-1"})
	if err != nil {
		t.Fatalf("tenant-wide event subject: %v", err)
	}
	parsed, err := nats.ParseScope(tenantWide)
	if err != nil {
		t.Fatalf("ParseScope(tenant-wide): %v", err)
	}
	if parsed.ProjectID != "" {
		t.Fatalf("tenant-wide subject parsed as project %q", parsed.ProjectID)
	}

	projectSubject, err := nats.EventSubject(tenancy.Scope{TenantID: "tenant-1", ProjectID: "a_b"})
	if err != nil {
		t.Fatalf("project event subject: %v", err)
	}
	parsed, err = nats.ParseScope(projectSubject)
	if err != nil {
		t.Fatalf("ParseScope(project): %v", err)
	}
	if parsed.ProjectID != "a_b" {
		t.Fatalf("project subject parsed as project %q; want a_b", parsed.ProjectID)
	}
	if projectSubject == tenantWide {
		t.Fatal("project and tenant-wide event subjects collide")
	}
}
