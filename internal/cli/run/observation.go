package run

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/optimiware/kodama/internal/cli/client"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
)

// Observer is the run-owned port for observing one run's retained events
// (api/observation.yaml). Both operations are read-only and scoped: every call,
// including every reconnect, names the tenant, project and run, so access is
// authorized again by the server each time.
type Observer interface {
	// Snapshot fetches the application/json snapshot of the run's event scope
	// and the cursor it is consistent with.
	Snapshot(ctx context.Context, scope client.Scope, runID string) (Snapshot, error)
	// Open starts the event stream. A non-empty cursor resumes exactly after it
	// (Last-Event-ID); empty starts a fresh stream that begins with a snapshot
	// frame. A non-2xx answer is returned as a *client.RemoteError.
	Open(ctx context.Context, scope client.Scope, runID, cursor string) (Stream, error)
}

// Stream yields complete frames. Next returns io.EOF at a clean end and another
// error when the stream broke; Close releases the connection and its timers.
type Stream interface {
	Next() (Frame, error)
	Close() error
}

// Snapshot is the narrow current state of the observed projection.
type Snapshot struct {
	Cursor    string     `json:"cursor"`
	Resources []Resource `json:"resources"`
}

// Resource is one observed resource.
type Resource struct {
	Type    string      `json:"type"`
	ID      string      `json:"id"`
	Version json.Number `json:"version"`
	State   string      `json:"state"`
	RunID   string      `json:"runId,omitempty"`
	StepID  string      `json:"stepId,omitempty"`
}

// HTTPObserverConfig wires the HTTP adapter of Observer.
type HTTPObserverConfig struct {
	// BaseURL is the factory origin.
	BaseURL string
	// Tokens supplies the opaque access token; the adapter never parses it.
	Tokens client.AccessTokenSource
	// HTTP is copied; its redirect policy is replaced by a refusal, so a
	// credential cannot follow a redirect to another destination.
	HTTP                  *http.Client
	AllowInsecureLoopback bool
	// MaxFrameBytes bounds one frame; IdleTimeout reconnects a silent stream.
	MaxFrameBytes int
	IdleTimeout   time.Duration
	// Now dates HTTP-date Retry-After guidance; time.Now when nil.
	Now func() time.Time
}

type httpObserver struct {
	base   *url.URL
	tokens client.AccessTokenSource
	http   http.Client
	json   *client.Client
	max    int
	idle   time.Duration
	now    func() time.Time
}

// NewHTTPObserver builds the HTTP adapter. It addresses only the factory
// origin under /api/v1, only the two scoped observation GET routes of one run,
// and refuses redirects.
func NewHTTPObserver(cfg HTTPObserverConfig) (Observer, error) {
	if cfg.Tokens == nil {
		return nil, errors.New("run observer: an access token source is required")
	}
	base, err := client.ParseBaseURL(cfg.BaseURL, cfg.AllowInsecureLoopback)
	if err != nil {
		return nil, err
	}
	o := &httpObserver{base: base, tokens: cfg.Tokens, max: cfg.MaxFrameBytes, idle: cfg.IdleTimeout, now: cfg.Now}
	if o.now == nil {
		o.now = time.Now
	}
	if cfg.HTTP != nil {
		o.http = *cfg.HTTP
	}
	o.http.Timeout = 0 // a stream is bounded by its context, not a total deadline
	o.http.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if o.max <= 0 {
		o.max = 1 << 20
	}
	o.json, err = client.New(client.Config{BaseURL: cfg.BaseURL, AllowInsecureLoopback: cfg.AllowInsecureLoopback, Tokens: cfg.Tokens, HTTP: cfg.HTTP})
	if err != nil {
		return nil, err
	}
	return o, nil
}

func (o *httpObserver) eventsPath(scope client.Scope, runID string) ([]string, error) {
	if err := scope.Tenant.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", client.ErrInvalidRequest, err)
	}
	if err := scope.Project.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", client.ErrInvalidRequest, err)
	}
	if err := domain.RunID(runID).Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", client.ErrInvalidRequest, err)
	}
	return []string{"runs", runID, "events"}, nil
}

// Snapshot reads the JSON representation through the shared client.
func (o *httpObserver) Snapshot(ctx context.Context, scope client.Scope, runID string) (Snapshot, error) {
	path, err := o.eventsPath(scope, runID)
	if err != nil {
		return Snapshot{}, err
	}
	var raw json.RawMessage
	if _, err := o.json.Do(ctx, client.Request{Method: http.MethodGet, Scope: scope, Path: path}, &raw); err != nil {
		return Snapshot{}, err
	}
	s, ok := decodeSnapshot(raw)
	if !ok {
		return Snapshot{}, &client.TransportError{Method: http.MethodGet, Path: "snapshot", Err: errors.Join(client.ErrMalformedResponse, errors.New("invalid snapshot"))}
	}
	return s, nil
}

// Open starts the event stream.
func (o *httpObserver) Open(ctx context.Context, scope client.Scope, runID, cursor string) (Stream, error) {
	segs, err := o.eventsPath(scope, runID)
	if err != nil {
		return nil, err
	}
	escaped := client.APIPrefix + "/tenants/" + url.PathEscape(string(scope.Tenant)) + "/projects/" + url.PathEscape(string(scope.Project))
	for _, s := range segs {
		escaped += "/" + url.PathEscape(s)
	}
	decoded, err := url.PathUnescape(escaped)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", client.ErrInvalidRequest, err)
	}
	token, err := o.tokens.AccessToken(ctx)
	if err != nil {
		return nil, err
	}
	if token == "" || strings.ContainsAny(token, " \t\r\n\x00") {
		return nil, fmt.Errorf("%w: credential is empty or malformed", client.ErrNotAuthenticated)
	}
	u := *o.base
	u.Path, u.RawPath = decoded, escaped
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("run observer: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	if cursor != "" {
		req.Header.Set("Last-Event-ID", cursor)
	}
	res, err := o.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, &client.TransportError{Method: http.MethodGet, Path: escaped, Err: err}
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		defer res.Body.Close()
		return nil, remoteFailure(res, o.now())
	}
	if mt, _, _ := mime.ParseMediaType(res.Header.Get("Content-Type")); mt != "text/event-stream" {
		_ = res.Body.Close()
		return nil, &client.TransportError{Method: http.MethodGet, Path: escaped,
			Err: errors.Join(client.ErrMalformedResponse, errors.New("response is not an event stream"))}
	}
	return newHTTPStream(res.Body, o.max, o.idle), nil
}

// remoteFailure classifies a non-2xx answer exactly as the shared client does.
func remoteFailure(res *http.Response, now time.Time) error {
	e := &client.RemoteError{Status: res.StatusCode, RequestID: res.Header.Get("X-Request-ID")}
	e.RetryAfter = parseRetryAfter(res.Header.Get("Retry-After"), now)
	body, err := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	var env httpapi.ErrorEnvelope
	if mt, _, _ := mime.ParseMediaType(res.Header.Get("Content-Type")); err == nil && (mt == "application/json" || strings.HasSuffix(mt, "+json")) &&
		json.Unmarshal(body, &env) == nil && env.Error.Code != "" {
		e.Code, e.Message, e.Details = env.Error.Code, env.Error.Message, env.Error.Details
		if env.Error.RequestId != "" {
			e.RequestID = env.Error.RequestId
		}
		return e
	}
	e.Malformed = true
	return e
}

// httpStream owns one response body and the idle watchdog that closes it.
type httpStream struct {
	body   io.ReadCloser
	frames *frameReader
	timer  *time.Timer
	idle   time.Duration
	once   sync.Once
}

func newHTTPStream(body io.ReadCloser, max int, idle time.Duration) *httpStream {
	s := &httpStream{body: body, idle: idle}
	s.frames = newFrameReader(&activityReader{r: body, touch: s.touch}, max)
	if idle > 0 {
		s.timer = time.AfterFunc(idle, func() { _ = body.Close() })
	}
	return s
}

func (s *httpStream) touch() {
	if s.timer != nil {
		s.timer.Reset(s.idle)
	}
}

func (s *httpStream) Next() (Frame, error) { return s.frames.next() }

func (s *httpStream) Close() error {
	var err error
	s.once.Do(func() {
		if s.timer != nil {
			s.timer.Stop()
		}
		err = s.body.Close()
	})
	return err
}

// activityReader resets the idle watchdog whenever bytes arrive, so heartbeats
// keep a quiet but healthy stream open.
type activityReader struct {
	r     io.Reader
	touch func()
}

func (a *activityReader) Read(p []byte) (int, error) {
	n, err := a.r.Read(p)
	if n > 0 {
		a.touch()
	}
	return n, err
}
