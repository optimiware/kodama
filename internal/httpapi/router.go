package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/optimiware/kodama/internal/domain"
)

// RouterConfig wires the router. Authenticator, Authorizer and RateLimiter are
// required: there is no default that grants access or disables throttling.
type RouterConfig struct {
	Authenticator Authenticator
	Authorizer    Authorizer
	RateLimiter   RateLimiter
	// Prefix is prepended to every route, for example "/api/v1". It must be
	// empty or start with "/" and contain no wildcards.
	Prefix string
	// MaxRequestBytes and MaxResponseBytes bound JSON bodies; zero selects
	// DefaultMaxRequestBytes and DefaultMaxResponseBytes.
	MaxRequestBytes  int64
	MaxResponseBytes int
	// Logger receives server-side failure records; nil discards them. It never
	// receives request bodies, headers or query strings.
	Logger *slog.Logger
}

// Handler serves a protected route after authentication, rate limiting and
// authorization succeeded for c.
type Handler func(w http.ResponseWriter, r *http.Request, c *Call)

// Route is the explicit registration of one protected endpoint. Registration
// without an Action or Handler is rejected, so a route cannot exist without
// authorization metadata.
type Route struct {
	Method string
	// Path may contain {tenantId} and {projectId} (projectId requires tenantId)
	// plus other single-segment wildcards. The scope of the authorization
	// target follows from the wildcards present: none means principal-level,
	// {tenantId} tenant-level, both project-level.
	Path   string
	Action Action
	// ResourceKind and ResourceParam optionally name the addressed resource:
	// ResourceParam is the wildcard holding its ID. Both or neither.
	ResourceKind  string
	ResourceParam string
	Handler       Handler
}

// Call is what an authorized handler receives.
type Call struct {
	Principal Principal
	Action    Action
	Target    Target
	RequestID string
	// Pattern is the registered "METHOD /path/{wildcards}" pattern; it
	// identifies the endpoint for cursors and idempotency.
	Pattern string

	// pathValues holds the request's value for every wildcard of Pattern, in
	// the order the wildcards appear. It is captured by the router so cursors
	// bind the concrete collection, not just the route template.
	pathValues []string
	router     *Router
}

// Router is an http.Handler over net/http's ServeMux. The zero Router serves
// 503 for every request.
type Router struct {
	cfg RouterConfig
	set settings
	mux *http.ServeMux
	h   http.Handler
}

// NewRouter validates cfg and returns a router with no routes.
func NewRouter(cfg RouterConfig) (*Router, error) {
	switch {
	case cfg.Authenticator == nil:
		return nil, errors.New("httpapi: RouterConfig.Authenticator is required")
	case cfg.Authorizer == nil:
		return nil, errors.New("httpapi: RouterConfig.Authorizer is required")
	case cfg.RateLimiter == nil:
		return nil, errors.New("httpapi: RouterConfig.RateLimiter is required (use httpapi.Unlimited() deliberately)")
	}
	if cfg.Prefix != "" && (!strings.HasPrefix(cfg.Prefix, "/") || strings.ContainsAny(cfg.Prefix, "{}") || strings.HasSuffix(cfg.Prefix, "/")) {
		return nil, fmt.Errorf("httpapi: invalid route prefix %q", cfg.Prefix)
	}
	if cfg.MaxRequestBytes < 0 || cfg.MaxResponseBytes < 0 {
		return nil, errors.New("httpapi: size bounds must not be negative")
	}
	s := settings{maxRequestBytes: cfg.MaxRequestBytes, maxResponseBytes: cfg.MaxResponseBytes, logger: cfg.Logger}
	if s.maxRequestBytes == 0 {
		s.maxRequestBytes = DefaultMaxRequestBytes
	}
	if s.maxResponseBytes == 0 {
		s.maxResponseBytes = DefaultMaxResponseBytes
	}
	rt := &Router{cfg: cfg, set: s, mux: http.NewServeMux()}
	rt.h = withRequestID(s, http.HandlerFunc(rt.serve))
	return rt, nil
}

var (
	wildcardPattern = regexp.MustCompile(`\{([^{}]*)\}`)
	wildcardName    = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*$`)
	idRegexp        = regexp.MustCompile(domain.IDPattern)
)

var protectedMethods = map[string]bool{
	http.MethodGet: true, http.MethodPost: true, http.MethodPut: true,
	http.MethodPatch: true, http.MethodDelete: true,
}

// Handle registers a protected route. It never panics: an invalid route or a
// pattern conflict is returned as an error.
func (rt *Router) Handle(route Route) (err error) {
	if rt == nil || rt.mux == nil {
		return errors.New("httpapi: router was not created with NewRouter")
	}
	if !protectedMethods[route.Method] {
		return fmt.Errorf("httpapi: unsupported method %q", route.Method)
	}
	if route.Action == "" {
		return fmt.Errorf("httpapi: route %s %s has no authorization action", route.Method, route.Path)
	}
	if route.Handler == nil {
		return fmt.Errorf("httpapi: route %s %s has no handler", route.Method, route.Path)
	}
	names, err := wildcards(route.Path)
	if err != nil {
		return err
	}
	if _, ok := names["projectId"]; ok {
		if _, ok := names["tenantId"]; !ok {
			return fmt.Errorf("httpapi: route %s has {projectId} without {tenantId}", route.Path)
		}
	}
	if (route.ResourceKind == "") != (route.ResourceParam == "") {
		return fmt.Errorf("httpapi: route %s must set ResourceKind and ResourceParam together", route.Path)
	}
	if route.ResourceParam != "" {
		if _, ok := names[route.ResourceParam]; !ok {
			return fmt.Errorf("httpapi: route %s has no wildcard {%s}", route.Path, route.ResourceParam)
		}
	}
	pattern := route.Method + " " + rt.cfg.Prefix + route.Path
	defer func() {
		if p := recover(); p != nil { // ServeMux panics on conflicting patterns.
			err = fmt.Errorf("httpapi: register %q: %v", pattern, p)
		}
	}()
	rt.mux.Handle(pattern, rt.protect(route, pattern))
	return nil
}

// HandlePublic registers an endpoint that is deliberately served without
// authentication, such as a liveness probe. It is separate from Handle so that
// every unauthenticated route is an explicit, greppable decision.
func (rt *Router) HandlePublic(method, path string, h http.HandlerFunc) (err error) {
	if rt == nil || rt.mux == nil {
		return errors.New("httpapi: router was not created with NewRouter")
	}
	if method != http.MethodGet && method != http.MethodHead {
		return errors.New("httpapi: public routes must be GET or HEAD")
	}
	if h == nil {
		return errors.New("httpapi: public route requires a handler")
	}
	if names, err := wildcards(path); err != nil || len(names) > 0 {
		return errors.New("httpapi: public routes cannot have wildcards")
	}
	pattern := method + " " + rt.cfg.Prefix + path
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("httpapi: register %q: %v", pattern, p)
		}
	}()
	rt.mux.Handle(pattern, h)
	return nil
}

func wildcards(path string) (map[string]struct{}, error) {
	if !strings.HasPrefix(path, "/") {
		return nil, fmt.Errorf("httpapi: route path %q must start with /", path)
	}
	names := map[string]struct{}{}
	for _, m := range wildcardPattern.FindAllStringSubmatch(path, -1) {
		if !wildcardName.MatchString(m[1]) {
			return nil, fmt.Errorf("httpapi: route path %q has unsupported wildcard {%s}", path, m[1])
		}
		if _, dup := names[m[1]]; dup {
			return nil, fmt.Errorf("httpapi: route path %q repeats {%s}", path, m[1])
		}
		names[m[1]] = struct{}{}
	}
	if strings.ContainsAny(wildcardPattern.ReplaceAllString(path, ""), "{}") {
		return nil, fmt.Errorf("httpapi: route path %q has a malformed wildcard", path)
	}
	return names, nil
}

// ServeHTTP implements http.Handler.
func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if rt == nil || rt.h == nil {
		w.Header().Set(RequestIDHeader, newRequestID())
		http.Error(w, `{"error":{"code":"unavailable","message":"router is not configured","requestId":"","details":{}}}`, http.StatusServiceUnavailable)
		return
	}
	rt.h.ServeHTTP(w, r)
}

func (rt *Router) serve(w http.ResponseWriter, r *http.Request) {
	defer func() {
		p := recover()
		if p == nil {
			return
		}
		if p == http.ErrAbortHandler {
			panic(p)
		}
		// Log the panic's type only: its value could carry request data.
		loggerFrom(r.Context()).Error("handler panic", slog.String("requestId", RequestIDFrom(r.Context())), slog.String("type", fmt.Sprintf("%T", p)))
		WriteError(w, r, InternalServerError(errors.New("handler panic")))
	}()
	if _, pattern := rt.mux.Handler(r); pattern == "" {
		rt.unmatched(w, r)
		return
	}
	rt.mux.ServeHTTP(w, r)
}

// unmatched renders the mux's 404/405 as the standard envelope.
func (rt *Router) unmatched(w http.ResponseWriter, r *http.Request) {
	h, _ := rt.mux.Handler(r)
	probe := &probeWriter{header: http.Header{}}
	h.ServeHTTP(probe, r)
	if probe.status == http.StatusMethodNotAllowed {
		e := NewError(http.StatusMethodNotAllowed, domain.CodeInvalidRequest, "the method is not allowed for this resource")
		if allow := probe.header.Get("Allow"); allow != "" {
			e.Header = http.Header{"Allow": {allow}}
		}
		WriteError(w, r, e)
		return
	}
	WriteError(w, r, NotFoundError())
}

type probeWriter struct {
	header http.Header
	status int
}

func (p *probeWriter) Header() http.Header         { return p.header }
func (p *probeWriter) Write(b []byte) (int, error) { return len(b), nil }
func (p *probeWriter) WriteHeader(code int) {
	if p.status == 0 {
		p.status = code
	}
}

// protect wraps a route's handler in the fixed order: authenticate, validate
// identifiers, rate limit, authorize, serve. Nothing past authentication runs
// for an unauthenticated request, and nothing past authorization runs for a
// refused one.
func (rt *Router) protect(route Route, pattern string) http.Handler {
	_, hasTenant := mustWildcards(route.Path)["tenantId"]
	_, hasProject := mustWildcards(route.Path)["projectId"]
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		principal, err := rt.cfg.Authenticator.Authenticate(ctx, r)
		if err != nil {
			WriteError(w, r, authenticationError(err))
			return
		}
		if principal.Validate() != nil {
			WriteError(w, r, UnauthorizedError())
			return
		}

		target := Target{}
		if hasTenant {
			target.TenantID = domain.TenantID(r.PathValue("tenantId"))
			if target.TenantID.Validate() != nil {
				WriteError(w, r, BadRequestError("the tenant identifier is not valid"))
				return
			}
		}
		if hasProject {
			target.ProjectID = domain.ProjectID(r.PathValue("projectId"))
			if target.ProjectID.Validate() != nil {
				WriteError(w, r, BadRequestError("the project identifier is not valid"))
				return
			}
		}
		if route.ResourceParam != "" {
			id := r.PathValue(route.ResourceParam)
			if !idRegexp.MatchString(id) {
				WriteError(w, r, BadRequestError("the resource identifier is not valid"))
				return
			}
			target.Resource = Reference{Kind: route.ResourceKind, ID: id}
		}

		decision, err := rt.cfg.RateLimiter.Allow(ctx, RateKey{
			Principal: principal, TenantID: target.TenantID, ProjectID: target.ProjectID, Action: route.Action,
		})
		if err != nil {
			WriteError(w, r, UnavailableError(0, err))
			return
		}
		if !decision.Allowed {
			WriteError(w, r, RateLimitedError(decision.RetryAfter))
			return
		}

		if err := rt.cfg.Authorizer.Authorize(ctx, principal, route.Action, target); err != nil {
			WriteError(w, r, authorizationError(err))
			return
		}

		route.Handler(w, r, &Call{
			Principal: principal, Action: route.Action, Target: target,
			RequestID: RequestIDFrom(ctx), Pattern: pattern,
			pathValues: pathValues(pattern, r), router: rt,
		})
	})
}

// pathValues returns the request's value for each wildcard of pattern, in
// pattern order. The router matched the request against pattern, so every
// wildcard has a value; the values keep their decoded form and are framed, never
// joined, wherever they are authenticated.
func pathValues(pattern string, r *http.Request) []string {
	ms := wildcardPattern.FindAllStringSubmatch(pattern, -1)
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, r.PathValue(m[1]))
	}
	return out
}

func mustWildcards(path string) map[string]struct{} {
	names, err := wildcards(path)
	if err != nil {
		panic(err) // validated by Handle before protect is called
	}
	return names
}

// Scope returns the tenant/project scope for project-level routes.
func (c *Call) Scope() (domain.Scope, error) {
	s := domain.Scope{TenantID: c.Target.TenantID, ProjectID: c.Target.ProjectID}
	if err := s.Validate(); err != nil {
		return domain.Scope{}, err
	}
	return s, nil
}

// AuthorizeReference is the replay check for an idempotent response, made after
// any wait on a concurrent request and before cached content is released. It
// asks the Authorizer again, with this call's principal, scope and action,
// about the original route target (including the addressed resource of a
// resource-scoped route), and then, when ref names a different resource, about
// ref as well. A zero or identical ref adds no second question, but the original
// target is always rechecked, so a stored response without a target is never
// released on stale route or resource access. Refusals and outages are returned
// already mapped to their API errors. Callers must not hold a SQL transaction.
func (c *Call) AuthorizeReference(ctx context.Context, ref Reference) error {
	if c == nil || c.router == nil {
		return UnavailableError(0, errors.New("httpapi: call is not bound to a router"))
	}
	if err := c.router.cfg.Authorizer.Authorize(ctx, c.Principal, c.Action, c.Target); err != nil {
		return authorizationError(err)
	}
	if ref == (Reference{}) || ref == c.Target.Resource {
		return nil
	}
	t := c.Target
	t.Resource = ref
	if err := c.router.cfg.Authorizer.Authorize(ctx, c.Principal, c.Action, t); err != nil {
		return authorizationError(err)
	}
	return nil
}

// CursorScope binds a list request to this call's tenant, project, endpoint and
// every concrete path identifier of the route (for example the run in
// /runs/{runId}/steps) plus the normalized filters and sort the module applied.
func (c *Call) CursorScope(filters map[string][]string, sort string) CursorScope {
	return CursorScope{
		TenantID: c.Target.TenantID, ProjectID: c.Target.ProjectID,
		Endpoint: c.Pattern, PathValues: append([]string(nil), c.pathValues...),
		Filters: filters, Sort: sort,
	}
}
