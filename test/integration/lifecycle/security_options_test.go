//go:build integration

package lifecycle

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/shell/integration/oidc"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	identityfixture "github.com/optimiware/kodama/test/support/boundaries/identity"
	"github.com/optimiware/kodama/test/support/factory"
)

// TestFactoryTLSWorkloadRoundTrip proves the opt-in harness serves real HTTPS
// with a private CA: the workload URL is HTTPS, the exported CA is the only
// trust needed by an ordinary client, and the same CA validates the service name
// a workload actually dials through the forwarder.
func TestFactoryTLSWorkloadRoundTrip(t *testing.T) {
	beforeTransport := http.DefaultTransport
	if _, ok := beforeTransport.(*http.Transport); !ok {
		t.Fatalf("http.DefaultTransport = %T; want *http.Transport", beforeTransport)
	}

	harness := factory.Start(t, factory.WithTLS())

	if http.DefaultTransport != beforeTransport {
		t.Fatalf("harness replaced http.DefaultTransport")
	}
	// The standard library's own lazy HTTP/2 configuration may publish an empty
	// default TLSClientConfig on the process transport when any helper performs
	// a plain-HTTP request. That is not a harness change, so the guard is that
	// the harness never installs its private CA, disables verification or lowers
	// the TLS floor process-wide.
	if current, ok := http.DefaultTransport.(*http.Transport); !ok {
		t.Fatalf("http.DefaultTransport = %T after harness start; want *http.Transport", http.DefaultTransport)
	} else if current.TLSClientConfig != nil {
		if current.TLSClientConfig.InsecureSkipVerify {
			t.Fatalf("harness installed InsecureSkipVerify process-wide")
		}
		if current.TLSClientConfig.RootCAs != nil {
			t.Fatalf("harness installed a private CA pool process-wide")
		}
		if current.TLSClientConfig.MinVersion != 0 && current.TLSClientConfig.MinVersion < tls.VersionTLS12 {
			t.Fatalf("harness lowered the process-wide TLS minimum to %x", current.TLSClientConfig.MinVersion)
		}
	}
	if harness.TLS == nil {
		t.Fatalf("WithTLS returned a harness without TLS material")
	}
	if len(harness.TLS.CAPEM) == 0 {
		t.Fatalf("WithTLS exported no CA material")
	}
	if harness.WorkloadAddress == "" {
		t.Fatalf("WithTLS exposed no concrete workload edge address")
	}

	workload, err := url.Parse(harness.WorkloadURL)
	if err != nil {
		t.Fatalf("parse workload URL %q: %v", harness.WorkloadURL, err)
	}
	if workload.Scheme != "https" {
		t.Fatalf("workload URL scheme = %q; want https", workload.Scheme)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(harness.TLS.CAPEM) {
		t.Fatalf("exported CA material is not a usable certificate pool")
	}
	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			RootCAs:    pool,
			MinVersion: tls.VersionTLS12,
		}},
		Timeout: 15 * time.Second,
	}

	// An ordinary client built from the exported CA reaches the public edge.
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, harness.BaseURL+"/mcp", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build MCP request: %v", err)
	}
	request.Header.Set("Content-Type", api.MediaType)
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("trusted client request over HTTPS: %v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("trusted client status = %d; want %d", response.StatusCode, http.StatusOK)
	}

	// The harness's own public edge transport must also use the private CA, so
	// every existing REST/MCP helper keeps working unchanged under WithTLS.
	harness.MustREST(harness.AdminToken, "membership.list", api.MembershipListRequest{ProjectID: factory.ProjectID}, nil, "")

	// The certificate the harness actually wired covers every real dialling
	// name, plus the concrete host this run's workload edge binds.
	for _, host := range []string{
		"127.0.0.1",
		"::1",
		"localhost",
		"host.containers.internal",
		factory.KubernetesHostAlias,
	} {
		if err := harness.TLS.Leaf().VerifyHostname(host); err != nil {
			t.Fatalf("wired service certificate does not cover %q: %v", host, err)
		}
	}
	listenHost, _, err := net.SplitHostPort(harness.WorkloadAddress)
	if err != nil {
		t.Fatalf("split workload address %q: %v", harness.WorkloadAddress, err)
	}
	if listenHost == "" {
		t.Fatalf("workload address %q has no host", harness.WorkloadAddress)
	}
	if err := harness.TLS.Leaf().VerifyHostname(listenHost); err != nil {
		t.Fatalf("wired service certificate does not cover the bound host %q: %v", listenHost, err)
	}

	// The transport the harness REST/MCP helpers really use is the fixture's
	// private-CA transport: it trusts BaseURL and refuses a server signed by a
	// foreign CA. This pins the helper path, not just fixture.Client().
	helperTransport := harness.Client("helper-probe").RoundTripper
	if helperTransport == nil {
		t.Fatalf("harness helper client has no round tripper")
	}
	helperClient := &http.Client{Transport: helperTransport, Timeout: 15 * time.Second}
	helperRequest, err := http.NewRequestWithContext(t.Context(), http.MethodPost, harness.BaseURL+"/mcp", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build helper MCP request: %v", err)
	}
	helperRequest.Header.Set("Content-Type", api.MediaType)
	helperResponse, err := helperClient.Do(helperRequest)
	if err != nil {
		t.Fatalf("harness helper transport refused the fixture-CA edge: %v", err)
	}
	_ = helperResponse.Body.Close()
	if helperResponse.StatusCode != http.StatusOK {
		t.Fatalf("harness helper transport status = %d; want %d", helperResponse.StatusCode, http.StatusOK)
	}

	foreign := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(foreign.Close)
	foreignResponse, foreignErr := helperClient.Get(foreign.URL)
	if foreignErr == nil {
		_ = foreignResponse.Body.Close()
		t.Fatalf("harness helper transport accepted a server signed by a foreign CA")
	}
	if _, ok := errors.AsType[x509.UnknownAuthorityError](foreignErr); !ok {
		t.Fatalf("harness helper transport foreign error = %v; want an unknown-authority refusal", foreignErr)
	}

	// The workload-facing certificate covers the alias a workload dials, and the
	// same exported CA validates it through the real forwarder.
	connection, err := tls.DialWithDialer(
		&net.Dialer{Timeout: 10 * time.Second},
		"tcp",
		harness.WorkloadAddress,
		&tls.Config{RootCAs: pool, ServerName: workload.Hostname(), MinVersion: tls.VersionTLS12},
	)
	if err != nil {
		t.Fatalf("trusted client validate workload alias %q: %v", workload.Hostname(), err)
	}
	_ = connection.Close()
}

// TestFactoryTLSRejectsUntrustedCA proves the opt-in HTTPS edge is not
// effectively insecure: an ordinary client using the system trust store, without
// the exported CA, is refused with a certificate-verification error rather than
// connecting.
func TestFactoryTLSRejectsUntrustedCA(t *testing.T) {
	harness := factory.Start(t, factory.WithTLS())

	workload, err := url.Parse(harness.WorkloadURL)
	if err != nil {
		t.Fatalf("parse workload URL %q: %v", harness.WorkloadURL, err)
	}
	if workload.Scheme != "https" {
		t.Fatalf("workload URL scheme = %q; want https", workload.Scheme)
	}

	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}},
		Timeout:   15 * time.Second,
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, harness.BaseURL+"/mcp", bytes.NewReader([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)))
	if err != nil {
		t.Fatalf("build MCP request: %v", err)
	}
	request.Header.Set("Content-Type", api.MediaType)
	response, err := client.Do(request)
	if err == nil {
		_ = response.Body.Close()
		t.Fatalf("system-trust client connected to the private-CA edge")
	}
	if _, ok := errors.AsType[*tls.CertificateVerificationError](err); !ok {
		t.Fatalf("client error = %v; want a TLS certificate verification error", err)
	}
	if _, ok := errors.AsType[x509.UnknownAuthorityError](err); !ok {
		t.Fatalf("client error = %v; want an unknown-authority refusal", err)
	}
}

// TestFactoryOIDCUsesCurrentMembership proves humans authenticate through the
// real OIDC verifier and receive only their current stored membership: the same
// signed token is unauthorized with no membership, is a reviewer after a
// reviewer grant (despite an admin role claim), and is an admin after an admin
// grant.
func TestFactoryOIDCUsesCurrentMembership(t *testing.T) {
	harness := factory.Start(t, factory.WithOIDC())
	if harness.OIDC == nil {
		t.Fatalf("WithOIDC returned a harness without an OIDC fixture")
	}

	// The bootstrapped OIDC human is the tenant-wide admin identity: its
	// authority came from a verified token (the real verifier fetched the JWKS),
	// not from a static-token substitute.
	adminToken := harness.OIDC.AdminToken(t)
	harness.MustREST(adminToken, "membership.list", api.MembershipListRequest{ProjectID: factory.ProjectID}, nil, "")
	if harness.OIDC.Provider.JWKSFetchCount() == 0 {
		t.Fatalf("OIDC access token was accepted without the real verifier fetching the JWKS")
	}

	const subject = "oidc-current-membership"
	principalID, err := oidc.PrincipalIDFor(harness.OIDC.Issuer, subject)
	if err != nil {
		t.Fatalf("derive OIDC principal id: %v", err)
	}
	token := harness.OIDC.Mint(t, identityfixture.TokenSpec{
		Subject: subject,
		// The token claims admin. Only the current stored membership may grant it.
		Roles: []string{string(tenancy.RoleAdmin)},
	})

	// A correctly signed token with no committed membership is unauthenticated.
	_, missing := harness.REST(token, "membership.list", api.MembershipListRequest{ProjectID: factory.ProjectID}, nil, "")
	if missing.Code != api.CodeUnauthenticated {
		t.Fatalf("token without current membership code = %q (%s); want %q",
			missing.Code, missing.Message, api.CodeUnauthenticated)
	}

	// The current admin grants the project reviewer role through the public API.
	reviewerGrantResult := harness.MustREST(adminToken, "membership.grant", api.MembershipGrantRequest{
		ProjectID:   factory.ProjectID,
		PrincipalID: string(principalID),
		Roles:       []string{string(tenancy.RoleReviewer)},
	}, nil, "oidc-grant-reviewer")
	var reviewerGrant api.MembershipGrantResult
	harness.Decode(reviewerGrantResult, &reviewerGrant)
	if reviewerGrant.Version == 0 {
		t.Fatalf("reviewer membership grant returned no committed version")
	}

	// The same token is now bounded by the stored reviewer membership: the admin
	// claim is ignored, an admin-only project operation is refused, and a
	// reviewer-scoped inbox read succeeds.
	_, forbidden := harness.REST(token, "secret.write", api.SecretWriteRequest{
		ProjectID: factory.ProjectID,
		SecretID:  "OIDC_REVIEWER_DENIED",
		Value:     "reviewer-must-not-write",
	}, nil, "oidc-reviewer-denied")
	if forbidden.Code != api.CodeForbidden {
		t.Fatalf("reviewer membership with admin claim code = %q (%s); want %q",
			forbidden.Code, forbidden.Message, api.CodeForbidden)
	}
	harness.MustREST(token, "task.inbox", api.TaskInboxRequest{ProjectID: factory.ProjectID}, nil, "oidc-reviewer-inbox")

	// The same token follows the membership upgrade without reissuance: the
	// project Admin grant now admits the admin-only write the reviewer row
	// refused.
	upgradeVersion := reviewerGrant.Version
	harness.MustREST(adminToken, "membership.grant", api.MembershipGrantRequest{
		ProjectID:   factory.ProjectID,
		PrincipalID: string(principalID),
		Roles:       []string{string(tenancy.RoleAdmin)},
	}, &upgradeVersion, "oidc-grant-admin")
	harness.MustREST(token, "secret.write", api.SecretWriteRequest{
		ProjectID: factory.ProjectID,
		SecretID:  "OIDC_ADMIN_UPGRADED",
		Value:     "admin-may-write",
	}, nil, "oidc-admin-upgraded")
}

// TestFactoryOIDCAuthorityNegatives proves the OIDC path refuses authority on
// the next request, not only on token issuance: a revoked membership is
// unauthenticated with the same token, a downgrade withdraws a write and keeps
// the remaining role, and a foreign tenant or project claim is refused at the
// scope boundary.
func TestFactoryOIDCAuthorityNegatives(t *testing.T) {
	harness := factory.Start(t, factory.WithOIDC())
	if harness.OIDC == nil {
		t.Fatalf("WithOIDC returned a harness without an OIDC fixture")
	}

	adminToken := harness.OIDC.AdminToken(t)
	harness.MustREST(adminToken, "membership.list", api.MembershipListRequest{ProjectID: factory.ProjectID}, nil, "")

	// (a) A grant followed by membership.revoke is unauthenticated on the very
	// next request with the same still-valid token.
	const revokedSubject = "oidc-revoked-membership"
	revokedPrincipal, err := oidc.PrincipalIDFor(harness.OIDC.Issuer, revokedSubject)
	if err != nil {
		t.Fatalf("derive revoked-subject principal id: %v", err)
	}
	revokedToken := harness.OIDC.Mint(t, identityfixture.TokenSpec{
		Subject: revokedSubject,
		Roles:   []string{string(tenancy.RoleReviewer)},
	})
	revokedGrant := harness.MustREST(adminToken, "membership.grant", api.MembershipGrantRequest{
		ProjectID:   factory.ProjectID,
		PrincipalID: string(revokedPrincipal),
		Roles:       []string{string(tenancy.RoleReviewer)},
	}, nil, "oidc-revoke-grant")
	var revokedGrantResult api.MembershipGrantResult
	harness.Decode(revokedGrant, &revokedGrantResult)
	harness.MustREST(revokedToken, "task.inbox", api.TaskInboxRequest{ProjectID: factory.ProjectID}, nil, "oidc-revoke-inbox")
	harness.MustREST(adminToken, "membership.revoke", api.MembershipRevokeRequest{
		ProjectID:   factory.ProjectID,
		PrincipalID: string(revokedPrincipal),
	}, &revokedGrantResult.Version, "oidc-revoke")
	_, afterRevoke := harness.REST(revokedToken, "task.inbox", api.TaskInboxRequest{ProjectID: factory.ProjectID}, nil, "oidc-revoke-inbox-after")
	if afterRevoke.Code != api.CodeUnauthenticated {
		t.Fatalf("revoked membership code = %q (%s); want %q",
			afterRevoke.Code, afterRevoke.Message, api.CodeUnauthenticated)
	}

	// (b) An admin -> reviewer downgrade on the same token withdraws the
	// admin-only write while the stored reviewer role keeps the token working.
	const downgradeSubject = "oidc-downgraded-membership"
	downgradePrincipal, err := oidc.PrincipalIDFor(harness.OIDC.Issuer, downgradeSubject)
	if err != nil {
		t.Fatalf("derive downgrade-subject principal id: %v", err)
	}
	downgradeToken := harness.OIDC.Mint(t, identityfixture.TokenSpec{
		Subject: downgradeSubject,
		// A stale admin claim must not survive the downgrade either.
		Roles: []string{string(tenancy.RoleAdmin)},
	})
	adminGrant := harness.MustREST(adminToken, "membership.grant", api.MembershipGrantRequest{
		ProjectID:   factory.ProjectID,
		PrincipalID: string(downgradePrincipal),
		Roles:       []string{string(tenancy.RoleAdmin)},
	}, nil, "oidc-downgrade-grant-admin")
	var adminGrantResult api.MembershipGrantResult
	harness.Decode(adminGrant, &adminGrantResult)
	harness.MustREST(downgradeToken, "secret.write", api.SecretWriteRequest{
		ProjectID: factory.ProjectID,
		SecretID:  "OIDC_DOWNGRADE_ADMIN_WRITE",
		Value:     "admin-before-downgrade",
	}, nil, "oidc-downgrade-admin-write")
	downgraded := harness.MustREST(adminToken, "membership.grant", api.MembershipGrantRequest{
		ProjectID:   factory.ProjectID,
		PrincipalID: string(downgradePrincipal),
		Roles:       []string{string(tenancy.RoleReviewer)},
	}, &adminGrantResult.Version, "oidc-downgrade-grant-reviewer")
	var downgradedResult api.MembershipGrantResult
	harness.Decode(downgraded, &downgradedResult)
	if downgradedResult.Version <= adminGrantResult.Version {
		t.Fatalf("downgrade version = %d; want a new version after %d", downgradedResult.Version, adminGrantResult.Version)
	}
	_, afterDowngrade := harness.REST(downgradeToken, "secret.write", api.SecretWriteRequest{
		ProjectID: factory.ProjectID,
		SecretID:  "OIDC_DOWNGRADE_REVIEWER_DENIED",
		Value:     "reviewer-must-not-write",
	}, nil, "oidc-downgrade-denied")
	if afterDowngrade.Code != api.CodeForbidden {
		t.Fatalf("downgraded membership code = %q (%s); want %q",
			afterDowngrade.Code, afterDowngrade.Message, api.CodeForbidden)
	}
	harness.MustREST(downgradeToken, "task.inbox", api.TaskInboxRequest{ProjectID: factory.ProjectID}, nil, "oidc-downgrade-inbox")

	// (c) A correctly signed token for another tenant has no committed
	// membership in this tenant's scopes, so it is unauthenticated.
	wrongTenantToken := harness.OIDC.Mint(t, identityfixture.TokenSpec{
		Subject: harness.OIDC.AdminSubject,
		Tenant:  "tenant-foreign",
	})
	_, wrongTenant := harness.REST(wrongTenantToken, "membership.list", api.MembershipListRequest{ProjectID: factory.ProjectID}, nil, "")
	if wrongTenant.Code != api.CodeUnauthenticated {
		t.Fatalf("wrong-tenant token code = %q (%s); want %q",
			wrongTenant.Code, wrongTenant.Message, api.CodeUnauthenticated)
	}

	// (d) A wrong-project claim is refused at the scope boundary. The
	// tenant-wide administrator authenticates (its membership is tenant-wide)
	// and then a request naming the fixture project is not_found, because the
	// credential's own project differs. A project-scoped subject has no row in
	// the foreign project scope at all, so its resolution is unauthenticated.
	const foreignProject = "project-foreign"
	adminWrongProject := harness.OIDC.Mint(t, identityfixture.TokenSpec{
		Subject: harness.OIDC.AdminSubject,
		Project: foreignProject,
	})
	_, adminForeignProject := harness.REST(adminWrongProject, "membership.list", api.MembershipListRequest{ProjectID: factory.ProjectID}, nil, "")
	if adminForeignProject.Code != api.CodeNotFound {
		t.Fatalf("tenant-wide admin wrong-project code = %q (%s); want %q",
			adminForeignProject.Code, adminForeignProject.Message, api.CodeNotFound)
	}
	scopedWrongProject := harness.OIDC.Mint(t, identityfixture.TokenSpec{
		Subject: downgradeSubject,
		Project: foreignProject,
	})
	_, scopedForeignProject := harness.REST(scopedWrongProject, "membership.list", api.MembershipListRequest{ProjectID: factory.ProjectID}, nil, "")
	if scopedForeignProject.Code != api.CodeUnauthenticated {
		t.Fatalf("project-scoped wrong-project code = %q (%s); want %q",
			scopedForeignProject.Code, scopedForeignProject.Message, api.CodeUnauthenticated)
	}
}

// TestFactoryOIDCRejectsIssuerAudienceExpiry proves the served edge resolves
// humans through the real verifier: correctly signed tokens with a foreign
// issuer, another audience or an expired lifetime are refused as
// unauthenticated, while an otherwise identical valid token is accepted.
func TestFactoryOIDCRejectsIssuerAudienceExpiry(t *testing.T) {
	harness := factory.Start(t, factory.WithOIDC())
	if harness.OIDC == nil {
		t.Fatalf("WithOIDC returned a harness without an OIDC fixture")
	}

	// A valid token is accepted, so the refusals below exercise the verifier and
	// not a map lookup that would reject every OIDC token.
	harness.MustREST(harness.OIDC.AdminToken(t), "membership.list", api.MembershipListRequest{ProjectID: factory.ProjectID}, nil, "")

	cases := []struct {
		name string
		spec identityfixture.TokenSpec
	}{
		{
			name: "issuer",
			spec: identityfixture.TokenSpec{
				Subject: harness.OIDC.AdminSubject,
				Issuer:  "https://issuer.invalid",
			},
		},
		{
			name: "audience",
			spec: identityfixture.TokenSpec{
				Subject:  harness.OIDC.AdminSubject,
				Audience: "another-audience",
			},
		},
		{
			name: "expiry",
			spec: identityfixture.TokenSpec{
				Subject:   harness.OIDC.AdminSubject,
				ExpiresAt: harness.OIDC.Provider.Clock().Now().Add(-time.Minute),
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			token := harness.OIDC.Mint(t, testCase.spec)
			_, publicError := harness.REST(token, "membership.list", api.MembershipListRequest{ProjectID: factory.ProjectID}, nil, "")
			if publicError.Code != api.CodeUnauthenticated {
				t.Fatalf("%s token code = %q (%s); want %q",
					testCase.name, publicError.Code, publicError.Message, api.CodeUnauthenticated)
			}
		})
	}
}

// TestFactorySecurityOptionsCloseJoins proves cancellation closes every
// resource the security options own and joins the serving work: after Close the
// serving goroutine has returned, the workload forwarder and the public TLS
// listener refuse connections, the OIDC provider is closed, and Close stays
// idempotent.
func TestFactorySecurityOptionsCloseJoins(t *testing.T) {
	harness := factory.Start(t, factory.WithTLS(), factory.WithOIDC())
	if harness.TLS == nil || harness.OIDC == nil {
		t.Fatalf("combined security options returned TLS=%v OIDC=%v", harness.TLS != nil, harness.OIDC != nil)
	}
	if harness.WorkloadAddress == "" {
		t.Fatalf("combined security options exposed no concrete workload edge address")
	}

	base, err := url.Parse(harness.BaseURL)
	if err != nil {
		t.Fatalf("parse base URL %q: %v", harness.BaseURL, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(harness.TLS.CAPEM) {
		t.Fatalf("exported CA material is not a usable certificate pool")
	}
	issuer := harness.OIDC.Issuer
	oidcClient := harness.OIDC.Provider.ClientWithTimeout(2 * time.Second)
	served := harness.ServingDone()
	if served == nil {
		t.Fatalf("harness exposed no serving-done barrier")
	}

	harness.Close()
	// Close must have joined the serving goroutine before it returned. The
	// barrier is non-blocking on purpose: a bounded wait here would pass even
	// when Close never joined and the goroutine merely finished on its own.
	select {
	case <-served:
	default:
		t.Fatalf("Serve was not joined by Close")
	}
	// Close is idempotent and joins the already-returned serving work.
	harness.Close()

	if connection, err := net.DialTimeout("tcp", harness.WorkloadAddress, 2*time.Second); err == nil {
		_ = connection.Close()
		t.Fatalf("workload forwarder still accepts connections after close")
	}
	connection, err := tls.DialWithDialer(
		&net.Dialer{Timeout: 2 * time.Second},
		"tcp",
		base.Host,
		&tls.Config{RootCAs: pool, ServerName: base.Hostname(), MinVersion: tls.VersionTLS12},
	)
	if err == nil {
		_ = connection.Close()
		t.Fatalf("public TLS listener still accepts connections after close")
	}

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, issuer+"/.well-known/openid-configuration", nil)
	if err != nil {
		t.Fatalf("build OIDC discovery request: %v", err)
	}
	response, err := oidcClient.Do(request)
	if err == nil {
		_ = response.Body.Close()
		t.Fatalf("OIDC provider still serves discovery after close")
	}
}
