package agent

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"regexp"
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

func noArgs(args []string) error {
	if len(args) > 0 {
		return root.Usagef("unexpected argument %q", args[0])
	}
	return nil
}

var limitPattern = regexp.MustCompile(`^[1-9][0-9]{0,8}$`)

func requireText(set map[string]bool, flagName, v string) (string, error) {
	if !set[flagName] {
		return "", root.Usagef("--%s is required", flagName)
	}
	if strings.TrimSpace(v) == "" {
		return "", root.Usagef("--%s must not be empty", flagName)
	}
	return v, nil
}

// pageQuery builds the pagination query. Cursors are opaque and passed through
// unchanged; the server owns the limit range.
func pageQuery(set map[string]bool, limit, cursor string) (url.Values, error) {
	q := url.Values{}
	if set["limit"] {
		if !limitPattern.MatchString(limit) {
			return nil, root.Usagef("--limit must be a positive integer")
		}
		q.Set("limit", limit)
	}
	if set["cursor"] {
		if cursor == "" {
			return nil, root.Usagef("--cursor must not be empty")
		}
		q.Set("cursor", cursor)
	}
	return q, nil
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

// advise attaches operation-specific recovery guidance to a failed
// registration without hiding the server's code, request ID or exit class. A
// registration whose outcome is unknown has the generic text replaced by
// guidance that fits a content-addressed operation (it has no idempotency key).
func advise(err error, text string) error {
	var re *client.RemoteError
	if errors.As(err, &re) && !re.Malformed && re.Status == 409 {
		c := *re
		c.Message = strings.TrimRight(re.Message, " .") + ". " + text
		c.Details = make(map[string]any, len(re.Details)+1)
		for k, v := range re.Details {
			c.Details[k] = v
		}
		c.Details["recovery"] = text
		return &c
	}
	if p := root.Classify(err); p.OutcomeUncertain {
		e := root.NewError(p.Exit, p.Code, "%s", p.Message)
		e.Details = root.UncertainDetails(text)
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

func items(m map[string]any) []map[string]any {
	list, _ := m["items"].([]any)
	out := make([]map[string]any, 0, len(list))
	for _, e := range list {
		if o, ok := e.(map[string]any); ok {
			out = append(out, o)
		}
	}
	return out
}

func pageFooter(m map[string]any) string {
	if c, ok := m["nextCursor"].(string); ok && c != "" {
		return fmt.Sprintf("next page: --cursor %s", c)
	}
	return "no further pages"
}
