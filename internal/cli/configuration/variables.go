// Package configuration contributes the `kodama variable` and `kodama secret`
// command groups over the configuration API of api/configuration.yaml.
//
// Variables are typed nonsecret values. Secrets are write-only: their bytes
// are read only from a protected file or explicitly redirected standard input,
// encoded as standard padded base64 for the request, and never printed, logged,
// stored or echoed. Encryption, retention, revocation accounting and every
// authorization decision stay in the server; the commands print only the
// management metadata and retention reports it returns.
package configuration

import (
	"context"
	"flag"
	"net/http"

	"github.com/optimiware/kodama/internal/cli/client"
	"github.com/optimiware/kodama/internal/cli/root"
)

const variableAdvice = "reread the variable (`kodama variable get`) before editing again; the CLI does not retry or refresh the version for you"

// NewVariableCommands returns the variable command group.
func NewVariableCommands() root.Contribution {
	return root.Contribution{
		Group:   "variable",
		Summary: "manage typed project variables",
		Commands: []root.Command{
			{Name: "create", Summary: "create a typed variable (requires --idempotency-key)", Need: root.NeedFactory, Bind: bindVariableCreate},
			{Name: "get", Summary: "read a variable", Need: root.NeedFactory, Bind: bindVariableGet},
			{Name: "list", Summary: "list variables (one page)", Need: root.NeedFactory, Bind: bindVariableList},
			{Name: "update", Summary: "change a variable's value or description under --if-version", Need: root.NeedFactory, Bind: bindVariableUpdate},
			{Name: "delete", Summary: "logically delete a variable under --if-version", Need: root.NeedFactory, Bind: bindVariableDelete},
		},
	}
}

// scalarFlags selects the value of a variable: an explicit string, or an
// explicit JSON scalar for booleans and integers. A bare string is never
// guessed into another type.
type scalarFlags struct {
	text *string
	json *string
}

func bindScalar(fs *flag.FlagSet) scalarFlags {
	return scalarFlags{
		text: fs.String("value", "", "the value as a string"),
		json: fs.String("value-json", "", "the value as one JSON scalar: a string, true/false or an integer"),
	}
}

// value returns the chosen value and whether one was given.
func (s scalarFlags) value(set map[string]bool) (any, bool, error) {
	switch {
	case set["value"] && set["value-json"]:
		return nil, false, root.Usagef("--value and --value-json conflict: choose one")
	case set["value"]:
		return *s.text, true, nil
	case set["value-json"]:
		v, err := parseScalar("value-json", *s.json)
		return v, err == nil, err
	}
	return nil, false, nil
}

func requireKey(set map[string]bool, v string) (string, error) {
	if !set["idempotency-key"] {
		return "", root.Usagef("--idempotency-key is required: choose a key and reuse it unchanged to replay this request after a lost response")
	}
	if v == "" {
		return "", root.Usagef("--idempotency-key must not be empty")
	}
	return v, nil
}

func bindVariableCreate(fs *flag.FlagSet) root.RunFunc {
	scalar := bindScalar(fs)
	description := fs.String("description", "", "variable description")
	key := fs.String("idempotency-key", "", "idempotency key (required)")
	return func(ctx context.Context, inv *root.Invocation, args []string) (root.Result, error) {
		name, err := oneArg(args, "the variable name")
		if err != nil {
			return root.Result{}, err
		}
		set := explicit(fs)
		idem, err := requireKey(set, *key)
		if err != nil {
			return root.Result{}, err
		}
		value, ok, err := scalar.value(set)
		if err != nil {
			return root.Result{}, err
		}
		if !ok {
			return root.Result{}, root.Usagef("--value or --value-json is required")
		}
		body := map[string]any{"name": name, "value": value}
		if set["description"] {
			body["description"] = *description
		}
		c, scope, err := connect(inv)
		if err != nil {
			return root.Result{}, err
		}
		raw, _, err := send(ctx, c, client.Request{Method: http.MethodPost, Scope: scope, Path: []string{"variables"}, Body: body, IdempotencyKey: idem})
		if err != nil {
			return root.Result{}, err
		}
		return variableResult(raw)
	}
}

func bindVariableGet(fs *flag.FlagSet) root.RunFunc {
	return func(ctx context.Context, inv *root.Invocation, args []string) (root.Result, error) {
		id, err := oneArg(args, "the variable ID")
		if err != nil {
			return root.Result{}, err
		}
		c, scope, err := connect(inv)
		if err != nil {
			return root.Result{}, err
		}
		raw, _, err := send(ctx, c, client.Request{Method: http.MethodGet, Scope: scope, Path: []string{"variables", id}})
		if err != nil {
			return root.Result{}, err
		}
		return variableResult(raw)
	}
}

func bindVariableList(fs *flag.FlagSet) root.RunFunc {
	limit := fs.String("limit", "", "page size")
	cursor := fs.String("cursor", "", "opaque cursor from a previous page's nextCursor")
	return func(ctx context.Context, inv *root.Invocation, args []string) (root.Result, error) {
		return listPage(ctx, inv, args, explicit(fs), *limit, *cursor, "variables", variableListText)
	}
}

func bindVariableUpdate(fs *flag.FlagSet) root.RunFunc {
	ifVersion := fs.String("if-version", "", "resource version you observed (required)")
	scalar := bindScalar(fs)
	description := fs.String("description", "", "new description")
	return func(ctx context.Context, inv *root.Invocation, args []string) (root.Result, error) {
		id, err := oneArg(args, "the variable ID")
		if err != nil {
			return root.Result{}, err
		}
		set := explicit(fs)
		version, err := requireVersion(set, "if-version", *ifVersion)
		if err != nil {
			return root.Result{}, err
		}
		body := map[string]any{}
		value, ok, err := scalar.value(set)
		if err != nil {
			return root.Result{}, err
		}
		if ok {
			body["value"] = value
		}
		if set["description"] {
			body["description"] = *description
		}
		if len(body) == 0 {
			return root.Result{}, root.Usagef("nothing to change: pass --value, --value-json or --description")
		}
		c, scope, err := connect(inv)
		if err != nil {
			return root.Result{}, err
		}
		raw, _, err := send(ctx, c, client.Request{Method: http.MethodPatch, Scope: scope, Path: []string{"variables", id}, IfVersion: version, Body: body})
		if err != nil {
			return root.Result{}, advise(err, variableAdvice)
		}
		return variableResult(raw)
	}
}

func bindVariableDelete(fs *flag.FlagSet) root.RunFunc {
	ifVersion := fs.String("if-version", "", "resource version you observed (required)")
	return func(ctx context.Context, inv *root.Invocation, args []string) (root.Result, error) {
		id, err := oneArg(args, "the variable ID")
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
		if _, _, err := send(ctx, c, client.Request{Method: http.MethodDelete, Scope: scope, Path: []string{"variables", id}, IfVersion: version}); err != nil {
			return root.Result{}, advise(err, variableAdvice)
		}
		return root.Result{
			Data: map[string]any{"deleted": true, "id": id},
			Text: "deleted variable " + id + " (logical delete; the name stays reserved and the value history is retained)",
		}, nil
	}
}

func variableResult(raw []byte) (root.Result, error) {
	doc, err := document(raw)
	if err != nil {
		return root.Result{}, err
	}
	return root.Result{Data: doc, Text: variableText(doc)}, nil
}

// listPage reads one bounded page of a collection.
func listPage(ctx context.Context, inv *root.Invocation, args []string, set map[string]bool, limit, cursor string, collection string, render func(map[string]any) string) (root.Result, error) {
	if err := noArgs(args); err != nil {
		return root.Result{}, err
	}
	q, err := pageQuery(set, limit, cursor)
	if err != nil {
		return root.Result{}, err
	}
	c, scope, err := connect(inv)
	if err != nil {
		return root.Result{}, err
	}
	raw, _, err := send(ctx, c, client.Request{Method: http.MethodGet, Scope: scope, Path: []string{collection}, Query: q})
	if err != nil {
		return root.Result{}, err
	}
	doc, err := document(raw)
	if err != nil {
		return root.Result{}, err
	}
	return root.Result{Data: doc, Text: render(doc)}, nil
}
