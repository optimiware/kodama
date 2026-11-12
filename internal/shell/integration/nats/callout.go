package nats

// NATS per-credential authorization callout.
//
// The reference broker authenticates every external connection through a
// signed authorization callout instead of one shared client credential. A
// client presents its current Kodama credential (the same bearer token used by
// HTTP and MCP) as the NATS auth token. The broker forwards the request to this
// service, which resolves the credential's current principal, asks the one
// authorizer for the scope's event grants and returns a short-lived user JWT
// limited to that exact scope. A missing, disabled, expired, replaced or
// revoked credential is an error response, so the broker refuses the
// connection before any subject is reachable.
//
// The callout response never widens a grant: the subject list is derived from
// the authenticated scope, not from the request. The service itself connects
// with the broker's bypass user, which is listed in the broker's auth_users and
// can only receive authorization requests.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha512"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

const (
	// CalloutSubject is the declared broker authorization-request subject.
	CalloutSubject = "$SYS.REQ.USER.AUTH"
	// calloutAudience is the request audience the broker sets.
	calloutAudience = "nats-authorization-request"
	// calloutResponseType and calloutRequestType are the declared JWT claim
	// types exchanged with the broker.
	calloutResponseType = "authorization_response"
	calloutUserType     = "user"
	// calloutJWTVersion is the user JWT claim version the broker accepts.
	calloutJWTVersion = 2
	// DefaultGrantLifetime caps one issued user JWT. A grant is always shorter
	// than the credential's own expiry, so the broker closes an established
	// connection and the client re-runs the callout while the credential is
	// still current. Revocation, replacement and disablement then invalidate
	// an established connection within this bound instead of surviving for the
	// credential's full remaining lifetime.
	DefaultGrantLifetime = 60 * time.Second
	// calloutDenyAll is the subject wildcard that turns an empty publish allow
	// list into an explicit deny. NATS treats an empty allow list with no deny
	// as unrestricted, so it must never be emitted alone.
	calloutDenyAll = ">"
	// calloutRequestTimeout bounds one authorization request below the
	// broker's request timeout. A slow dependency fails closed instead of
	// leaving the broker waiting.
	calloutRequestTimeout = 1500 * time.Millisecond
)

// CalloutRequest is one raw authorization request delivered on the system
// subject together with its reply inbox.
type CalloutRequest struct {
	Subject string
	Reply   string
	Data    []byte
}

// CalloutConnection is the narrow broker connection the authorization service
// needs. It is deliberately raw: the system authorization subject is not a
// scope-qualified Kodama subject, and the service only ever receives requests
// and publishes replies to the request's reply inbox.
type CalloutConnection interface {
	SubscribeCallout(context.Context, string, func(context.Context, CalloutRequest) error) (Subscription, error)
	PublishCallout(ctx context.Context, reply string, data []byte) error
}

// CalloutValidity reads the current credential expiry of one principal. It is
// the same current-authority read event.subscribe uses, so a callout grant is
// bounded by the credential and never by a longer cached value.
type CalloutValidity interface {
	ValidUntil(context.Context, identity.Principal) (time.Time, error)
}

// CalloutConfig configures the broker authorization service. Every dependency
// is explicit and construction never dials the broker.
type CalloutConfig struct {
	Connection CalloutConnection
	// IssuerSeed is the account seed that signs every response. The broker is
	// configured with the matching public account key as auth_callout.issuer.
	IssuerSeed string
	// Account is the broker account admitted users are bound to. It must name
	// the account that holds the Kodama streams and the bypass users.
	Account string
	// Principal resolves the presented Kodama credential.
	Principal authorize.CurrentPrincipal
	// Authorizer is the one grant matrix shared by HTTP, MCP and NATS.
	Authorizer authorize.Authorizer
	// Validity bounds a grant by the principal's current credential.
	Validity CalloutValidity
	// Clock is the injected time source.
	Clock Clock
	// GrantLifetime caps every issued grant. Zero selects
	// DefaultGrantLifetime; a negative value is refused.
	GrantLifetime time.Duration
}

// Callout is the live NATS authorization service.
type Callout struct {
	connection    CalloutConnection
	issuer        ed25519.PrivateKey
	issuerID      string
	account       string
	principal     authorize.CurrentPrincipal
	authorizer    authorize.Authorizer
	validity      CalloutValidity
	clock         Clock
	grantLifetime time.Duration

	subscription Subscription
}

// NewCallout validates the configuration and prepares the signing key. It does
// not open a connection.
func NewCallout(config CalloutConfig) (*Callout, error) {
	switch {
	case config.Connection == nil:
		return nil, fault.New(fault.InvalidInput, "nats callout connection is nil")
	case strings.TrimSpace(config.IssuerSeed) == "":
		return nil, fault.New(fault.InvalidInput, "nats callout issuer seed is empty")
	case strings.TrimSpace(config.Account) == "":
		return nil, fault.New(fault.InvalidInput, "nats callout account is empty")
	case config.Principal == nil:
		return nil, fault.New(fault.InvalidInput, "nats callout principal resolver is nil")
	case config.Authorizer == nil:
		return nil, fault.New(fault.InvalidInput, "nats callout authorizer is nil")
	case config.Validity == nil:
		return nil, fault.New(fault.InvalidInput, "nats callout credential validity reader is nil")
	case config.Clock == nil:
		return nil, fault.New(fault.InvalidInput, "nats callout clock is nil")
	case config.GrantLifetime < 0:
		return nil, fault.New(fault.InvalidInput, "nats callout grant lifetime is negative")
	}
	grantLifetime := config.GrantLifetime
	if grantLifetime == 0 {
		grantLifetime = DefaultGrantLifetime
	}
	issuer, issuerID, err := decodeAccountSeed(config.IssuerSeed)
	if err != nil {
		return nil, err
	}
	return &Callout{
		connection:    config.Connection,
		issuer:        issuer,
		issuerID:      issuerID,
		account:       config.Account,
		principal:     config.Principal,
		authorizer:    config.Authorizer,
		validity:      config.Validity,
		clock:         config.Clock,
		grantLifetime: grantLifetime,
	}, nil
}

// Start subscribes to the fixed authorization-request subject. It is
// idempotent only in the sense that a second Start replaces nothing: a live
// service refuses a second subscription.
func (callout *Callout) Start(ctx context.Context) error {
	if callout == nil {
		return fault.New(fault.InvalidInput, "nats callout is nil")
	}
	if callout.subscription != nil {
		return fault.New(fault.InternalFailure, "nats callout is already started")
	}
	if err := ctx.Err(); err != nil {
		return fault.Wrap(fault.DeadlineExpired, "nats callout start was canceled", err)
	}
	subscription, err := callout.connection.SubscribeCallout(ctx, CalloutSubject, callout.handle)
	if err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "nats callout subscription failed", err)
	}
	callout.subscription = subscription
	return nil
}

// Close releases the authorization subscription. It is idempotent.
func (callout *Callout) Close(ctx context.Context) error {
	if callout == nil || callout.subscription == nil {
		return nil
	}
	subscription := callout.subscription
	callout.subscription = nil
	return subscription.Close(ctx)
}

// handle processes one authorization request. A malformed or unauthenticated
// request produces no reply (the broker times out and refuses the connection);
// a current credential gets a signed grant, and a known-but-invalid credential
// gets an explicit error response.
func (callout *Callout) handle(ctx context.Context, request CalloutRequest) error {
	if ctx == nil {
		ctx = context.Background()
	}
	// The broker waits a bounded time for its authorization reply. Never let a
	// slow credential, scope or grant lookup hold that reply open past it.
	ctx, cancel := context.WithTimeout(ctx, calloutRequestTimeout)
	defer cancel()
	if strings.TrimSpace(request.Reply) == "" {
		return nil
	}
	decoded, err := decodeCalloutRequest(request.Data)
	if err != nil {
		// Do not reply: the broker delays and refuses the connection without
		// revealing why a forged request was malformed.
		return nil
	}
	credential := decoded.credential
	if credential == "" {
		return callout.replyError(ctx, request.Reply, decoded, "credential is missing")
	}
	principal, err := callout.principal.Resolve(ctx, credential)
	if err != nil {
		return callout.replyError(ctx, request.Reply, decoded, "credential is not current")
	}
	if err := principal.Validate(); err != nil {
		return callout.replyError(ctx, request.Reply, decoded, "credential principal is invalid")
	}
	if !principal.Enabled {
		return callout.replyError(ctx, request.Reply, decoded, "credential principal is disabled")
	}
	if principal.Scope.Validate() != nil {
		return callout.replyError(ctx, request.Reply, decoded, "credential principal has no scope")
	}
	validUntil, err := callout.validity.ValidUntil(ctx, principal)
	if err != nil {
		return callout.replyError(ctx, request.Reply, decoded, "credential validity is unavailable")
	}
	now := callout.clock.Now()
	if validUntil.IsZero() || !validUntil.After(now) {
		return callout.replyError(ctx, request.Reply, decoded, "credential has no remaining validity")
	}
	permissions, err := callout.permissions(ctx, principal)
	if err != nil {
		return callout.replyError(ctx, request.Reply, decoded, err.Error())
	}
	// A grant ends at the credential expiry or the fixed cap, whichever comes
	// first. The cap forces the broker to close the connection and re-run the
	// callout, so a revoked, replaced or disabled credential invalidates an
	// already-established connection instead of surviving to its full expiry.
	grantExpiry := validUntil
	if capped := now.Add(callout.grantLifetime); capped.Before(grantExpiry) {
		grantExpiry = capped
	}
	userJWT, err := callout.userJWT(decoded.userNkey, grantExpiry, permissions)
	if err != nil {
		return fault.Wrap(fault.InternalFailure, "nats callout user jwt could not be signed", err)
	}
	response, err := callout.responseJWT(decoded.userNkey, decoded.serverID, userJWT, "")
	if err != nil {
		return fault.Wrap(fault.InternalFailure, "nats callout response could not be signed", err)
	}
	return callout.connection.PublishCallout(ctx, request.Reply, []byte(response))
}

// permissions derives the exact allow lists for one current principal. A
// denied or unavailable grant is an error, never an empty-but-admitted user.
func (callout *Callout) permissions(ctx context.Context, principal identity.Principal) (calloutPermissions, error) {
	if err := callout.authorize(ctx, principal, OperationEventSubscribe); err != nil {
		return calloutPermissions{}, err
	}
	subscribe, err := scopeSubjects(principal.Scope)
	if err != nil {
		return calloutPermissions{}, err
	}
	permissions := calloutPermissions{Subscribe: subscribe}
	switch principal.Kind {
	case identity.KindHuman:
		// Humans read events but never publish through the broker ingress.
	case identity.KindService, identity.KindExecution:
		if err := callout.authorize(ctx, principal, OperationEventPublish); err == nil {
			ingress, err := IngressSubject(principal.Scope)
			if err != nil {
				return calloutPermissions{}, err
			}
			permissions.Publish = []string{string(ingress), "_INBOX.>"}
		}
	default:
		return calloutPermissions{}, fault.New(fault.Unauthenticated, "principal kind cannot use the broker")
	}
	return permissions, nil
}

func (callout *Callout) authorize(ctx context.Context, principal identity.Principal, operation authorize.OperationName) error {
	decision, err := callout.authorizer.Authorize(ctx, principal, operation, principal.Scope)
	if err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "authorization is unavailable", err)
	}
	if decision != authorize.DecisionAllow {
		return fault.New(fault.Forbidden, "operation is not granted in the credential scope")
	}
	return nil
}

// replyError publishes an explicit error response. The user JWT is absent, so
// the broker refuses the connection.
func (callout *Callout) replyError(ctx context.Context, reply string, request calloutRequest, message string) error {
	response, err := callout.responseJWT(request.userNkey, request.serverID, "", message)
	if err != nil {
		return fault.Wrap(fault.InternalFailure, "nats callout error response could not be signed", err)
	}
	return callout.connection.PublishCallout(ctx, reply, []byte(response))
}

// userJWT signs one user claim limited to the supplied permissions and expiry.
// An empty publish allow list always carries an explicit deny: NATS treats an
// empty allow list with no deny as unrestricted.
func (callout *Callout) userJWT(userNkey string, expires time.Time, permissions calloutPermissions) (string, error) {
	publishAllow := permissions.Publish
	publishDeny := []string{CalloutSubject}
	if len(publishAllow) == 0 {
		publishAllow = []string{}
		publishDeny = append(publishDeny, calloutDenyAll)
	}
	claims := map[string]any{
		"jti": calloutJTI(),
		"iat": callout.clock.Now().UTC().Unix(),
		"iss": callout.issuerID,
		"sub": userNkey,
		"aud": callout.account,
		"nats": map[string]any{
			"pub":     map[string]any{"allow": publishAllow, "deny": publishDeny},
			"sub":     map[string]any{"allow": permissions.Subscribe},
			"subs":    -1,
			"data":    -1,
			"payload": -1,
			"type":    calloutUserType,
			"version": calloutJWTVersion,
		},
		"exp": expires.UTC().Unix(),
	}
	return signCalloutJWT(callout.issuer, claims)
}

// responseJWT signs the outer authorization response. Exactly one of userJWT or
// message is set.
func (callout *Callout) responseJWT(userNkey, serverID, userJWT, message string) (string, error) {
	payload := map[string]any{
		"type":    calloutResponseType,
		"version": calloutJWTVersion,
	}
	if message != "" {
		payload["error"] = message
	} else {
		payload["jwt"] = userJWT
	}
	claims := map[string]any{
		"jti":  calloutJTI(),
		"iat":  callout.clock.Now().UTC().Unix(),
		"iss":  callout.issuerID,
		"sub":  userNkey,
		"aud":  serverID,
		"nats": payload,
	}
	return signCalloutJWT(callout.issuer, claims)
}

// calloutPermissions is one exact subject allow list pair.
type calloutPermissions struct {
	Publish   []string
	Subscribe []string
}

// scopeSubjects returns the subjects a credential in one scope may read. The
// event and invalidation subjects are exact; logs use one scope-qualified
// wildcard because every execution in the scope is readable. No wildcard can
// escape the scope and no command subject is included.
func scopeSubjects(scope tenancy.Scope) ([]string, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	event, err := EventSubject(scope)
	if err != nil {
		return nil, err
	}
	invalidation, err := InvalidationSubject(scope)
	if err != nil {
		return nil, err
	}
	project := TenantWideProject
	if scope.ProjectID != "" {
		project = string(scope.ProjectID)
	}
	logs := SubjectPrefix + "." + string(scope.TenantID) + "." + project + ".logs.>"
	return []string{string(event), string(invalidation), logs, "_INBOX.>"}, nil
}

// calloutRequest is the validated subset of a broker authorization request.
type calloutRequest struct {
	userNkey   string
	serverID   string
	credential string
}

// decodeCalloutRequest parses and validates the signed request envelope. The
// envelope arrives over the broker's protected system subject, so the service
// does not need to trust an unauthenticated publisher: the broker denies
// publishing there to every user. The checks here still refuse a malformed
// request rather than deriving permissions from arbitrary JSON.
func decodeCalloutRequest(data []byte) (calloutRequest, error) {
	claims, err := decodeCalloutJWT(data)
	if err != nil {
		return calloutRequest{}, err
	}
	if claims.audience != calloutAudience {
		return calloutRequest{}, fault.New(fault.InvalidInput, "authorization request audience is not the broker request audience")
	}
	if !strings.HasPrefix(claims.issuer, "N") {
		return calloutRequest{}, fault.New(fault.InvalidInput, "authorization request issuer is not a server key")
	}
	if !strings.HasPrefix(claims.userNkey, "U") {
		return calloutRequest{}, fault.New(fault.InvalidInput, "authorization request has no user nkey")
	}
	if claims.serverID == "" {
		return calloutRequest{}, fault.New(fault.InvalidInput, "authorization request has no server id")
	}
	if claims.token == "" && claims.password == "" {
		return calloutRequest{}, fault.New(fault.InvalidInput, "authorization request carries no credential")
	}
	return calloutRequest{
		userNkey:   claims.userNkey,
		serverID:   claims.serverID,
		credential: claims.credential,
	}, nil
}

// decodedRequest is the raw decoded request used while validating.
type decodedRequest struct {
	audience   string
	issuer     string
	userNkey   string
	serverID   string
	token      string
	password   string
	credential string
}

// decodeCalloutJWT decodes the JWT payload. The broker signature is not
// re-verified because only the broker can publish on the protected subject;
// the payload shape and audience are still validated.
func decodeCalloutJWT(data []byte) (decodedRequest, error) {
	parts := strings.Split(strings.TrimSpace(string(data)), ".")
	if len(parts) != 3 {
		return decodedRequest{}, fault.New(fault.InvalidInput, "authorization request is not a jwt")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return decodedRequest{}, fault.Wrap(fault.InvalidInput, "authorization request payload is not base64url", err)
	}
	var envelope struct {
		Audience string `json:"aud"`
		Issuer   string `json:"iss"`
		NATS     struct {
			UserNkey string `json:"user_nkey"`
			ServerID struct {
				ID string `json:"id"`
			} `json:"server_id"`
			ConnectOptions struct {
				Token    string `json:"auth_token"`
				Username string `json:"user"`
				Password string `json:"pass"`
			} `json:"connect_opts"`
		} `json:"nats"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return decodedRequest{}, fault.Wrap(fault.InvalidInput, "authorization request claims are not json", err)
	}
	credential := envelope.NATS.ConnectOptions.Token
	if credential == "" {
		credential = envelope.NATS.ConnectOptions.Password
	}
	return decodedRequest{
		audience:   envelope.Audience,
		issuer:     envelope.Issuer,
		userNkey:   envelope.NATS.UserNkey,
		serverID:   envelope.NATS.ServerID.ID,
		token:      envelope.NATS.ConnectOptions.Token,
		password:   envelope.NATS.ConnectOptions.Password,
		credential: credential,
	}, nil
}

// signCalloutJWT encodes one JWT with the declared NATS nkey header.
func signCalloutJWT(signer ed25519.PrivateKey, claims map[string]any) (string, error) {
	header := []byte(`{"typ":"JWT","alg":"ed25519-nkey"}`)
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	toSign := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	signature := ed25519.Sign(signer, []byte(toSign))
	return toSign + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

// calloutJTI returns a fresh non-repeating claim identity. It carries no
// credential material.
func calloutJTI() string {
	seed := make([]byte, 16)
	if _, err := rand.Read(seed); err != nil {
		sum := sha512.Sum512_256([]byte(fmt.Sprintf("%d", time.Now().UnixNano())))
		seed = sum[:16]
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(seed)
}
