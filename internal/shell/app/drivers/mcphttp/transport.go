// Package mcphttp implements the bounded MCP-over-HTTP driver behind the
// mcpproxy.Transport port.
//
// The driver is bound at construction to an explicit, trusted per-server
// endpoint and an explicit credential mapping. One Call performs exactly one
// JSON-RPC 2.0 tools/call POST to that endpoint with the pinned MCP protocol
// version, maps the standard MCP tool result to the proxy's
// {"content","isError"} shape, and never replays a tool call.
//
// Failures keep the port's two-valued dispatch evidence honest. A refusal that
// is proven to precede the request write is reported as
// *mcpproxy.UpstreamError{Dispatched: false, Class: fault.DependencyUnavailable}.
// Every outcome after the request may have been written — a lost response, a
// redirect, a non-2xx status, a malformed, oversized or mismatched response, a
// read error or a cancellation — is
// *mcpproxy.UpstreamError{Dispatched: true, Class: fault.UncertainExternalEffect}.
//
// The driver never follows a redirect, so a configured credential is never
// forwarded to a redirect target. It never includes a credential value, an
// upstream body or a raw endpoint in a diagnostic. The package opens no socket
// in its unit suite; real MCP conformance belongs to the integration suite.
package mcphttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/textproto"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/mcpproxy"
	"github.com/optimiware/kodama/internal/shell/secret"
)

// DefaultMaxResponseBytes bounds one upstream response when Config does not set
// a bound. It is the mcpproxy request/response bound (1 MiB).
const DefaultMaxResponseBytes = 1 << 20

// Credential maps one configured secret name to exactly one request header.
// Scheme is optional and is prepended to the resolved value with one space, so
// {"Authorization", "Bearer"} sends "Authorization: Bearer <value>".
type Credential struct {
	// Secret is the configured secret name the proxy resolves for this server.
	Secret id.SecretName
	// Header is the request header the resolved value is written to.
	Header string
	// Scheme is an optional authentication scheme prefix.
	Scheme string
}

// Server is one trusted external MCP upstream. Endpoint is the only URL this
// driver ever posts to for the named server.
type Server struct {
	// Name is the configured server identity the proxy selects.
	Name string
	// Endpoint is the absolute trusted http or https URL without a query or
	// fragment. A server with any mapped credential requires https unless the
	// host is loopback.
	Endpoint string
	// Credentials is the explicit secret-to-header mapping. A call must present
	// exactly these secret names; an unmapped or missing credential refuses
	// before dispatch.
	Credentials []Credential
}

// Config binds one driver instance to its trusted servers and HTTP boundary.
type Config struct {
	// Servers is the configured upstream set. It must be non-empty and server
	// names must be unique.
	Servers []Server
	// Client optionally supplies HTTP client settings such as Timeout and Jar.
	// New copies the client and the caller's client is never mutated. It always
	// installs a private *http.Transport with Proxy=nil, so neither
	// HTTP_PROXY/HTTPS_PROXY nor a caller proxy policy can determine routing and
	// a configured credential never reaches a proxy. A caller transport must be
	// a non-nil *http.Transport that is not the shared http.DefaultTransport; New
	// clones it, clears its proxy function and preserves its explicit TLS, dial
	// and HTTP/2 settings. Any other RoundTripper is refused because the
	// driver's dispatch evidence depends on the net/http transport's httptrace
	// callbacks. Nil uses a driver-built transport with explicit dial, TLS and
	// idle defaults that never honours an environment HTTP proxy.
	Client *http.Client
	// MaxResponseBytes optionally bounds one upstream response. Zero uses
	// DefaultMaxResponseBytes; a negative value is refused.
	MaxResponseBytes int
}

// Transport is the bounded MCP-over-HTTP driver. It is safe for concurrent
// calls and allocates a distinct JSON-RPC id for every call.
type Transport struct {
	servers          map[string]serverBinding
	client           *http.Client
	maxResponseBytes int
	nextID           atomic.Uint64
}

// serverBinding is the immutable construction-time copy of one trusted server.
type serverBinding struct {
	endpoint    string
	credentials []Credential
}

// New validates the trusted configuration and binds one transport. Every
// configuration failure is fault.InvalidInput and never renders a credential,
// an endpoint or an embedded URL password.
func New(config Config) (*Transport, error) {
	if len(config.Servers) == 0 {
		return nil, invalidConfig("MCP transport has no configured servers")
	}
	maxResponseBytes := config.MaxResponseBytes
	if maxResponseBytes < 0 {
		return nil, invalidConfig("MCP transport response bound is negative")
	}
	if maxResponseBytes == 0 {
		maxResponseBytes = DefaultMaxResponseBytes
	}

	servers := make(map[string]serverBinding, len(config.Servers))
	for _, server := range config.Servers {
		if server.Name == "" {
			return nil, invalidConfig("MCP transport server name is empty")
		}
		if _, exists := servers[server.Name]; exists {
			return nil, invalidConfig("MCP transport server name is duplicated")
		}
		parsedEndpoint, err := validateEndpoint(server.Endpoint)
		if err != nil {
			return nil, err
		}
		credentials, err := validateCredentials(server.Credentials)
		if err != nil {
			return nil, err
		}
		// A credential must never cross a cleartext non-loopback hop. Loopback
		// http stays allowed for local servers and tests.
		if len(credentials) > 0 && parsedEndpoint.Scheme == "http" && !isLoopbackHost(parsedEndpoint.Hostname()) {
			return nil, invalidConfig("MCP transport requires https for a credentialed non-loopback upstream")
		}
		servers[server.Name] = serverBinding{
			endpoint:    server.Endpoint,
			credentials: credentials,
		}
	}

	client := &http.Client{}
	if config.Client != nil {
		// Copy the client so the driver's redirect refusal cannot be overridden
		// and a shared caller client is never mutated.
		copied := *config.Client
		client = &copied
	}
	// Every successful construction installs a private transport. The driver's
	// pre-dispatch classification relies on the net/http transport's httptrace
	// callbacks, so an opaque RoundTripper that might skip them is refused
	// rather than silently weakening the dispatch evidence.
	owned, err := ownedTransport(client.Transport)
	if err != nil {
		return nil, err
	}
	client.Transport = owned
	client.CheckRedirect = refuseRedirect

	return &Transport{
		servers:          servers,
		client:           client,
		maxResponseBytes: maxResponseBytes,
	}, nil
}

// Call performs one MCP JSON-RPC 2.0 tools/call against the configured trusted
// endpoint. It returns the mapped {"content","isError"} response body on
// success, or a classified *mcpproxy.UpstreamError. The upstream response body
// is closed exactly once on every path.
func (transport *Transport) Call(ctx context.Context, request mcpproxy.UpstreamRequest) (io.ReadCloser, error) {
	binding, ok := transport.servers[request.Server]
	if !ok {
		return nil, refused("MCP server is not configured")
	}
	if request.Endpoint != binding.endpoint {
		return nil, refused("MCP endpoint is not the configured trusted endpoint")
	}
	if request.Tool == "" {
		return nil, refused("MCP tool name is empty")
	}
	if len(request.Arguments) == 0 || !json.Valid(request.Arguments) || string(bytes.TrimSpace(request.Arguments)) == "null" {
		return nil, refused("MCP tool arguments are not a usable JSON value")
	}
	headers, err := binding.resolve(request.Credentials)
	if err != nil {
		return nil, err
	}

	requestID := json.RawMessage(strconv.FormatUint(transport.nextID.Add(1), 10))
	payload, err := encodeRequest(requestID, request.Tool, request.Arguments)
	if err != nil {
		return nil, refused("MCP tool request could not be encoded")
	}

	callCtx := ctx
	if request.Timeout > 0 {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(ctx, request.Timeout)
		defer cancel()
	}

	// Dispatch evidence is deliberately conservative. WroteRequest alone is not
	// proof that nothing was dispatched: on HTTP/2 it fires only after the final
	// DATA flush, the stream writer is not awaited, and a cancellation in that
	// gap leaves the request on the wire with the flag still false. net/http also
	// skips WroteRequest when the HEADERS write fails. A connection that was
	// obtained is therefore enough to make a later failure ambiguous; only a
	// failure with no connection at all (for example a dial failure) is provably
	// pre-dispatch.
	var gotConn atomic.Bool
	var wroteRequest atomic.Bool
	callCtx = httptrace.WithClientTrace(callCtx, &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) {
			gotConn.Store(true)
		},
		WroteRequest: func(httptrace.WroteRequestInfo) {
			wroteRequest.Store(true)
		},
	})

	httpRequest, err := http.NewRequestWithContext(callCtx, http.MethodPost, binding.endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, refused("MCP upstream request could not be built")
	}
	httpRequest.Header.Set("Content-Type", api.MediaType)
	httpRequest.Header.Set("Accept", api.MediaType)
	httpRequest.Header.Set("MCP-Protocol-Version", api.MCPProtocolVersion)
	for _, header := range headers {
		httpRequest.Header.Set(header.name, header.value)
	}
	// NewRequestWithContext installs a GetBody rewind function for the bytes
	// reader. net/http treats a POST with GetBody as replayable when an
	// Idempotency-Key header is present, so a lost response could be retried.
	// The driver never replays a tool call: clear the rewind function so the
	// request is non-replayable whatever headers are configured.
	httpRequest.GetBody = nil

	response, err := transport.client.Do(httpRequest)
	if err != nil {
		if wroteRequest.Load() || gotConn.Load() {
			return nil, uncertain("MCP upstream request outcome is uncertain")
		}
		return nil, preDispatch("MCP upstream is unavailable")
	}
	if response.Body == nil {
		return nil, uncertain("MCP upstream response has no body")
	}
	// Exactly one close for every path that received a response.
	defer response.Body.Close()

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, uncertain("MCP upstream returned a non-success status")
	}
	if !isJSONMediaType(response.Header.Get("Content-Type")) {
		return nil, uncertain("MCP upstream returned an unexpected content type")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, int64(transport.maxResponseBytes)+1))
	if err != nil {
		return nil, uncertain("MCP upstream response could not be read")
	}
	if len(data) > transport.maxResponseBytes {
		return nil, uncertain("MCP upstream response exceeds the configured bound")
	}
	mapped, err := mapResponse(data, requestID)
	if err != nil {
		return nil, uncertain("MCP upstream response is malformed")
	}
	return io.NopCloser(bytes.NewReader(mapped)), nil
}

// resolve maps the presented resolved credentials onto the configured header
// set. The presented names must equal the configured names exactly: a missing
// configured credential, an unmapped presented credential, a duplicate name or
// a header-injecting value refuses before any network write.
func (binding serverBinding) resolve(resolved []secret.Resolved) ([]requestHeader, error) {
	if len(resolved) != len(binding.credentials) {
		return nil, refused("MCP credentials do not match the configured mapping")
	}
	values := make(map[id.SecretName]string, len(resolved))
	for _, credential := range resolved {
		if _, duplicate := values[credential.Name]; duplicate {
			return nil, refused("MCP credentials contain a duplicate name")
		}
		if !validHeaderValue(credential.Value) {
			return nil, refused("MCP credential value is not a valid header value")
		}
		values[credential.Name] = credential.Value
	}
	headers := make([]requestHeader, 0, len(binding.credentials))
	for _, credential := range binding.credentials {
		value, present := values[credential.Secret]
		if !present {
			return nil, refused("MCP credential mapping is incomplete")
		}
		if credential.Scheme != "" {
			value = credential.Scheme + " " + value
		}
		headers = append(headers, requestHeader{name: credential.Header, value: value})
	}
	return headers, nil
}

// requestHeader is one resolved request header.
type requestHeader struct {
	name  string
	value string
}

// refuseRedirect stops the HTTP client at the first redirect. Returning
// http.ErrUseLastResponse yields the redirect response itself, so the driver
// can report an uncertain outcome without ever sending the configured
// credentials to the redirect target.
func refuseRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

// Explicit transport defaults. They mirror the documented net/http defaults
// the driver relied on when it cloned the standard transport, but the driver
// builds them itself so no process-wide ambient transport can determine routing.
const (
	dialTimeout           = 30 * time.Second
	dialKeepAlive         = 30 * time.Second
	maxIdleConns          = 100
	idleConnTimeout       = 90 * time.Second
	tlsHandshakeTimeout   = 10 * time.Second
	expectContinueTimeout = 1 * time.Second
)

// newOwnedTransport builds the driver-owned transport from explicit defaults
// with no proxy function, so an environment proxy can never observe a
// credentialed request. It deliberately does not clone http.DefaultTransport:
// a mutable process-wide transport must not determine the driver's routing.
func newOwnedTransport() *http.Transport {
	return &http.Transport{
		Proxy: nil,
		DialContext: (&net.Dialer{
			Timeout:   dialTimeout,
			KeepAlive: dialKeepAlive,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          maxIdleConns,
		IdleConnTimeout:       idleConnTimeout,
		TLSHandshakeTimeout:   tlsHandshakeTimeout,
		ExpectContinueTimeout: expectContinueTimeout,
	}
}

// ownedTransport returns the private transport New installs. A nil transport
// gets fresh explicit defaults; a caller-supplied *http.Transport is cloned so
// its explicit TLS, dial and HTTP/2 settings are preserved. Every returned
// transport has Proxy=nil. A typed-nil transport, the shared
// http.DefaultTransport and any opaque RoundTripper are refused: the driver's
// dispatch evidence depends on the net/http transport's httptrace callbacks, and
// silently falling back would make an injected client look safe while its
// routing and trace behavior stayed unknown.
func ownedTransport(transport http.RoundTripper) (*http.Transport, error) {
	if transport == nil {
		return newOwnedTransport(), nil
	}
	caller, ok := transport.(*http.Transport)
	if !ok {
		return nil, invalidConfig("MCP transport client does not use a supported HTTP transport")
	}
	if caller == nil {
		return nil, invalidConfig("MCP transport client supplies a nil HTTP transport")
	}
	if http.DefaultTransport != nil && caller == http.DefaultTransport {
		return nil, invalidConfig("MCP transport client supplies the shared default HTTP transport")
	}
	owned := caller.Clone()
	owned.Proxy = nil
	return owned, nil
}

// isLoopbackHost reports whether host is a development loopback host that may
// receive a credential over cleartext. The parsed URL hostname is compared
// exactly, so lookalikes such as "localhost.evil.com" are refused. This mirrors
// the repository's public client rule (client/http.go).
func isLoopbackHost(host string) bool {
	switch strings.ToLower(host) {
	case "127.0.0.1", "::1", "localhost":
		return true
	}
	return false
}

// validateEndpoint accepts only an absolute http or https URL with a host and
// no userinfo, query or fragment. A query string is refused because it can
// carry an inline token that must live in the explicit credential mapping
// instead. The diagnostic never echoes the endpoint.
func validateEndpoint(endpoint string) (*url.URL, error) {
	if endpoint == "" {
		return nil, invalidConfig("MCP transport server endpoint is empty")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return nil, invalidConfig("MCP transport server endpoint is not a valid URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, invalidConfig("MCP transport server endpoint scheme is not http or https")
	}
	if parsed.Host == "" {
		return nil, invalidConfig("MCP transport server endpoint has no host")
	}
	if parsed.User != nil {
		return nil, invalidConfig("MCP transport server endpoint must not carry userinfo")
	}
	if parsed.RawQuery != "" || parsed.ForceQuery {
		return nil, invalidConfig("MCP transport server endpoint must not carry a query string")
	}
	if parsed.Fragment != "" || parsed.RawFragment != "" {
		return nil, invalidConfig("MCP transport server endpoint must not carry a fragment")
	}
	return parsed, nil
}

// validateCredentials checks the explicit mapping and returns an immutable
// copy. Secret names must be valid declared names, headers must be valid HTTP
// field names that do not collide with the driver's own protocol headers, and
// no secret or header may be mapped twice.
func validateCredentials(credentials []Credential) ([]Credential, error) {
	validated := make([]Credential, 0, len(credentials))
	secrets := make(map[id.SecretName]bool, len(credentials))
	headers := make(map[string]bool, len(credentials))
	for _, credential := range credentials {
		if _, err := id.ParseSecretName(string(credential.Secret)); err != nil {
			return nil, invalidConfig("MCP credential secret name is invalid")
		}
		if credential.Header == "" || !validHeaderName(credential.Header) {
			return nil, invalidConfig("MCP credential header is not a valid HTTP field name")
		}
		canonicalHeader := textproto.CanonicalMIMEHeaderKey(credential.Header)
		if reservedHeader(canonicalHeader) {
			return nil, invalidConfig("MCP credential header collides with a protocol header")
		}
		if credential.Scheme != "" && !validToken(credential.Scheme) {
			return nil, invalidConfig("MCP credential scheme is invalid")
		}
		if secrets[credential.Secret] {
			return nil, invalidConfig("MCP credential secret is mapped twice")
		}
		if headers[canonicalHeader] {
			return nil, invalidConfig("MCP credential header is mapped twice")
		}
		secrets[credential.Secret] = true
		headers[canonicalHeader] = true
		validated = append(validated, credential)
	}
	return validated, nil
}

// reservedHeader reports whether a configured credential header would collide
// with a header the driver sets for the protocol itself, or with a header that
// would make net/http treat the tools/call POST as replayable. The driver never
// replays a tool call, so those names are refused outright.
func reservedHeader(canonical string) bool {
	// CanonicalMIMEHeaderKey lowercases every letter after a dash, so the
	// MCP protocol header is compared through the same canonicalization.
	switch canonical {
	case textproto.CanonicalMIMEHeaderKey("Content-Type"),
		textproto.CanonicalMIMEHeaderKey("Accept"),
		textproto.CanonicalMIMEHeaderKey("MCP-Protocol-Version"),
		textproto.CanonicalMIMEHeaderKey("Host"),
		textproto.CanonicalMIMEHeaderKey("Content-Length"),
		textproto.CanonicalMIMEHeaderKey("Transfer-Encoding"),
		textproto.CanonicalMIMEHeaderKey("Connection"),
		textproto.CanonicalMIMEHeaderKey("Idempotency-Key"),
		textproto.CanonicalMIMEHeaderKey("X-Idempotency-Key"):
		return true
	default:
		return false
	}
}

// validHeaderName reports whether name is a non-empty RFC 7230 token.
func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for index := 0; index < len(name); index++ {
		if !isTokenCharacter(name[index]) {
			return false
		}
	}
	return true
}

// validToken reports whether value is a non-empty RFC 7230 token.
func validToken(value string) bool {
	if value == "" {
		return false
	}
	for index := 0; index < len(value); index++ {
		if !isTokenCharacter(value[index]) {
			return false
		}
	}
	return true
}

func isTokenCharacter(character byte) bool {
	switch {
	case character >= 'a' && character <= 'z':
		return true
	case character >= 'A' && character <= 'Z':
		return true
	case character >= '0' && character <= '9':
		return true
	}
	switch character {
	case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
		return true
	default:
		return false
	}
}

// validHeaderValue reports whether value is a legal HTTP field value: no
// carriage return, line feed or other control character that could split or
// corrupt the header block.
func validHeaderValue(value string) bool {
	for index := 0; index < len(value); index++ {
		character := value[index]
		if character == '\t' {
			continue
		}
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

// isJSONMediaType accepts the JSON media type, with or without media
// type parameters.
func isJSONMediaType(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil {
		return false
	}
	return mediaType == api.MediaType
}

// encodeRequest builds the single JSON-RPC tools/call request body.
func encodeRequest(requestID json.RawMessage, tool string, arguments json.RawMessage) ([]byte, error) {
	type params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	type request struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  params          `json:"params"`
	}
	return json.Marshal(request{
		JSONRPC: api.MCPJSONRPC,
		ID:      requestID,
		Method:  api.MCPMethodToolsCall,
		Params:  params{Name: tool, Arguments: arguments},
	})
}

// rpcResponse is the accepted JSON-RPC response envelope.
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   json.RawMessage `json:"error"`
}

// toolResult is the standard MCP tools/call result. Content must be present;
// isError is optional and defaults to false.
type toolResult struct {
	Content json.RawMessage `json:"content"`
	IsError *bool           `json:"isError"`
}

// mappedResponse is the proxy's transport response shape.
type mappedResponse struct {
	Content json.RawMessage `json:"content"`
	IsError bool            `json:"isError"`
}

// mapResponse strictly decodes one JSON-RPC response and maps its tools/call
// result to the proxy's response shape. Trailing content, a wrong JSON-RPC
// version, a missing or mismatched id, both or neither of result/error, an
// error response, a non-object result, a result without a content member and
// invalid JSON are all malformed.
func mapResponse(data []byte, requestID json.RawMessage) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var response rpcResponse
	if err := decoder.Decode(&response); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("MCP upstream response has trailing content")
	}
	if response.JSONRPC != api.MCPJSONRPC {
		return nil, errors.New("MCP upstream response has an unsupported JSON-RPC version")
	}
	if len(response.ID) == 0 || string(bytes.TrimSpace(response.ID)) == "null" {
		return nil, errors.New("MCP upstream response has no id")
	}
	if !sameJSONValue(response.ID, requestID) {
		return nil, errors.New("MCP upstream response id does not match the request id")
	}
	hasResult := len(response.Result) > 0
	hasError := len(response.Error) > 0 && string(bytes.TrimSpace(response.Error)) != "null"
	if hasResult == hasError {
		return nil, errors.New("MCP upstream response must carry exactly one result or error")
	}
	if hasError {
		return nil, errors.New("MCP upstream response is an error response")
	}
	var result toolResult
	if err := json.Unmarshal(response.Result, &result); err != nil {
		return nil, errors.New("MCP upstream result is not an object")
	}
	if result.Content == nil {
		return nil, errors.New("MCP upstream result has no content member")
	}
	isError := result.IsError != nil && *result.IsError
	return json.Marshal(mappedResponse{Content: result.Content, IsError: isError})
}

// sameJSONValue compares two JSON values after decoding, so encoding
// differences such as surrounding whitespace do not matter. A string id never
// equals a numeric id, and numeric ids are compared by their exact decoded
// json.Number text.
func sameJSONValue(left, right json.RawMessage) bool {
	leftValue, leftErr := decodeJSONValue(left)
	rightValue, rightErr := decodeJSONValue(right)
	if leftErr != nil || rightErr != nil {
		return false
	}
	return reflect.DeepEqual(leftValue, rightValue)
}

func decodeJSONValue(data json.RawMessage) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return value, nil
}

// invalidConfig builds one configuration fault. The message is static so it
// can never render a credential, an endpoint or a URL password.
func invalidConfig(message string) error {
	return fault.New(fault.InvalidInput, message)
}

// refused reports a proven pre-dispatch refusal. The port recognizes exactly
// one pre-effect class, so a refused call is never reported as an uncertain
// external effect.
func refused(message string) error {
	return &mcpproxy.UpstreamError{
		Class:      fault.DependencyUnavailable,
		Message:    message,
		Dispatched: false,
	}
}

// preDispatch reports a network failure proven to precede the request write.
func preDispatch(message string) error {
	return &mcpproxy.UpstreamError{
		Class:      fault.DependencyUnavailable,
		Message:    message,
		Dispatched: false,
	}
}

// uncertain reports an outcome after the request may have been written. Such
// an outcome is never replayed.
func uncertain(message string) error {
	return &mcpproxy.UpstreamError{
		Class:      fault.UncertainExternalEffect,
		Message:    message,
		Dispatched: true,
	}
}

var _ mcpproxy.Transport = (*Transport)(nil)
