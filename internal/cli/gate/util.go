package gate

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"strings"

	"github.com/optimiware/kodama/internal/cli/client"
	"github.com/optimiware/kodama/internal/cli/root"
)

// Small request and rendering helpers. Every contribution package keeps its
// own copy so the packages stay independent of each other; this is transport
// plumbing only and carries no business rule.

// explicit reports the flags that were set on the command line.
func explicit(fs *flag.FlagSet) map[string]bool {
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	return set
}

// oneArg requires exactly one nonblank positional argument.
func oneArg(args []string, what string) (string, error) {
	switch {
	case len(args) == 0:
		return "", root.Usagef("%s is required", what)
	case len(args) > 1:
		return "", root.Usagef("unexpected argument %q", args[1])
	case strings.TrimSpace(args[0]) == "":
		return "", root.Usagef("%s must not be empty", what)
	}
	return args[0], nil
}

func requireText(set map[string]bool, flagName, v string) (string, error) {
	if !set[flagName] {
		return "", root.Usagef("--%s is required", flagName)
	}
	if strings.TrimSpace(v) == "" {
		return "", root.Usagef("--%s must not be empty", flagName)
	}
	return v, nil
}

// connect resolves the project scope, then the authenticated client. The scope
// is routing only: the server authorizes every request.
func connect(inv *root.Invocation) (*client.Client, client.Scope, error) {
	scope, err := inv.Context.RequireProject()
	if err != nil {
		return nil, client.Scope{}, err
	}
	c, err := inv.Client()
	if err != nil {
		return nil, client.Scope{}, err
	}
	return c, scope, nil
}

// send performs one request and never retries.
func send(ctx context.Context, c *client.Client, req client.Request) (json.RawMessage, *client.Response, error) {
	var raw json.RawMessage
	resp, err := c.Do(ctx, req, &raw)
	if err != nil {
		return nil, resp, err
	}
	return raw, resp, nil
}

func document(raw json.RawMessage) (map[string]any, error) {
	v, err := root.NormalizeJSON(raw)
	if err != nil {
		return nil, root.NewError(root.ExitFailure, "malformed_response", "the factory returned a response that is not a JSON document")
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, root.NewError(root.ExitFailure, "malformed_response", "the factory returned a response that is not a JSON object")
	}
	return m, nil
}

// advise attaches operation-specific recovery guidance to a failed decision
// without hiding the server's code, request ID, details or exit class. A
// stale subject or version conflict gains the reread guidance and an
// idempotency conflict gains its own; a decision whose outcome is unknown has
// the generic text replaced by guidance naming the unchanged retry identity.
func advise(err error) error {
	var re *client.RemoteError
	if errors.As(err, &re) && !re.Malformed {
		text := ""
		switch {
		case re.Status == 412 || re.Code == "stale_gate_subject":
			text = staleAdvice
		case re.Status == 409 && re.Code == "idempotency_conflict":
			text = conflictAdvice
		}
		if text != "" {
			c := *re
			c.Message = strings.TrimRight(re.Message, " .") + ". " + text
			c.Details = make(map[string]any, len(re.Details)+1)
			for k, v := range re.Details {
				c.Details[k] = v
			}
			c.Details["recovery"] = text
			return &c
		}
		return err
	}
	if p := root.Classify(err); p.OutcomeUncertain {
		e := root.NewError(p.Exit, p.Code, "%s", p.Message)
		e.Details = root.UncertainDetails(uncertainAdvice)
		return e
	}
	return err
}

func field(m map[string]any, key string) string {
	switch v := m[key].(type) {
	case nil:
		return "-"
	case string:
		if v == "" {
			return "-"
		}
		return v
	case json.Number:
		return v.String()
	case bool:
		if v {
			return "true"
		}
		return "false"
	default:
		b, _ := root.EncodeJSON(v)
		return strings.TrimSpace(string(b))
	}
}
