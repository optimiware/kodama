package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/optimiware/kodama/internal/cli/root"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/pipeline"
)

// DefaultMaxFileBytes bounds how much of a pipeline file the CLI reads. The
// shared parser enforces its own, usually smaller, document limit.
const DefaultMaxFileBytes = 1 << 20

// loaded is a pipeline file read from disk or standard input.
type loaded struct {
	data []byte
	json bool
}

// load reads the pipeline file. A path ending in .json is parsed as strict
// JSON; anything else (including standard input, "-") as the contract's YAML
// profile.
func (c *engine) load(ctx context.Context, stdin io.Reader, path string) (loaded, error) {
	if strings.TrimSpace(path) == "" {
		return loaded{}, root.Usagef("--file is required")
	}
	if err := ctx.Err(); err != nil {
		return loaded{}, err
	}
	var r io.Reader
	var wake func()
	if path == "-" {
		if stdin == nil {
			return loaded{}, root.Usagef("--file -: no standard input is available")
		}
		r = stdin
	} else {
		// Check the type before opening: opening a FIFO with no writer blocks.
		pre, err := os.Stat(path)
		if err != nil {
			return loaded{}, root.Usagef("cannot read %q: %s", path, readReason(err))
		}
		if !pre.Mode().IsRegular() {
			return loaded{}, root.Usagef("%q is not a regular file", path)
		}
		f, err := os.Open(path)
		if err != nil {
			return loaded{}, root.Usagef("cannot read %q: %s", path, readReason(err))
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil || !st.Mode().IsRegular() || !os.SameFile(pre, st) {
			return loaded{}, root.Usagef("%q is not a regular file", path)
		}
		r = f
		wake = func() { _ = f.Close() }
	}
	data, err := readBounded(ctx, r, c.maxFile, wake)
	if err != nil {
		if isContextError(err) {
			return loaded{}, err
		}
		return loaded{}, root.Usagef("the pipeline input could not be read")
	}
	if int64(len(data)) > c.maxFile {
		return loaded{}, root.Usagef("the pipeline file is larger than %d bytes", c.maxFile)
	}
	return loaded{data: data, json: strings.HasSuffix(strings.ToLower(path), ".json")}, nil
}

func readReason(err error) string {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "no such file"
	case errors.Is(err, os.ErrPermission):
		return "permission denied"
	}
	return "unreadable"
}

// engine holds the injected validation context of one command group.
type engine struct {
	validator *pipeline.Validator
	actor     pipeline.Actor
	fallback  *domain.Scope
	maxFile   int64
}

// scopeFor chooses the scope the local validation context uses: the selected
// tenant and project, else the configured diagnostic scope. A selection grants
// nothing here: validation is offline and the server re-validates on register.
func (c *engine) scopeFor(ec root.EffectiveContext) (domain.Scope, error) {
	if ec.Tenant != "" && ec.Project != "" {
		return domain.Scope{TenantID: ec.Tenant, ProjectID: ec.Project}, nil
	}
	if c.fallback != nil {
		return *c.fallback, nil
	}
	return domain.Scope{}, root.Usagef("no project selected: pass --tenant and --project or run `kodama context use`; local validation checks references against that scope")
}

// validate runs the shared validator, the same code the registration route runs.
func (c *engine) validate(inv *root.Invocation, in loaded, ctx context.Context) (pipeline.Validated, error) {
	scope, err := c.scopeFor(inv.Context)
	if err != nil {
		return pipeline.Validated{}, err
	}
	var v pipeline.Validated
	if in.json {
		v, err = c.validator.ValidateJSON(ctx, scope, c.actor, in.data)
	} else {
		v, err = c.validator.ValidateYAML(ctx, scope, c.actor, in.data)
	}
	if err != nil {
		return pipeline.Validated{}, classifyValidation(err)
	}
	return v, nil
}

// parseOnly performs the structural parse the shared validator uses, without
// semantic or reference checks: register leaves those to the server.
func (c *engine) parseOnly(in loaded) (pipeline.Document, error) {
	var doc pipeline.Document
	var err error
	if in.json {
		doc, err = c.validator.ParseJSON(in.data)
	} else {
		doc, err = c.validator.ParseYAML(in.data)
	}
	if err != nil {
		return pipeline.Document{}, classifyValidation(err)
	}
	return doc, nil
}

// classifyValidation maps validator failures to CLI failures. A rejected
// document is a validation failure (exit 2) with bounded, content-free
// diagnostics; an unavailable lookup fails closed (exit 1).
func classifyValidation(err error) error {
	var ve *pipeline.ValidationError
	if errors.As(err, &ve) {
		list := make([]any, 0, len(ve.Diagnostics))
		var parts []string
		for i, d := range ve.Diagnostics {
			list = append(list, map[string]any{"code": d.Code, "path": d.Path, "message": d.Message})
			if i < 5 {
				parts = append(parts, d.Code+" at "+d.Path)
			}
		}
		code := "invalid_definition"
		if ve.Stage == pipeline.StageSyntax {
			code = "invalid_document"
		}
		msg := fmt.Sprintf("the pipeline document is invalid (%d finding(s)): %s", len(ve.Diagnostics), strings.Join(parts, "; "))
		if len(ve.Diagnostics) > 5 || ve.Truncated {
			msg += "; and more (see --output json details.diagnostics)"
		}
		e := root.NewError(root.ExitUsage, code, "%s", msg)
		e.Details = map[string]any{"diagnostics": list, "truncated": ve.Truncated}
		return e
	}
	if errors.Is(err, pipeline.ErrUnavailable) {
		return root.NewError(root.ExitFailure, "validation_unavailable",
			"local validation cannot complete because a required lookup is unavailable (%v); nothing was accepted", err)
	}
	return err
}

// registrationBody returns the JSON document to send to the registration route:
// the Pipeline document itself, never a file name or a wrapper. A YAML source is
// converted structurally and checked to parse back to the identical document,
// so the server sees exactly what the local parser accepted.
func (c *engine) registrationBody(in loaded, doc pipeline.Document) (json.RawMessage, error) {
	if in.json {
		return json.RawMessage(bytes.TrimSpace(in.data)), nil
	}
	var tree any
	if err := yaml.Unmarshal(in.data, &tree); err != nil {
		return nil, root.NewError(root.ExitFailure, "document_conversion_failed", "the YAML document could not be converted to JSON")
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(tree); err != nil {
		return nil, root.NewError(root.ExitFailure, "document_conversion_failed", "the YAML document could not be converted to JSON")
	}
	body := bytes.TrimSpace(buf.Bytes())
	again, err := pipeline.ParseJSON(body, pipeline.Options{MaxBytes: len(body) + 1, MaxDepth: 64, MaxNodes: 1 << 22})
	if err != nil || !reflect.DeepEqual(again, doc) {
		return nil, root.NewError(root.ExitFailure, "document_conversion_failed", "the YAML document did not convert to an equivalent JSON document; nothing was sent")
	}
	return json.RawMessage(body), nil
}
