package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/optimiware/kodema/api"
	"github.com/optimiware/kodema/internal/fault"
)

const (
	maxSSELineBytes      = 512 << 10
	maxClientSeenEvents  = 4096
	defaultStreamBaseDly = 100 * time.Millisecond
	defaultStreamMaxDly  = 5 * time.Second
)

// EventStreamRequest selects one project's live HTTP SSE event stream.
type EventStreamRequest struct {
	ProjectID string
	Since     string
	Types     []string
}

// EventStream yields decoded project events in strictly monotonic order.
type EventStream interface {
	Next(ctx context.Context) (api.StreamEvent, error)
	Close() error
}

// EventStreamer opens a live project event stream. HTTPTransport implements
// EventStreamer alongside Transport.
type EventStreamer interface {
	StreamEvents(ctx context.Context, request EventStreamRequest) (EventStream, error)
}

var _ EventStreamer = (*HTTPTransport)(nil)

// StreamEvents connects to GET /api/v1/projects/{project}/events/stream and
// returns a cursor-resuming, deduplicating EventStream. Authentication,
// authorization, scope visibility, and parameter errors fail immediately during
// StreamEvents; subsequent EOFs (for example on server credential-expiry close)
// or transient network drops reconnect transparently using Last-Event-ID and a
// freshly read credential from Source.
func (transport *HTTPTransport) StreamEvents(ctx context.Context, request EventStreamRequest) (EventStream, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := api.ProjectEventStreamPath(request.ProjectID)
	if err != nil {
		return nil, fault.New(fault.InvalidInput, publicMessage(err))
	}
	var initialSeq uint64
	if request.Since != "" {
		seq, err := api.DecodeEventCursor(request.Since)
		if err != nil {
			return nil, fault.New(fault.InvalidInput, publicMessage(err))
		}
		initialSeq = seq
	}
	if err := api.ValidateEventTypes(request.Types); err != nil {
		return nil, fault.New(fault.InvalidInput, publicMessage(err))
	}
	if transport.RoundTripper == nil {
		return nil, fault.New(fault.InvalidInput, "the HTTP transport has no RoundTripper")
	}
	if transport.Source == nil {
		return nil, fault.New(fault.InvalidInput, "the HTTP transport has no credential source")
	}
	base, err := parseBaseURL(transport.BaseURL)
	if err != nil {
		return nil, fault.New(fault.InvalidInput, err.Error())
	}

	retry := transport.Retry
	if retry.MaxAttempts <= 0 {
		retry = RetryPolicy{MaxAttempts: 3, BaseDelay: defaultStreamBaseDly, MaxDelay: defaultStreamMaxDly}
	} else if err := retry.Validate(); err != nil {
		return nil, fault.New(fault.InvalidInput, err.Error())
	}

	streamCtx, cancel := context.WithCancel(context.Background())
	stream := &httpEventStream{
		transport:  transport,
		base:       base,
		path:       path,
		types:      append([]string(nil), request.Types...),
		retry:      retry,
		lastCursor: request.Since,
		lastSeq:    initialSeq,
		seenIDs:    make(map[string]struct{}),
		streamCtx:  streamCtx,
		cancel:     cancel,
	}

	// Establish the initial connection synchronously so permanent refusals
	// (401/403/404/400) surface immediately from StreamEvents.
	if err := stream.connectInitial(ctx); err != nil {
		_ = stream.Close()
		return nil, err
	}
	return stream, nil
}

type httpEventStream struct {
	transport *HTTPTransport
	base      *url.URL
	path      string
	types     []string
	retry     RetryPolicy

	lastCursor           string
	lastSeq              uint64
	seenIDs              map[string]struct{}
	zeroEventDisconnects int

	streamCtx context.Context
	cancel    context.CancelFunc

	mu         sync.Mutex
	closed     bool
	body       io.ReadCloser
	bodyCancel context.CancelFunc
	scanner    *bufio.Scanner
}

func (stream *httpEventStream) Close() error {
	stream.mu.Lock()
	if stream.closed {
		stream.mu.Unlock()
		return nil
	}
	stream.closed = true
	body := stream.body
	bodyCancel := stream.bodyCancel
	stream.body = nil
	stream.bodyCancel = nil
	stream.scanner = nil
	stream.mu.Unlock()

	stream.cancel()
	if bodyCancel != nil {
		bodyCancel()
	}
	if body != nil {
		return body.Close()
	}
	return nil
}

func (stream *httpEventStream) isClosed() bool {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	return stream.closed
}

func (stream *httpEventStream) setBody(body io.ReadCloser, bodyCancel context.CancelFunc) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 8192), maxSSELineBytes)

	stream.mu.Lock()
	if stream.closed {
		stream.mu.Unlock()
		if bodyCancel != nil {
			bodyCancel()
		}
		_ = body.Close()
		return context.Canceled
	}
	previous := stream.body
	previousCancel := stream.bodyCancel
	stream.body = body
	stream.bodyCancel = bodyCancel
	stream.scanner = scanner
	stream.mu.Unlock()

	if previousCancel != nil {
		previousCancel()
	}
	if previous != nil {
		_ = previous.Close()
	}
	return nil
}

func (stream *httpEventStream) closeCurrentBody() {
	stream.mu.Lock()
	body := stream.body
	bodyCancel := stream.bodyCancel
	stream.body = nil
	stream.bodyCancel = nil
	stream.scanner = nil
	stream.mu.Unlock()
	if bodyCancel != nil {
		bodyCancel()
	}
	if body != nil {
		_ = body.Close()
	}
}

func (stream *httpEventStream) currentScanner() *bufio.Scanner {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	return stream.scanner
}

func (stream *httpEventStream) connectInitial(ctx context.Context) error {
	var lastErr error
	for attempt := 1; attempt <= stream.retry.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err, retryable := stream.dial(ctx, false)
		if err == nil {
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		lastErr = err
		if !retryable || attempt == stream.retry.MaxAttempts {
			return err
		}
		if waitErr := wait(ctx, stream.transport.Sleep, stream.retry, attempt); waitErr != nil {
			return waitErr
		}
	}
	return lastErr
}

func (stream *httpEventStream) reconnect(ctx context.Context) error {
	attempt := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if stream.isClosed() {
			return io.EOF
		}
		attempt++
		err, retryable := stream.dial(ctx, true)
		if err == nil {
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if stream.isClosed() {
			return io.EOF
		}
		if !retryable {
			return err
		}
		policy := stream.retry
		if policy.BaseDelay <= 0 {
			policy.BaseDelay = defaultStreamBaseDly
		}
		if policy.MaxDelay <= 0 {
			policy.MaxDelay = defaultStreamMaxDly
		}
		if waitErr := wait(ctx, stream.transport.Sleep, policy, attempt); waitErr != nil {
			return waitErr
		}
	}
}

func (stream *httpEventStream) dial(ctx context.Context, isReconnect bool) (error, bool) {
	now := time.Now
	if stream.transport.Now != nil {
		now = stream.transport.Now
	}
	header, err := Bearer(ctx, stream.transport.Source, now())
	if err != nil {
		return err, false
	}

	target := *stream.base
	target.Path = strings.TrimSuffix(stream.base.Path, "/") + stream.path
	target.RawPath = ""
	query := url.Values{}
	if !isReconnect && stream.lastCursor != "" {
		query.Set("since", stream.lastCursor)
	}
	for _, eventType := range stream.types {
		query.Add("type", eventType)
	}
	target.RawQuery = query.Encode()

	reqCtx, reqCancel := context.WithCancel(stream.streamCtx)
	stopDialCancel := context.AfterFunc(ctx, reqCancel)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, target.String(), nil)
	if err != nil {
		stopDialCancel()
		reqCancel()
		return fault.New(fault.InternalFailure, "cannot build the event stream request"), false
	}
	req.Header.Set("Accept", api.EventStreamMediaType)
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Authorization", header)
	if isReconnect && stream.lastCursor != "" {
		req.Header.Set("Last-Event-ID", stream.lastCursor)
	}

	response, err := stream.transport.RoundTripper.RoundTrip(req)
	stopDialCancel()
	if err != nil {
		reqCancel()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr, false
		}
		return fault.New(fault.DependencyUnavailable, "the event stream endpoint is unavailable"), true
	}
	if response == nil {
		reqCancel()
		return fault.New(fault.InternalFailure, "the HTTP transport returned no response"), false
	}
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		reqCancel()
		if response.Body != nil {
			_ = response.Body.Close()
		}
		return fault.New(fault.InternalFailure, "redirect responses are never followed"), false
	}
	if response.StatusCode != http.StatusOK {
		reqCancel()
		_, failure, retryable := stream.transport.decode("event.subscribe", response, false, false)
		return failure, retryable
	}
	mediaType, _, mediaErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if mediaErr != nil || !strings.EqualFold(mediaType, api.EventStreamMediaType) {
		reqCancel()
		if response.Body != nil {
			_ = response.Body.Close()
		}
		return fault.New(fault.InternalFailure, "the event stream response is not text/event-stream"), false
	}
	if response.Body == nil {
		reqCancel()
		return fault.New(fault.InternalFailure, "the event stream response has no body"), false
	}
	stream.recordCursor(strings.TrimSpace(response.Header.Get("X-Kodema-Event-Cursor")))
	if err := stream.setBody(response.Body, reqCancel); err != nil {
		return err, false
	}
	return nil, false
}

func (stream *httpEventStream) recordCursor(cursor string) {
	if cursor == "" {
		return
	}
	if seq, err := api.DecodeEventCursor(cursor); err == nil {
		if stream.lastCursor == "" || seq > stream.lastSeq {
			stream.lastCursor = cursor
		}
		if seq > stream.lastSeq {
			stream.lastSeq = seq
		}
		return
	}
	stream.lastCursor = cursor
}

func (stream *httpEventStream) backoffZeroEventDisconnect(ctx context.Context) error {
	stream.zeroEventDisconnects++
	if stream.zeroEventDisconnects <= 1 {
		return nil
	}
	policy := stream.retry
	if policy.BaseDelay <= 0 {
		policy.BaseDelay = defaultStreamBaseDly
	}
	if policy.MaxDelay <= 0 {
		policy.MaxDelay = defaultStreamMaxDly
	}
	return wait(ctx, stream.transport.Sleep, policy, stream.zeroEventDisconnects-1)
}

func (stream *httpEventStream) Next(ctx context.Context) (api.StreamEvent, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		if err := ctx.Err(); err != nil {
			return api.StreamEvent{}, err
		}
		if stream.isClosed() {
			return api.StreamEvent{}, io.EOF
		}

		scanner := stream.currentScanner()
		if scanner == nil {
			if err := stream.reconnect(ctx); err != nil {
				return api.StreamEvent{}, err
			}
			continue
		}

		// Close the active body if ctx is cancelled while blocked in Scan().
		stopCancel := context.AfterFunc(ctx, func() {
			stream.closeCurrentBody()
		})
		evt, ok, err := readNextSSEEvent(scanner, stream.recordCursor)
		stopCancel()

		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return api.StreamEvent{}, ctxErr
			}
			if stream.isClosed() {
				return api.StreamEvent{}, io.EOF
			}
			// Transient EOF or stream read error: close and resume via Last-Event-ID.
			stream.closeCurrentBody()
			if errors.Is(err, errMalformedSSEPayload) {
				return api.StreamEvent{}, fault.Wrap(fault.InternalFailure, "the event stream payload is invalid", err)
			}
			if errors.Is(err, bufio.ErrTooLong) {
				return api.StreamEvent{}, fault.Wrap(fault.InternalFailure, "the event stream line exceeds maximum size", err)
			}
			if waitErr := stream.backoffZeroEventDisconnect(ctx); waitErr != nil {
				return api.StreamEvent{}, waitErr
			}
			if reconnectErr := stream.reconnect(ctx); reconnectErr != nil {
				return api.StreamEvent{}, reconnectErr
			}
			continue
		}
		if !ok {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return api.StreamEvent{}, ctxErr
			}
			if stream.isClosed() {
				return api.StreamEvent{}, io.EOF
			}
			stream.closeCurrentBody()
			if waitErr := stream.backoffZeroEventDisconnect(ctx); waitErr != nil {
				return api.StreamEvent{}, waitErr
			}
			if reconnectErr := stream.reconnect(ctx); reconnectErr != nil {
				return api.StreamEvent{}, reconnectErr
			}
			continue
		}

		// Deduplicate across reconnect boundaries using monotonic sequence and ID.
		if evt.Sequence > 0 && evt.Sequence <= stream.lastSeq {
			continue
		}
		if evt.ID != "" {
			if _, dup := stream.seenIDs[evt.ID]; dup {
				if evt.Sequence > stream.lastSeq {
					stream.lastSeq = evt.Sequence
				}
				if evt.Cursor != "" {
					stream.lastCursor = evt.Cursor
				}
				continue
			}
		}

		stream.zeroEventDisconnects = 0
		if evt.Sequence > stream.lastSeq {
			stream.lastSeq = evt.Sequence
		}
		if evt.Cursor != "" {
			stream.lastCursor = evt.Cursor
		}
		if evt.ID != "" {
			if len(stream.seenIDs) >= maxClientSeenEvents {
				clear(stream.seenIDs)
			}
			stream.seenIDs[evt.ID] = struct{}{}
		}
		return evt, nil
	}
}

var errMalformedSSEPayload = errors.New("malformed SSE event payload")

// readNextSSEEvent reads the next complete SSE frame from scanner, ignoring
// comment/keepalive lines while reporting any frame id via onCursor so idle
// streams preserve their resume cursor across reconnects. Returns (evt, true,
// nil) on a decoded frame, (zero, false, nil) on clean EOF, or (zero, false,
// err) on read/decode error.
func readNextSSEEvent(scanner *bufio.Scanner, onCursor func(string)) (api.StreamEvent, bool, error) {
	var (
		eventID   string
		eventType string
		dataLines []string
	)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if len(dataLines) == 0 {
				if eventID != "" && onCursor != nil {
					onCursor(eventID)
				}
				eventID = ""
				eventType = ""
				continue
			}
			payload := strings.Join(dataLines, "\n")
			var evt api.StreamEvent
			if err := json.Unmarshal([]byte(payload), &evt); err != nil {
				return api.StreamEvent{}, false, errors.Join(errMalformedSSEPayload, err)
			}
			if evt.Cursor == "" && eventID != "" {
				evt.Cursor = eventID
			}
			if evt.Type == "" && eventType != "" {
				evt.Type = eventType
			}
			return evt, true, nil
		}
		if strings.HasPrefix(line, ":") {
			// SSE comment / keepalive frame.
			continue
		}
		field, value, found := strings.Cut(line, ":")
		if !found {
			field = line
			value = ""
		} else if strings.HasPrefix(value, " ") {
			value = value[1:]
		}
		switch field {
		case "id":
			if !strings.ContainsRune(value, 0) {
				eventID = value
			}
		case "event":
			eventType = value
		case "data":
			dataLines = append(dataLines, value)
		}
	}
	if err := scanner.Err(); err != nil {
		return api.StreamEvent{}, false, err
	}
	return api.StreamEvent{}, false, nil
}
