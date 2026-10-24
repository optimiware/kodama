// Package gate contributes the `kodama gate` commands: get and decide. They
// translate user input into the public requests of api/gates.yaml and render
// the answers. Inspection is a read and never approves anything. A decision is
// sent only when the caller explicitly supplies the decision, the subject
// digest they reviewed, a reason, the gate version they observed and an
// idempotency key; the CLI never infers, refreshes, regenerates or retries any
// of them. Every rule - reviewer authority, subject and version checks,
// applicability, conformance, replay - stays in the server.
package gate

import (
	"context"
	"flag"

	"github.com/optimiware/kodama/internal/cli/root"
)

const (
	staleAdvice = "inspect the gate again (`kodama gate get`) and review its current subject, evidence and version; decide again only deliberately, with the new digest and version and a new idempotency key - the CLI does not retry or refresh them for you"

	conflictAdvice = "the idempotency key was already used for a different decision: inspect the gate (`kodama gate get`) to see whether a decision was recorded; reuse a key only with the identical decision, digest, reason and version"

	uncertainAdvice = "the decision may have been recorded: inspect the gate (`kodama gate get`), or repeat the identical command - same gate, decision, subject digest, reason, --if-version and --idempotency-key - to replay the stored result; never use a different key for the same decision"
)

// NewCommands returns the gate command group for registration by the
// composition root.
func NewCommands() root.Contribution {
	return root.Contribution{
		Group:   "gate",
		Summary: "inspect gates and record an explicit human decision",
		Commands: []root.Command{
			{Name: "get", Summary: "inspect a gate: subject, evidence, version and applicability (read-only)", Need: root.NeedFactory, Bind: bindGet},
			{Name: "decide", Summary: "record the one immutable decision of a gate", Need: root.NeedFactory, Bind: bindDecide},
		},
	}
}

func bindGet(fs *flag.FlagSet) root.RunFunc {
	return func(ctx context.Context, inv *root.Invocation, args []string) (root.Result, error) {
		id, err := oneArg(args, "the gate ID")
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
		return root.Result{Data: doc, Text: gateText(doc, false)}, nil
	}
}

func bindDecide(fs *flag.FlagSet) root.RunFunc {
	decision := fs.String("decision", "", "approve or reject (required)")
	digest := fs.String("subject-digest", "", "the subject digest you reviewed, sha256:<64 hex> (required)")
	reason := fs.String("reason", "", "your reason, recorded with the decision (required)")
	ifVersion := fs.String("if-version", "", "gate version you observed (required)")
	key := fs.String("idempotency-key", "", "caller-chosen key that makes a retry replay, never repeat, the decision (required)")
	return func(ctx context.Context, inv *root.Invocation, args []string) (root.Result, error) {
		id, err := oneArg(args, "the gate ID")
		if err != nil {
			return root.Result{}, err
		}
		set := explicit(fs)
		body, version, idem, err := decisionInputs(set, *decision, *digest, *reason, *ifVersion, *key)
		if err != nil {
			return root.Result{}, err
		}
		c, scope, err := connect(inv)
		if err != nil {
			return root.Result{}, err
		}
		raw, resp, err := send(ctx, c, decideRequest(scope, id, version, idem, body))
		if err != nil {
			return root.Result{}, advise(err)
		}
		doc, err := document(raw)
		if err != nil {
			return root.Result{}, err
		}
		return root.Result{Data: doc, Text: gateText(doc, resp != nil && resp.Replayed)}, nil
	}
}
