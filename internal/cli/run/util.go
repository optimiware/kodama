package run

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
	"github.com/optimiware/kodama/internal/domain"
)

// This file holds the small request and rendering helpers shared by this
// package's commands. Every contribution package keeps its own copy so the
// packages stay independent of each other; the logic is transport plumbing
// only and carries no business rule.

// explicit reports the flags that were set on the command line, so an omitted
// flag is distinguishable from one set to its zero value.
func explicit(fs *flag.FlagSet) map[string]bool {
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	return set
}

// stringList is a repeatable string flag.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// noArgs rejects positional arguments.
func noArgs(args []string) error {
	if len(args) > 0 {
		return root.Usagef("unexpected argument %q", args[0])
	}
	return nil
}

// oneArg requires exactly one positional argument.
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

var (
	versionPattern = regexp.MustCompile(`^[1-9][0-9]{0,15}$`)
	limitPattern   = regexp.MustCompile(`^[1-9][0-9]{0,8}$`)
)

// parseVersion reads an expected resource version exactly as written.
func parseVersion(flagName, v string) (domain.Version, error) {
	if !versionPattern.MatchString(v) {
		return 0, root.Usagef("--%s must be a positive integer resource version", flagName)
	}
	var n int64
	for _, c := range v {
		n = n*10 + int64(c-'0')
	}
	ver := domain.Version(n)
	if err := ver.Validate(); err != nil {
		return 0, root.Usagef("--%s must be between 1 and %d", flagName, int64(domain.MaxSafeInteger))
	}
	return ver, nil
}

// requireVersion parses a mandatory --if-version style flag. The version is
// the caller's observation: it is never defaulted or refreshed.
func requireVersion(set map[string]bool, flagName, v string) (domain.Version, error) {
	if !set[flagName] {
		return 0, root.Usagef("--%s is required: pass the resource version you observed", flagName)
	}
	return parseVersion(flagName, v)
}

// requireText rejects an omitted or blank mandatory text flag.
func requireText(set map[string]bool, flagName, v string) (string, error) {
	if !set[flagName] {
		return "", root.Usagef("--%s is required", flagName)
	}
	if strings.TrimSpace(v) == "" {
		return "", root.Usagef("--%s must not be empty", flagName)
	}
	return v, nil
}

// pageQuery builds the shared pagination query. Cursors are opaque and passed
// through unchanged; a limit is only checked for decimal syntax, the server
// owns the range.
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

// connect resolves the project scope and the authenticated client. The scope
// is checked first so a missing selection is a usage failure, not a credential
// failure. A saved or explicit selection is routing only: the server decides.
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

// send performs one request and returns the response body verbatim. It never
// retries and never alters the version or key the caller chose.
func send(ctx context.Context, c *client.Client, req client.Request) (json.RawMessage, *client.Response, error) {
	var raw json.RawMessage
	resp, err := c.Do(ctx, req, &raw)
	if err != nil {
		return nil, resp, err
	}
	return raw, resp, nil
}

// document decodes a response body so it re-renders with sorted keys while
// keeping numbers exactly as the server wrote them.
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

// advise attaches operation-specific recovery guidance to a failed mutation
// without hiding the server's code, request ID or exit class. A conflict or
// precondition failure gains the guidance in its message and details; a
// mutation whose outcome is unknown has the generic text replaced by the
// guidance that fits this operation (some operations carry no idempotency key).
func advise(err error, text string) error {
	var re *client.RemoteError
	if errors.As(err, &re) && !re.Malformed && (re.Status == 409 || re.Status == 412) {
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
		// Rebuilt without a cause: a retained transport error would classify
		// ahead of the replacement guidance.
		e := root.NewError(p.Exit, p.Code, "%s", p.Message)
		e.Details = root.UncertainDetails(text)
		return e
	}
	return err
}

// field renders one member of a decoded document for human output.
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

// items returns the decoded items of a page.
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

// pageFooter reports the continuation cursor of a page.
func pageFooter(m map[string]any) string {
	if c, ok := m["nextCursor"].(string); ok && c != "" {
		return fmt.Sprintf("next page: --cursor %s", c)
	}
	return "no further pages"
}
