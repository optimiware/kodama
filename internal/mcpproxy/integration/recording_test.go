package integration_test

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/internal/mcpproxy"
	"github.com/optimiware/kodama/internal/mcpproxy/mcptest"
)

func auditCount(t *testing.T, s *mcptest.Stack, action string) int {
	t.Helper()
	var n int
	if err := s.RT.Owner.QueryRow(context.Background(), `SELECT count(*) FROM audit WHERE action = $1`, action).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func invoke(t *testing.T, s *mcptest.Stack, r mcptest.Ready, id mcpproxy.SessionID, req string) error {
	t.Helper()
	_, err := s.Service.Invoke(s.Context(), r.W.Caller, r.Reg.ID, id, mcpproxy.InvokeRequest{ID: mcpproxy.RequestID(req), Tool: "echo", Arguments: []byte(`{"text":"payload-SENTINEL"}`)})
	return err
}

// Test_recording_policy: disabled or unauthorized recording persists no raw
// payload while safe metadata remains; enabled recording reaches only the
// authorized sink and never retries a tool.
func Test_recording_policy(t *testing.T) {
	cases := []struct {
		name       string
		cfg        mcptest.Config
		transcript bool
		wantSink   int
	}{
		{"no recorder configured", mcptest.Config{RecordRaw: true}, true, 0},
		{"raw recording disabled", mcptest.Config{Recorder: true, RecordRaw: false}, true, 0},
		{"pinned transcript policy forbids it", mcptest.Config{Recorder: true, RecordRaw: true}, false, 0},
		{"every gate allows it", mcptest.Config{Recorder: true, RecordRaw: true}, true, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := mcptest.New(t, c.cfg)
			r := s.Setup([]string{"echo"}, []string{"echo"}, nil)
			s.Policy.Set(r.W.Attempt, []string{mcpproxy.Qualify(r.Reg.ID, "echo")}, nil, c.transcript)
			id := openSession(t, s, r)
			if err := invoke(t, s, r, id, "req_t033_rec"); err != nil {
				t.Fatal(err)
			}
			if got := len(s.Recorder.Calls()); got != c.wantSink {
				t.Fatalf("sink received %d calls, want %d", got, c.wantSink)
			}
			// Safe metadata remains regardless, and never holds payload content.
			if auditCount(t, s, "mcp_tool.admitted") != 1 || auditCount(t, s, "mcp_tool.completed") != 1 {
				t.Fatal("metadata audit is missing")
			}
			var meta string
			if err := s.RT.Owner.QueryRow(context.Background(), `SELECT coalesce(string_agg(metadata::text, ' '), '') FROM audit`).Scan(&meta); err != nil {
				t.Fatal(err)
			}
			if contains(meta, "payload-SENTINEL") {
				t.Fatal("raw payload reached the audit trail")
			}
			var rows string
			if err := s.RT.Owner.QueryRow(context.Background(), `SELECT coalesce(string_agg(t::text, ' '), '') FROM (SELECT * FROM mcp_invocations) t`).Scan(&rows); err != nil {
				t.Fatal(err)
			}
			if contains(rows, "payload-SENTINEL") {
				t.Fatal("raw payload reached the receipt")
			}
		})
	}

	t.Run("a recorder failure does not fail or repeat the call", func(t *testing.T) {
		s := mcptest.New(t, mcptest.Config{Recorder: true, RecordRaw: true})
		s.Recorder.Fail = true
		r := s.Setup([]string{"echo"}, []string{"echo"}, nil)
		id := openSession(t, s, r)
		if err := invoke(t, s, r, id, "req_t033_recfail"); err != nil {
			t.Fatalf("a recording failure failed the call: %v", err)
		}
		if n := s.Fixture.Executed("echo"); n != 1 {
			t.Fatalf("the tool ran %d times", n)
		}
	})
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
