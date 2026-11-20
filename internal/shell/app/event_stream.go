package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/optimiware/kodema/api"
	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/id"
	"github.com/optimiware/kodema/internal/shell/authorize"
	"github.com/optimiware/kodema/internal/shell/event"
	"github.com/optimiware/kodema/internal/shell/identity"
	natsbroker "github.com/optimiware/kodema/internal/shell/integration/nats"
	"github.com/optimiware/kodema/internal/shell/store"
	"github.com/optimiware/kodema/internal/shell/tenancy"
	transportops "github.com/optimiware/kodema/internal/shell/transport/ops"
)

const (
	defaultEventStreamPollInterval      = 500 * time.Millisecond
	defaultEventStreamKeepaliveInterval = 15 * time.Second
	maxSeenStreamEventIDs               = 4096
)

// eventWakeupSubscriber is the optional broker seam that wakes SSE streams when
// committed events arrive on a scope-qualified NATS subject (for example from
// another replica).
type eventWakeupSubscriber interface {
	SubscribeEventWakeup(context.Context, tenancy.Scope, func()) (natsbroker.Subscription, error)
}

// eventWakeupHub fans out non-blocking per-scope wakeup notifications to active
// project SSE streams. It carries no event payload and never substitutes for
// durable event-table reads.
type eventWakeupHub struct {
	mu   sync.Mutex
	subs map[tenancy.Scope]map[chan struct{}]struct{}
}

func newEventWakeupHub() *eventWakeupHub {
	return &eventWakeupHub{
		subs: make(map[tenancy.Scope]map[chan struct{}]struct{}),
	}
}

func (hub *eventWakeupHub) subscribe(scope tenancy.Scope) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	hub.mu.Lock()
	set := hub.subs[scope]
	if set == nil {
		set = make(map[chan struct{}]struct{})
		hub.subs[scope] = set
	}
	set[ch] = struct{}{}
	hub.mu.Unlock()

	var once sync.Once
	unsubscribe := func() {
		once.Do(func() {
			hub.mu.Lock()
			if current := hub.subs[scope]; current != nil {
				delete(current, ch)
				if len(current) == 0 {
					delete(hub.subs, scope)
				}
			}
			hub.mu.Unlock()
		})
	}
	return ch, unsubscribe
}

func (hub *eventWakeupHub) notify(scope tenancy.Scope) {
	if hub == nil {
		return
	}
	hub.mu.Lock()
	set := hub.subs[scope]
	if len(set) == 0 {
		hub.mu.Unlock()
		return
	}
	channels := make([]chan struct{}, 0, len(set))
	for ch := range set {
		channels = append(channels, ch)
	}
	hub.mu.Unlock()

	for _, ch := range channels {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// eventNotifyingStore wraps store.Executor so local commits containing event
// rows wake active SSE streams immediately after the transaction commits.
type eventNotifyingStore struct {
	inner store.Executor
	hub   *eventWakeupHub
}

var _ store.Executor = (*eventNotifyingStore)(nil)

func (s *eventNotifyingStore) Read(ctx context.Context, scope tenancy.Scope, table, rowID string) (store.Row, error) {
	return s.inner.Read(ctx, scope, table, rowID)
}

func (s *eventNotifyingStore) Execute(ctx context.Context, scope tenancy.Scope, unit store.Unit) (store.UnitResult, error) {
	result, err := s.inner.Execute(ctx, scope, unit)
	if err == nil && len(unit.Events) > 0 && s.hub != nil {
		notified := make(map[tenancy.Scope]struct{}, 2)
		for _, envelope := range unit.Events {
			target := envelope.Scope
			if target == (tenancy.Scope{}) {
				target = scope
			}
			if _, ok := notified[target]; !ok {
				notified[target] = struct{}{}
				s.hub.notify(target)
			}
		}
	}
	return result, err
}

// eventNotifyingController wraps store.Controller so Kernel execution and
// action transitions that commit events wake active SSE streams immediately.
type eventNotifyingController struct {
	inner store.Controller
	hub   *eventWakeupHub
}

var _ store.Controller = (*eventNotifyingController)(nil)

func (c *eventNotifyingController) Read(ctx context.Context, table, rowID string) (store.Row, error) {
	return c.inner.Read(ctx, table, rowID)
}

func (c *eventNotifyingController) Scan(ctx context.Context, scan store.ControllerScan) (store.QueryPage, error) {
	return c.inner.Scan(ctx, scan)
}

func (c *eventNotifyingController) Execute(ctx context.Context, executionID string, unit store.Unit) (store.UnitResult, error) {
	result, err := c.inner.Execute(ctx, executionID, unit)
	if err == nil && len(unit.Events) > 0 && c.hub != nil {
		notified := make(map[tenancy.Scope]struct{}, 2)
		for _, envelope := range unit.Events {
			if envelope.Scope == (tenancy.Scope{}) {
				continue
			}
			if _, ok := notified[envelope.Scope]; !ok {
				notified[envelope.Scope] = struct{}{}
				c.hub.notify(envelope.Scope)
			}
		}
	}
	return result, err
}

// projectEventStreamConfig supplies the dependencies for the authorized HTTP
// SSE project event stream handler.
type projectEventStreamConfig struct {
	Principals        authorize.CurrentPrincipal
	Scopes            transportops.ScopeResolver
	Subscriber        *event.Subscriber
	Reader            *event.Reader
	Hub               *eventWakeupHub
	BrokerWakeup      eventWakeupSubscriber
	Clock             Clock
	PollInterval      time.Duration
	KeepaliveInterval time.Duration
}

// projectEventStreamHandler serves GET /api/v1/projects/{project}/events/stream
// as text/event-stream backed by the same authentication, scope visibility,
// event.subscribe grant/expiry, and event.read durable sequence contract.
type projectEventStreamHandler struct {
	principals        authorize.CurrentPrincipal
	scopes            transportops.ScopeResolver
	subscriber        *event.Subscriber
	reader            *event.Reader
	hub               *eventWakeupHub
	brokerWakeup      eventWakeupSubscriber
	clock             Clock
	pollInterval      time.Duration
	keepaliveInterval time.Duration

	closeOnce sync.Once
	done      chan struct{}
}

func newProjectEventStreamHandler(cfg projectEventStreamConfig) (*projectEventStreamHandler, error) {
	switch {
	case cfg.Principals == nil:
		return nil, fault.New(fault.InvalidInput, "project event stream requires a principal resolver")
	case cfg.Scopes == nil:
		return nil, fault.New(fault.InvalidInput, "project event stream requires a scope resolver")
	case cfg.Subscriber == nil:
		return nil, fault.New(fault.InvalidInput, "project event stream requires an event subscriber")
	case cfg.Reader == nil:
		return nil, fault.New(fault.InvalidInput, "project event stream requires an event reader")
	case cfg.Hub == nil:
		return nil, fault.New(fault.InvalidInput, "project event stream requires a wakeup hub")
	case cfg.Clock == nil:
		return nil, fault.New(fault.InvalidInput, "project event stream requires a clock")
	}
	pollInterval := cfg.PollInterval
	if pollInterval <= 0 {
		pollInterval = defaultEventStreamPollInterval
	}
	keepaliveInterval := cfg.KeepaliveInterval
	if keepaliveInterval <= 0 {
		keepaliveInterval = defaultEventStreamKeepaliveInterval
	}
	return &projectEventStreamHandler{
		principals:        cfg.Principals,
		scopes:            cfg.Scopes,
		subscriber:        cfg.Subscriber,
		reader:            cfg.Reader,
		hub:               cfg.Hub,
		brokerWakeup:      cfg.BrokerWakeup,
		clock:             cfg.Clock,
		pollInterval:      pollInterval,
		keepaliveInterval: keepaliveInterval,
		done:              make(chan struct{}),
	}, nil
}

// Close terminates all open SSE streams cleanly during server shutdown.
func (handler *projectEventStreamHandler) Close(context.Context) error {
	if handler == nil {
		return nil
	}
	handler.closeOnce.Do(func() {
		close(handler.done)
	})
	return nil
}

// ServeProjectStream handles GET /api/v1/projects/{project}/events/stream.
func (handler *projectEventStreamHandler) ServeProjectStream(writer http.ResponseWriter, request *http.Request, projectID string) {
	if request.Method != http.MethodGet {
		writer.Header().Set("Allow", http.MethodGet)
		writeHTTPFixedError(writer, http.StatusMethodNotAllowed, api.Error{
			Code:    api.CodeInvalidInput,
			Message: "the project event stream accepts only GET",
		})
		return
	}
	if hasHTTPRequestBody(request) {
		writeHTTPError(writer, fault.New(fault.InvalidInput, "the project event stream does not accept a request body"))
		return
	}

	ctx := request.Context()
	// Authenticate before resolving project scope or touching any durable row.
	credential := strictBearerCredential(request.Header.Get("Authorization"))
	principal, err := handler.principals.Resolve(ctx, credential)
	if err != nil {
		writeHTTPError(writer, err)
		return
	}
	if err := principal.Validate(); err != nil {
		writeHTTPError(writer, fault.Wrap(fault.Unauthenticated, "principal is invalid", err))
		return
	}

	resolution, err := handler.scopes.Resolve(ctx, principal, transportops.Call{
		Operation: authorize.OperationName("event.subscribe"),
		Params:    map[string]string{"project": projectID},
	})
	if err != nil {
		writeHTTPError(writer, err)
		return
	}
	scope := resolution.Scope

	requestedSeq, hasCursor, types, err := parseEventStreamParams(request)
	if err != nil {
		writeHTTPError(writer, err)
		return
	}

	grant, err := handler.subscriber.Setup(ctx, event.SubscribeCommand{
		Actor: principal,
		Scope: scope,
		Setup: event.SubscribeSetup{
			Scope:  scope,
			Types:  types,
			Cursor: requestedSeq,
		},
	})
	if err != nil {
		writeHTTPError(writer, err)
		return
	}

	// Determine exclusive starting sequence:
	// - When an explicit since/after/Last-Event-ID cursor was supplied (including
	//   cursor(0) to backfill from the beginning), backfill strictly after it.
	// - Otherwise start live from the committed cursor returned by Setup.
	afterSeq := grant.Cursor
	if hasCursor {
		afterSeq = requestedSeq
	}

	// Subscribe to live wakeups BEFORE backfilling committed rows so events
	// committed concurrently with backfill are never missed.
	wakeupCh, unsubHub := handler.hub.subscribe(scope)
	defer unsubHub()
	if handler.brokerWakeup != nil {
		if sub, subErr := handler.brokerWakeup.SubscribeEventWakeup(ctx, scope, func() {
			handler.hub.notify(scope)
		}); subErr == nil && sub != nil {
			defer func() { _ = sub.Close(context.Background()) }()
		}
	}

	writer.Header().Set("Content-Type", api.EventStreamMediaType+"; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-cache, no-transform")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.Header().Set("X-Accel-Buffering", "no")
	writer.WriteHeader(http.StatusOK)
	flusher, _ := writer.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}

	handler.streamLoop(ctx, writer, flusher, credential, principal, scope, types, afterSeq, grant.ExpiresAt, wakeupCh)
}

func (handler *projectEventStreamHandler) streamLoop(
	ctx context.Context,
	writer http.ResponseWriter,
	flusher http.Flusher,
	credential string,
	initialPrincipal identity.Principal,
	scope tenancy.Scope,
	types []event.Type,
	afterSeq uint64,
	expiresAt time.Time,
	wakeupCh <-chan struct{},
) {
	currentPrincipal := initialPrincipal
	seen := make(map[id.EventID]struct{})

	isExpired := func() bool {
		return !handler.clock.Now().Before(expiresAt)
	}

	revalidate := func() bool {
		if isExpired() {
			return false
		}
		refreshed, err := handler.principals.Resolve(ctx, credential)
		if err != nil || !refreshed.Enabled || refreshed.Validate() != nil {
			return false
		}
		if refreshed.Kind == identity.KindHuman {
			if exp := refreshed.VerifiedExpiry(); !exp.IsZero() && exp.Before(expiresAt) {
				expiresAt = exp
				if isExpired() {
					return false
				}
			}
		}
		currentPrincipal = refreshed
		return true
	}

	drain := func() bool {
		for {
			select {
			case <-ctx.Done():
				return false
			case <-handler.done:
				return false
			default:
			}
			if !revalidate() {
				return false
			}
			page, err := handler.reader.Read(ctx, event.ReadCommand{
				Actor: currentPrincipal,
				Scope: scope,
				Query: event.ReadQuery{
					Scope:         scope,
					AfterSequence: afterSeq,
					Types:         types,
					Limit:         event.MaxReadLimit,
				},
			})
			if err != nil {
				return false
			}
			for index, envelope := range page.Events {
				if isExpired() {
					return false
				}
				seq := page.Next
				if index < len(page.Sequences) {
					seq = page.Sequences[index]
				}
				if seq <= afterSeq {
					continue
				}
				if _, duplicate := seen[envelope.ID]; duplicate {
					afterSeq = seq
					continue
				}
				streamEvt, err := buildStreamEvent(envelope, seq)
				if err != nil {
					return false
				}
				if err := writeSSEEvent(writer, flusher, streamEvt); err != nil {
					return false
				}
				afterSeq = seq
				if len(seen) >= maxSeenStreamEventIDs {
					clear(seen)
				}
				seen[envelope.ID] = struct{}{}
			}
			if page.Next > afterSeq {
				afterSeq = page.Next
			}
			if !page.HasMore {
				return true
			}
		}
	}

	// Backfill any committed events after afterSeq immediately.
	if !drain() {
		return
	}

	pollTicker := time.NewTicker(handler.pollInterval)
	defer pollTicker.Stop()
	keepaliveTicker := time.NewTicker(handler.keepaliveInterval)
	defer keepaliveTicker.Stop()

	remaining := expiresAt.Sub(handler.clock.Now())
	if remaining <= 0 {
		return
	}
	expiryTimer := time.NewTimer(remaining)
	defer expiryTimer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-handler.done:
			return
		case <-expiryTimer.C:
			rem := expiresAt.Sub(handler.clock.Now())
			if rem <= 0 {
				return
			}
			expiryTimer.Reset(rem)
		case <-wakeupCh:
			if !drain() {
				return
			}
		case <-pollTicker.C:
			if !drain() {
				return
			}
		case <-keepaliveTicker.C:
			if !revalidate() {
				return
			}
			if _, err := fmt.Fprint(writer, ": keepalive\n\n"); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}

func buildStreamEvent(envelope event.Envelope, sequence uint64) (api.StreamEvent, error) {
	eventType, ok := api.CanonicalEventType(string(envelope.Type))
	if !ok {
		return api.StreamEvent{}, fault.New(fault.InternalFailure, "committed event carries an unknown lifecycle type")
	}
	cursor, err := api.EncodeEventCursor(sequence)
	if err != nil {
		return api.StreamEvent{}, fault.Wrap(fault.InternalFailure, "event cursor encoding failed", err)
	}
	return api.StreamEvent{
		ID:             string(envelope.ID),
		Cursor:         cursor,
		Sequence:       sequence,
		Type:           eventType,
		ReportType:     string(envelope.ReportType),
		Source:         api.EventSource{Kind: string(envelope.Source.Kind), PrincipalID: string(envelope.Source.PrincipalID)},
		At:             envelope.At,
		Scope:          api.EventScope{TenantID: string(envelope.Scope.TenantID), ProjectID: string(envelope.Scope.ProjectID)},
		SubjectID:      envelope.SubjectID,
		SubjectVersion: envelope.SubjectVersion,
		CorrelationID:  envelope.CorrelationID,
		CausationID:    envelope.CausationID,
		Data:           api.DecodeEventData(envelope.Data),
	}, nil
}

func writeSSEEvent(writer http.ResponseWriter, flusher http.Flusher, evt api.StreamEvent) error {
	payload, err := json.Marshal(evt)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(writer, "id: %s\nevent: %s\ndata: %s\n\n", evt.Cursor, evt.Type, payload); err != nil {
		return err
	}
	if flusher != nil {
		flusher.Flush()
	}
	return nil
}

func parseEventStreamParams(request *http.Request) (uint64, bool, []event.Type, error) {
	query := request.URL.Query()
	for key := range query {
		switch key {
		case "since", "after", "type":
		default:
			return 0, false, nil, fault.New(fault.InvalidInput, fmt.Sprintf("unknown query parameter %q", key))
		}
	}

	sinceValues := query["since"]
	afterValues := query["after"]
	if len(sinceValues) > 1 {
		return 0, false, nil, fault.New(fault.InvalidInput, "query parameter \"since\" must not be repeated")
	}
	if len(afterValues) > 1 {
		return 0, false, nil, fault.New(fault.InvalidInput, "query parameter \"after\" must not be repeated")
	}
	since := ""
	if len(sinceValues) == 1 {
		since = strings.TrimSpace(sinceValues[0])
	}
	after := ""
	if len(afterValues) == 1 {
		after = strings.TrimSpace(afterValues[0])
	}
	if since != "" && after != "" && since != after {
		return 0, false, nil, fault.New(fault.InvalidInput, "query parameters \"since\" and \"after\" disagree")
	}

	// Standard EventSource reconnect header takes precedence over query cursor.
	cursorStr := strings.TrimSpace(request.Header.Get("Last-Event-ID"))
	if cursorStr == "" {
		if since != "" {
			cursorStr = since
		} else {
			cursorStr = after
		}
	}

	var (
		requestedSeq uint64
		hasCursor    bool
	)
	if cursorStr != "" {
		seq, err := api.DecodeEventCursor(cursorStr)
		if err != nil {
			return 0, false, nil, fault.New(fault.InvalidInput, err.Error())
		}
		requestedSeq = seq
		hasCursor = true
	}

	rawTypes := query["type"]
	var (
		publicTypes []string
		eventTypes  []event.Type
		seenTypes   = make(map[string]struct{})
	)
	for _, raw := range rawTypes {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			return 0, false, nil, fault.New(fault.InvalidInput, "query parameter \"type\" must not be empty")
		}
		if _, dup := seenTypes[trimmed]; dup {
			continue
		}
		seenTypes[trimmed] = struct{}{}
		publicTypes = append(publicTypes, trimmed)
		eventTypes = append(eventTypes, event.Type(trimmed))
	}
	if err := api.ValidateEventTypes(publicTypes); err != nil {
		return 0, false, nil, fault.New(fault.InvalidInput, err.Error())
	}

	return requestedSeq, hasCursor, eventTypes, nil
}

// strictBearerCredential extracts the bearer token using the same two-field
// Bearer rule as httpapi.
func strictBearerCredential(header string) string {
	fields := strings.Fields(header)
	if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") {
		return ""
	}
	return fields[1]
}
