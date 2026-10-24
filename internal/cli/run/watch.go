package run

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"time"

	"github.com/optimiware/kodama/internal/cli/client"
	"github.com/optimiware/kodama/internal/cli/root"
)

// observation states of the client. They describe only what this process
// believes about its own subscription; the run's lifecycle belongs to the server.
type observeState int

const (
	stateInitial observeState = iota // no cursor yet: a fresh stream starts with a snapshot
	stateObserving
	stateReconnecting
	stateResynchronizing
	stateTerminal
)

// observed summarizes one watch for the result document.
type observed struct {
	Cursor     string `json:"cursor"`
	Events     int    `json:"events"`
	Reconnects int    `json:"reconnects"`
	Resyncs    int    `json:"resyncs"`
}

// resyncLimit bounds consecutive resynchronizations that deliver no event.
const resyncLimit = 3

// dedupeWindow bounds the remembered event IDs.
const dedupeWindow = 4096

func isTerminalStatus(s string) bool {
	return s == "succeeded" || s == "failed" || s == "cancelled"
}

// watcher follows one run. It maintains only client-side observation state.
type watcher struct {
	obs   Observer
	c     *client.Client
	scope client.Scope
	runID string
	opts  WatchOptions

	state   observeState
	cursor  string
	seen    map[string]struct{}
	order   []string
	version int64  // highest run resource version applied; never regresses
	status  string // run state last observed on the stream
	stats   observed
}

// readRun fetches the full run; it is the authority for the final result.
func (w *watcher) readRun(ctx context.Context) (map[string]any, string, error) {
	raw, _, err := send(ctx, w.c, runRequest(w.scope, w.runID))
	if err != nil {
		return nil, "", err
	}
	doc, err := document(raw)
	if err != nil {
		return nil, "", err
	}
	s, _ := doc["status"].(string)
	return doc, s, nil
}

// watch blocks until the run is terminal, the context ends, or the stream cannot
// be maintained. It returns the full run read after the terminal observation.
func (w *watcher) watch(ctx context.Context) (map[string]any, observed, error) {
	w.seen = map[string]struct{}{}
	run, status, err := w.readRun(ctx)
	if err != nil {
		return nil, w.stats, err
	}
	if isTerminalStatus(status) {
		w.state = stateTerminal
		return run, w.stats, nil
	}
	var (
		failures int
		resyncs  int
		need     bool // a snapshot must be fetched before the next subscription
	)
	for {
		if err := ctx.Err(); err != nil {
			return nil, w.stats, err
		}
		if need {
			w.state = stateResynchronizing
			snap, err := w.obs.Snapshot(ctx, w.scope, w.runID)
			if err != nil {
				if retry, wait := retryable(ctx, err); retry && failures < w.opts.MaxReconnects {
					failures++
					if err := w.backoff(ctx, failures, wait); err != nil {
						return nil, w.stats, err
					}
					continue
				}
				return nil, w.stats, w.fail(ctx, err, "the snapshot for resynchronization could not be read")
			}
			if !validSnapshot(snap) {
				return nil, w.stats, observationError("malformed_snapshot", "the resynchronization snapshot is invalid; the resume cursor was not changed", nil)
			}
			w.stats.Resyncs++
			w.cursor = snap.Cursor
			w.stats.Cursor = w.cursor
			w.applySnapshot(snap)
			need = false
		} else if w.status != "" && isTerminalStatus(w.status) {
			// The observed state said terminal: confirm it with the full run.
			run, status, err = w.readRun(ctx)
			if err != nil {
				return nil, w.stats, err
			}
			if isTerminalStatus(status) {
				w.state = stateTerminal
				return run, w.stats, nil
			}
			w.status = status // the resource disagrees: keep observing
		}
		if w.status != "" && isTerminalStatus(w.status) {
			continue // after a snapshot: confirm through the branch above
		}

		w.state = stateObserving
		if failures > 0 {
			w.state = stateReconnecting
		}
		stream, err := w.obs.Open(ctx, w.scope, w.runID, w.cursor)
		if err != nil {
			var re *client.RemoteError
			if errors.As(err, &re) && re.Status == http.StatusGone && re.Code == "cursor_expired" {
				resyncs++
				if resyncs > resyncLimit {
					return nil, w.stats, observationError("resync_loop", "the cursor expired repeatedly without progress; giving up", err)
				}
				need = true
				continue
			}
			if retry, wait := retryable(ctx, err); retry && failures < w.opts.MaxReconnects {
				failures++
				w.stats.Reconnects++
				if err := w.backoff(ctx, failures, wait); err != nil {
					return nil, w.stats, err
				}
				continue
			}
			return nil, w.stats, w.fail(ctx, err, "the observation stream could not be opened")
		}
		progressed, resync, err := w.consume(ctx, stream)
		_ = stream.Close()
		if progressed {
			failures, resyncs = 0, 0
		}
		switch {
		case err != nil && ctx.Err() != nil:
			return nil, w.stats, ctx.Err()
		case err != nil && fatalStream(err):
			return nil, w.stats, err
		case resync:
			resyncs++
			if resyncs > resyncLimit {
				return nil, w.stats, observationError("resync_loop", "retention overtook the stream repeatedly without progress; giving up", nil)
			}
			need = true
		case isTerminalStatus(w.status):
			// confirmed at the top of the loop
		default:
			// The stream ended or broke: resume from the last accepted cursor.
			if failures >= w.opts.MaxReconnects {
				return nil, w.stats, observationError("observation_unavailable", "the observation stream kept ending without progress; the run was not watched to completion", err)
			}
			failures++
			w.stats.Reconnects++
			if err := w.backoff(ctx, failures, 0); err != nil {
				return nil, w.stats, err
			}
		}
	}
}

// fatalStream reports stream errors that reconnecting cannot fix.
func fatalStream(err error) bool {
	var oe *root.Error
	return errors.As(err, &oe) || errors.Is(err, errFrameTooLarge) || errors.Is(err, errBadFrame)
}

// consume reads frames until the stream ends, a terminal state is observed, or
// a resynchronization is requested. It reports whether any frame was accepted.
func (w *watcher) consume(ctx context.Context, s Stream) (progressed, resync bool, err error) {
	for {
		f, err := s.Next()
		if err != nil {
			if errors.Is(err, errFrameTooLarge) {
				return progressed, false, observationError("frame_too_large", "an observation frame exceeded the size bound", err)
			}
			if errors.Is(err, errBadFrame) {
				return progressed, false, observationError("malformed_stream", "the observation stream is malformed", err)
			}
			return progressed, false, err
		}
		switch f.Kind {
		case FrameHeartbeat:
			continue
		case FrameResync:
			if !validResync(f.Data) {
				return progressed, false, observationError("malformed_stream", "the resynchronization request is malformed", nil)
			}
			return progressed, true, nil
		case FrameSnapshot:
			if w.cursor != "" || progressed {
				return progressed, false, observationError("malformed_stream", "a snapshot frame arrived on a resumed stream", nil)
			}
			snap, ok := decodeSnapshot(f.Data)
			if !ok || (f.ID != "" && f.ID != snap.Cursor) {
				return progressed, false, observationError("malformed_stream", "the snapshot frame is malformed", nil)
			}
			w.cursor = snap.Cursor
			w.stats.Cursor = w.cursor
			w.applySnapshot(snap)
			progressed = true
		case FrameCheckpoint:
			cursor, ok := decodeCheckpoint(f.Data)
			if w.cursor == "" || !ok || (f.ID != "" && f.ID != cursor) {
				return progressed, false, observationError("malformed_stream", "the checkpoint frame is malformed", nil)
			}
			w.cursor = cursor
			w.stats.Cursor = w.cursor
			progressed = true
		case FrameEvent:
			accepted, err := w.applyEvent(f)
			if err != nil {
				return progressed, false, err
			}
			progressed = progressed || accepted
		}
		if isTerminalStatus(w.status) {
			return progressed, false, nil
		}
	}
}

type envelope struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	TenantID string `json:"tenantId"`
	Project  string `json:"projectId"`
	Resource struct {
		Type    string      `json:"type"`
		ID      string      `json:"id"`
		Version json.Number `json:"version"`
	} `json:"resource"`
	Data struct {
		To string `json:"to"`
	} `json:"data"`
}

// applyEvent accepts one complete event frame. The resume cursor advances only
// here, after the frame was decoded and validated; a replayed event is skipped
// without moving it.
func (w *watcher) applyEvent(f Frame) (bool, error) {
	if w.cursor == "" {
		return false, observationError("malformed_stream", "an event arrived before the stream's snapshot", nil)
	}
	var e envelope
	if err := json.Unmarshal(f.Data, &e); err != nil || e.ID == "" || e.Type == "" || f.ID == "" || (f.Event != "message" && f.Event != e.Type) {
		return false, observationError("malformed_stream", "an observation event is malformed", nil)
	}
	if !validCursor(f.ID) {
		return false, observationError("malformed_stream", "an observation event carries an invalid cursor", nil)
	}
	if string(w.scope.Tenant) != e.TenantID || (e.Project != "" && string(w.scope.Project) != e.Project) {
		return false, observationError("scope_mismatch", "an event for another tenant or project arrived on this scoped stream; observation stopped", nil)
	}
	// Validate everything needed to apply the event before any state moves: a
	// frame that cannot be applied must not advance the cursor or be remembered
	// as seen, or a resume would skip it.
	ownRunChange := e.Type == "run.state_changed" && e.Resource.Type == "run" && e.Resource.ID == w.runID
	var version int64
	if ownRunChange {
		v, ok := positiveVersion(e.Resource.Version)
		if !ok || e.Data.To == "" {
			return false, observationError("malformed_stream", "a run state event lacks a valid resource version or target state", nil)
		}
		version = v
	}
	if _, dup := w.seen[e.ID]; dup {
		return false, nil
	}
	w.remember(e.ID)
	w.stats.Events++
	w.cursor = f.ID
	w.stats.Cursor = w.cursor
	if ownRunChange && version > w.version {
		w.version = version
		w.status = e.Data.To
	}
	return true, nil
}

// applySnapshot adopts the run's state from a snapshot unless it is older than
// what this client already applied.
func (w *watcher) applySnapshot(s Snapshot) {
	for _, r := range s.Resources {
		if r.Type != "run" || r.ID != w.runID {
			continue
		}
		if v, ok := positiveVersion(r.Version); ok && v >= w.version {
			w.version = v
			w.status = r.State
		}
	}
}

func (w *watcher) remember(id string) {
	w.seen[id] = struct{}{}
	w.order = append(w.order, id)
	if len(w.order) > dedupeWindow {
		delete(w.seen, w.order[0])
		w.order = w.order[1:]
	}
}

// retryable classifies an error from a read-only observation call. Throttling,
// unavailability and transport breaks are retried within bounds; authorization,
// scope and request errors terminate observation. The returned duration is the
// server's retry guidance, when any.
func retryable(ctx context.Context, err error) (bool, time.Duration) {
	if ctx.Err() != nil {
		return false, 0
	}
	var re *client.RemoteError
	if errors.As(err, &re) {
		switch {
		case re.Status == http.StatusTooManyRequests, re.Status >= 500:
			return true, re.RetryAfter
		}
		return false, 0
	}
	var te *client.TransportError
	if errors.As(err, &te) {
		return !errors.Is(err, client.ErrMalformedResponse) && !errors.Is(err, client.ErrResponseTooLarge), 0
	}
	return false, 0
}

// fail returns err unchanged when the context ended (so the exit is 5) and
// otherwise the classified failure.
func (w *watcher) fail(ctx context.Context, err error, _ string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// backoff waits min(BackoffMin*2^(n-1), BackoffMax), or the server's guidance
// when that is longer but still within the bound.
func (w *watcher) backoff(ctx context.Context, attempt int, hint time.Duration) error {
	d := w.opts.BackoffMin << (attempt - 1)
	if d <= 0 || d > w.opts.BackoffMax {
		d = w.opts.BackoffMax
	}
	if hint > d {
		d = hint
		if d > w.opts.BackoffMax {
			d = w.opts.BackoffMax
		}
	}
	return w.opts.Sleep(ctx, d)
}

// observationError reports a failure of the observation itself. The cause is
// deliberately not retained: a wrapped transport or remote error would
// classify ahead of this code and hide it.
func observationError(code, msg string, _ error) error {
	return root.NewError(root.ExitFailure, code, "%s", msg)
}

func bindWatch(newObserver func(root.EffectiveContext) (Observer, error), opts WatchOptions) func(*flag.FlagSet) root.RunFunc {
	return func(fs *flag.FlagSet) root.RunFunc {
		exitOnTerminal := fs.Bool("exit-on-terminal", false, "exit 1 when the run ends failed or cancelled")
		return func(ctx context.Context, inv *root.Invocation, args []string) (root.Result, error) {
			id, err := oneArg(args, "the run ID")
			if err != nil {
				return root.Result{}, err
			}
			c, scope, err := connect(inv)
			if err != nil {
				return root.Result{}, err
			}
			obs, err := newObserver(inv.Context)
			if err != nil {
				return root.Result{}, err
			}
			w := &watcher{obs: obs, c: c, scope: scope, runID: id, opts: opts}
			run, stats, err := w.watch(ctx)
			if err != nil {
				return root.Result{}, err
			}
			status, _ := run["status"].(string)
			if *exitOnTerminal && (status == "failed" || status == "cancelled") {
				e := root.NewError(root.ExitFailure, "run_"+status, "run %s ended %s", id, status)
				e.Details = map[string]any{"runId": id, "status": status, "reason": run["reason"], "failureReason": run["failureReason"]}
				return root.Result{}, e
			}
			return root.Result{
				Data: map[string]any{"run": run, "observation": stats},
				Text: runText(run) + fmt.Sprintf("observed %d event(s); the run is %s. Work completion is a separate human transition: a finished run never completes a work item.\n", stats.Events, status),
			}, nil
		}
	}
}
