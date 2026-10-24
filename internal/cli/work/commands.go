// Package work contributes the `kodama work` commands: create, get, list,
// update, history and transition. They translate user input into the public
// work-management requests of api/work.yaml and render the answers. Every
// business rule - transitions, WIP, permissions, history, completion - stays in
// the server; the commands send the caller's explicit expected version and
// never retry, refresh or guess one.
package work

import (
	"context"
	"flag"
	"fmt"
	"net/url"

	"github.com/optimiware/kodama/internal/cli/client"
	"github.com/optimiware/kodama/internal/cli/root"
)

const createAdvice = "list work items (`kodama work list`) to see whether the item was created before creating it again; creation has no idempotency key"

const staleAdvice = "reread the work item (`kodama work get`) and its history (`kodama work history`) before editing again; the CLI does not retry or refresh the version for you"

// NewCommands returns the work command group for registration by the
// composition root.
func NewCommands() root.Contribution {
	return root.Contribution{
		Group:   "work",
		Summary: "create, edit and transition work items",
		Commands: []root.Command{
			{Name: "create", Summary: "create a work item in backlog", Need: root.NeedFactory, Bind: bindCreate},
			{Name: "get", Summary: "read a work item", Need: root.NeedFactory, Bind: bindGet},
			{Name: "list", Summary: "list work items (one page)", Need: root.NeedFactory, Bind: bindList},
			{Name: "update", Summary: "edit a work item under --if-version", Need: root.NeedFactory, Bind: bindUpdate},
			{Name: "history", Summary: "list a work item's immutable history (one page)", Need: root.NeedFactory, Bind: bindHistory},
			{Name: "transition", Summary: "request an explicit state transition under --if-version", Need: root.NeedFactory, Bind: bindTransition},
		},
	}
}

// contentFlags are the editable content members shared by create and update.
type contentFlags struct {
	specFile     *string
	acceptFile   *string
	evidenceFile *string
}

func bindContent(fs *flag.FlagSet, verb string) contentFlags {
	return contentFlags{
		specFile:     fs.String("spec-file", "", verb+" specification Markdown file, or - for standard input"),
		acceptFile:   fs.String("acceptance-file", "", verb+" acceptance criteria JSON file: [{\"id\":\"AC-1\",\"text\":\"...\"}]"),
		evidenceFile: fs.String("evidence-file", "", verb+" evidence links JSON file: [{\"type\":\"artifact|commit\",\"id\":\"...\"}]"),
	}
}

// fill adds the content members that were set explicitly.
func (c contentFlags) fill(ctx context.Context, inv *root.Invocation, set map[string]bool, body map[string]any) error {
	in := &reader{ctx: ctx, stdin: inv.Streams.In}
	if set["spec-file"] {
		spec, err := in.readSpec("spec-file", *c.specFile)
		if err != nil {
			return err
		}
		body["spec"] = spec
	}
	if set["acceptance-file"] {
		var criteria []criterion
		if err := in.readJSON("acceptance-file", *c.acceptFile, &criteria); err != nil {
			return err
		}
		body["acceptanceCriteria"] = nonNil(criteria)
	}
	if set["evidence-file"] {
		var refs []evidenceRef
		if err := in.readJSON("evidence-file", *c.evidenceFile, &refs); err != nil {
			return err
		}
		body["evidenceLinks"] = nonNil(refs)
	}
	return nil
}

func bindCreate(fs *flag.FlagSet) root.RunFunc {
	kind := fs.String("kind", "", "epic, story, task or defect (required)")
	title := fs.String("title", "", "work item title (required)")
	content := bindContent(fs, "the")
	assignee := fs.String("assignee", "", "assignee principal ID")
	parent := fs.String("parent", "", "parent epic work item ID")
	return func(ctx context.Context, inv *root.Invocation, args []string) (root.Result, error) {
		if err := noArgs(args); err != nil {
			return root.Result{}, err
		}
		set := explicit(fs)
		if _, err := requireText(set, "kind", *kind); err != nil {
			return root.Result{}, err
		}
		if err := checkEnum("kind", *kind, workKinds); err != nil {
			return root.Result{}, err
		}
		if _, err := requireText(set, "title", *title); err != nil {
			return root.Result{}, err
		}
		body := map[string]any{"kind": *kind, "title": *title}
		if err := content.fill(ctx, inv, set, body); err != nil {
			return root.Result{}, err
		}
		for _, f := range []struct {
			name, key string
			v         *string
		}{{"assignee", "assigneeId", assignee}, {"parent", "parentId", parent}} {
			if set[f.name] {
				if _, err := requireText(set, f.name, *f.v); err != nil {
					return root.Result{}, err
				}
				body[f.key] = *f.v
			}
		}
		c, scope, err := connect(inv)
		if err != nil {
			return root.Result{}, err
		}
		raw, _, err := send(ctx, c, createRequest(scope, body))
		if err != nil {
			return root.Result{}, advise(err, createAdvice)
		}
		return itemResult(raw)
	}
}

func bindGet(fs *flag.FlagSet) root.RunFunc {
	return func(ctx context.Context, inv *root.Invocation, args []string) (root.Result, error) {
		id, err := oneArg(args, "the work item ID")
		if err != nil {
			return root.Result{}, err
		}
		c, scope, err := connect(inv)
		if err != nil {
			return root.Result{}, err
		}
		raw, _, err := send(ctx, c, getRequest(scope, id))
		if err != nil {
			return root.Result{}, err
		}
		return itemResult(raw)
	}
}

func bindList(fs *flag.FlagSet) root.RunFunc {
	var states, kinds stringList
	fs.Var(&states, "state", "filter by state; repeatable (any of)")
	fs.Var(&kinds, "kind", "filter by kind; repeatable (any of)")
	assignee := fs.String("assignee", "", "filter by assignee ID")
	parent := fs.String("parent", "", "filter by parent ID")
	blocked := fs.Bool("blocked", false, "filter by blocked (use --blocked=false for unblocked)")
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
		for _, s := range states {
			if err := checkEnum("state", s, workStates); err != nil {
				return root.Result{}, err
			}
			q.Add("state", s)
		}
		for _, k := range kinds {
			if err := checkEnum("kind", k, workKinds); err != nil {
				return root.Result{}, err
			}
			q.Add("kind", k)
		}
		for _, f := range []struct {
			name, key string
			v         *string
		}{{"assignee", "assigneeId", assignee}, {"parent", "parentId", parent}} {
			if set[f.name] {
				if _, err := requireText(set, f.name, *f.v); err != nil {
					return root.Result{}, err
				}
				q.Set(f.key, *f.v)
			}
		}
		if set["blocked"] {
			q.Set("blocked", fmt.Sprint(*blocked))
		}
		return page(ctx, inv, q, listRequest, listText)
	}
}

func bindHistory(fs *flag.FlagSet) root.RunFunc {
	limit := fs.String("limit", "", "page size")
	cursor := fs.String("cursor", "", "opaque cursor from a previous page's nextCursor")
	return func(ctx context.Context, inv *root.Invocation, args []string) (root.Result, error) {
		id, err := oneArg(args, "the work item ID")
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
		raw, _, err := send(ctx, c, historyRequest(scope, id, q))
		if err != nil {
			return root.Result{}, err
		}
		doc, err := document(raw)
		if err != nil {
			return root.Result{}, err
		}
		return root.Result{Data: doc, Text: historyText(doc)}, nil
	}
}

func bindUpdate(fs *flag.FlagSet) root.RunFunc {
	ifVersion := fs.String("if-version", "", "resource version you observed (required)")
	title := fs.String("title", "", "new title")
	content := bindContent(fs, "replacement")
	assignee := fs.String("assignee", "", "set the assignee")
	clearAssignee := fs.Bool("clear-assignee", false, "clear the assignee")
	parent := fs.String("parent", "", "set the parent epic")
	clearParent := fs.Bool("clear-parent", false, "clear the parent")
	blocked := fs.String("blocked-reason", "", "set the blocked reason")
	clearBlocked := fs.Bool("clear-blocked-reason", false, "clear the blocked reason")
	return func(ctx context.Context, inv *root.Invocation, args []string) (root.Result, error) {
		id, err := oneArg(args, "the work item ID")
		if err != nil {
			return root.Result{}, err
		}
		set := explicit(fs)
		version, err := requireVersion(set, "if-version", *ifVersion)
		if err != nil {
			return root.Result{}, err
		}
		body := map[string]any{}
		if set["title"] {
			if _, err := requireText(set, "title", *title); err != nil {
				return root.Result{}, err
			}
			body["title"] = *title
		}
		if err := content.fill(ctx, inv, set, body); err != nil {
			return root.Result{}, err
		}
		// A nullable member is either set or explicitly cleared, never both.
		for _, n := range []struct {
			setFlag, clearFlag, key string
			value                   *string
			clear                   *bool
		}{
			{"assignee", "clear-assignee", "assigneeId", assignee, clearAssignee},
			{"parent", "clear-parent", "parentId", parent, clearParent},
			{"blocked-reason", "clear-blocked-reason", "blockedReason", blocked, clearBlocked},
		} {
			clearing := set[n.clearFlag] && *n.clear
			switch {
			case set[n.setFlag] && clearing:
				return root.Result{}, root.Usagef("--%s and --%s conflict", n.setFlag, n.clearFlag)
			case set[n.setFlag]:
				if *n.value == "" {
					return root.Result{}, root.Usagef("--%s must not be empty; use --%s to clear it", n.setFlag, n.clearFlag)
				}
				body[n.key] = *n.value
			case clearing:
				body[n.key] = nil
			}
		}
		if len(body) == 0 {
			return root.Result{}, root.Usagef("nothing to change: pass at least one field to update")
		}
		c, scope, err := connect(inv)
		if err != nil {
			return root.Result{}, err
		}
		raw, _, err := send(ctx, c, updateRequest(scope, id, version, body))
		if err != nil {
			return root.Result{}, advise(err, staleAdvice)
		}
		return itemResult(raw)
	}
}

func bindTransition(fs *flag.FlagSet) root.RunFunc {
	ifVersion := fs.String("if-version", "", "resource version you observed (required)")
	to := fs.String("to", "", "target state (required)")
	reason := fs.String("reason", "", "reason recorded with the transition")
	reviewFile := fs.String("review-file", "", "JSON ReviewClaim file for a transition to review")
	completionFile := fs.String("completion-file", "", "JSON CompletionClaim file for a human completion")
	return func(ctx context.Context, inv *root.Invocation, args []string) (root.Result, error) {
		id, err := oneArg(args, "the work item ID")
		if err != nil {
			return root.Result{}, err
		}
		set := explicit(fs)
		version, err := requireVersion(set, "if-version", *ifVersion)
		if err != nil {
			return root.Result{}, err
		}
		if _, err := requireText(set, "to", *to); err != nil {
			return root.Result{}, err
		}
		if err := checkEnum("to", *to, workStates); err != nil {
			return root.Result{}, err
		}
		body := map[string]any{"to": *to}
		if set["reason"] {
			if *reason == "" {
				return root.Result{}, root.Usagef("--reason must not be empty")
			}
			body["reason"] = *reason
		}
		in := &reader{ctx: ctx, stdin: inv.Streams.In}
		if set["review-file"] {
			var claim reviewClaim
			if err := in.readJSON("review-file", *reviewFile, &claim); err != nil {
				return root.Result{}, err
			}
			body["review"] = claim
		}
		if set["completion-file"] {
			var claim completionClaim
			if err := in.readJSON("completion-file", *completionFile, &claim); err != nil {
				return root.Result{}, err
			}
			body["completion"] = claim
		}
		c, scope, err := connect(inv)
		if err != nil {
			return root.Result{}, err
		}
		raw, _, err := send(ctx, c, transitionRequest(scope, id, version, body))
		if err != nil {
			return root.Result{}, advise(err, staleAdvice)
		}
		return itemResult(raw)
	}
}

func itemResult(raw []byte) (root.Result, error) {
	doc, err := document(raw)
	if err != nil {
		return root.Result{}, err
	}
	return root.Result{Data: doc, Text: itemText(doc)}, nil
}

func page(ctx context.Context, inv *root.Invocation, q url.Values, build func(client.Scope, url.Values) client.Request, render func(map[string]any) string) (root.Result, error) {
	c, scope, err := connect(inv)
	if err != nil {
		return root.Result{}, err
	}
	raw, _, err := send(ctx, c, build(scope, q))
	if err != nil {
		return root.Result{}, err
	}
	doc, err := document(raw)
	if err != nil {
		return root.Result{}, err
	}
	return root.Result{Data: doc, Text: render(doc)}, nil
}

// nonNil keeps an explicitly supplied empty list as [] rather than null.
func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
