// Package workspace contributes the `kodama workspace` commands: provision,
// get, sync, archive and restore. They address the public workspace routes of
// api/workspace.yaml and print the authoritative Git information the server
// returns (repository ID, branch, accepted commit). They never run Git, launch
// an editor, derive a clone URL or fetch publisher credentials: a human clones
// the branch with their own authorized Git identity.
package workspace

import (
	"context"
	"flag"
	"net/http"

	"github.com/optimiware/kodama/internal/cli/client"
	"github.com/optimiware/kodama/internal/cli/root"
)

// NewCommands returns the workspace command group for registration by the
// composition root.
func NewCommands() root.Contribution {
	return root.Contribution{
		Group:   "workspace",
		Summary: "provision, inspect, sync, archive and restore workspaces",
		Commands: []root.Command{
			{Name: "provision", Summary: "provision (or read) the workspace of a work item", Need: root.NeedFactory, Bind: bindProvision},
			{Name: "get", Summary: "read a workspace: branch, accepted commit, writer and state", Need: root.NeedFactory, Bind: bindGet},
			{Name: "sync", Summary: "observe authorized human Git updates under --if-version", Need: root.NeedFactory, Bind: bindAction("sync")},
			{Name: "archive", Summary: "archive a workspace under --if-version", Need: root.NeedFactory, Bind: bindAction("archive")},
			{Name: "restore", Summary: "reopen the same workspace under --if-version", Need: root.NeedFactory, Bind: bindAction("restore")},
		},
	}
}

func bindProvision(fs *flag.FlagSet) root.RunFunc {
	work := fs.String("work", "", "work item ID (required)")
	return func(ctx context.Context, inv *root.Invocation, args []string) (root.Result, error) {
		if err := noArgs(args); err != nil {
			return root.Result{}, err
		}
		id, err := requireText(explicit(fs), "work", *work)
		if err != nil {
			return root.Result{}, err
		}
		c, scope, err := connect(inv)
		if err != nil {
			return root.Result{}, err
		}
		// Provisioning is idempotent by work-item ownership: no key and no
		// version are needed, and repeating the command is the recovery path.
		raw, resp, err := send(ctx, c, client.Request{Method: http.MethodPost, Scope: scope, Path: []string{"work-items", id, "workspace"}})
		if err != nil {
			return root.Result{}, advise(err, "reread the work item and the workspace before provisioning again")
		}
		return render(raw, resp.Status)
	}
}

func bindGet(fs *flag.FlagSet) root.RunFunc {
	return func(ctx context.Context, inv *root.Invocation, args []string) (root.Result, error) {
		id, err := oneArg(args, "the workspace ID")
		if err != nil {
			return root.Result{}, err
		}
		c, scope, err := connect(inv)
		if err != nil {
			return root.Result{}, err
		}
		raw, resp, err := send(ctx, c, client.Request{Method: http.MethodGet, Scope: scope, Path: []string{"workspaces", id}})
		if err != nil {
			return root.Result{}, err
		}
		return render(raw, resp.Status)
	}
}

// pendingAdvice explains the retained-request behaviour of archive and the
// settlement conflicts of the other actions without refreshing any version.
const pendingAdvice = "poll `kodama workspace get` for the current state and version, then repeat the command with that --if-version once publication settles; the CLI does not retry or refresh the version"

// bindAction binds a versioned action on an existing workspace. Restore
// addresses the same workspace: it never provisions a replacement.
func bindAction(action string) func(*flag.FlagSet) root.RunFunc {
	return func(fs *flag.FlagSet) root.RunFunc {
		ifVersion := fs.String("if-version", "", "workspace resource version you observed (required)")
		return func(ctx context.Context, inv *root.Invocation, args []string) (root.Result, error) {
			id, err := oneArg(args, "the workspace ID")
			if err != nil {
				return root.Result{}, err
			}
			version, err := requireVersion(explicit(fs), "if-version", *ifVersion)
			if err != nil {
				return root.Result{}, err
			}
			c, scope, err := connect(inv)
			if err != nil {
				return root.Result{}, err
			}
			raw, resp, err := send(ctx, c, client.Request{
				Method: http.MethodPost, Scope: scope, Path: []string{"workspaces", id, action}, IfVersion: version,
			})
			if err != nil {
				return root.Result{}, advise(err, pendingAdvice)
			}
			return render(raw, resp.Status)
		}
	}
}

func render(raw []byte, status int) (root.Result, error) {
	doc, err := document(raw)
	if err != nil {
		return root.Result{}, err
	}
	return root.Result{Data: doc, Text: workspaceText(doc, status)}, nil
}
