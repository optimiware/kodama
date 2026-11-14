// Package registry implements the pipeline.Resolver registry digest seam.
//
// Resolution is a pre-admission read: a mutable tag is looked up once and
// becomes an immutable sha256 digest before any snapshot exists. The resolver
// holds no cache, so a moved tag is looked up again and a later failure is
// never answered with an earlier digest.
//
// Credentials are installation-level. The credential source is asked about
// exactly two origins: the configured registry API origin, used for manifest
// requests, and the origin of the Bearer token realm the registry challenged
// for, used for that one token request only. A credential is never forwarded
// across origins, rendered into errors, or copied into the resolved image. The
// package performs no I/O of its own: every network effect goes through the
// injected http.RoundTripper.
package registry

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"math"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/pipeline"
)

const (
	// DefaultMaxResponseBytes bounds a manifest or index body when the config
	// leaves MaxResponseBytes at its zero value.
	DefaultMaxResponseBytes int64 = 1 << 20
	// DefaultMaxRedirects bounds a redirect chain when the config leaves
	// MaxRedirects at its zero value.
	DefaultMaxRedirects = 3
)

const (
	dockerManifestMediaType = "application/vnd.docker.distribution.manifest.v2+json"
	dockerIndexMediaType    = "application/vnd.docker.distribution.manifest.list.v2+json"
	ociManifestMediaType    = "application/vnd.oci.image.manifest.v1+json"
	ociIndexMediaType       = "application/vnd.oci.image.index.v1+json"
)

// CredentialScheme selects how an installation credential is presented.
type CredentialScheme string

const (
	// CredentialAnonymous sends no Authorization header.
	CredentialAnonymous CredentialScheme = "anonymous"
	// CredentialBasic sends an HTTP Basic Authorization header.
	CredentialBasic CredentialScheme = "basic"
	// CredentialBearer sends an HTTP Bearer Authorization header.
	CredentialBearer CredentialScheme = "bearer"
)

// Credential is one installation-level registry credential. Its values are
// never included in a result or an error.
type Credential struct {
	Scheme   CredentialScheme
	Username string
	Password string
	Token    string
}

// CredentialSource supplies the installation credential for one origin. The
// origin is "{scheme}://{host}", for example "https://registry.example.com".
// A resolver consults the source for exactly two origins: its configured
// registry API origin (manifest requests) and the origin of a Bearer token
// realm it was challenged by (that token request only). No other origin is ever
// asked about.
type CredentialSource interface {
	Credential(ctx context.Context, origin string) (Credential, error)
}

// Config configures a Resolver. A zero MaxResponseBytes or MaxRedirects selects
// the corresponding default; a zero AllowedMediaTypes selects the OCI/Docker
// manifest and index defaults.
type Config struct {
	Transport         http.RoundTripper
	Credentials       CredentialSource
	AllowedMediaTypes []string
	MaxResponseBytes  int64
	MaxRedirects      int
	AllowInsecureHTTP bool
}

// Validate refuses configuration that cannot produce a bounded, typed read.
func (config Config) Validate() error {
	if config.Transport == nil {
		return fault.New(fault.InvalidInput, "registry transport is required")
	}
	if config.MaxResponseBytes < 0 {
		return fault.New(fault.InvalidInput, "registry response bound must not be negative")
	}
	if config.MaxRedirects < 0 {
		return fault.New(fault.InvalidInput, "registry redirect cap must not be negative")
	}
	for _, mediaType := range config.AllowedMediaTypes {
		if strings.TrimSpace(mediaType) == "" {
			return fault.New(fault.InvalidInput, "registry allowed media type is empty")
		}
		if _, _, err := mime.ParseMediaType(mediaType); err != nil {
			return fault.New(fault.InvalidInput, "registry allowed media type is invalid")
		}
	}
	return nil
}

// DefaultAllowedMediaTypes returns the OCI and Docker manifest and index media
// types. Each call returns a fresh slice.
func DefaultAllowedMediaTypes() []string {
	return []string{
		dockerManifestMediaType,
		dockerIndexMediaType,
		ociManifestMediaType,
		ociIndexMediaType,
	}
}

// Resolver resolves image references to immutable sha256 digests. It holds no
// mutable per-call state and is safe for concurrent use.
type Resolver struct {
	transport        http.RoundTripper
	credentials      CredentialSource
	allowedMedia     []string
	allowedMediaSet  map[string]struct{}
	maxResponseBytes int64
	maxRedirects     int
	insecure         bool
}

// New validates the configuration and builds a Resolver.
func New(config Config) (*Resolver, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}

	mediaTypes := config.AllowedMediaTypes
	if len(mediaTypes) == 0 {
		mediaTypes = DefaultAllowedMediaTypes()
	}
	normalized := make([]string, 0, len(mediaTypes))
	set := make(map[string]struct{}, len(mediaTypes))
	for _, mediaType := range mediaTypes {
		parsed, _, err := mime.ParseMediaType(mediaType)
		if err != nil {
			return nil, fault.New(fault.InvalidInput, "registry allowed media type is invalid")
		}
		if _, duplicate := set[parsed]; duplicate {
			continue
		}
		set[parsed] = struct{}{}
		normalized = append(normalized, parsed)
	}

	maxResponseBytes := config.MaxResponseBytes
	if maxResponseBytes == 0 {
		maxResponseBytes = DefaultMaxResponseBytes
	}
	maxRedirects := config.MaxRedirects
	if maxRedirects == 0 {
		maxRedirects = DefaultMaxRedirects
	}

	return &Resolver{
		transport:        config.Transport,
		credentials:      config.Credentials,
		allowedMedia:     normalized,
		allowedMediaSet:  set,
		maxResponseBytes: maxResponseBytes,
		maxRedirects:     maxRedirects,
		insecure:         config.AllowInsecureHTTP,
	}, nil
}

// Resolve looks up one reference and returns the repository plus the sha256 of
// the exact manifest or index bytes the registry returned.
func (resolver *Resolver) Resolve(ctx context.Context, request pipeline.ResolveRequest) (pipeline.ResolvedImage, error) {
	var zero pipeline.ResolvedImage
	if err := request.Validate(); err != nil {
		return zero, referenceFault(err)
	}
	reference, err := parseReference(request.Reference)
	if err != nil {
		return zero, referenceFault(err)
	}
	if err := ctx.Err(); err != nil {
		return zero, deadlineFault(err)
	}

	scheme := "https"
	if resolver.insecure {
		scheme = "http"
	}
	origin := scheme + "://" + apiHost(reference.host)
	current := manifestURL(scheme, reference)
	// Refuse a host that cannot form a request URL before consulting the
	// credential source: a malformed host has no request to carry a credential
	// and no origin worth asking about.
	if _, err := http.NewRequestWithContext(ctx, http.MethodGet, current, nil); err != nil {
		return zero, fault.New(fault.InvalidInput, "registry manifest request is invalid")
	}

	credential, err := resolver.credential(ctx, origin)
	if err != nil {
		return zero, err
	}

	// bearer is the token a 401 challenge was answered with; it is requested at
	// most once per resolution and never sent across origins.
	bearer := ""
	// redirects counts manifest redirect hops only: the token handshake is not
	// a redirect and never consumes the configured redirect budget.
	redirects := 0
	for {
		if err := ctx.Err(); err != nil {
			return zero, deadlineFault(err)
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, current, nil)
		if err != nil {
			return zero, fault.New(fault.InvalidInput, "registry manifest request is invalid")
		}
		request.Header.Set("Accept", strings.Join(resolver.allowedMedia, ", "))
		if bearer != "" {
			if originOf(current) == origin {
				request.Header.Set("Authorization", "Bearer "+bearer)
			}
		} else if authorization, ok, err := credential.authorization(); err != nil {
			return zero, err
		} else if ok && originOf(current) == origin {
			request.Header.Set("Authorization", authorization)
		}

		response, err := resolver.transport.RoundTrip(request)
		if err != nil {
			if contextError := ctx.Err(); contextError != nil {
				return zero, deadlineFault(contextError)
			}
			return zero, fault.Wrap(fault.DependencyUnavailable, "registry is unavailable", err)
		}
		if response == nil {
			return zero, fault.New(fault.DependencyUnavailable, "registry returned no response")
		}

		if isRedirect(response.StatusCode) {
			location := response.Header.Get("Location")
			_ = response.Body.Close()
			if strings.TrimSpace(location) == "" {
				return zero, fault.New(fault.InvalidInput, "registry redirect has no location")
			}
			target, err := redirectTarget(current, location)
			if err != nil {
				return zero, err
			}
			if target == "" {
				return zero, fault.New(fault.InvalidInput, "registry redirect target is invalid")
			}
			if redirects >= resolver.maxRedirects {
				return zero, fault.New(fault.InvalidInput, "registry redirect cap exceeded")
			}
			redirects++
			current = target
			continue
		}

		// The handshake only runs for the configured origin: a 401 from a
		// redirected-to foreign origin is an ordinary rejection, because the
		// retry would carry no Authorization header there.
		if response.StatusCode == http.StatusUnauthorized && bearer == "" && originOf(current) == origin {
			if challenge, ok := parseBearerChallenge(response.Header.Values("WWW-Authenticate")); ok {
				if !absoluteRealm(challenge.realm) {
					// A realm that is not an absolute URL cannot be fetched.
					// Answer the 401 as the ordinary rejection it already is
					// rather than turning a broken challenge into a fetch error.
					return resolver.consume(ctx, response, reference)
				}
				_ = response.Body.Close()
				token, err := resolver.fetchToken(ctx, challenge)
				if err != nil {
					return zero, err
				}
				bearer = token
				continue
			}
		}

		return resolver.consume(ctx, response, reference)
	}
}

func (resolver *Resolver) credential(ctx context.Context, origin string) (Credential, error) {
	if resolver.credentials == nil {
		return Credential{Scheme: CredentialAnonymous}, nil
	}
	credential, err := resolver.credentials.Credential(ctx, origin)
	if err != nil {
		if contextError := ctx.Err(); contextError != nil {
			return Credential{}, deadlineFault(contextError)
		}
		return Credential{}, fault.Wrap(fault.InternalFailure, "registry credential source failed", err)
	}
	return credential, nil
}

func (resolver *Resolver) consume(ctx context.Context, response *http.Response, reference parsedReference) (pipeline.ResolvedImage, error) {
	var zero pipeline.ResolvedImage
	defer func() {
		_ = response.Body.Close()
	}()

	switch {
	case response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices:
	case response.StatusCode == http.StatusUnauthorized:
		return zero, fault.New(fault.Unauthenticated, "registry authentication was rejected")
	case response.StatusCode == http.StatusForbidden:
		return zero, fault.New(fault.Forbidden, "registry access is forbidden")
	case response.StatusCode == http.StatusNotFound:
		return zero, fault.New(fault.NotFound, "registry manifest was not found")
	case response.StatusCode == http.StatusTooManyRequests, response.StatusCode >= http.StatusInternalServerError:
		return zero, fault.New(fault.DependencyUnavailable, "registry is unavailable")
	default:
		return zero, fault.New(fault.InvalidInput, "registry returned an unexpected status")
	}

	contentType := response.Header.Get("Content-Type")
	if strings.TrimSpace(contentType) == "" {
		return zero, fault.New(fault.InvalidInput, "registry response has no media type")
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return zero, fault.New(fault.InvalidInput, "registry response media type is invalid")
	}
	if _, allowed := resolver.allowedMediaSet[mediaType]; !allowed {
		return zero, fault.New(fault.InvalidInput, "registry response media type is not allowed")
	}

	body, err := readBounded(response.Body, resolver.maxResponseBytes)
	if err != nil {
		if contextError := ctx.Err(); contextError != nil {
			return zero, deadlineFault(contextError)
		}
		return zero, fault.Wrap(fault.DependencyUnavailable, "registry response could not be read", err)
	}
	if int64(len(body)) > resolver.maxResponseBytes {
		return zero, fault.New(fault.InvalidInput, "registry response exceeds the configured bound")
	}

	var document map[string]json.RawMessage
	if err := json.Unmarshal(body, &document); err != nil || document == nil {
		return zero, fault.New(fault.InvalidInput, "registry response is not a JSON object")
	}

	sum := sha256.Sum256(body)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	if headerDigest := strings.TrimSpace(response.Header.Get("Docker-Content-Digest")); headerDigest != "" && headerDigest != digest {
		return zero, fault.New(fault.InvalidInput, "registry content digest header does not match the response body")
	}
	if reference.pinned && digest != reference.digest {
		return zero, fault.New(fault.ChangedIdempotencyContent, "registry returned content that does not match the pinned digest")
	}

	return pipeline.ResolvedImage{Reference: reference.repository, Digest: digest}, nil
}

// readBounded reads at most bound+1 bytes so the caller can distinguish a body
// exactly at the bound from one byte over it.
func readBounded(body io.Reader, bound int64) ([]byte, error) {
	if bound == math.MaxInt64 {
		return io.ReadAll(body)
	}
	return io.ReadAll(io.LimitReader(body, bound+1))
}

func deadlineFault(cause error) error {
	return fault.Wrap(fault.DeadlineExpired, "registry resolution was canceled or exceeded its deadline", cause)
}

func isRedirect(status int) bool {
	switch status {
	case http.StatusMovedPermanently,
		http.StatusFound,
		http.StatusSeeOther,
		http.StatusTemporaryRedirect,
		http.StatusPermanentRedirect:
		return true
	default:
		return false
	}
}

// redirectTarget resolves a Location header and refuses a scheme downgrade to
// plaintext http unless the test-only insecure flag is set.
func redirectTarget(current, location string) (string, error) {
	base, err := url.Parse(current)
	if err != nil {
		return "", fault.New(fault.InvalidInput, "registry request URL is invalid")
	}
	relative, err := url.Parse(location)
	if err != nil {
		return "", fault.New(fault.InvalidInput, "registry redirect location is invalid")
	}
	target := base.ResolveReference(relative)
	if target.Host == "" {
		return "", fault.New(fault.InvalidInput, "registry redirect target has no host")
	}
	if target.Scheme != "https" && target.Scheme != "http" {
		return "", fault.New(fault.InvalidInput, "registry redirect target scheme is invalid")
	}
	if target.Scheme == "http" && base.Scheme == "https" {
		// A plaintext downgrade is only reachable through the explicit
		// test-only insecure flag, which makes the origin http already.
		return "", fault.New(fault.InvalidInput, "registry redirect downgrades to plaintext http")
	}
	return target.String(), nil
}

func originOf(value string) string {
	parsed, err := url.Parse(value)
	if err != nil {
		return ""
	}
	return parsed.Scheme + "://" + parsed.Host
}

func manifestURL(scheme string, reference parsedReference) string {
	return scheme + "://" + apiHost(reference.host) + "/v2/" + reference.path + "/manifests/" + reference.lookup
}

func (credential Credential) authorization() (string, bool, error) {
	switch credential.Scheme {
	case "", CredentialAnonymous:
		return "", false, nil
	case CredentialBearer:
		return "Bearer " + credential.Token, true, nil
	case CredentialBasic:
		encoded := base64.StdEncoding.EncodeToString([]byte(credential.Username + ":" + credential.Password))
		return "Basic " + encoded, true, nil
	default:
		return "", false, fault.New(fault.InvalidInput, "registry credential scheme is not supported")
	}
}

type parsedReference struct {
	repository string
	host       string
	path       string
	lookup     string
	digest     string
	pinned     bool
}

// referenceFault marks a refused reference text for admission. It keeps the
// parse fault's class and public message and adds the shared cause.
func referenceFault(err error) error {
	message, ok := fault.MessageOf(err)
	if !ok {
		message = pipeline.ErrImageReference.Error()
	}
	return fault.Wrap(fault.InvalidInput, message, pipeline.ErrImageReference)
}

func parseReference(value string) (parsedReference, error) {
	if value == "" || strings.ContainsAny(value, " \t\r\n") {
		return parsedReference{}, fault.New(fault.InvalidInput, "image reference is empty or contains whitespace")
	}
	if strings.Count(value, "@") > 1 {
		return parsedReference{}, fault.New(fault.InvalidInput, "image reference contains more than one digest separator")
	}

	repositoryPart := value
	pinned := false
	digest := ""
	if separator := strings.LastIndex(value, "@"); separator >= 0 {
		repositoryPart = value[:separator]
		digest = value[separator+1:]
		pinned = true
		if !validDigest(digest) {
			return parsedReference{}, fault.New(fault.InvalidInput, "image reference digest must be a full sha256 digest")
		}
	}

	slash := strings.Index(repositoryPart, "/")
	if slash <= 0 || slash == len(repositoryPart)-1 {
		return parsedReference{}, fault.New(fault.InvalidInput, "image reference must name a host and repository")
	}
	host := repositoryPart[:slash]
	path := repositoryPart[slash+1:]
	if !validHost(host) {
		return parsedReference{}, fault.New(fault.InvalidInput, "image reference host is invalid")
	}

	tag := ""
	if !pinned {
		colon := strings.LastIndex(path, ":")
		if colon < 0 {
			return parsedReference{}, fault.New(fault.InvalidInput, "image reference must carry a tag or an immutable digest")
		}
		tag = path[colon+1:]
		path = path[:colon]
		if !validTag(tag) {
			return parsedReference{}, fault.New(fault.InvalidInput, "image reference tag is invalid")
		}
	}
	if pinned && strings.Contains(path, ":") {
		return parsedReference{}, fault.New(fault.InvalidInput, "image reference carries both a tag and a digest; use repository@sha256:<digest>")
	}
	if !validPath(path) {
		return parsedReference{}, fault.New(fault.InvalidInput, "image reference repository path is invalid")
	}

	lookup := tag
	if pinned {
		lookup = digest
	}
	return parsedReference{
		repository: host + "/" + path,
		host:       host,
		path:       path,
		lookup:     lookup,
		digest:     digest,
		pinned:     pinned,
	}, nil
}

// apiHost returns the host that serves the registry API for an image name host.
// Docker Hub names images under docker.io (and index.docker.io), but serves the
// API, and answers anything else with a redirect, at registry-1.docker.io. The
// resolved image keeps the name host exactly as written, which the container
// runtime understands.
//
// Letter case, one trailing root dot and the explicit default https port all
// name the same Docker Hub host, so they are normalized for that decision only.
// The port is separated first, so at most one root dot is stripped from the host
// name itself: two trailing dots ("docker.io..") stay another name, and a dot
// after the port ("docker.io:443.") leaves a port that is not "443", so that
// malformed name is left as written and never mapped. Lookalike names (an added
// label, a suffix or a non-default port) are other registries and are never
// mapped. A bracketed host is an IP literal, and a bracketed value that is not
// an IP literal is therefore never a Docker Hub name: it is left as written so
// that net.SplitHostPort can never strip the brackets and turn "[docker.io]:443"
// into the Docker Hub name "docker.io".
func apiHost(host string) string {
	name := strings.ToLower(host)
	if strings.HasPrefix(name, "[") && !bracketedIPLiteral(name) {
		return host
	}
	if withoutPort, port, err := net.SplitHostPort(name); err == nil {
		if port != "443" {
			return host
		}
		name = withoutPort
	}
	name = strings.TrimSuffix(name, ".")
	switch name {
	case "docker.io", "index.docker.io":
		return "registry-1.docker.io"
	default:
		return host
	}
}

func validDigest(value string) bool {
	const prefix = "sha256:"
	if !strings.HasPrefix(value, prefix) {
		return false
	}
	hexValue := strings.TrimPrefix(value, prefix)
	if len(hexValue) != 64 {
		return false
	}
	_, err := hex.DecodeString(hexValue)
	return err == nil
}

func validHost(value string) bool {
	if value == "" || strings.ContainsAny(value, "/@ \t\r\n") {
		return false
	}
	if strings.ContainsAny(value, "[]") {
		// A bracketed host must be an IP literal, optionally with one port.
		// A bracketed DNS name such as "[docker.io]" is not an image reference
		// host: refusing it also keeps net.SplitHostPort from stripping the
		// brackets and mapping "[docker.io]:443" to Docker Hub.
		return bracketedIPLiteral(value)
	}
	if name, port, found := strings.Cut(value, ":"); found {
		// At most one colon, between a name and a port in 1..65535. Anything
		// else would only fail later, as an unavailable registry.
		number, err := strconv.ParseUint(port, 10, 16)
		if name == "" || err != nil || number == 0 {
			return false
		}
	}
	for _, character := range value {
		switch {
		case character >= 'a' && character <= 'z':
		case character >= 'A' && character <= 'Z':
		case character >= '0' && character <= '9':
		case character == '.', character == '-', character == '_', character == ':':
		default:
			return false
		}
	}
	return true
}

// bracketedIPLiteral reports whether value is a bracketed IP literal, optionally
// followed by one decimal port. The value must start with '[', close the
// literal exactly once, and hold nothing but an IP address inside the brackets;
// anything after them must be ":" and a port in 1..65535.
func bracketedIPLiteral(value string) bool {
	if !strings.HasPrefix(value, "[") {
		return false
	}
	closing := strings.IndexByte(value, ']')
	if closing < 0 {
		return false
	}
	if net.ParseIP(value[1:closing]) == nil {
		return false
	}
	rest := value[closing+1:]
	if rest == "" {
		return true
	}
	if !strings.HasPrefix(rest, ":") {
		return false
	}
	port := rest[1:]
	if port == "" || len(port) > 5 {
		return false
	}
	number := 0
	for _, character := range port {
		if character < '0' || character > '9' {
			return false
		}
		number = number*10 + int(character-'0')
	}
	return number > 0 && number <= 65535
}

func validPath(value string) bool {
	if value == "" || strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") {
		return false
	}
	for component := range strings.SplitSeq(value, "/") {
		if component == "" || component == "." || component == ".." {
			return false
		}
		for _, character := range component {
			switch {
			case character >= 'a' && character <= 'z':
			case character >= 'A' && character <= 'Z':
			case character >= '0' && character <= '9':
			case character == '.', character == '-', character == '_':
			default:
				return false
			}
		}
	}
	return true
}

func validTag(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for index, character := range value {
		switch {
		case character >= 'a' && character <= 'z':
		case character >= 'A' && character <= 'Z':
		case character >= '0' && character <= '9':
		case character == '.', character == '-', character == '_':
			if index == 0 && (character == '.' || character == '-') {
				return false
			}
		default:
			return false
		}
	}
	return true
}
