package root_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/cli/client"
	"github.com/optimiware/kodama/internal/cli/client/clienttest"
	"github.com/optimiware/kodama/internal/cli/root"
)

// probe is a stand-in product contribution exercising the shared transport and
// rendering exactly as a later command would.
func probe() root.Contribution {
	return root.Contribution{Group: "probe", Summary: "test probe", Commands: []root.Command{{
		Name: "get", Summary: "GET /api/v1/probe", Need: root.NeedFactory,
		Bind: func(fs *flag.FlagSet) root.RunFunc {
			method := fs.String("method", "GET", "")
			return func(ctx context.Context, inv *root.Invocation, args []string) (root.Result, error) {
				c, err := inv.Client()
				if err != nil {
					return root.Result{}, err
				}
				var raw json.RawMessage
				_, err = c.Do(ctx, client.Request{Method: *method, Path: []string{"probe"}, IfVersion: 1}, &raw)
				if err != nil {
					return root.Result{}, err
				}
				data, err := root.NormalizeJSON(raw)
				if err != nil {
					return root.Result{}, err
				}
				return root.Result{Data: data, Text: "ok"}, nil
			}
		},
	}}}
}

func probeRunner(t *testing.T, srv *clienttest.Server, src client.AccessTokenSource) (*root.Runner, *bytes.Buffer, *bytes.Buffer) {
	return newRunner(t, root.RunnerConfig{Contexts: newMem(), Tokens: &tokenResolver{src: src}, AllowInsecureLoopback: true}, probe())
}

type noToken struct{}

func (noToken) AccessToken(context.Context) (string, error) {
	return "", errors.Join(client.ErrNotAuthenticated, errors.New("nothing stored"))
}

func Test_exit_json_contract(t *testing.T) {
	status := http.StatusOK
	var header map[string]string
	srv := clienttest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status/100 == 2 {
			w.Header().Set("Content-Type", "application/json")
			// keys deliberately unsorted, with an additive field and an integer above 2^53
			_, _ = w.Write([]byte(`{"z":1,"id":"wi_1","nested":{"y":2,"x":1},"future":[true,null],"big":9007199254740993}`))
			return
		}
		clienttest.WriteJSON(w, status, map[string]any{"error": map[string]any{
			"code": "code_for_" + http.StatusText(status), "message": "m", "requestId": "req_77",
			"details": map[string]any{"b": 2, "a": 1}, "extraField": "ignored",
		}}, header)
	}))
	args := func(extra ...string) []string {
		return append([]string{"probe", "get", "--factory", srv.URL}, extra...)
	}

	t.Run("success is one deterministic JSON document on stdout only", func(t *testing.T) {
		var first []byte
		for i := 0; i < 2; i++ {
			status = 200
			r, out, errb := probeRunner(t, srv, clienttest.Tokens("tok"))
			if code := r.Run(context.Background(), args("--output", "json")); code != 0 {
				t.Fatalf("exit %d: %s", code, errb)
			}
			if errb.Len() != 0 {
				t.Fatalf("stderr: %q", errb)
			}
			if i == 0 {
				first = append([]byte(nil), out.Bytes()...)
			} else if !bytes.Equal(first, out.Bytes()) {
				t.Fatal("output is not deterministic")
			}
		}
		want := `{"big":9007199254740993,"future":[true,null],"id":"wi_1","nested":{"x":1,"y":2},"z":1}` + "\n"
		if string(first) != want {
			t.Fatalf("got  %q\nwant %q", first, want)
		}
		// human mode keeps stdout free of JSON and stderr empty
		r, out, errb := probeRunner(t, srv, clienttest.Tokens("tok"))
		if code := r.Run(context.Background(), args()); code != 0 || out.String() != "ok\n" || errb.Len() != 0 {
			t.Fatalf("human: %d %q %q", code, out, errb)
		}
	})

	t.Run("every exit category", func(t *testing.T) {
		cases := []struct {
			name   string
			status int
			header map[string]string
			exit   root.ExitCode
			retry  int64
		}{
			{"bad request", 400, nil, 1, 0},
			{"unauthenticated", 401, nil, 3, 0},
			{"forbidden", 403, nil, 3, 0},
			{"not found", 404, nil, 1, 0},
			{"conflict", 409, nil, 4, 0},
			{"version conflict", 412, nil, 4, 0},
			{"version required", 428, nil, 2, 0},
			{"rate limited", 429, map[string]string{"Retry-After": "9"}, 1, 9},
			{"internal", 500, nil, 1, 0},
			{"unavailable", 503, map[string]string{"Retry-After": "3"}, 1, 3},
		}
		for _, c := range cases {
			status, header = c.status, c.header
			r, out, errb := probeRunner(t, srv, clienttest.Tokens("tok"))
			code := r.Run(context.Background(), args("--output", "json", "--method", "PUT"))
			if code != c.exit {
				t.Errorf("%s: exit %d want %d", c.name, code, c.exit)
			}
			if out.Len() != 0 {
				t.Errorf("%s: stdout not empty: %q", c.name, out)
			}
			if bytes.Count(errb.Bytes(), []byte("\n")) != 1 {
				t.Fatalf("%s: stderr must be exactly one document: %q", c.name, errb)
			}
			var env struct {
				Error struct {
					Code, Message, RequestID string
					Details                  map[string]any
					Status                   int
					RetryAfterSeconds        int64
				}
			}
			dec := json.NewDecoder(errb)
			dec.DisallowUnknownFields()
			if err := dec.Decode(&env); err != nil {
				t.Fatalf("%s: %v (%s)", c.name, err, errb)
			}
			e := env.Error
			if e.Code != "code_for_"+http.StatusText(c.status) || e.RequestID != "req_77" || e.Status != c.status ||
				e.RetryAfterSeconds != c.retry || e.Details["a"] != float64(1) || e.Details["b"] != float64(2) {
				t.Errorf("%s: %+v", c.name, e)
			}
		}
		status, header = 200, nil
	})

	t.Run("stderr bytes are deterministic and details are sorted", func(t *testing.T) {
		status, header = 409, nil
		r, _, errb := probeRunner(t, srv, clienttest.Tokens("tok"))
		r.Run(context.Background(), args("--output", "json"))
		want := `{"error":{"code":"code_for_Conflict","message":"m","requestId":"req_77","details":{"a":1,"b":2},"status":409}}` + "\n"
		if errb.String() != want {
			t.Fatalf("got  %q\nwant %q", errb, want)
		}
		status = 200
	})

	t.Run("human errors go to stderr as one line", func(t *testing.T) {
		status = 412
		r, out, errb := probeRunner(t, srv, clienttest.Tokens("tok"))
		if code := r.Run(context.Background(), args()); code != 4 || out.Len() != 0 {
			t.Fatalf("%d %q", code, out)
		}
		if got := errb.String(); !strings.HasPrefix(got, "error: m (code code_for_") || !strings.Contains(got, "request req_77") || strings.Count(got, "\n") != 1 {
			t.Fatalf("%q", got)
		}
		status = 200
	})

	t.Run("local failures: empty request id, stable codes", func(t *testing.T) {
		type tc struct {
			name string
			args []string
			src  client.AccessTokenSource
			exit root.ExitCode
			code string
		}
		for _, c := range []tc{
			{"unknown group", []string{"nope", "x", "--output", "json"}, nil, 2, "usage"},
			{"unknown command", []string{"probe", "nope", "--output=json"}, nil, 2, "usage"},
			{"bad flag", []string{"probe", "get", "--bogus", "--output", "json"}, nil, 2, "usage"},
			{"no factory", []string{"probe", "get", "--output", "json"}, nil, 2, "usage"},
			{"empty tenant", []string{"probe", "get", "--factory", srv.URL, "--tenant", "", "--output", "json"}, nil, 2, "usage"},
			{"no credential", args("--output", "json"), noToken{}, 3, "not_authenticated"},
		} {
			r, out, errb := probeRunner(t, srv, c.src)
			if c.src == nil {
				r, out, errb = probeRunner(t, srv, clienttest.Tokens("tok"))
			}
			code := r.Run(context.Background(), c.args)
			if code != c.exit || out.Len() != 0 {
				t.Errorf("%s: exit %d stdout %q", c.name, code, out)
			}
			var env struct {
				Error struct{ Code, RequestID string }
			}
			if err := json.Unmarshal(errb.Bytes(), &env); err != nil || env.Error.Code != c.code || env.Error.RequestID != "" {
				t.Errorf("%s: %s", c.name, errb)
			}
		}
	})

	t.Run("an unknown output format is a human-rendered usage failure", func(t *testing.T) {
		r, out, errb := probeRunner(t, srv, clienttest.Tokens("tok"))
		if code := r.Run(context.Background(), args("--output", "xml")); code != 2 || out.Len() != 0 || !strings.HasPrefix(errb.String(), "error: --output must be") {
			t.Fatalf("%d %q %q", code, out, errb)
		}
	})

	t.Run("a missing credential never starts a login", func(t *testing.T) {
		before := len(srv.Requests())
		r, _, _ := probeRunner(t, srv, noToken{})
		if code := r.Run(context.Background(), args("--non-interactive", "--output", "json")); code != 3 {
			t.Fatalf("exit %d", code)
		}
		if len(srv.Requests()) != before {
			t.Fatal("a request was sent without a credential")
		}
	})

	t.Run("timeout and interruption exit 5", func(t *testing.T) {
		slow := clienttest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		}))
		r, out, errb := probeRunner(t, slow, clienttest.Tokens("tok"))
		code := r.Run(context.Background(), []string{"probe", "get", "--factory", slow.URL, "--timeout", "100ms", "--output", "json"})
		if code != 5 || out.Len() != 0 || !strings.Contains(errb.String(), `"code":"timeout"`) {
			t.Fatalf("%d %q %q", code, out, errb)
		}
		ctx, cancel := context.WithCancel(context.Background())
		go func() { time.Sleep(50 * time.Millisecond); cancel() }()
		r, _, errb = probeRunner(t, slow, clienttest.Tokens("tok"))
		code = r.Run(ctx, []string{"probe", "get", "--factory", slow.URL, "--output", "json"})
		if code != 5 || !strings.Contains(errb.String(), `"code":"interrupted"`) {
			t.Fatalf("%d %q", code, errb)
		}
		if code := r.Run(context.Background(), []string{"probe", "get", "--factory", slow.URL, "--timeout", "0s"}); code != 2 {
			t.Fatalf("non-positive timeout exit %d", code)
		}
	})

	t.Run("malformed and unexpected responses", func(t *testing.T) {
		bad := clienttest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "PUT" {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":`))
				return
			}
			http.Error(w, "<html>gateway</html>", 502)
		}))
		r, out, errb := probeRunner(t, bad, clienttest.Tokens("tok"))
		if code := r.Run(context.Background(), []string{"probe", "get", "--factory", bad.URL, "--output", "json"}); code != 1 || out.Len() != 0 ||
			!strings.Contains(errb.String(), `"code":"unexpected_response"`) || strings.Contains(errb.String(), "gateway") {
			t.Fatalf("%q %q", out, errb)
		}
		r, out, errb = probeRunner(t, bad, clienttest.Tokens("tok"))
		if code := r.Run(context.Background(), []string{"probe", "get", "--factory", bad.URL, "--method", "PUT", "--output", "json"}); code != 1 || out.Len() != 0 ||
			!strings.Contains(errb.String(), `"code":"malformed_response"`) || !strings.Contains(errb.String(), `"outcomeUncertain":true`) {
			t.Fatalf("%q %q", out, errb)
		}
	})

	t.Run("token is never rendered", func(t *testing.T) {
		status = 500
		r, out, errb := probeRunner(t, srv, clienttest.Tokens("super-secret-token"))
		r.Run(context.Background(), args("--output", "json"))
		if strings.Contains(out.String()+errb.String(), "super-secret-token") {
			t.Fatal("token leaked")
		}
		status = 200
	})
}

func Test_runner_registration(t *testing.T) {
	if _, err := root.NewRunner(root.RunnerConfig{}, probe(), probe()); err == nil {
		t.Fatal("duplicate command accepted")
	}
	r, out, _ := newRunner(t, root.RunnerConfig{}, probe(), root.NewContextCommands())
	if code := r.Run(context.Background(), []string{"help"}); code != 0 || !strings.Contains(out.String(), "context") || !strings.Contains(out.String(), "probe") {
		t.Fatalf("%d %q", code, out)
	}
}

// Test_mutation_outcome_uncertainty proves a mutation the server received
// keeps its uncertainty and recovery guidance whatever exit code the failure
// maps to, in both output formats.
func Test_mutation_outcome_uncertainty(t *testing.T) {
	var received atomic.Int32
	hang := clienttest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		<-r.Context().Done()
	}))
	gateway := clienttest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		http.Error(w, "<html>bad gateway</html>", 502)
	}))
	rejecting := clienttest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		clienttest.WriteError(w, 409, "conflict", "stale", "req_1", nil)
	}))

	type envelope struct {
		Error struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	decode := func(t *testing.T, s string) envelope {
		t.Helper()
		var e envelope
		if err := json.Unmarshal([]byte(s), &e); err != nil {
			t.Fatalf("stderr is not one JSON envelope: %q", s)
		}
		return e
	}
	run := func(t *testing.T, ctx context.Context, srv *clienttest.Server, method string, extra ...string) (root.ExitCode, string, string) {
		t.Helper()
		r, out, errb := probeRunner(t, srv, clienttest.Tokens("tok"))
		code := r.Run(ctx, append([]string{"probe", "get", "--factory", srv.URL, "--method", method}, extra...))
		return code, out.String(), errb.String()
	}
	cancelled := func() (context.Context, func()) {
		received.Store(0)
		ctx, cancel := context.WithCancel(context.Background())
		var once sync.Once
		go func() {
			for received.Load() == 0 {
				time.Sleep(5 * time.Millisecond)
			}
			once.Do(cancel)
		}()
		return ctx, cancel
	}

	for _, tc := range []struct {
		name string
		ctx  func() (context.Context, func())
		args []string
		code root.ExitCode
		want string
	}{
		{"timeout", func() (context.Context, func()) { return context.Background(), func() {} }, []string{"--timeout", "150ms"}, 5, "timeout"},
		{"cancellation", cancelled, nil, 5, "interrupted"},
	} {
		t.Run(tc.name+" after the server received a mutation", func(t *testing.T) {
			received.Store(0)
			ctx, stop := tc.ctx()
			defer stop()
			code, out, errb := run(t, ctx, hang, "PUT", append([]string{"--output", "json"}, tc.args...)...)
			if code != tc.code || out != "" || received.Load() != 1 {
				t.Fatalf("exit %d received %d out %q err %q", code, received.Load(), out, errb)
			}
			e := decode(t, errb)
			if e.Error.Code != tc.want || e.Error.Details["outcomeUncertain"] != true || e.Error.Details["recovery"] != root.RecoveryGuidance {
				t.Fatalf("uncertainty lost: %q", errb)
			}

			received.Store(0)
			ctx, stop = tc.ctx()
			defer stop()
			code, out, errb = run(t, ctx, hang, "PUT", tc.args...)
			if code != tc.code || out != "" || !strings.Contains(errb, "outcome unknown: "+root.RecoveryGuidance) {
				t.Fatalf("human output lost guidance: %d %q", code, errb)
			}
		})
	}

	t.Run("a read that times out is not uncertain", func(t *testing.T) {
		code, _, errb := run(t, context.Background(), hang, "GET", "--timeout", "100ms", "--output", "json")
		e := decode(t, errb)
		if code != 5 || e.Error.Details["outcomeUncertain"] != nil || strings.Contains(errb, "recovery") {
			t.Fatalf("%d %q", code, errb)
		}
		_, _, human := run(t, context.Background(), hang, "GET", "--timeout", "100ms")
		if strings.Contains(human, "outcome unknown") {
			t.Fatalf("%q", human)
		}
	})

	t.Run("a malformed non-2xx answer to a mutation is uncertain", func(t *testing.T) {
		received.Store(0)
		code, out, errb := run(t, context.Background(), gateway, "PUT", "--output", "json")
		e := decode(t, errb)
		if code != 1 || out != "" || received.Load() != 1 || e.Error.Code != "unexpected_response" ||
			e.Error.Details["outcomeUncertain"] != true || e.Error.Details["recovery"] != root.RecoveryGuidance {
			t.Fatalf("%d %q", code, errb)
		}
		_, _, human := run(t, context.Background(), gateway, "PUT")
		if !strings.Contains(human, "outcome unknown: "+root.RecoveryGuidance) || strings.Contains(human, "bad gateway") {
			t.Fatalf("%q", human)
		}
	})

	t.Run("a malformed non-2xx answer to a read is not uncertain", func(t *testing.T) {
		_, _, errb := run(t, context.Background(), gateway, "GET", "--output", "json")
		if e := decode(t, errb); e.Error.Details["outcomeUncertain"] != nil {
			t.Fatalf("%q", errb)
		}
	})

	t.Run("a definite rejection is not uncertain", func(t *testing.T) {
		code, _, errb := run(t, context.Background(), rejecting, "PUT", "--output", "json")
		if e := decode(t, errb); code != 4 || e.Error.Details["outcomeUncertain"] != nil {
			t.Fatalf("%d %q", code, errb)
		}
	})
}
