package root_test

import (
	"context"
	"encoding/json"
	"flag"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/cli/client"
	"github.com/optimiware/kodama/internal/cli/client/clienttest"
	"github.com/optimiware/kodama/internal/cli/root"
)

// resourceCommand is a stand-in product command: `work update <id> [flags]`
// with a command-specific --if-version flag. It reports what the shared
// foundation resolved so tests can see which flags were honoured.
func resourceCommand() root.Contribution {
	return root.Contribution{Group: "work", Summary: "test work", Commands: []root.Command{{
		Name: "update", Summary: "update a resource", Need: root.NeedFactory,
		Bind: func(fs *flag.FlagSet) root.RunFunc {
			ifVersion := fs.Int64("if-version", 0, "")
			note := fs.String("note", "", "")
			return func(ctx context.Context, inv *root.Invocation, args []string) (root.Result, error) {
				deadline, hasDeadline := ctx.Deadline()
				return root.Result{Data: map[string]any{
					"args": args, "ifVersion": *ifVersion, "note": *note,
					"tenant": inv.Context.Tenant, "project": inv.Context.Project, "context": inv.Context.Name,
					"nonInteractive": inv.Context.NonInteractive, "factory": inv.Context.Factory,
					"hasDeadline": hasDeadline && time.Until(deadline) < time.Hour,
				}}, nil
			}
		},
	}}}
}

type resourceResult struct {
	Args           []string `json:"args"`
	IfVersion      int64    `json:"ifVersion"`
	Note           string   `json:"note"`
	Tenant         string   `json:"tenant"`
	Project        string   `json:"project"`
	Context        string   `json:"context"`
	NonInteractive bool     `json:"nonInteractive"`
	HasDeadline    bool     `json:"hasDeadline"`
}

func Test_flags_after_positional_arguments(t *testing.T) {
	st := newMem()
	seed(t, st)
	run := func(t *testing.T, args ...string) (root.ExitCode, resourceResult, string) {
		t.Helper()
		r, out, errb := newRunner(t, root.RunnerConfig{Contexts: st}, resourceCommand())
		code := r.Run(context.Background(), args)
		var res resourceResult
		if code == 0 {
			if err := json.Unmarshal(out.Bytes(), &res); err != nil {
				t.Fatalf("stdout is not JSON: %q", out)
			}
		}
		return code, res, errb.String()
	}

	t.Run("documented shape: id then command and output flags", func(t *testing.T) {
		code, res, errb := run(t, "work", "update", "wi_1042", "--if-version", "1", "--output", "json")
		if code != 0 || len(res.Args) != 1 || res.Args[0] != "wi_1042" || res.IfVersion != 1 {
			t.Fatalf("%d %+v %q", code, res, errb)
		}
	})

	t.Run("trailing common flags are honoured by the foundation", func(t *testing.T) {
		code, res, errb := run(t, "work", "update", "wi_1042", "--tenant", "t017_two", "--context", "other",
			"--non-interactive", "--timeout", "30s", "--if-version=7", "--note", "n", "--output", "json")
		if code != 0 {
			t.Fatalf("%d %q", code, errb)
		}
		if res.Tenant != "t017_two" || res.Context != "other" || !res.NonInteractive || !res.HasDeadline ||
			res.IfVersion != 7 || res.Note != "n" || len(res.Args) != 1 || res.Args[0] != "wi_1042" {
			t.Fatalf("trailing flags were ignored: %+v", res)
		}
	})

	t.Run("flags interleaved between positionals", func(t *testing.T) {
		code, res, errb := run(t, "work", "update", "a", "--project", "p017_y", "b", "--output", "json", "c")
		if code != 0 || strings.Join(res.Args, ",") != "a,b,c" || res.Project != "p017_y" || res.Tenant != "t017_one" {
			t.Fatalf("%d %+v %q", code, res, errb)
		}
	})

	t.Run("a flag value that looks like a positional or terminator stays a value", func(t *testing.T) {
		code, res, errb := run(t, "work", "update", "id", "--note", "--", "--output", "json")
		if code != 0 || res.Note != "--" || strings.Join(res.Args, ",") != "id" {
			t.Fatalf("%d %+v %q", code, res, errb)
		}
	})

	t.Run("the terminator ends flag parsing and is preserved verbatim", func(t *testing.T) {
		code, res, errb := run(t, "work", "update", "--output", "json", "--", "--tenant", "-x", "wi_1")
		if code != 0 || strings.Join(res.Args, ",") != "--tenant,-x,wi_1" || res.Tenant != "t017_one" {
			t.Fatalf("%d %+v %q", code, res, errb)
		}
		code, res, errb = run(t, "work", "update", "wi_1", "--output", "json", "--", "--output", "--")
		if code != 0 || strings.Join(res.Args, ",") != "wi_1,--output,--" {
			t.Fatalf("%d %+v %q", code, res, errb)
		}
	})

	t.Run("a lone dash is positional", func(t *testing.T) {
		code, res, errb := run(t, "work", "update", "-", "--output", "json")
		if code != 0 || strings.Join(res.Args, ",") != "-" {
			t.Fatalf("%d %+v %q", code, res, errb)
		}
	})

	t.Run("trailing validation still applies: explicit empty and bad values", func(t *testing.T) {
		for _, args := range [][]string{
			{"work", "update", "wi_1", "--tenant", "", "--output", "json"},
			{"work", "update", "wi_1", "--timeout", "0s", "--output", "json"},
			{"work", "update", "wi_1", "--output", "yaml"},
			{"work", "update", "wi_1", "--bogus", "--output", "json"},
			{"work", "update", "wi_1", "--if-version", "x", "--output", "json"},
			{"work", "update", "wi_1", "--tenant"},
		} {
			code, _, errb := run(t, args...)
			if code != root.ExitUsage {
				t.Errorf("%v: exit %d %q", args, code, errb)
			}
		}
	})

	t.Run("a trailing -h prints usage", func(t *testing.T) {
		r, out, _ := newRunner(t, root.RunnerConfig{Contexts: st}, resourceCommand())
		if code := r.Run(context.Background(), []string{"work", "update", "wi_1", "-h"}); code != 0 || !strings.Contains(out.String(), "if-version") {
			t.Fatalf("%d %q", code, out)
		}
	})
}

// Test_tls_handshake_timeout_exit proves a transport timeout that does not wrap
// context.DeadlineExceeded - a stalled TLS handshake with no command deadline -
// still maps to exit 5 and the stable timeout code.
func Test_tls_handshake_timeout_exit(t *testing.T) {
	origin := stalledTLSServer(t)

	httpClient := &http.Client{Transport: &http.Transport{TLSHandshakeTimeout: 150 * time.Millisecond}}
	for _, method := range []string{"GET", "PUT"} {
		r, out, errb := newRunner(t, root.RunnerConfig{
			Contexts: newMem(), Tokens: &tokenResolver{src: clienttest.Tokens("tok")}, HTTP: httpClient,
		}, probe())
		done := make(chan root.ExitCode, 1)
		go func() {
			// No --timeout and a background context: only the handshake timeout fires.
			done <- r.Run(context.Background(), []string{"probe", "get", "--factory", origin, "--method", method, "--output", "json"})
		}()
		var code root.ExitCode
		select {
		case code = <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("command hung on a stalled TLS handshake")
		}
		var e struct {
			Error struct {
				Code    string         `json:"code"`
				Details map[string]any `json:"details"`
			} `json:"error"`
		}
		if err := json.Unmarshal(errb.Bytes(), &e); err != nil {
			t.Fatalf("%s: stderr is not one JSON envelope: %q", method, errb)
		}
		if code != root.ExitTimeout || out.Len() != 0 || e.Error.Code != "timeout" {
			t.Fatalf("%s: exit %d code %q stdout %q stderr %q", method, code, e.Error.Code, out, errb)
		}
		// A mutation that never got past the handshake keeps its uncertainty:
		// the CLI cannot tell how far the request travelled.
		if uncertain := e.Error.Details["outcomeUncertain"] == true; uncertain != (method == "PUT") {
			t.Fatalf("%s: outcomeUncertain = %v", method, uncertain)
		}
	}

	// And the sentinel-free classification itself.
	p := root.Classify(&client.TransportError{Method: "GET", Err: timeoutErr{}})
	if p.Exit != root.ExitTimeout || p.Code != "timeout" {
		t.Fatalf("%+v", p)
	}
}

// timeoutErr is a net.Error-style timeout that does not wrap any context sentinel.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "tls: handshake timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return false }
