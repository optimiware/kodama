package git

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
)

// ErrBodyTooLarge reports a smart-HTTP request body above the configured
// bound. The backend receives this error from its body reader; the handler
// maps it to 413 instead of exposing the backend's internal error.
var ErrBodyTooLarge = errors.New("smart HTTP request body is too large")

var (
	errMalformedRoute   = errors.New("malformed smart HTTP route")
	errWriteDenied      = errors.New("actor may read but may not write")
	errRepositoryHidden = errors.New("repository is not visible to the actor")
)

// ErrRepositoryUnregistered reports a requested repository with no registered
// resource identity. The smart-HTTP edge reports it exactly like a credential
// failure so a client cannot use the status to learn whether a repository
// exists.
var ErrRepositoryUnregistered = errors.New("repository has no registered resource identity")

// UnregisteredRepositoryError returns the edge refusal for a repository with no
// registered resource identity. The production repository-to-resource resolvers
// return it, and it carries the unauthenticated class rather than a server-fault
// class: the edge must answer the uniform 401 challenge, never a 500 that would
// reveal the repository is unmapped.
func UnregisteredRepositoryError(repository string) error {
	return fault.Wrap(fault.Unauthenticated,
		fmt.Sprintf("repository %q has no registered resource identity", repository),
		ErrRepositoryUnregistered)
}

// DefaultSmartHTTPMaxBodyBytes bounds one smart-HTTP request body when the
// configuration leaves the bound unset.
const DefaultSmartHTTPMaxBodyBytes int64 = 512 << 20

// GitBackendRequest is one validated smart-HTTP backend invocation. It carries
// only the resolved service, repository, path, allow-listed environment and
// allow-listed request headers. It never carries the Authorization header or a
// raw actor credential.
type GitBackendRequest struct {
	Service     string      `json:"service"`
	Repository  string      `json:"repository"`
	Path        string      `json:"path"`
	Environment []string    `json:"environment,omitempty"`
	Headers     http.Header `json:"-"`
}

// GitResponseWriter is the backend response sink. It exposes status, headers
// and body writes; the handler filters headers through an allow-list.
type GitResponseWriter interface {
	Header() http.Header
	WriteHeader(status int)
	Write([]byte) (int, error)
}

// GitBackend runs one validated smart-HTTP exchange. A concrete adapter maps
// it to `git http-backend` (CGI) or an equivalent native command; the unit
// stage injects a deterministic fake.
type GitBackend interface {
	Serve(ctx context.Context, request GitBackendRequest, body io.Reader, response GitResponseWriter) error
}

// ReadActorVerifier authenticates one forwarded actor credential for a
// repository-level read: it calls the public `git.read` resource operation with
// an empty subject and no approval, then returns the local rule identity. The
// read contract forbids a `git.ref.read` verification record, so the
// read path uses this repository-level verification and then the local ordered
// read rules; updates keep the `git.ref.update` verification path inside
// Service.Update.
type ReadActorVerifier interface {
	VerifyReadActor(ctx context.Context, repository, credential string) (Identity, error)
}

// ActorHandleIssuer turns one authenticated credential into an opaque handle
// for the backend environment. The handle, never the raw credential, is placed
// in KODAMA_ACTOR_REF.
type ActorHandleIssuer interface {
	IssueActor(ctx context.Context, credential string) (string, error)
}

// SmartHTTPConfig is the complete smart-HTTP adapter dependency set.
type SmartHTTPConfig struct {
	Service      *Service
	Backend      GitBackend
	Actors       ReadActorVerifier
	ActorHandles ActorHandleIssuer
	// ResourceID resolves the registered resource metadata identity for a
	// tenant-relative repository. Either ResourceID or ResourceIDContext is
	// required so the backend environment always carries the registered
	// identity the hook reports against.
	ResourceID func(repository string) (string, error)
	// ResourceIDContext resolves the registered resource metadata identity with
	// the request context. When set, it takes precedence over ResourceID.
	ResourceIDContext func(ctx context.Context, repository string) (string, error)
	// SourceEvent mints the unique per-request push identity placed in the
	// backend environment as KODAMA_SOURCE_EVENT. It is required: a repeated
	// identical push must never reuse an earlier journal identity.
	SourceEvent  func() (string, error)
	MaxBodyBytes int64
	Timeout      time.Duration
}

// SmartHTTP is the independent Git smart-HTTP front handler. It authenticates
// the actor, applies the local read/write-at-all rules, and streams the
// validated exchange to the injected backend.
type SmartHTTP struct {
	service           *Service
	backend           GitBackend
	actors            ReadActorVerifier
	handles           ActorHandleIssuer
	resourceID        func(string) (string, error)
	resourceIDContext func(context.Context, string) (string, error)
	sourceEvent       func() (string, error)
	maxBody           int64
	timeout           time.Duration
}

var _ http.Handler = (*SmartHTTP)(nil)

// NewSmartHTTP validates every boundary and returns the handler.
func NewSmartHTTP(config SmartHTTPConfig) (*SmartHTTP, error) {
	switch {
	case config.Service == nil:
		return nil, fmt.Errorf("smart HTTP service is missing")
	case config.Backend == nil:
		return nil, fmt.Errorf("smart HTTP backend is missing")
	case config.Actors == nil:
		return nil, fmt.Errorf("smart HTTP read actor verifier is missing")
	case config.ActorHandles == nil:
		return nil, fmt.Errorf("smart HTTP actor handle issuer is missing")
	case config.ResourceID == nil && config.ResourceIDContext == nil:
		return nil, fmt.Errorf("smart HTTP resource resolver is missing")
	case config.SourceEvent == nil:
		return nil, fmt.Errorf("smart HTTP source event source is missing")
	}
	maxBody := config.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = DefaultSmartHTTPMaxBodyBytes
	}
	return &SmartHTTP{
		service:           config.Service,
		backend:           config.Backend,
		actors:            config.Actors,
		handles:           config.ActorHandles,
		resourceID:        config.ResourceID,
		resourceIDContext: config.ResourceIDContext,
		sourceEvent:       config.SourceEvent,
		maxBody:           maxBody,
		timeout:           config.Timeout,
	}, nil
}

// NewSourceEventID returns the crypto/rand-backed per-request push identity
// generator used by production composition. Tests inject a deterministic
// generator so they can assert distinct and repeated deliveries.
func NewSourceEventID() func() (string, error) {
	return func() (string, error) {
		var raw [16]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return "", fmt.Errorf("generate source event id: %w", err)
		}
		return hex.EncodeToString(raw[:]), nil
	}
}

// smartRoute is one parsed smart-HTTP route.
type smartRoute struct {
	service    string
	repository string
	action     string
	method     string
}

// ServeHTTP implements http.Handler for the three smart protocol routes.
func (handler *SmartHTTP) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	ctx := request.Context()
	if handler.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, handler.timeout)
		defer cancel()
	}
	if err := ctx.Err(); err != nil {
		writeRefusal(writer, statusForContext(err))
		return
	}

	route, err := parseSmartRoute(request)
	if err != nil {
		writeRefusal(writer, http.StatusBadRequest)
		return
	}
	credential, ok := smartCredential(request)
	if !ok {
		writeUnauthorized(writer)
		return
	}
	identity, err := handler.actors.VerifyReadActor(ctx, route.repository, credential)
	if err != nil {
		status, retry := verificationStatus(err)
		writeStatusRefusal(writer, status, retry)
		return
	}
	if err := handler.authorize(ctx, route, identity); err != nil {
		status, retry := refusalStatus(err)
		writeStatusRefusal(writer, status, retry)
		return
	}

	handle, err := handler.handles.IssueActor(ctx, credential)
	if err != nil || handle == "" || strings.Contains(handle, credential) || !validActorHandle(handle) {
		writeRefusal(writer, http.StatusInternalServerError)
		return
	}
	resourceID, err := handler.resolveResourceID(ctx, route.repository)
	if err != nil {
		// The repository-to-resource resolution uses the class table, never a
		// special case: an unregistered repository is deliberately reported as
		// a credential refusal so its existence stays hidden.
		status, retry := verificationStatus(err)
		writeStatusRefusal(writer, status, retry)
		return
	}
	if !validJournalID(resourceID) {
		writeRefusal(writer, http.StatusInternalServerError)
		return
	}
	path, err := handler.service.repositoryPath(route.repository)
	if err != nil {
		status, retry := refusalStatus(err)
		writeStatusRefusal(writer, status, retry)
		return
	}

	var body io.Reader
	if route.method == http.MethodPost {
		if request.ContentLength > handler.maxBody {
			writeRefusal(writer, http.StatusRequestEntityTooLarge)
			return
		}
		body = &boundedReader{reader: request.Body, remaining: handler.maxBody}
	}
	environment := []string{
		EnvActorRef + "=" + handle,
		EnvRepository + "=" + route.repository,
		EnvResourceID + "=" + resourceID,
	}
	if route.service == "git-receive-pack" {
		sourceEvent, err := handler.sourceEvent()
		if err != nil || !validJournalID(sourceEvent) {
			writeRefusal(writer, http.StatusInternalServerError)
			return
		}
		environment = append(environment, EnvSourceEvent+"="+sourceEvent)
	}
	backendRequest := GitBackendRequest{
		Service:     route.service,
		Repository:  route.repository,
		Path:        path,
		Environment: environment,
		Headers:     allowRequestHeaders(request.Header),
	}
	response := newBackendResponse(writer)
	if err := handler.backend.Serve(ctx, backendRequest, body, response); err != nil {
		if response.written() {
			return
		}
		switch {
		case errors.Is(err, ErrBodyTooLarge):
			writeRefusal(writer, http.StatusRequestEntityTooLarge)
		case errors.Is(err, context.DeadlineExceeded):
			writeRefusal(writer, http.StatusGatewayTimeout)
		case errors.Is(err, context.Canceled):
			writeRefusal(writer, http.StatusRequestTimeout)
		default:
			writeRefusal(writer, http.StatusBadGateway)
		}
		return
	}
	if !response.written() {
		response.WriteHeader(http.StatusOK)
	}
}

func (handler *SmartHTTP) resolveResourceID(ctx context.Context, repository string) (string, error) {
	if handler.resourceIDContext != nil {
		return handler.resourceIDContext(ctx, repository)
	}
	return handler.resourceID(repository)
}

// authorize applies the repository-level read or write-at-all decision. The
// read decision is repository-level through AuthorizeRead; the exact per-ref
// decision for a receive stays in the update hook. A server that may not write
// is refused as write-denied when it may at least read the repository, and is
// otherwise hidden.
func (handler *SmartHTTP) authorize(ctx context.Context, route smartRoute, identity Identity) error {
	switch route.service {
	case "git-upload-pack":
		decision, err := handler.service.AuthorizeRead(ctx, route.repository, identity)
		if err != nil {
			return err
		}
		if decision != DecisionAllow {
			return ErrPolicyDenied
		}
		return nil
	case "git-receive-pack":
		write, err := handler.service.authorizeAny(ctx, route.repository, identity, UpdateCreate)
		if err != nil {
			return err
		}
		if write == DecisionAllow {
			return nil
		}
		read, err := handler.service.AuthorizeRead(ctx, route.repository, identity)
		if err != nil {
			return err
		}
		if read == DecisionAllow {
			return errWriteDenied
		}
		return errRepositoryHidden
	default:
		return errMalformedRoute
	}
}

// authorizeAny is the repository-level form of AuthorizeRead/Authorize: it
// reuses the same ordered rule matcher and permission coverage, but asks
// whether any ref is visible because the smart-HTTP advertisement has no single
// attempt ref. It is an admission check, not a replacement for the hook's exact
// per-ref update decision.
func (service *Service) authorizeAny(ctx context.Context, repository string, identity Identity, update UpdateKind) (Decision, error) {
	if err := identity.Validate(); err != nil {
		return DecisionDeny, err
	}
	if !update.Valid() {
		return DecisionDeny, fmt.Errorf("unknown update kind %q", update)
	}
	policy, err := service.currentPolicy(ctx, repository)
	if err != nil {
		return DecisionDeny, err
	}
	var denies []Rule
	for _, rule := range policy.Rules {
		if !ruleMatchesIdentity(rule.Principals, identity) {
			continue
		}
		if rule.Permission == PermissionDeny {
			denies = append(denies, rule)
			continue
		}
		if !rule.Permission.covers(update) {
			continue
		}
		if ruleVisible(rule, denies) {
			return DecisionAllow, nil
		}
	}
	return DecisionDeny, nil
}

// ruleVisible reports whether an allow rule covers at least one ref not
// already denied by an earlier matching deny rule.
func ruleVisible(allow Rule, denies []Rule) bool {
	if allow.Ref == "" {
		// A ref-less allow covers every ref; only a ref-less deny or the
		// all-refs `refs/` prefix denies it entirely.
		for _, deny := range denies {
			if deny.Ref == "" || deny.Ref == "refs/" {
				return false
			}
		}
		return true
	}
	for _, deny := range denies {
		if deny.Ref == "" {
			return false
		}
		if strings.HasSuffix(allow.Ref, "/") {
			if strings.HasSuffix(deny.Ref, "/") && strings.HasPrefix(allow.Ref, deny.Ref) {
				return false
			}
			continue
		}
		if deny.Ref == allow.Ref {
			return false
		}
		if strings.HasSuffix(deny.Ref, "/") && strings.HasPrefix(allow.Ref, deny.Ref) {
			return false
		}
	}
	return true
}

// parseSmartRoute validates the path, method and service query. Encoded
// slashes, traversal, empty segments and unbounded names refuse before any
// repository lookup.
func parseSmartRoute(request *http.Request) (smartRoute, error) {
	if request.URL == nil {
		return smartRoute{}, errMalformedRoute
	}
	escaped := strings.ToLower(request.URL.EscapedPath())
	if strings.Contains(escaped, "%2f") || strings.Contains(escaped, "%5c") {
		return smartRoute{}, errMalformedRoute
	}
	path := request.URL.Path
	if !strings.HasPrefix(path, "/") || strings.Contains(path, "\\") {
		return smartRoute{}, errMalformedRoute
	}
	segments := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(segments) < 2 {
		return smartRoute{}, errMalformedRoute
	}
	var action string
	var repositoryParts []string
	switch {
	case len(segments) >= 3 && segments[len(segments)-2] == "info" && segments[len(segments)-1] == "refs":
		action = "info/refs"
		repositoryParts = segments[:len(segments)-2]
	case segments[len(segments)-1] == "git-upload-pack", segments[len(segments)-1] == "git-receive-pack":
		action = segments[len(segments)-1]
		repositoryParts = segments[:len(segments)-1]
	default:
		return smartRoute{}, errMalformedRoute
	}
	if len(repositoryParts) == 0 {
		return smartRoute{}, errMalformedRoute
	}
	for _, part := range repositoryParts {
		if part == "" || part == "." || part == ".." {
			return smartRoute{}, errMalformedRoute
		}
	}
	repository := strings.Join(repositoryParts, "/")
	// A trailing `.git` is a transport suffix; repositories are configured by
	// their tenant-relative name without it.
	repository = strings.TrimSuffix(repository, ".git")
	if !validRepositoryName(repository) {
		return smartRoute{}, errMalformedRoute
	}

	route := smartRoute{repository: repository, action: action, method: request.Method}
	switch action {
	case "info/refs":
		if request.Method != http.MethodGet {
			return smartRoute{}, errMalformedRoute
		}
		values, ok := request.URL.Query()["service"]
		if !ok || len(values) != 1 {
			return smartRoute{}, errMalformedRoute
		}
		if values[0] != "git-upload-pack" && values[0] != "git-receive-pack" {
			return smartRoute{}, errMalformedRoute
		}
		route.service = values[0]
	case "git-upload-pack", "git-receive-pack":
		if request.Method != http.MethodPost {
			return smartRoute{}, errMalformedRoute
		}
		if !validRequestContentType(request, action) {
			return smartRoute{}, errMalformedRoute
		}
		route.service = action
	default:
		return smartRoute{}, errMalformedRoute
	}
	return route, nil
}

// validRequestContentType checks the exact Git request media type.
func validRequestContentType(request *http.Request, service string) bool {
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil {
		return false
	}
	if service == "git-upload-pack" {
		return mediaType == "application/x-git-upload-pack-request"
	}
	return mediaType == "application/x-git-receive-pack-request"
}

// smartCredential extracts the bearer token or the credential-helper password
// for username `kodama`. The credential is returned only to the injected
// verification/handle boundaries.
func smartCredential(request *http.Request) (string, bool) {
	header := request.Header.Get("Authorization")
	if header == "" {
		return "", false
	}
	scheme, value, ok := strings.Cut(header, " ")
	if !ok {
		return "", false
	}
	switch {
	case strings.EqualFold(scheme, "Bearer"):
		token := strings.TrimSpace(value)
		if token == "" || len(token) > api.MaxCredentialBytes {
			return "", false
		}
		return token, true
	case strings.EqualFold(scheme, "Basic"):
		username, password, ok := request.BasicAuth()
		if !ok || username != "kodama" || password == "" || len(password) > api.MaxCredentialBytes {
			return "", false
		}
		return password, true
	default:
		return "", false
	}
}

// validActorHandle bounds the opaque backend handle. A handle is never a raw
// credential: it is a short opaque reference resolved by the hook's
// ActorSource.
func validActorHandle(value string) bool {
	if value == "" || len(value) > 256 {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		switch {
		case character >= 'a' && character <= 'z':
		case character >= 'A' && character <= 'Z':
		case character >= '0' && character <= '9':
		case character == '-', character == '_', character == '.', character == ':':
		default:
			return false
		}
	}
	return true
}

// boundedReader allows exactly max bytes and reports ErrBodyTooLarge when the
// underlying body has more.
type boundedReader struct {
	reader    io.Reader
	remaining int64
	exceeded  bool
}

func (reader *boundedReader) Read(buffer []byte) (int, error) {
	if reader.exceeded {
		return 0, ErrBodyTooLarge
	}
	if reader.remaining == 0 {
		var probe [1]byte
		read, err := reader.reader.Read(probe[:])
		if read > 0 {
			reader.exceeded = true
			return 0, ErrBodyTooLarge
		}
		if err != nil {
			return 0, err
		}
		return 0, nil
	}
	if int64(len(buffer)) > reader.remaining {
		buffer = buffer[:reader.remaining]
	}
	read, err := reader.reader.Read(buffer)
	reader.remaining -= int64(read)
	return read, err
}

// backendRequestHeaders is the allow-list forwarded to the backend. The
// Authorization, Cookie and arbitrary X-* headers are never forwarded.
var backendRequestHeaders = map[string]bool{
	"Accept":           true,
	"Content-Encoding": true,
	"Content-Type":     true,
	"Git-Protocol":     true,
	"User-Agent":       true,
}

func allowRequestHeaders(source http.Header) http.Header {
	headers := make(http.Header)
	for key, values := range source {
		if !backendRequestHeaders[http.CanonicalHeaderKey(key)] {
			continue
		}
		headers[http.CanonicalHeaderKey(key)] = append([]string(nil), values...)
	}
	return headers
}

// backendResponseHeaders is the allow-list returned to the Git client.
var backendResponseHeaders = map[string]bool{
	"Cache-Control":    true,
	"Content-Encoding": true,
	"Content-Length":   true,
	"Content-Type":     true,
	"Expires":          true,
	"Git-Protocol":     true,
	"Pragma":           true,
}

// backendResponse filters backend headers and preserves the write-once status.
type backendResponse struct {
	target http.ResponseWriter
	header http.Header
	status int
	wrote  bool
}

func newBackendResponse(target http.ResponseWriter) *backendResponse {
	return &backendResponse{target: target, header: make(http.Header)}
}

func (response *backendResponse) Header() http.Header {
	return response.header
}

func (response *backendResponse) WriteHeader(status int) {
	if response.wrote {
		return
	}
	if status < 100 || status > 999 {
		status = http.StatusOK
	}
	response.wrote = true
	response.status = status
	target := response.target.Header()
	for key, values := range response.header {
		if !backendResponseHeaders[http.CanonicalHeaderKey(key)] {
			continue
		}
		target[http.CanonicalHeaderKey(key)] = append([]string(nil), values...)
	}
	response.target.WriteHeader(status)
}

func (response *backendResponse) Write(body []byte) (int, error) {
	if !response.wrote {
		response.WriteHeader(http.StatusOK)
	}
	return response.target.Write(body)
}

// Flush lets a streaming backend adapter type-assert the response sink to a
// flusher without exposing the underlying http.ResponseWriter.
func (response *backendResponse) Flush() {
	if !response.wrote {
		response.WriteHeader(http.StatusOK)
	}
	if flusher, ok := response.target.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (response *backendResponse) written() bool {
	return response.wrote
}

// writeUnauthorized emits the uniform Basic challenge without revealing
// whether a repository exists.
func writeUnauthorized(writer http.ResponseWriter) {
	writer.Header().Set("WWW-Authenticate", `Basic realm="kodama"`)
	writeRefusal(writer, http.StatusUnauthorized)
}

// writeRefusal writes one sanitized, bodyless-detail refusal.
func writeRefusal(writer http.ResponseWriter, status int) {
	writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	_, _ = io.WriteString(writer, http.StatusText(status)+"\n")
}

// retryAfterSeconds is the small whole-second Retry-After hint attached to a
// transient server failure. Git clients retry the smart-HTTP exchange after
// the delay; a missing or large value makes them give up or hammer the edge.
const retryAfterSeconds = 2

// writeStatusRefusal writes one sanitized refusal. A transient failure carries
// a bounded Retry-After hint; an authentication failure keeps the Basic
// challenge and never the hint.
func writeStatusRefusal(writer http.ResponseWriter, status int, retry bool) {
	if retry {
		writer.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds))
	}
	if status == http.StatusUnauthorized {
		writeUnauthorized(writer)
		return
	}
	writeRefusal(writer, status)
}

// verificationStatus maps one failed actor verification to the smart-HTTP
// status and reports whether the failure is transient (and so carries a
// Retry-After hint):
//
//	unauthenticated (invalid, expired or cleared token)  401 + Basic challenge
//	forbidden (authenticated, no authority)              403
//	not_found (repository hidden)                        404
//	invalid_input                                        400
//	changed_idempotency_content                          409
//	version_conflict                                     503 + Retry-After
//	dependency_unavailable                               503 + Retry-After
//	uncertain_external_effect                            503 + Retry-After
//	deadline_expired                                     503 + Retry-After
//	unsupported_capability                               500
//	internal_failure                                     500
//	unclassified error                                   500
//
// An unclassified error is a safe server failure, never a credential verdict:
// the edge must not tell Git its credential is bad when the verifier itself
// failed, and the generic body keeps the private cause out of the response.
func verificationStatus(err error) (int, bool) {
	switch {
	case errors.Is(err, context.Canceled):
		return http.StatusRequestTimeout, false
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusServiceUnavailable, true
	}
	class, ok := fault.ClassOf(err)
	if !ok {
		return http.StatusInternalServerError, false
	}
	switch class {
	case fault.Unauthenticated:
		return http.StatusUnauthorized, false
	case fault.Forbidden:
		return http.StatusForbidden, false
	case fault.NotFound:
		return http.StatusNotFound, false
	case fault.InvalidInput:
		return http.StatusBadRequest, false
	case fault.ChangedIdempotencyContent:
		return http.StatusConflict, false
	case fault.VersionConflict, fault.DependencyUnavailable, fault.UncertainExternalEffect, fault.DeadlineExpired:
		return http.StatusServiceUnavailable, true
	default:
		return http.StatusInternalServerError, false
	}
}

// refusalStatus maps an authorization/dependency error to the uniform status
// and whether it is a transient failure carrying a Retry-After hint. It covers
// both the authorization path (which can hit an unavailable committed policy
// or a conflicting store version) and the repository path resolution.
func refusalStatus(err error) (int, bool) {
	switch {
	case errors.Is(err, ErrInvalidRepository):
		return http.StatusBadRequest, false
	case errors.Is(err, ErrRepositoryUnknown):
		return http.StatusNotFound, false
	case errors.Is(err, ErrPolicyDenied):
		return http.StatusNotFound, false
	case errors.Is(err, errRepositoryHidden):
		return http.StatusNotFound, false
	case errors.Is(err, errWriteDenied):
		return http.StatusForbidden, false
	case errors.Is(err, context.Canceled):
		return http.StatusRequestTimeout, false
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, false
	default:
		// An unavailable committed policy (missing admin repository or ref,
		// unreadable blob) or a conflicting store version is a transient
		// dependency failure. The body stays the generic status text, so no
		// path, policy or store content reaches the client.
		class, ok := fault.ClassOf(err)
		if ok {
			switch class {
			case fault.Forbidden:
				return http.StatusForbidden, false
			case fault.VersionConflict, fault.DependencyUnavailable, fault.UncertainExternalEffect, fault.DeadlineExpired:
				return http.StatusServiceUnavailable, true
			}
		}
		return http.StatusInternalServerError, false
	}
}

func statusForContext(err error) int {
	if errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout
	}
	return http.StatusRequestTimeout
}
