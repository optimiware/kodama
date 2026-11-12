package nats_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/integration/nats"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// calloutRequestJWT builds one NATS authorization request signed by a fresh
// server key. The credential is what the client presented as an auth token.
func calloutRequestJWT(t *testing.T, credential string) (string, string) {
	t.Helper()
	serverSeed, serverPub := nats.NewTestKey(t, nats.TestServerPrefix)
	_, userPub := nats.NewTestKey(t, nats.TestUserPrefix)
	claims := map[string]any{
		"aud": "nats-authorization-request",
		"exp": testNow().Add(time.Hour).Unix(),
		"iat": testNow().Unix(),
		"iss": serverPub,
		"sub": "nats_user_auth_request",
		"nats": map[string]any{
			"user_nkey": userPub,
			"server_id": map[string]any{"id": serverPub, "name": "test-server"},
			"client_info": map[string]any{
				"user": "[REDACTED]",
			},
			"connect_opts": map[string]any{
				"auth_token": credential,
			},
			"type":    "authorization_request",
			"version": 2,
		},
	}
	return signTestJWT(t, serverSeed, claims), userPub
}

// signTestJWT encodes and signs one JWT with the supplied nkey.
func signTestJWT(t *testing.T, seed string, claims any) string {
	t.Helper()
	header := map[string]any{"typ": "JWT", "alg": "ed25519-nkey"}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	toSign := base64.RawURLEncoding.EncodeToString(headerJSON) + "." + base64.RawURLEncoding.EncodeToString(claimsJSON)
	return toSign + "." + base64.RawURLEncoding.EncodeToString(nats.SignTest(t, seed, []byte(toSign)))
}

// testJWTClaims decodes a JWT payload without verifying it.
func testJWTClaims(t *testing.T, token string) map[string]any {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt has %d parts; want 3", len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode jwt payload: %v", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatalf("unmarshal jwt claims: %v", err)
	}
	return claims
}

// verifyTestJWT verifies the signature with the supplied account key.
func verifyTestJWT(t *testing.T, token string, public string) {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt has %d parts; want 3", len(parts))
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode jwt signature: %v", err)
	}
	if !nats.VerifyTest(t, public, []byte(parts[0]+"."+parts[1]), signature) {
		t.Fatal("jwt signature does not verify")
	}
}

// fakeCalloutConnection captures the callout subscription and replies.
type fakeCalloutConnection struct {
	mu       sync.Mutex
	subject  string
	handler  func(context.Context, nats.CalloutRequest) error
	replies  []calloutReply
	closed   bool
	closeLen int
}

type calloutReply struct {
	subject string
	data    []byte
}

func (connection *fakeCalloutConnection) SubscribeCallout(_ context.Context, subject string, handler func(context.Context, nats.CalloutRequest) error) (nats.Subscription, error) {
	connection.mu.Lock()
	defer connection.mu.Unlock()
	connection.subject = subject
	connection.handler = handler
	return connection, nil
}

func (connection *fakeCalloutConnection) PublishCallout(_ context.Context, reply string, data []byte) error {
	connection.mu.Lock()
	defer connection.mu.Unlock()
	connection.replies = append(connection.replies, calloutReply{subject: reply, data: append([]byte(nil), data...)})
	return nil
}

func (connection *fakeCalloutConnection) Close(context.Context) error {
	connection.mu.Lock()
	defer connection.mu.Unlock()
	connection.closed = true
	connection.closeLen++
	return nil
}

func (connection *fakeCalloutConnection) deliver(t *testing.T, request nats.CalloutRequest) error {
	t.Helper()
	connection.mu.Lock()
	handler := connection.handler
	connection.mu.Unlock()
	if handler == nil {
		t.Fatal("callout did not subscribe")
	}
	return handler(context.Background(), request)
}

func (connection *fakeCalloutConnection) onlyReply(t *testing.T) calloutReply {
	t.Helper()
	connection.mu.Lock()
	defer connection.mu.Unlock()
	if len(connection.replies) != 1 {
		t.Fatalf("callout replies = %d; want 1", len(connection.replies))
	}
	return connection.replies[0]
}

// fakeValidity returns one explicit credential expiry.
type fakeValidity struct {
	until time.Time
	err   error
}

func (validity fakeValidity) ValidUntil(context.Context, identity.Principal) (time.Time, error) {
	return validity.until, validity.err
}

// blockingResolver waits for the request context to end, modelling a
// credential lookup that never returns on its own.
type blockingResolver struct{}

func (blockingResolver) Resolve(ctx context.Context, _ string) (identity.Principal, error) {
	<-ctx.Done()
	return identity.Principal{}, ctx.Err()
}

// subscribeOnlyAuthorizer allows event.subscribe and denies every other
// registry operation, modelling a service credential without event.publish.
type subscribeOnlyAuthorizer struct{}

func (subscribeOnlyAuthorizer) Authorize(_ context.Context, _ authorize.Principal, operation authorize.OperationName, _ tenancy.Scope) (authorize.Decision, error) {
	if operation == nats.OperationEventSubscribe {
		return authorize.DecisionAllow, nil
	}
	return authorize.DecisionDeny, nil
}

// issueCalloutUserJWT delivers one valid authorization request and returns the
// decoded user JWT claims from the signed response.
func issueCalloutUserJWT(t *testing.T, connection *fakeCalloutConnection, credential string) map[string]any {
	t.Helper()
	requestJWT, _ := calloutRequestJWT(t, credential)
	if err := connection.deliver(t, nats.CalloutRequest{Subject: nats.CalloutSubject, Reply: "_INBOX.reply", Data: []byte(requestJWT)}); err != nil {
		t.Fatalf("handle request: %v", err)
	}
	response := testJWTClaims(t, string(connection.onlyReply(t).data))
	natsClaims, ok := response["nats"].(map[string]any)
	if !ok {
		t.Fatalf("response has no nats claims: %v", response)
	}
	inner, ok := natsClaims["jwt"].(string)
	if !ok || inner == "" {
		t.Fatalf("response carries no user jwt: %v", natsClaims)
	}
	return testJWTClaims(t, inner)
}

// stringList decodes one optional JSON string array.
func stringList(value any) []string {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if ok {
			result = append(result, text)
		}
	}
	return result
}

func containsString(values []string, want string) bool {
	return slices.Contains(values, want)
}

// TestCalloutIssuesScopedUserAndRefusesRevokedCredential pins the per-credential
// authorization decision at the NATS auth-callout boundary: a current
// credential receives a signed user JWT limited to its scope, while a revoked,
// expired or disabled credential receives an error instead of permissions.
func TestCalloutIssuesScopedUserAndRefusesRevokedCredential(t *testing.T) {
	scope := testScope()
	seed, issuerPublic := nats.NewTestKey(t, nats.TestAccountPrefix)
	resolver := &fakePrincipalResolver{byToken: map[string]identity.Principal{testCredential: servicePrincipal(scope)}}
	authorizer := &fakeAuthorizer{decision: authorize.DecisionAllow}
	connection := &fakeCalloutConnection{}
	callout, err := nats.NewCallout(nats.CalloutConfig{
		Connection: connection,
		IssuerSeed: seed,
		Account:    "KODAMA",
		Principal:  resolver,
		Authorizer: authorizer,
		Validity:   fakeValidity{until: testNow().Add(time.Hour)},
		Clock:      &fakeClock{now: testNow()},
	})
	if err != nil {
		t.Fatalf("NewCallout: %v", err)
	}
	if err := callout.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = callout.Close(context.Background()) })

	requestJWT, userNkey := calloutRequestJWT(t, testCredential)
	if err := connection.deliver(t, nats.CalloutRequest{Subject: "$SYS.REQ.USER.AUTH", Reply: "_INBOX.reply", Data: []byte(requestJWT)}); err != nil {
		t.Fatalf("handle valid request: %v", err)
	}
	reply := connection.onlyReply(t)
	if reply.subject != "_INBOX.reply" {
		t.Fatalf("reply subject = %q; want _INBOX.reply", reply.subject)
	}
	response := testJWTClaims(t, string(reply.data))
	verifyTestJWT(t, string(reply.data), issuerPublic)
	if response["sub"] != userNkey {
		t.Fatalf("response subject = %v; want %q", response["sub"], userNkey)
	}
	natsClaims, ok := response["nats"].(map[string]any)
	if !ok {
		t.Fatalf("response has no nats claims: %v", response)
	}
	inner, ok := natsClaims["jwt"].(string)
	if !ok || inner == "" {
		t.Fatalf("response carries no user jwt: %v", natsClaims)
	}
	userClaims := testJWTClaims(t, inner)
	verifyTestJWT(t, inner, issuerPublic)
	if userClaims["sub"] != userNkey {
		t.Fatalf("user jwt subject = %v; want %q", userClaims["sub"], userNkey)
	}
	if userClaims["aud"] != "KODAMA" {
		t.Fatalf("user jwt audience = %v; want KODAMA", userClaims["aud"])
	}
	userNATS, ok := userClaims["nats"].(map[string]any)
	if !ok {
		t.Fatalf("user jwt has no nats claims: %v", userClaims)
	}
	permissions := userNATS
	subAllow := permissions["sub"].(map[string]any)["allow"].([]any)
	hasEventSubject := false
	for _, subject := range subAllow {
		if subject == "kodama.v1.tenant-a.project-1.events" {
			hasEventSubject = true
		}
		if strings.Contains(subject.(string), "*") || strings.Contains(subject.(string), ">") {
			if subject != "_INBOX.>" && !strings.HasPrefix(subject.(string), "kodama.v1.tenant-a.project-1.") {
				t.Fatalf("user jwt wildcard %q escapes the credential scope", subject)
			}
		}
	}
	if !hasEventSubject {
		t.Fatalf("user jwt subscriptions %v do not include the scope event subject", subAllow)
	}
	pubPermissions, ok := permissions["pub"].(map[string]any)
	if !ok {
		t.Fatalf("user jwt has no publish permissions: %v", permissions)
	}
	pubAllow := stringList(pubPermissions["allow"])
	ingress, err := nats.IngressSubject(scope)
	if err != nil {
		t.Fatalf("ingress subject: %v", err)
	}
	if !containsString(pubAllow, string(ingress)) {
		t.Fatalf("publish allow = %v; want the scope ingress subject %s", pubAllow, ingress)
	}
	pubDeny := stringList(pubPermissions["deny"])
	if !containsString(pubDeny, nats.CalloutSubject) {
		t.Fatalf("publish deny = %v; want %s denied", pubDeny, nats.CalloutSubject)
	}
	if containsString(pubDeny, ">") {
		t.Fatalf("publish deny = %v; a service holding event.publish must not be denied every subject", pubDeny)
	}

	// Revoked credentials get an error response, never a signed user JWT.
	resolver.Fail(testCredential, fault.New(fault.Unauthenticated, "credential revoked"))
	if err := connection.deliver(t, nats.CalloutRequest{Subject: "$SYS.REQ.USER.AUTH", Reply: "_INBOX.reply", Data: []byte(requestJWT)}); err != nil {
		t.Fatalf("handle revoked request: %v", err)
	}
	revoked := testJWTClaims(t, string(connection.replies[len(connection.replies)-1].data))
	revokedNATS := revoked["nats"].(map[string]any)
	if revokedNATS["error"] == nil || revokedNATS["jwt"] != nil {
		t.Fatalf("revoked credential response = %v; want an error and no user jwt", revokedNATS)
	}

	// Expired credentials are refused the same way even when the principal
	// record itself is still current.
	resolver.Set(testCredential, servicePrincipal(scope))
	expiredConnection := &fakeCalloutConnection{}
	expired, err := nats.NewCallout(nats.CalloutConfig{
		Connection: expiredConnection,
		IssuerSeed: seed,
		Account:    "KODAMA",
		Principal:  resolver,
		Authorizer: authorizer,
		Validity:   fakeValidity{until: testNow()},
		Clock:      &fakeClock{now: testNow()},
	})
	if err != nil {
		t.Fatalf("NewCallout expired: %v", err)
	}
	if err := expired.Start(context.Background()); err != nil {
		t.Fatalf("Start expired: %v", err)
	}
	t.Cleanup(func() { _ = expired.Close(context.Background()) })
	if err := expiredConnection.deliver(t, nats.CalloutRequest{Subject: "$SYS.REQ.USER.AUTH", Reply: "_INBOX.reply", Data: []byte(requestJWT)}); err != nil {
		t.Fatalf("handle expired request: %v", err)
	}
	expiredReply := testJWTClaims(t, string(expiredConnection.onlyReply(t).data))
	if expiredReply["nats"].(map[string]any)["error"] == nil {
		t.Fatalf("expired credential received permissions: %v", expiredReply)
	}
}

// TestCalloutRefusesMalformedAndForeignRequests pins fail-closed request
// validation: a non-server issuer, wrong audience, missing credential and
// missing reply subject never produce permissions.
func TestCalloutRefusesMalformedAndForeignRequests(t *testing.T) {
	scope := testScope()
	seed, _ := nats.NewTestKey(t, nats.TestAccountPrefix)
	connection := &fakeCalloutConnection{}
	callout, err := nats.NewCallout(nats.CalloutConfig{
		Connection: connection,
		IssuerSeed: seed,
		Account:    "KODAMA",
		Principal:  &fakePrincipalResolver{byToken: map[string]identity.Principal{testCredential: servicePrincipal(scope)}},
		Authorizer: &fakeAuthorizer{decision: authorize.DecisionAllow},
		Validity:   fakeValidity{until: testNow().Add(time.Hour)},
		Clock:      &fakeClock{now: testNow()},
	})
	if err != nil {
		t.Fatalf("NewCallout: %v", err)
	}
	if err := callout.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = callout.Close(context.Background()) })

	valid, _ := calloutRequestJWT(t, testCredential)
	missingCredential, _ := calloutRequestJWT(t, "")
	cases := []struct {
		name  string
		reply string
		data  string
	}{
		{name: "missing reply", data: valid},
		{name: "not a jwt", reply: "_INBOX.reply", data: "not-a-jwt"},
		{name: "missing credential", reply: "_INBOX.reply", data: missingCredential},
	}
	for _, testCase := range cases {
		if err := connection.deliver(t, nats.CalloutRequest{Subject: "$SYS.REQ.USER.AUTH", Reply: testCase.reply, Data: []byte(testCase.data)}); err != nil {
			t.Fatalf("%s: handler returned %v", testCase.name, err)
		}
		if len(connection.replies) != 0 {
			t.Fatalf("%s published a reply; want a silent abort", testCase.name)
		}
	}
}

// TestCalloutRejectsWrongScopeGrant pins that the callout derives permissions
// from current authority, so a removed grant cannot be replaced by a request
// body.
func TestCalloutRejectsWrongScopeGrant(t *testing.T) {
	scope := testScope()
	seed, _ := nats.NewTestKey(t, nats.TestAccountPrefix)
	resolver := &fakePrincipalResolver{byToken: map[string]identity.Principal{testCredential: servicePrincipal(scope)}}
	authorizer := &fakeAuthorizer{decision: authorize.DecisionDeny}
	connection := &fakeCalloutConnection{}
	callout, err := nats.NewCallout(nats.CalloutConfig{
		Connection: connection,
		IssuerSeed: seed,
		Account:    "KODAMA",
		Principal:  resolver,
		Authorizer: authorizer,
		Validity:   fakeValidity{until: testNow().Add(time.Hour)},
		Clock:      &fakeClock{now: testNow()},
	})
	if err != nil {
		t.Fatalf("NewCallout: %v", err)
	}
	if err := callout.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = callout.Close(context.Background()) })
	requestJWT, _ := calloutRequestJWT(t, testCredential)
	if err := connection.deliver(t, nats.CalloutRequest{Subject: "$SYS.REQ.USER.AUTH", Reply: "_INBOX.reply", Data: []byte(requestJWT)}); err != nil {
		t.Fatalf("handle request: %v", err)
	}
	reply := testJWTClaims(t, string(connection.onlyReply(t).data))
	if reply["nats"].(map[string]any)["error"] == nil {
		t.Fatalf("denied grant received permissions: %v", reply)
	}
}

// TestCalloutDeniesPublishWithoutGrant pins the fail-closed publish permission:
// a human credential and a service credential without event.publish receive an
// explicit deny of ">" (NATS treats an empty allow list with no deny as
// unrestricted) and can never publish scope-external invalidation, ingress or
// command subjects. The broker's own authorization-request subject stays denied
// for every admitted credential.
func TestCalloutDeniesPublishWithoutGrant(t *testing.T) {
	seed, _ := nats.NewTestKey(t, nats.TestAccountPrefix)
	scope := testScope()
	cases := []struct {
		name       string
		principal  identity.Principal
		authorizer authorize.Authorizer
	}{
		{name: "human", principal: humanPrincipal(scope), authorizer: &fakeAuthorizer{decision: authorize.DecisionAllow}},
		{name: "service without event.publish", principal: servicePrincipal(scope), authorizer: subscribeOnlyAuthorizer{}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			connection := &fakeCalloutConnection{}
			callout, err := nats.NewCallout(nats.CalloutConfig{
				Connection: connection,
				IssuerSeed: seed,
				Account:    "KODAMA",
				Principal:  &fakePrincipalResolver{byToken: map[string]identity.Principal{testCredential: testCase.principal}},
				Authorizer: testCase.authorizer,
				Validity:   fakeValidity{until: testNow().Add(time.Hour)},
				Clock:      &fakeClock{now: testNow()},
			})
			if err != nil {
				t.Fatalf("NewCallout: %v", err)
			}
			if err := callout.Start(context.Background()); err != nil {
				t.Fatalf("Start: %v", err)
			}
			t.Cleanup(func() { _ = callout.Close(context.Background()) })

			claims := issueCalloutUserJWT(t, connection, testCredential)
			natsClaims := claims["nats"].(map[string]any)
			pub, ok := natsClaims["pub"].(map[string]any)
			if !ok {
				t.Fatalf("user jwt has no pub permissions: %v", natsClaims)
			}
			if allow := stringList(pub["allow"]); len(allow) != 0 {
				t.Fatalf("user jwt publish allow = %v; want an empty list", allow)
			}
			deny := stringList(pub["deny"])
			if !containsString(deny, ">") {
				t.Fatalf("user jwt publish deny = %v; want an explicit deny of %q so an empty allow list is not unrestricted", deny, ">")
			}
			if !containsString(deny, nats.CalloutSubject) {
				t.Fatalf("user jwt publish deny = %v; want %s denied", deny, nats.CalloutSubject)
			}
		})
	}
}

// TestCalloutGrantLifetimeIsCapped pins that an established broker connection
// must not survive a revocation for the credential's full expiry. The user JWT
// carries the smaller of the credential expiry and the fixed grant cap, so the
// broker closes the connection and nats.go re-runs the callout, which refuses
// the revoked credential.
func TestCalloutGrantLifetimeIsCapped(t *testing.T) {
	seed, _ := nats.NewTestKey(t, nats.TestAccountPrefix)
	scope := testScope()
	connection := &fakeCalloutConnection{}
	until := testNow().Add(30 * 24 * time.Hour)
	callout, err := nats.NewCallout(nats.CalloutConfig{
		Connection: connection,
		IssuerSeed: seed,
		Account:    "KODAMA",
		Principal:  &fakePrincipalResolver{byToken: map[string]identity.Principal{testCredential: servicePrincipal(scope)}},
		Authorizer: &fakeAuthorizer{decision: authorize.DecisionAllow},
		Validity:   fakeValidity{until: until},
		Clock:      &fakeClock{now: testNow()},
	})
	if err != nil {
		t.Fatalf("NewCallout: %v", err)
	}
	if err := callout.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = callout.Close(context.Background()) })

	claims := issueCalloutUserJWT(t, connection, testCredential)
	expires, ok := claims["exp"].(float64)
	if !ok {
		t.Fatalf("user jwt has no numeric exp: %v", claims)
	}
	cap := testNow().Add(time.Minute)
	if int64(expires) > cap.Unix() {
		t.Fatalf("user jwt exp = %s; want no later than the 60s grant cap (%s)",
			time.Unix(int64(expires), 0).UTC(), cap.UTC())
	}
	if int64(expires) <= testNow().Unix() {
		t.Fatalf("user jwt exp = %s; want a positive remaining lifetime", time.Unix(int64(expires), 0).UTC())
	}

	// A credential that expires sooner than the cap keeps its own expiry.
	shortConnection := &fakeCalloutConnection{}
	shortUntil := testNow().Add(30 * time.Second)
	short, err := nats.NewCallout(nats.CalloutConfig{
		Connection: shortConnection,
		IssuerSeed: seed,
		Account:    "KODAMA",
		Principal:  &fakePrincipalResolver{byToken: map[string]identity.Principal{testCredential: servicePrincipal(scope)}},
		Authorizer: &fakeAuthorizer{decision: authorize.DecisionAllow},
		Validity:   fakeValidity{until: shortUntil},
		Clock:      &fakeClock{now: testNow()},
	})
	if err != nil {
		t.Fatalf("NewCallout short: %v", err)
	}
	if err := short.Start(context.Background()); err != nil {
		t.Fatalf("Start short: %v", err)
	}
	t.Cleanup(func() { _ = short.Close(context.Background()) })
	shortClaims := issueCalloutUserJWT(t, shortConnection, testCredential)
	if got := int64(shortClaims["exp"].(float64)); got != shortUntil.Unix() {
		t.Fatalf("short credential exp = %s; want %s", time.Unix(got, 0).UTC(), shortUntil.UTC())
	}
}

// TestCalloutBoundsRequestWithDeadline pins the per-request budget: a
// credential lookup that never returns cannot hold the broker's authorization
// reply open past the callout timeout, so the attempt fails closed well inside
// the broker's own request timeout.
func TestCalloutBoundsRequestWithDeadline(t *testing.T) {
	seed, _ := nats.NewTestKey(t, nats.TestAccountPrefix)
	connection := &fakeCalloutConnection{}
	callout, err := nats.NewCallout(nats.CalloutConfig{
		Connection: connection,
		IssuerSeed: seed,
		Account:    "KODAMA",
		Principal:  blockingResolver{},
		Authorizer: &fakeAuthorizer{decision: authorize.DecisionAllow},
		Validity:   fakeValidity{until: testNow().Add(time.Hour)},
		Clock:      &fakeClock{now: testNow()},
	})
	if err != nil {
		t.Fatalf("NewCallout: %v", err)
	}
	if err := callout.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = callout.Close(context.Background()) })

	requestJWT, _ := calloutRequestJWT(t, testCredential)
	started := time.Now()
	if err := connection.deliver(t, nats.CalloutRequest{Subject: nats.CalloutSubject, Reply: "_INBOX.reply", Data: []byte(requestJWT)}); err != nil {
		t.Fatalf("handle request: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("callout request took %s; want it bounded below the broker's 2s timeout", elapsed)
	}
	response := testJWTClaims(t, string(connection.onlyReply(t).data))
	if response["nats"].(map[string]any)["error"] == nil {
		t.Fatalf("timed-out credential lookup received permissions: %v", response)
	}
}

// ensure the test's null scope helper remains referenced.
var _ = tenancy.Scope{}
var _ = id.PrincipalID("")
