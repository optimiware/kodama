// Package pipeline contributes the `kodama pipeline` commands: validate,
// register, get and list. Validation is local and offline: it calls the same
// internal/pipeline.Validator the registration route uses, so a document is
// valid here or nowhere, and it contacts no validation endpoint. Registration
// parses the document locally (syntax only), sends the Pipeline document itself
// to the registration route, and reports what the server answered; the server
// performs the authoritative validation and allocates versions.
package pipeline

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"

	"github.com/optimiware/kodama/internal/cli/client"
	"github.com/optimiware/kodama/internal/cli/root"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/pipeline"
)

// Config injects the local validation context. Nothing here is inferred from a
// credential or a token: a diagnostic identity and the installation's
// validation policies are supplied by the composition root, and a lookup that
// is not supplied fails closed inside the validator.
type Config struct {
	// Validator is the shared structural and semantic validator, built with the
	// installation's enabled step kinds and any profile or reference lookups.
	// A self-contained M1 document needs only the capability binding; a
	// reference to a secret, environment or agent profile needs the matching
	// lookup, otherwise validation reports it unavailable and fails.
	Validator *pipeline.Validator
	// Actor identifies the diagnostic caller handed to reference policies. It is
	// used only by offline validation and never represents authenticated
	// authority; the server authorizes registration from the access token.
	Actor pipeline.Actor
	// DiagnosticScope is used when no tenant and project are selected. Optional.
	DiagnosticScope *domain.Scope
	// MaxFileBytes bounds the file read; zero selects DefaultMaxFileBytes.
	MaxFileBytes int64
}

// NewCommands returns the pipeline command group.
func NewCommands(cfg Config) (root.Contribution, error) {
	if cfg.Validator == nil {
		return root.Contribution{}, errors.New("pipeline cli: a shared validator is required")
	}
	if err := cfg.Actor.Validate(); err != nil {
		return root.Contribution{}, fmt.Errorf("pipeline cli: a diagnostic actor is required: %w", err)
	}
	if cfg.DiagnosticScope != nil {
		if err := cfg.DiagnosticScope.Validate(); err != nil {
			return root.Contribution{}, fmt.Errorf("pipeline cli: invalid diagnostic scope: %w", err)
		}
	}
	e := &engine{validator: cfg.Validator, actor: cfg.Actor, fallback: cfg.DiagnosticScope, maxFile: cfg.MaxFileBytes}
	if e.maxFile <= 0 {
		e.maxFile = DefaultMaxFileBytes
	}
	return root.Contribution{
		Group:   "pipeline",
		Summary: "validate, register and read pipeline definitions",
		Commands: []root.Command{
			{Name: "validate", Summary: "validate a pipeline file locally with the shared validator (no network)", Need: root.NeedContext, Bind: e.bindValidate},
			{Name: "register", Summary: "register a pipeline file as an immutable definition version", Need: root.NeedFactory, Bind: e.bindRegister},
			{Name: "get", Summary: "read a registered pipeline definition", Need: root.NeedFactory, Bind: bindGet},
			{Name: "list", Summary: "list registered pipeline definitions (one page)", Need: root.NeedFactory, Bind: bindList},
		},
	}, nil
}

func (e *engine) bindValidate(fs *flag.FlagSet) root.RunFunc {
	file := fs.String("file", "", "pipeline file (YAML, or .json), or - for standard input (required)")
	return func(ctx context.Context, inv *root.Invocation, args []string) (root.Result, error) {
		if err := noArgs(args); err != nil {
			return root.Result{}, err
		}
		in, err := e.load(ctx, inv.Streams.In, *file)
		if err != nil {
			return root.Result{}, err
		}
		v, err := e.validate(inv, in, ctx)
		if err != nil {
			return root.Result{}, err
		}
		return root.Result{
			Data: map[string]any{"valid": true, "name": v.Name(), "digest": string(v.Digest())},
			Text: fmt.Sprintf("valid: pipeline %q, canonical digest %s\nThis was a local check; `kodama pipeline register` asks the server, which validates again and allocates the version.", v.Name(), v.Digest()),
		}, nil
	}
}

const registerAdvice = "registration is content-addressed: repeat `kodama pipeline register` with the identical file to converge on one definition, or read `kodama pipeline list --name <name>` first; versions are allocated by the server only"

func (e *engine) bindRegister(fs *flag.FlagSet) root.RunFunc {
	file := fs.String("file", "", "pipeline file (YAML, or .json), or - for standard input (required)")
	return func(ctx context.Context, inv *root.Invocation, args []string) (root.Result, error) {
		if err := noArgs(args); err != nil {
			return root.Result{}, err
		}
		in, err := e.load(ctx, inv.Streams.In, *file)
		if err != nil {
			return root.Result{}, err
		}
		doc, err := e.parseOnly(in)
		if err != nil {
			return root.Result{}, err
		}
		body, err := e.registrationBody(in, doc)
		if err != nil {
			return root.Result{}, err
		}
		c, scope, err := connect(inv)
		if err != nil {
			return root.Result{}, err
		}
		raw, resp, err := send(ctx, c, client.Request{Method: http.MethodPost, Scope: scope, Path: []string{"pipeline-definitions"}, Body: body})
		if err != nil {
			return root.Result{}, advise(err, registerAdvice)
		}
		m, err := document(raw)
		if err != nil {
			return root.Result{}, err
		}
		return root.Result{Data: m, Text: registeredText(m, resp.Status)}, nil
	}
}

func bindGet(fs *flag.FlagSet) root.RunFunc {
	return func(ctx context.Context, inv *root.Invocation, args []string) (root.Result, error) {
		id, err := oneArg(args, "the definition ID")
		if err != nil {
			return root.Result{}, err
		}
		c, scope, err := connect(inv)
		if err != nil {
			return root.Result{}, err
		}
		raw, _, err := send(ctx, c, client.Request{Method: http.MethodGet, Scope: scope, Path: []string{"pipeline-definitions", id}})
		if err != nil {
			return root.Result{}, err
		}
		m, err := document(raw)
		if err != nil {
			return root.Result{}, err
		}
		return root.Result{Data: m, Text: definitionText(m)}, nil
	}
}

func bindList(fs *flag.FlagSet) root.RunFunc {
	name := fs.String("name", "", "restrict the list to one pipeline name")
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
		raw, _, err := send(ctx, c, client.Request{Method: http.MethodGet, Scope: scope, Path: []string{"pipeline-definitions"}, Query: q})
		if err != nil {
			return root.Result{}, err
		}
		m, err := document(raw)
		if err != nil {
			return root.Result{}, err
		}
		return root.Result{Data: m, Text: listText(m)}, nil
	}
}
