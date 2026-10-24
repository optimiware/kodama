// Package run contributes the `kodama run` commands: start, get, list, steps,
// cancel, watch and logs. They submit keyed, exact requests and report the
// server's answers; admission, scheduling, cancellation settlement and log
// access remain server-owned. Nothing here retries a mutation, completes a work
// item after a successful run, or treats a cancellation request as a settled
// cancellation.
package run

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"time"

	"github.com/optimiware/kodama/internal/cli/client"
	"github.com/optimiware/kodama/internal/cli/root"
)

// Config injects the observation transport. The shared client buffers whole
// responses and cannot carry Last-Event-ID, so `run watch` uses a narrow,
// read-only observation adapter built from the same credential resolver and
// HTTP client the runner uses.
type Config struct {
	// Tokens is the runner's credential resolver. Required unless Observer is set.
	Tokens root.TokenResolver
	// HTTP is copied for observation requests; nil selects a default client with
	// no overall timeout (streams are bounded by the command context).
	HTTP *http.Client
	// AllowInsecureLoopback mirrors the runner's development exception.
	AllowInsecureLoopback bool
	// Observer builds the observation port for an effective context. Nil selects
	// the HTTP adapter (NewHTTPObserver). Tests and alternative transports
	// replace it.
	Observer func(root.EffectiveContext) (Observer, error)
	// Watch tunes reconnection and frame bounds; zero values select defaults.
	Watch WatchOptions
}

// WatchOptions bound observation. They protect the CLI; they are not server policy.
type WatchOptions struct {
	// MaxReconnects is how many consecutive reconnects without progress are
	// tolerated (default 5).
	MaxReconnects int
	// BackoffMin and BackoffMax bound the exponential reconnect delay
	// (default 250ms and 5s).
	BackoffMin, BackoffMax time.Duration
	// IdleTimeout reconnects a stream that delivered nothing, not even a
	// heartbeat (default 90s).
	IdleTimeout time.Duration
	// MaxFrameBytes bounds one SSE frame (default 1 MiB).
	MaxFrameBytes int
	// Sleep waits between reconnects; nil waits on a timer. A test hook.
	Sleep func(ctx context.Context, d time.Duration) error
}

func (o WatchOptions) withDefaults() WatchOptions {
	if o.MaxReconnects <= 0 {
		o.MaxReconnects = 5
	}
	if o.BackoffMin <= 0 {
		o.BackoffMin = 250 * time.Millisecond
	}
	if o.BackoffMax < o.BackoffMin {
		o.BackoffMax = 5 * time.Second
		if o.BackoffMax < o.BackoffMin {
			o.BackoffMax = o.BackoffMin
		}
	}
	if o.IdleTimeout <= 0 {
		o.IdleTimeout = 90 * time.Second
	}
	if o.MaxFrameBytes <= 0 {
		o.MaxFrameBytes = 1 << 20
	}
	if o.Sleep == nil {
		o.Sleep = sleepTimer
	}
	return o
}

func sleepTimer(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// NewCommands returns the run command group.
func NewCommands(cfg Config) (root.Contribution, error) {
	if cfg.Observer == nil && cfg.Tokens == nil {
		return root.Contribution{}, errors.New("run cli: a token resolver or an observer factory is required")
	}
	opts := cfg.Watch.withDefaults()
	newObserver := cfg.Observer
	if newObserver == nil {
		newObserver = func(ec root.EffectiveContext) (Observer, error) {
			src, err := cfg.Tokens.TokenSource(ec)
			if err != nil {
				return nil, err
			}
			return NewHTTPObserver(HTTPObserverConfig{
				BaseURL: ec.Factory, Tokens: src, HTTP: cfg.HTTP, AllowInsecureLoopback: cfg.AllowInsecureLoopback,
				MaxFrameBytes: opts.MaxFrameBytes, IdleTimeout: opts.IdleTimeout,
			})
		}
	}
	return root.Contribution{
		Group:   "run",
		Summary: "start, observe, cancel and read pipeline runs",
		Commands: []root.Command{
			{Name: "start", Summary: "start a run (requires --idempotency-key)", Need: root.NeedFactory, Bind: bindStart},
			{Name: "get", Summary: "read a run", Need: root.NeedFactory, Bind: bindGet},
			{Name: "list", Summary: "list runs (one page)", Need: root.NeedFactory, Bind: bindList},
			{Name: "steps", Summary: "list a run's steps and attempts (one page)", Need: root.NeedFactory, Bind: bindSteps},
			{Name: "cancel", Summary: "request cancellation of a run", Need: root.NeedFactory, Bind: bindCancel},
			{Name: "watch", Summary: "observe a run until it is terminal", Need: root.NeedFactory, Bind: bindWatch(newObserver, opts)},
			{Name: "logs", Summary: "read the bounded log of one attempt", Need: root.NeedFactory, Bind: bindLogs},
		},
	}, nil
}

const startAdvice = "the run may have been created: read `kodama run list --work <id>`, or repeat the identical command with the identical --idempotency-key to receive the original answer; a changed request under the same key conflicts and a new key creates another run"

var runStatuses = map[string]bool{"queued": true, "running": true, "succeeded": true, "failed": true, "cancelled": true}

func bindStart(fs *flag.FlagSet) root.RunFunc {
	work := fs.String("work", "", "work item ID (required)")
	definition := fs.String("definition", "", "pipeline definition ID (required)")
	revision := fs.String("content-revision", "", "observed work item content revision (required)")
	commit := fs.String("source-commit", "", "full source commit (required)")
	key := fs.String("idempotency-key", "", "idempotency key (required)")
	var inputs, jsonInputs stringList
	fs.Var(&inputs, "input", "run input NAME=VALUE as a string; repeatable (an artifact is its artifact ID)")
	fs.Var(&jsonInputs, "input-json", "run input NAME=JSON as one JSON string, boolean or integer; repeatable")
	return func(ctx context.Context, inv *root.Invocation, args []string) (root.Result, error) {
		if err := noArgs(args); err != nil {
			return root.Result{}, err
		}
		set := explicit(fs)
		for _, f := range []struct{ name, v string }{{"work", *work}, {"definition", *definition}} {
			if _, err := requireText(set, f.name, f.v); err != nil {
				return root.Result{}, err
			}
		}
		if !set["content-revision"] {
			return root.Result{}, root.Usagef("--content-revision is required: the content revision you observed")
		}
		rev, err := parseRevision(*revision)
		if err != nil {
			return root.Result{}, err
		}
		if _, err := requireText(set, "source-commit", *commit); err != nil {
			return root.Result{}, err
		}
		if !commitPattern.MatchString(*commit) {
			return root.Result{}, root.Usagef("--source-commit must be a full lowercase 40 or 64 digit Git object name")
		}
		if !set["idempotency-key"] {
			return root.Result{}, root.Usagef("--idempotency-key is required: choose a key and reuse it unchanged to replay this request after a lost response")
		}
		if *key == "" {
			return root.Result{}, root.Usagef("--idempotency-key must not be empty")
		}
		in := newInputSet()
		for _, s := range inputs {
			if err := in.add("input", s, stringScalar); err != nil {
				return root.Result{}, err
			}
		}
		for _, s := range jsonInputs {
			if err := in.add("input-json", s, jsonScalar); err != nil {
				return root.Result{}, err
			}
		}
		body := map[string]any{"workItemId": *work, "definitionId": *definition, "contentRevision": rev, "sourceCommit": *commit}
		if b := in.body(); b != nil {
			body["inputs"] = b
		}
		c, scope, err := connect(inv)
		if err != nil {
			return root.Result{}, err
		}
		raw, resp, err := send(ctx, c, client.Request{Method: http.MethodPost, Scope: scope, Path: []string{"runs"}, Body: body, IdempotencyKey: *key})
		if err != nil {
			return root.Result{}, advise(err, startAdvice)
		}
		doc, err := document(raw)
		if err != nil {
			return root.Result{}, err
		}
		text := runText(doc)
		if resp.Replayed {
			text += "This answer replays the stored result of the earlier request with the same key.\n"
		}
		return root.Result{Data: doc, Text: text}, nil
	}
}

func bindGet(fs *flag.FlagSet) root.RunFunc {
	return func(ctx context.Context, inv *root.Invocation, args []string) (root.Result, error) {
		id, err := oneArg(args, "the run ID")
		if err != nil {
			return root.Result{}, err
		}
		c, scope, err := connect(inv)
		if err != nil {
			return root.Result{}, err
		}
		raw, _, err := send(ctx, c, runRequest(scope, id))
		if err != nil {
			return root.Result{}, err
		}
		doc, err := document(raw)
		if err != nil {
			return root.Result{}, err
		}
		return root.Result{Data: doc, Text: runText(doc)}, nil
	}
}

func runRequest(scope client.Scope, id string) client.Request {
	return client.Request{Method: http.MethodGet, Scope: scope, Path: []string{"runs", id}}
}

func bindList(fs *flag.FlagSet) root.RunFunc {
	var statuses stringList
	fs.Var(&statuses, "status", "filter by status; repeatable (any of)")
	work := fs.String("work", "", "restrict to one work item")
	limit := fs.String("limit", "", "page size")
	cursor := fs.String("cursor", "", "opaque cursor from a previous page's nextCursor")
	return func(ctx context.Context, inv *root.Invocation, args []string) (root.Result, error) {
		if err := noArgs(args); err != nil {
			return root.Result{}, err
		}
		set := explicit(fs)
		q, err := pageQuery(set, *limit, *cursor)
		if err != nil {
			return root.Result{}, err
		}
		for _, s := range statuses {
			if !runStatuses[s] {
				return root.Result{}, root.Usagef("--status %q is not one of the accepted values", s)
			}
			q.Add("status", s)
		}
		if set["work"] {
			if _, err := requireText(set, "work", *work); err != nil {
				return root.Result{}, err
			}
			q.Set("workItemId", *work)
		}
		c, scope, err := connect(inv)
		if err != nil {
			return root.Result{}, err
		}
		raw, _, err := send(ctx, c, client.Request{Method: http.MethodGet, Scope: scope, Path: []string{"runs"}, Query: q})
		if err != nil {
			return root.Result{}, err
		}
		doc, err := document(raw)
		if err != nil {
			return root.Result{}, err
		}
		return root.Result{Data: doc, Text: runListText(doc)}, nil
	}
}

func bindSteps(fs *flag.FlagSet) root.RunFunc {
	limit := fs.String("limit", "", "page size")
	cursor := fs.String("cursor", "", "opaque cursor from a previous page's nextCursor")
	return func(ctx context.Context, inv *root.Invocation, args []string) (root.Result, error) {
		id, err := oneArg(args, "the run ID")
		if err != nil {
			return root.Result{}, err
		}
		q, err := pageQuery(explicit(fs), *limit, *cursor)
		if err != nil {
			return root.Result{}, err
		}
		c, scope, err := connect(inv)
		if err != nil {
			return root.Result{}, err
		}
		raw, _, err := send(ctx, c, client.Request{Method: http.MethodGet, Scope: scope, Path: []string{"runs", id, "steps"}, Query: q})
		if err != nil {
			return root.Result{}, err
		}
		doc, err := document(raw)
		if err != nil {
			return root.Result{}, err
		}
		return root.Result{Data: doc, Text: stepsText(doc)}, nil
	}
}

const cancelAdvice = "cancellation may have been recorded: read `kodama run get` to see cancelRequestedAt; repeating `kodama run cancel` is safe because the request is idempotent"

func bindCancel(fs *flag.FlagSet) root.RunFunc {
	return func(ctx context.Context, inv *root.Invocation, args []string) (root.Result, error) {
		id, err := oneArg(args, "the run ID")
		if err != nil {
			return root.Result{}, err
		}
		c, scope, err := connect(inv)
		if err != nil {
			return root.Result{}, err
		}
		raw, _, err := send(ctx, c, client.Request{Method: http.MethodPost, Scope: scope, Path: []string{"runs", id, "cancel"}})
		if err != nil {
			return root.Result{}, advise(err, cancelAdvice)
		}
		doc, err := document(raw)
		if err != nil {
			return root.Result{}, err
		}
		text := runText(doc)
		switch s := field(doc, "status"); s {
		case "succeeded", "failed", "cancelled":
			text += fmt.Sprintf("The run is already %s; cancelling a terminal run changes nothing.\n", s)
		default:
			text += "Cancellation is a request, not a settled outcome: the run stays " + s + " until its admitted work settles. Poll `kodama run get` or `kodama run watch`.\n"
		}
		return root.Result{Data: doc, Text: text}, nil
	}
}
