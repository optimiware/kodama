// Package agent contributes the `kodama agent` commands: list and register
// agent profile versions. They translate user input into the public requests
// of api/agents.yaml and render the answers. Registration is content-addressed
// and validated by the server (compatibility evidence, approved models and
// tools, design baseline); the command sends the submitted profile object
// exactly as supplied and never retries, versions or repairs it.
package agent

import (
	"context"
	"flag"

	"github.com/optimiware/kodama/internal/cli/root"
)

const registerAdvice = "registration is content-addressed: list the profile versions (`kodama agent list --name <name>`) to see whether it was registered, or repeat `kodama agent register` with the identical file to converge on the same version; the CLI never retries for you"

// NewCommands returns the agent command group for registration by the
// composition root.
func NewCommands() root.Contribution {
	return root.Contribution{
		Group:   "agent",
		Summary: "list and register agent profile versions",
		Commands: []root.Command{
			{Name: "list", Summary: "list registered agent profile versions (one page)", Need: root.NeedFactory, Bind: bindList},
			{Name: "register", Summary: "register an agent profile version from a JSON file", Need: root.NeedFactory, Bind: bindRegister},
		},
	}
}

func bindList(fs *flag.FlagSet) root.RunFunc {
	name := fs.String("name", "", "restrict the list to one profile name")
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
		if set["name"] {
			if _, err := requireText(set, "name", *name); err != nil {
				return root.Result{}, err
			}
			q.Set("name", *name)
		}
		c, scope, err := connect(inv)
		if err != nil {
			return root.Result{}, err
		}
		raw, _, err := send(ctx, c, listRequest(scope, q))
		if err != nil {
			return root.Result{}, err
		}
		doc, err := document(raw)
		if err != nil {
			return root.Result{}, err
		}
		return root.Result{Data: doc, Text: listText(doc)}, nil
	}
}

func bindRegister(fs *flag.FlagSet) root.RunFunc {
	file := fs.String("file", "", "agent profile JSON file, or - for standard input (required)")
	return func(ctx context.Context, inv *root.Invocation, args []string) (root.Result, error) {
		if err := noArgs(args); err != nil {
			return root.Result{}, err
		}
		set := explicit(fs)
		if !set["file"] {
			return root.Result{}, root.Usagef("--file is required: a profile JSON file, or - for standard input")
		}
		in := &reader{ctx: ctx, stdin: inv.Streams.In}
		profile, err := in.readProfile("file", *file)
		if err != nil {
			return root.Result{}, err
		}
		c, scope, err := connect(inv)
		if err != nil {
			return root.Result{}, err
		}
		raw, resp, err := send(ctx, c, registerRequest(scope, profile))
		if err != nil {
			return root.Result{}, advise(err, registerAdvice)
		}
		doc, err := document(raw)
		if err != nil {
			return root.Result{}, err
		}
		return root.Result{Data: doc, Text: registeredText(doc, resp.Status)}, nil
	}
}
