package httpapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/optimiware/kodama/internal/domain"
)

// This file holds the transport-side values of idempotency: the validated key,
// the canonical route, the request fingerprint and the replayable response. The
// transactional lifecycle lives in the PostgreSQL adapter
// (internal/httpapi/postgres), which stores these values.
//
// Conventions (README.md, "Idempotency"):
//
//   - The lookup key is (principal, tenant, method, canonical route, key). The
//     request hash is stored beside it and compared on a match; it is never part
//     of the key, because that would accept a changed request as a new one.
//   - The canonical route is the registered pattern with every wildcard replaced
//     by its validated concrete value, so two projects or resources never share a
//     route. It includes the router prefix and excludes the query string.
//   - The hash covers the query, every If-Match value and the exact body bytes,
//     framed so fields cannot run together. Byte-different but semantically equal
//     JSON is a different request.

// IdempotencyKeyHeader names the request header carrying the key.
const IdempotencyKeyHeader = "Idempotency-Key"

// IdempotencyReplayedHeader marks a response served from storage.
const IdempotencyReplayedHeader = "Idempotent-Replayed"

var idempotencyKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._~:-]{1,128}$`)

// ClientKey is a validated client-chosen Idempotency-Key value.
type ClientKey string

// Validate checks the key syntax.
func (k ClientKey) Validate() error {
	if !idempotencyKeyPattern.MatchString(string(k)) {
		return fmt.Errorf("%w: idempotency key must match %s", domain.ErrInvalidValue, idempotencyKeyPattern)
	}
	return nil
}

// ParseIdempotencyKey requires exactly one valid Idempotency-Key header.
func ParseIdempotencyKey(r *http.Request) (ClientKey, error) {
	vs := r.Header.Values(IdempotencyKeyHeader)
	if len(vs) != 1 {
		return "", BadRequestError("exactly one Idempotency-Key header is required")
	}
	k := ClientKey(vs[0])
	if k.Validate() != nil {
		return "", BadRequestError("Idempotency-Key must be 1 to 128 characters from A-Z a-z 0-9 . _ ~ : -")
	}
	return k, nil
}

// RequestHash is the SHA-256 fingerprint of the mutation-relevant request.
type RequestHash [sha256.Size]byte

// RequestIdentity is the validated identity of one idempotent request.
type RequestIdentity struct {
	Principal Principal
	TenantID  domain.TenantID
	// ProjectID is recorded for ownership when the route is project-level. It is
	// not a key dimension: the concrete Route already distinguishes projects.
	ProjectID domain.ProjectID
	Method    string
	Route     string
	Key       ClientKey
	Hash      RequestHash
}

// Validate checks every component; the PostgreSQL adapter calls it before use.
func (id RequestIdentity) Validate() error {
	if err := id.Principal.Validate(); err != nil {
		return err
	}
	if err := id.TenantID.Validate(); err != nil {
		return err
	}
	if id.ProjectID != "" {
		if err := id.ProjectID.Validate(); err != nil {
			return err
		}
	}
	switch id.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		return fmt.Errorf("%w: idempotent method %q", domain.ErrInvalidValue, id.Method)
	}
	if id.Route == "" || len(id.Route) > 1024 || !strings.HasPrefix(id.Route, "/") {
		return fmt.Errorf("%w: canonical route", domain.ErrInvalidValue)
	}
	return id.Key.Validate()
}

// Identity builds the request identity for an idempotent mutation on this
// call's route. body is the exact bytes read from the request (ReadBody). The
// tenant must be known: principal-level routes cannot be idempotent.
func (c *Call) Identity(r *http.Request, body []byte) (RequestIdentity, error) {
	key, err := ParseIdempotencyKey(r)
	if err != nil {
		return RequestIdentity{}, err
	}
	if c.Target.TenantID == "" {
		return RequestIdentity{}, InternalServerError(errors.New("httpapi: idempotent route has no tenant scope"))
	}
	route, err := CanonicalRoute(c.Pattern, r)
	if err != nil {
		return RequestIdentity{}, err
	}
	id := RequestIdentity{
		Principal: c.Principal, TenantID: c.Target.TenantID, ProjectID: c.Target.ProjectID,
		Method: r.Method, Route: route, Key: key, Hash: HashRequest(r, body),
	}
	if err := id.Validate(); err != nil {
		return RequestIdentity{}, InternalServerError(err)
	}
	return id, nil
}

// CanonicalRoute replaces each wildcard of the registered pattern with the
// request's validated path value. Values that are not opaque IDs are rejected.
func CanonicalRoute(pattern string, r *http.Request) (string, error) {
	_, path, ok := strings.Cut(pattern, " ")
	if !ok {
		return "", InternalServerError(errors.New("httpapi: malformed route pattern"))
	}
	var bad bool
	out := wildcardPattern.ReplaceAllStringFunc(path, func(w string) string {
		v := r.PathValue(w[1 : len(w)-1])
		if !idRegexp.MatchString(v) {
			bad = true
			return ""
		}
		return v
	})
	if bad {
		return "", BadRequestError("a path identifier is not valid")
	}
	return out, nil
}

// HashRequest fingerprints the query, If-Match values and body bytes. Query
// parameters are ordered by name and keep their value order.
func HashRequest(r *http.Request, body []byte) RequestHash {
	h := sha256.New()
	h.Write([]byte("kodama-idempotency-request-v1"))
	var b []byte
	q := r.URL.Query()
	names := make([]string, 0, len(q))
	for k := range q {
		names = append(names, k)
	}
	sort.Strings(names)
	b = binary.BigEndian.AppendUint32(b, uint32(len(names)))
	for _, k := range names {
		b = appendFrame(b, k)
		b = binary.BigEndian.AppendUint32(b, uint32(len(q[k])))
		for _, v := range q[k] {
			b = appendFrame(b, v)
		}
	}
	im := r.Header.Values("If-Match")
	b = binary.BigEndian.AppendUint32(b, uint32(len(im)))
	for _, v := range im {
		b = appendFrame(b, v)
	}
	b = binary.BigEndian.AppendUint64(b, uint64(len(body)))
	h.Write(b)
	h.Write(body)
	var out RequestHash
	copy(out[:], h.Sum(nil))
	return out
}

// Limits for a stored response.
const (
	// DefaultMaxStoredBodyBytes bounds the response body kept for replay.
	DefaultMaxStoredBodyBytes = 256 << 10
	// HardMaxStoredBodyBytes is the database constraint's ceiling.
	HardMaxStoredBodyBytes = 1 << 20
)

// storableHeaders are the only response headers kept for replay. Everything
// else (cookies, request IDs, credentials) is regenerated or dropped.
var storableHeaders = map[string]bool{"Etag": true, "Location": true, "Content-Location": true}

// StoredResponse is a successful JSON response that can be replayed.
type StoredResponse struct {
	Status int
	// Header holds only ETag, Location and Content-Location.
	Header map[string]string
	Body   []byte
}

// Validate enforces the storable shape: a 2xx status, a JSON object or array
// body within maxBody bytes, and allowlisted single-line headers.
func (s StoredResponse) Validate(maxBody int) error {
	if s.Status < 200 || s.Status > 299 {
		return fmt.Errorf("httpapi: stored response status %d is not 2xx", s.Status)
	}
	if maxBody <= 0 || maxBody > HardMaxStoredBodyBytes {
		maxBody = HardMaxStoredBodyBytes
	}
	if len(s.Body) > maxBody {
		return fmt.Errorf("httpapi: stored response body of %d bytes exceeds %d", len(s.Body), maxBody)
	}
	if len(s.Body) > 0 {
		// An empty body is a supported response. Anything else must be exactly
		// one complete JSON object or array: a truncated document or trailing
		// bytes would be committed and then replayed unchanged until expiry.
		if t := bytes.TrimLeft(s.Body, " \t\r\n"); len(t) == 0 || (t[0] != '{' && t[0] != '[') {
			return errors.New("httpapi: stored response body is not a JSON object or array")
		}
		if !json.Valid(s.Body) {
			return errors.New("httpapi: stored response body is not a single valid JSON document")
		}
	}
	for k, v := range s.Header {
		if !storableHeaders[http.CanonicalHeaderKey(k)] {
			return fmt.Errorf("httpapi: response header %q cannot be stored", k)
		}
		if strings.ContainsAny(v, "\r\n\x00") || len(v) > 2048 {
			return fmt.Errorf("httpapi: response header %q has an unsafe value", k)
		}
	}
	return nil
}

// NewStoredResponse marshals v and captures the headers worth replaying.
func NewStoredResponse(r *http.Request, status int, v any, header map[string]string) (StoredResponse, error) {
	body, err := MarshalJSON(r, v)
	if err != nil {
		return StoredResponse{}, err
	}
	return StoredResponse{Status: status, Header: header, Body: body}, nil
}

// Outcome is what an idempotent transaction callback returns on success.
type Outcome struct {
	Response StoredResponse
	// Target names the resource the response describes, for example the created
	// run. A replay is authorized against it before the stored body is released.
	// The zero value means the route-level authorization is sufficient.
	Target Reference
}

// WriteStored sends a stored response. replayed adds Idempotent-Replayed.
func WriteStored(w http.ResponseWriter, r *http.Request, s StoredResponse, replayed bool) {
	h := w.Header()
	for k, v := range s.Header {
		h.Set(k, v)
	}
	if replayed {
		h.Set(IdempotencyReplayedHeader, "true")
	}
	setJSONHeaders(h)
	w.WriteHeader(s.Status)
	_, _ = w.Write(s.Body)
}
