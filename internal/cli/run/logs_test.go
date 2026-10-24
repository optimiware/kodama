package run_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/cli/client/clienttest"
)

func rec(v map[string]any) string {
	b, _ := json.Marshal(v)
	return string(b) + "\n"
}

func metaRec(attempt, step, origin, completeness string, reasons ...string) string {
	if reasons == nil {
		reasons = []string{}
	}
	return rec(map[string]any{"type": "meta", "runId": runID, "attemptId": attempt, "stepId": step, "origin": origin,
		"completeness": completeness, "reasons": reasons, "tail": 100, "follow": false, "terminal": true,
		"limits": map[string]any{"maxBytes": 1, "maxLines": 1, "maxLineBytes": 1, "maxDurationSeconds": 1}, "future": "kept"})
}

func chunkRec(text string, truncated bool) string {
	return rec(map[string]any{"type": "chunk", "text": text, "lineTruncated": truncated})
}

func summaryRec(lines int, truncated bool, stop string) string {
	return rec(map[string]any{"type": "summary", "lines": lines, "bytes": 12, "truncated": truncated, "stop": stop})
}

// logServer serves the steps and logs routes; logBody builds the NDJSON body
// from the attempt the CLI asked for.
type logServer struct {
	steps   [][]map[string]any // pages of steps
	logBody func(q url.Values) string
}

func (l *logServer) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/steps"):
			page := 0
			if c := r.URL.Query().Get("cursor"); c != "" {
				page = int(c[len(c)-1] - '0')
			}
			var next any
			if page+1 < len(l.steps) {
				next = "p.c" + string(rune('0'+page+1))
			}
			items := make([]any, 0)
			for _, s := range l.steps[page] {
				items = append(items, s)
			}
			clienttest.WriteJSON(w, 200, map[string]any{"items": items, "nextCursor": next}, nil)
		case strings.HasSuffix(r.URL.Path, "/logs"):
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write([]byte(l.logBody(r.URL.Query())))
		default:
			t.Errorf("unexpected %s", r.URL.Path)
		}
	})
}

func step(id string, attempts ...string) map[string]any {
	var as []any
	for i, a := range attempts {
		as = append(as, map[string]any{"id": a, "ordinal": i + 1, "status": "failed"})
	}
	return map[string]any{"id": id, "kind": "container", "attempts": as}
}

// Test_logs_protocol: step-to-attempt resolution across pages, ambiguity,
// allowed query parameters, metadata and completeness preservation, and
// refusal of any response that is not meta, chunk*, summary.
func Test_logs_protocol(t *testing.T) {
	t.Run("step resolves across pages and only supported parameters are sent", func(t *testing.T) {
		l := &logServer{
			steps: [][]map[string]any{{step("build", "att_b")}, {step("test", "att_t1")}},
			logBody: func(q url.Values) string {
				return metaRec(q.Get("attemptId"), "test", "wrapper", "partial", "truncated") +
					chunkRec("ok  pkg", false) + chunkRec("long line", true) + summaryRec(2, true, "line_limit")
			},
		}
		srv := clienttest.NewServer(t, l.handler(t))
		r := runCLI(t, srv, "run", "logs", runID, "--step", "test", "--tail", "50", "--source", "wrapper", "--follow=false", "--output", "json")
		if r.code != 0 || r.err != "" {
			t.Fatalf("exit %d err %q", r.code, r.err)
		}
		reqs := srv.Requests()
		last := reqs[len(reqs)-1]
		q, _ := url.ParseQuery(last.RawQuery)
		if last.Path != base+"/runs/"+runID+"/logs" || q.Get("attemptId") != "att_t1" || q.Get("tail") != "50" || q.Get("source") != "wrapper" || len(q) != 3 {
			t.Fatalf("log request %s ? %s", last.Path, last.RawQuery)
		}
		for k := range q {
			if k == "step" || k == "namespace" || k == "pod" || k == "container" || k == "selector" {
				t.Fatalf("unsupported query parameter %q", k)
			}
		}
		doc := decodeJSON(t, r.out)
		meta := doc["meta"].(map[string]any)
		sum := doc["summary"].(map[string]any)
		lines := doc["lines"].([]any)
		if meta["origin"] != "wrapper" || meta["completeness"] != "partial" || meta["future"] != "kept" || sum["truncated"] != true || sum["stop"] != "line_limit" {
			t.Fatalf("metadata not preserved: %s", r.out)
		}
		if len(lines) != 2 || lines[1].(map[string]any)["lineTruncated"] != true {
			t.Fatalf("lines %v", lines)
		}
		h := runCLI(t, srv, "run", "logs", runID, "--step", "test")
		for _, want := range []string{"origin wrapper", "completeness partial (truncated)", "ok  pkg", "[line truncated]", "output truncated by a server bound", "not known to be complete", "not the run's outcome"} {
			if !strings.Contains(h.out, want) {
				t.Errorf("human output lacks %q: %q", want, h.out)
			}
		}
	})

	t.Run("several attempts need an explicit choice", func(t *testing.T) {
		l := &logServer{steps: [][]map[string]any{{step("test", "att_1", "att_2")}}, logBody: func(q url.Values) string {
			return metaRec(q.Get("attemptId"), "test", "live", "unknown") + summaryRec(0, false, "end")
		}}
		srv := clienttest.NewServer(t, l.handler(t))
		r := runCLI(t, srv, "run", "logs", runID, "--step", "test")
		if r.code != 2 || r.out != "" || !strings.Contains(r.err, "att_1") || !strings.Contains(r.err, "att_2") || !strings.Contains(r.err, "--attempt") {
			t.Fatalf("exit %d err %q", r.code, r.err)
		}
		for _, rq := range srv.Requests() {
			if strings.HasSuffix(rq.Path, "/logs") {
				t.Fatal("a log was read without an unambiguous attempt")
			}
		}
		ok := runCLI(t, srv, "run", "logs", runID, "--step", "test", "--attempt", "att_2")
		if ok.code != 0 || !strings.Contains(ok.out, "attempt att_2") {
			t.Fatalf("explicit: exit %d out %q err %q", ok.code, ok.out, ok.err)
		}
		bad := runCLI(t, srv, "run", "logs", runID, "--step", "test", "--attempt", "att_x")
		if bad.code != 2 {
			t.Fatalf("foreign attempt: exit %d", bad.code)
		}
		none := runCLI(t, srv, "run", "logs", runID, "--step", "absent")
		if none.code != 2 || !strings.Contains(none.err, "no step") {
			t.Fatalf("unknown step: exit %d err %q", none.code, none.err)
		}
	})

	t.Run("without --step the server selects and the attempt is read back", func(t *testing.T) {
		l := &logServer{logBody: func(q url.Values) string {
			if len(q) != 0 {
				t.Errorf("query %v", q)
			}
			return metaRec("att_only", "build", "live", "unknown", "controller_capture") + chunkRec("hello", false) + summaryRec(1, false, "end")
		}}
		srv := clienttest.NewServer(t, l.handler(t))
		r := runCLI(t, srv, "run", "logs", runID, "--output", "json")
		if r.code != 0 || len(srv.Requests()) != 1 {
			t.Fatalf("exit %d err %q requests %d", r.code, r.err, len(srv.Requests()))
		}
	})

	t.Run("an interrupted or malformed response is never a partial success", func(t *testing.T) {
		cases := map[string]struct {
			body string
			code string
			exit int
		}{
			"missing summary":       {metaRec("att_1", "build", "live", "unknown") + chunkRec("partial", false), "logs_incomplete", 1},
			"empty":                 {"", "logs_incomplete", 1},
			"chunk before meta":     {chunkRec("x", false) + summaryRec(1, false, "end"), "logs_malformed", 1},
			"record after summary":  {metaRec("att_1", "build", "live", "unknown") + summaryRec(0, false, "end") + chunkRec("late", false), "logs_malformed", 1},
			"duplicate meta":        {metaRec("att_1", "build", "live", "unknown") + metaRec("att_1", "build", "live", "unknown") + summaryRec(0, false, "end"), "logs_malformed", 1},
			"unknown record":        {metaRec("att_1", "build", "live", "unknown") + rec(map[string]any{"type": "debug"}) + summaryRec(0, false, "end"), "logs_malformed", 1},
			"not json":              {"not json\n", "logs_malformed", 1},
			"truncated last line":   {metaRec("att_1", "build", "live", "unknown") + `{"type":"summary","lines":0,`, "logs_malformed", 1},
			"blank line":            {metaRec("att_1", "build", "live", "unknown") + "\n" + summaryRec(0, false, "end"), "logs_malformed", 1},
			"incomplete meta":       {rec(map[string]any{"type": "meta", "runId": runID}) + summaryRec(0, false, "end"), "logs_malformed", 1},
			"summary without stop":  {metaRec("att_1", "build", "live", "unknown") + rec(map[string]any{"type": "summary", "lines": 0}), "logs_malformed", 1},
			"another run":           {strings.Replace(metaRec("att_1", "build", "live", "unknown"), runID, "run_other", 1) + summaryRec(0, false, "end"), "logs_mismatch", 1},
			"authorization lost":    {metaRec("att_1", "build", "live", "unknown") + chunkRec("secret-ish", false) + summaryRec(1, false, "authorization_changed"), "authorization_changed", 3},
			"dependency down":       {metaRec("att_1", "build", "live", "unknown") + chunkRec("x", false) + summaryRec(1, false, "unavailable"), "logs_unavailable", 1},
			"chunk without text":    {metaRec("att_1", "build", "live", "unknown") + rec(map[string]any{"type": "chunk", "lineTruncated": false}) + summaryRec(1, false, "end"), "logs_malformed", 1},
			"chunk without flag":    {metaRec("att_1", "build", "live", "unknown") + rec(map[string]any{"type": "chunk", "text": "x"}) + summaryRec(1, false, "end"), "logs_malformed", 1},
			"chunk text not text":   {metaRec("att_1", "build", "live", "unknown") + rec(map[string]any{"type": "chunk", "text": 7, "lineTruncated": false}) + summaryRec(1, false, "end"), "logs_malformed", 1},
			"chunk flag not bool":   {metaRec("att_1", "build", "live", "unknown") + rec(map[string]any{"type": "chunk", "text": "x", "lineTruncated": "no"}) + summaryRec(1, false, "end"), "logs_malformed", 1},
			"meta reasons absent":   {rec(map[string]any{"type": "meta", "runId": runID, "attemptId": "att_1", "stepId": "build", "origin": "live", "completeness": "unknown"}) + summaryRec(0, false, "end"), "logs_malformed", 1},
			"meta reasons typed":    {rec(map[string]any{"type": "meta", "runId": runID, "attemptId": "att_1", "stepId": "build", "origin": "live", "completeness": "unknown", "reasons": []any{1}}) + summaryRec(0, false, "end"), "logs_malformed", 1},
			"meta id not text":      {rec(map[string]any{"type": "meta", "runId": runID, "attemptId": 4, "stepId": "build", "origin": "live", "completeness": "unknown", "reasons": []any{}}) + summaryRec(0, false, "end"), "logs_malformed", 1},
			"summary no lines":      {metaRec("att_1", "build", "live", "unknown") + rec(map[string]any{"type": "summary", "bytes": 1, "truncated": false, "stop": "end"}), "logs_malformed", 1},
			"summary no bytes":      {metaRec("att_1", "build", "live", "unknown") + rec(map[string]any{"type": "summary", "lines": 0, "truncated": false, "stop": "end"}), "logs_malformed", 1},
			"summary no truncated":  {metaRec("att_1", "build", "live", "unknown") + rec(map[string]any{"type": "summary", "lines": 0, "bytes": 0, "stop": "end"}), "logs_malformed", 1},
			"summary lines text":    {metaRec("att_1", "build", "live", "unknown") + rec(map[string]any{"type": "summary", "lines": "0", "bytes": 0, "truncated": false, "stop": "end"}), "logs_malformed", 1},
			"summary lines float":   {metaRec("att_1", "build", "live", "unknown") + `{"type":"summary","lines":1.5,"bytes":0,"truncated":false,"stop":"end"}` + "\n", "logs_malformed", 1},
			"summary negative":      {metaRec("att_1", "build", "live", "unknown") + rec(map[string]any{"type": "summary", "lines": 0, "bytes": -1, "truncated": false, "stop": "end"}), "logs_malformed", 1},
			"summary flag not bool": {metaRec("att_1", "build", "live", "unknown") + rec(map[string]any{"type": "summary", "lines": 0, "bytes": 0, "truncated": 1, "stop": "end"}), "logs_malformed", 1},
			"unknown stop":          {metaRec("att_1", "build", "live", "unknown") + chunkRec("x", false) + summaryRec(1, false, "teapot"), "logs_malformed", 1},
			"stop not text":         {metaRec("att_1", "build", "live", "unknown") + rec(map[string]any{"type": "summary", "lines": 0, "bytes": 0, "truncated": false, "stop": 3}), "logs_malformed", 1},
			"source interrupted":    {metaRec("att_1", "build", "live", "unknown") + summaryRec(0, false, "interrupted"), "logs_interrupted", 1},
		}
		for name, tc := range cases {
			l := &logServer{logBody: func(url.Values) string { return tc.body }}
			srv := clienttest.NewServer(t, l.handler(t))
			for _, format := range []string{"json", "human"} {
				r := runCLI(t, srv, "run", "logs", runID, "--output", format)
				if int(r.code) != tc.exit || r.out != "" || (format == "json" && errorOf(t, r.err)["code"] != tc.code) {
					t.Errorf("%s/%s: exit %d out %q err %q", name, format, r.code, r.out, r.err)
				}
				if strings.Contains(r.err, "secret-ish") || strings.Contains(r.err, "partial") && name != "missing summary" {
					t.Errorf("%s: log content leaked into the diagnostic: %q", name, r.err)
				}
			}
		}
	})

	t.Run("another attempt than the one asked for is refused", func(t *testing.T) {
		l := &logServer{steps: [][]map[string]any{{step("test", "att_1")}}, logBody: func(url.Values) string {
			return metaRec("att_other", "test", "live", "unknown") + summaryRec(0, false, "end")
		}}
		srv := clienttest.NewServer(t, l.handler(t))
		if r := runCLI(t, srv, "run", "logs", runID, "--step", "test"); r.code != 1 || r.out != "" || !strings.Contains(r.err, "another run, step or attempt") {
			t.Fatalf("exit %d out %q err %q", r.code, r.out, r.err)
		}
	})

	t.Run("bad flags fail before any request", func(t *testing.T) {
		srv := clienttest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("request sent") }))
		for _, args := range [][]string{
			{"run", "logs", runID, "--tail", "0"}, {"run", "logs", runID, "--tail", "ten"}, {"run", "logs", runID, "--source", "pod"},
			{"run", "logs", runID, "--step", ""}, {"run", "logs"},
		} {
			if r := runCLI(t, srv, args...); r.code != 2 {
				t.Errorf("%v: exit %d", args, r.code)
			}
		}
	})
}

// Authorization and rate-limit answers before the first record keep their
// classification and request IDs.
func Test_logs_error_envelopes(t *testing.T) {
	for _, tc := range []struct{ status, exit int }{{400, 1}, {401, 3}, {403, 3}, {404, 1}, {429, 1}, {503, 1}} {
		srv := clienttest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			clienttest.WriteError(w, tc.status, "x", "m", "req_l", map[string]string{"Retry-After": "3"})
		}))
		r := runCLI(t, srv, "run", "logs", runID, "--output", "json")
		if int(r.code) != tc.exit || r.out != "" || errorOf(t, r.err)["requestId"] != "req_l" {
			t.Errorf("%d: exit %d out %q err %q", tc.status, r.code, r.out, r.err)
		}
	}
}

// Test_logs_additive_and_empty: additive members on chunk records survive, an empty log
// is a success, and every recognised stop reason is accepted.
func Test_logs_additive_and_empty(t *testing.T) {
	t.Run("additive chunk members are preserved", func(t *testing.T) {
		l := &logServer{logBody: func(url.Values) string {
			return metaRec("att_1", "build", "live", "unknown") +
				rec(map[string]any{"type": "chunk", "text": "hello", "lineTruncated": false, "seq": 7, "stream": "stderr"}) +
				summaryRec(1, false, "end")
		}}
		srv := clienttest.NewServer(t, l.handler(t))
		r := runCLI(t, srv, "run", "logs", runID, "--output", "json")
		if r.code != 0 {
			t.Fatalf("exit %d err %q", r.code, r.err)
		}
		lines := decodeJSON(t, r.out)["lines"].([]any)
		c := lines[0].(map[string]any)
		if len(lines) != 1 || c["text"] != "hello" || c["lineTruncated"] != false || c["seq"] == nil || c["stream"] != "stderr" || c["type"] != nil {
			t.Fatalf("chunk %v", c)
		}
	})
	t.Run("an empty log is a success with no lines", func(t *testing.T) {
		l := &logServer{logBody: func(url.Values) string {
			return metaRec("att_1", "build", "live", "unknown") + summaryRec(0, false, "end")
		}}
		srv := clienttest.NewServer(t, l.handler(t))
		r := runCLI(t, srv, "run", "logs", runID, "--output", "json")
		doc := decodeJSON(t, r.out)
		if r.code != 0 || len(doc["lines"].([]any)) != 0 {
			t.Fatalf("exit %d out %q err %q", r.code, r.out, r.err)
		}
	})
	t.Run("every recognised bound is a successful, flagged read", func(t *testing.T) {
		for _, stop := range []string{"end", "byte_limit", "line_limit", "duration_limit", "scan_limit"} {
			l := &logServer{logBody: func(url.Values) string {
				return metaRec("att_1", "build", "live", "unknown") + chunkRec("x", false) + summaryRec(1, stop != "end", stop)
			}}
			srv := clienttest.NewServer(t, l.handler(t))
			if r := runCLI(t, srv, "run", "logs", runID); r.code != 0 {
				t.Errorf("%s: exit %d err %q", stop, r.code, r.err)
			}
		}
	})
}
