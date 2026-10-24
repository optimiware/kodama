// Package deployment contributes the `kodama deployment` commands: list, get,
// create and cleanup of recorded deployments. They translate user input into
// the public requests of api/delivery.yaml and render the answers.
//
// A deployment is a durable record around an ordinary script. Creation names
// an existing attempt, the candidate source and the image: the environment,
// its pinned configuration, the run, step and action key come from the
// attempt's immutable context on the server, so the CLI has no way to send
// current environment settings, manifests or credentials. Cleanup addresses a
// recorded deployment by its ID and an observed version; it never selects
// objects, namespaces or environments. Permissions (deployment:manage),
// provenance of the image, idempotency and every lifecycle rule stay in the
// server; viewing a deployment never approves or changes anything.
package deployment

import (
	"context"
	"flag"
	"fmt"

	"github.com/optimiware/kodama/internal/cli/root"
)

const (
	createConflictAdvice = "the idempotency key or the logical action already has a different deployment: list the deployments (`kodama deployment list`) to see the recorded one; reuse a key only with the identical attempt, source and image"

	createUncertain = "the deployment may have been recorded: repeat the identical command - same --attempt-id, --repository-url, --commit, --image-artifact-id, --image-digest and --idempotency-key - to receive the stored receipt, or list the deployments (`kodama deployment list`); never use a different key for the same deployment"

	cleanupConflictAdvice = "reread the deployment (`kodama deployment get`) and decide again from its current version and state; the CLI does not retry or refresh the version for you"

	cleanupUncertain = "the cleanup request may have been recorded: reread the deployment (`kodama deployment get`) and check its cleanup state before requesting again; cleanup is only ever requested for a recorded deployment"
)

// NewCommands returns the deployment command group for registration by the
// composition root.
func NewCommands() root.Contribution {
	return root.Contribution{
		Group:   "deployment",
		Summary: "read, register and request cleanup of recorded deployments",
		Commands: []root.Command{
			{Name: "list", Summary: "list deployment records, optionally by environment or status (one page)", Need: root.NeedFactory, Bind: bindList},
			{Name: "get", Summary: "read a deployment record: status, outcome, objects and cleanup (read-only)", Need: root.NeedFactory, Bind: bindGet},
			{Name: "create", Summary: "register a deployment against an attempt's pinned context (requires --idempotency-key)", Need: root.NeedFactory, Bind: bindCreate},
			{Name: "cleanup", Summary: "request cleanup of a recorded, settled deployment under --if-version", Need: root.NeedFactory, Bind: bindCleanup},
		},
	}
}

func bindList(fs *flag.FlagSet) root.RunFunc {
	environmentID := fs.String("environment-id", "", "restrict the list to one environment")
	status := fs.String("status", "", "restrict the list to pending, running, succeeded or failed")
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
		if set["environment-id"] {
			v, err := requireText(set, "environment-id", *environmentID)
			if err != nil {
				return root.Result{}, err
			}
			q.Set("environmentId", v)
		}
		if set["status"] {
			switch *status {
			case "pending", "running", "succeeded", "failed":
				q.Set("status", *status)
			default:
				return root.Result{}, root.Usagef("--status must be pending, running, succeeded or failed")
			}
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

func bindGet(fs *flag.FlagSet) root.RunFunc {
	return func(ctx context.Context, inv *root.Invocation, args []string) (root.Result, error) {
		id, err := deploymentID(args)
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
		doc, err := document(raw)
		if err != nil {
			return root.Result{}, err
		}
		return root.Result{Data: doc, Text: deploymentText(doc)}, nil
	}
}

func bindCreate(fs *flag.FlagSet) root.RunFunc {
	attempt := fs.String("attempt-id", "", "the originating attempt whose pinned context supplies the environment, run, step and action (required)")
	repo := fs.String("repository-url", "", "candidate source repository URL, https without credentials (required)")
	commit := fs.String("commit", "", "candidate source commit, 40 or 64 lowercase hex digits (required)")
	artifact := fs.String("image-artifact-id", "", "the verified image artifact (required)")
	digest := fs.String("image-digest", "", "the image digest, sha256:<64 hex> (required)")
	key := fs.String("idempotency-key", "", "caller-chosen key that makes a retry replay, never repeat, the registration (required)")
	return func(ctx context.Context, inv *root.Invocation, args []string) (root.Result, error) {
		if err := noArgs(args); err != nil {
			return root.Result{}, err
		}
		body, idem, err := createInputs(explicit(fs), *attempt, *repo, *commit, *artifact, *digest, *key)
		if err != nil {
			return root.Result{}, err
		}
		c, scope, err := connect(inv)
		if err != nil {
			return root.Result{}, err
		}
		raw, resp, err := send(ctx, c, createRequest(scope, idem, body))
		if err != nil {
			return root.Result{}, advise(err, createConflictAdvice, createUncertain)
		}
		doc, err := document(raw)
		if err != nil {
			return root.Result{}, err
		}
		return root.Result{Data: doc, Text: receiptText(doc, resp != nil && resp.Replayed)}, nil
	}
}

func bindCleanup(fs *flag.FlagSet) root.RunFunc {
	ifVersion := fs.String("if-version", "", "deployment version you observed (required)")
	return func(ctx context.Context, inv *root.Invocation, args []string) (root.Result, error) {
		id, err := deploymentID(args)
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
		raw, _, err := send(ctx, c, cleanupRequest(scope, id, version))
		if err != nil {
			return root.Result{}, advise(err, cleanupConflictAdvice, cleanupUncertain)
		}
		doc, err := document(raw)
		if err != nil {
			return root.Result{}, err
		}
		return root.Result{Data: doc, Text: cleanupText(doc)}, nil
	}
}

func deploymentID(args []string) (string, error) {
	id, err := oneArg(args, "the deployment ID")
	if err != nil {
		return "", err
	}
	if !idPattern.MatchString(id) {
		return "", root.Usagef("the deployment ID must look like dep_ followed by 32 lowercase hex digits (got %s)", fmt.Sprintf("%q", id))
	}
	return id, nil
}
