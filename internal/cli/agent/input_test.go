package agent_test

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/cli/client/clienttest"
)

// TestProfileInput: file and stdin carry the same bytes; malformed, trailing,
// non-object and oversize input fail locally without any HTTP call; unknown
// and duplicate members survive to the server instead of being erased.
func TestProfileInput(t *testing.T) {
	ok := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"id":"ap_1","name":"coding","version":1,"reference":"coding@1","digest":"sha256:2","profile":{}}`))
	}

	t.Run("file and stdin parity", func(t *testing.T) {
		c := newCLI(t, http.HandlerFunc(ok))
		if r := c.run("agent", "register", "--file", writeFile(t, "p.json", string(fixture(t)))); r.code != 0 {
			t.Fatalf("file: %d %s", r.code, r.err)
		}
		c.in = strings.NewReader(string(fixture(t)))
		if r := c.run("agent", "register", "--file", "-"); r.code != 0 {
			t.Fatalf("stdin: %d %s", r.code, r.err)
		}
		reqs := c.srv.Requests()
		if len(reqs) != 2 || string(reqs[0].Body) != string(reqs[1].Body) {
			t.Fatalf("file and stdin differ: %+v", reqs)
		}
	})

	t.Run("unknown and duplicate members are not erased", func(t *testing.T) {
		c := newCLI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			clienttest.WriteError(w, 400, "invalid_request", "duplicate key", "req_dup", nil)
		}))
		doc := `{"name":"a","name":"b","extra":{"x":1},"big":9007199254740993}`
		r := c.run("agent", "register", "--file", writeFile(t, "p.json", doc))
		if r.code != 1 || !strings.Contains(r.err, "req_dup") {
			t.Fatalf("exit %d err %q", r.code, r.err)
		}
		if got := string(c.srv.Requests()[0].Body); got != doc {
			t.Fatalf("body altered: %s", got)
		}
	})

	t.Run("local rejections send nothing", func(t *testing.T) {
		c := newCLI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Errorf("unexpected request") }))
		cases := map[string]string{
			"empty":         "",
			"blank":         " \n ",
			"malformed":     `{"name":`,
			"trailing":      `{"name":"a"} {"name":"b"}`,
			"trailing junk": `{"name":"a"} x`,
			"array":         `[{"name":"a"}]`,
			"string":        `"a"`,
			"null":          `null`,
			"invalid utf8":  "{\"name\":\"\xff\"}",
			"oversize":      `{"instructions":"` + strings.Repeat("a", 600<<10) + `"}`,
		}
		for name, doc := range cases {
			if r := c.run("agent", "register", "--file", writeFile(t, "p.json", doc)); r.code != 2 || r.out != "" {
				t.Errorf("%s: exit %d out %q err %q", name, r.code, r.out, r.err)
			}
		}
		if r := c.run("agent", "register", "--file", "/nonexistent/profile.json"); r.code != 2 {
			t.Errorf("missing file: exit %d", r.code)
		}
		if r := c.run("agent", "register", "--file", t.TempDir()); r.code != 2 {
			t.Errorf("directory: exit %d", r.code)
		}
		c.in = nil
		if r := c.run("agent", "register", "--file", "-"); r.code != 2 {
			t.Errorf("no stdin: exit %d", r.code)
		}
		if n := len(c.srv.Requests()); n != 0 {
			t.Fatalf("%d requests were sent", n)
		}
	})

	t.Run("cancelled before reading", func(t *testing.T) {
		c := newCLI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Errorf("unexpected request") }))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if r := c.runWith(ctx, "agent", "register", "--file", writeFile(t, "p.json", string(fixture(t)))); r.code != 5 || r.out != "" {
			t.Fatalf("exit %d out %q err %q", r.code, r.out, r.err)
		}
	})

	t.Run("stalled stdin honours the timeout", func(t *testing.T) {
		c := newCLI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Errorf("unexpected request") }))
		pr, pw, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { pr.Close(); pw.Close() })
		c.in = pr
		_, _ = pw.WriteString(`{"name":"par`)
		start := time.Now()
		r := c.run("agent", "register", "--file", "-", "--timeout", "200ms")
		if r.code != 5 || r.out != "" {
			t.Fatalf("exit %d out %q err %q", r.code, r.out, r.err)
		}
		if time.Since(start) > 5*time.Second {
			t.Fatal("stalled input outlasted --timeout")
		}
		_, _ = pw.WriteString(`t"}`)
		time.Sleep(100 * time.Millisecond)
		if n := len(c.srv.Requests()); n != 0 {
			t.Fatalf("%d requests were sent", n)
		}
	})
}
