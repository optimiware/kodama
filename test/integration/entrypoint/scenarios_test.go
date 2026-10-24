package entrypoint_test

// Replay of contracts/runtime/v1/fixtures/scenarios.json (the T-003 shared
// fixtures) against the entrypoint: every recorded exchange is replayed
// through the real HTTP adapter against a server that answers exactly what the
// fixture says, and the failure scenarios the Step itself reacts to are
// replayed through the whole entrypoint.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/entrypoint"
)

type scenarioStep struct {
	Op         string          `json:"op"`
	Request    json.RawMessage `json:"request"`
	Status     int             `json:"status"`
	Body       json.RawMessage `json:"body"`
	ArtifactID string          `json:"artifactId"`
}

type scenarioDoc struct {
	Name         string         `json:"name"`
	Requirements []string       `json:"requirements"`
	Steps        []scenarioStep `json:"steps"`
}

func loadScenarios(t *testing.T) []scenarioDoc {
	t.Helper()
	data, err := os.ReadFile("../../../contracts/runtime/v1/fixtures/scenarios.json")
	if err != nil {
		t.Fatal(err)
	}
	var out []scenarioDoc
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatal("no scenarios")
	}
	return out
}

// replayServer answers the steps of one scenario in order and records any
// deviation of the request (route, artifact ID, canonical body).
type replayServer struct {
	*httptest.Server
	t       *testing.T
	attempt string
	mu      sync.Mutex
	steps   []scenarioStep
	next    int
	prime   *scenarioStep // answers the priming admit, outside the scenario
	errs    []string
}

func newReplayServer(t *testing.T, attempt string, steps []scenarioStep) *replayServer {
	s := &replayServer{t: t, attempt: attempt, steps: steps}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

func (s *replayServer) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	defer s.mu.Unlock()
	rest, _ := strings.CutPrefix(r.URL.Path, "/runtime/v1/attempts/"+s.attempt)
	var op, artifact string
	if strings.HasPrefix(rest, "/artifacts/") && strings.HasSuffix(rest, "/complete") {
		op = opByRoute[r.Method+" /artifacts/{artifactId}/complete"]
		artifact = strings.TrimSuffix(strings.TrimPrefix(rest, "/artifacts/"), "/complete")
	} else {
		op = opByRoute[r.Method+" "+rest]
	}
	var step scenarioStep
	if s.prime != nil {
		step = *s.prime
		s.prime = nil
	} else {
		if s.next >= len(s.steps) {
			s.errs = append(s.errs, "unexpected extra request "+op)
			w.WriteHeader(http.StatusTeapot)
			return
		}
		step = s.steps[s.next]
		s.next++
		if op != step.Op || artifact != step.ArtifactID {
			s.errs = append(s.errs, "step "+step.Op+": got op "+op+" artifact "+artifact)
		}
		if !sameJSON(body, step.Request) {
			s.errs = append(s.errs, "step "+step.Op+": request body differs from the fixture: "+string(body))
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(step.Status)
	_, _ = w.Write(step.Body)
}

// sameJSON compares canonical forms; an absent request equals JSON null.
func sameJSON(got, want []byte) bool {
	if len(bytes.TrimSpace(got)) == 0 {
		got = []byte("null")
	}
	a, err1 := runtimev1.Canonicalize(got)
	b, err2 := runtimev1.Canonicalize(want)
	return err1 == nil && err2 == nil && bytes.Equal(a, b)
}

func decodeStrict[T any, P interface {
	*T
	Validate() error
}](t *testing.T, raw json.RawMessage) T {
	t.Helper()
	v, err := runtimev1.DecodeRequest[T, P](raw)
	if err != nil {
		t.Fatalf("fixture request does not decode: %v\n%s", err, raw)
	}
	return v
}

// callStep sends one recorded request through the adapter.
func callStep(t *testing.T, c *entrypoint.Client, st scenarioStep) (any, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	switch st.Op {
	case "admit":
		return c.Admit(ctx, decodeStrict[runtimev1.AdmitRequest](t, st.Request))
	case "renewToken":
		return c.RenewToken(ctx)
	case "getContext":
		return c.Context(ctx)
	case "resolveSecret":
		return c.ResolveSecret(ctx, decodeStrict[runtimev1.SecretResolveRequest](t, st.Request))
	case "reserveArtifact":
		return c.ReserveArtifact(ctx, decodeStrict[runtimev1.ArtifactReserveRequest](t, st.Request))
	case "completeArtifact":
		return c.CompleteArtifact(ctx, domain.ArtifactID(st.ArtifactID), decodeStrict[runtimev1.ArtifactCompleteRequest](t, st.Request))
	case "proposeSource":
		return c.ProposeSource(ctx, decodeStrict[runtimev1.SourceProposalRequest](t, st.Request))
	case "submitResult":
		return c.SubmitResult(ctx, decodeStrict[runtimev1.ResultRequest](t, st.Request))
	}
	t.Fatalf("unknown op %q", st.Op)
	return nil, nil
}

// Test_scenarios_replay replays the shared runtime fixtures.
func Test_scenarios_replay(t *testing.T) {
	all := loadScenarios(t)
	var happyAdmit scenarioStep
	for _, s := range all {
		if s.Name == "happy_path" {
			happyAdmit = s.Steps[0]
		}
	}
	if happyAdmit.Op != "admit" {
		t.Fatal("happy_path fixture has no admit step")
	}
	retry := entrypoint.RetryPolicy{MaxAttempts: 2, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond}

	for _, sc := range all {
		t.Run("adapter/"+sc.Name, func(t *testing.T) {
			rs := newReplayServer(t, "att_01", sc.Steps)
			c, err := entrypoint.NewClient(rs.URL, "att_01", "", retry, nil)
			if err != nil {
				t.Fatal(err)
			}
			if sc.Steps[0].Op != "admit" {
				rs.prime = &happyAdmit
				if _, err := c.Admit(context.Background(), decodeStrict[runtimev1.AdmitRequest](t, happyAdmit.Request)); err != nil {
					t.Fatalf("priming admit: %v", err)
				}
			}
			for i, st := range sc.Steps {
				got, err := callStep(t, c, st)
				if st.Status >= 200 && st.Status < 300 {
					if err != nil {
						t.Fatalf("step %d %s: %v", i+1, st.Op, err)
					}
					raw, merr := json.Marshal(got)
					if merr != nil || !sameJSON(raw, st.Body) {
						t.Fatalf("step %d %s: decoded response differs from the fixture:\n%s\n%s", i+1, st.Op, raw, st.Body)
					}
					continue
				}
				var api *entrypoint.APIError
				if !errors.As(err, &api) {
					t.Fatalf("step %d %s: err = %v, want an API error", i+1, st.Op, err)
				}
				var want runtimev1.ErrorResponse
				if err := json.Unmarshal(st.Body, &want); err != nil {
					t.Fatal(err)
				}
				if api.Status != st.Status || api.Code != want.Error.Code || !reflect.DeepEqual(api.Details, want.Error.Details) {
					t.Fatalf("step %d %s: %+v, want %d %s %v", i+1, st.Op, api, st.Status, want.Error.Code, want.Error.Details)
				}
			}
			rs.mu.Lock()
			defer rs.mu.Unlock()
			if rs.next != len(sc.Steps) {
				t.Fatalf("%d of %d steps replayed", rs.next, len(sc.Steps))
			}
			for _, e := range rs.errs {
				t.Error(e)
			}
		})
	}

	// Scenarios the whole entrypoint reacts to: the admission refusals.
	for name, want := range map[string]int{
		"cancelled_admission":          entrypoint.ExitRevoked,
		"wrong_job":                    entrypoint.ExitAdmission,
		"incompatible_wrapper_profile": entrypoint.ExitAdmission,
	} {
		t.Run("entrypoint/"+name, func(t *testing.T) {
			var sc scenarioDoc
			for _, s := range all {
				if s.Name == name {
					sc = s
				}
			}
			f := newFixture(t, markRun+`echo hi`)
			rs := newReplayServer(t, string(f.srv.ctx.AttemptID), sc.Steps)
			f.cfg.ServerURL = rs.URL
			f.wantExit(f.run(), want)
			rs.mu.Lock()
			defer rs.mu.Unlock()
			if rs.next != 1 {
				t.Fatalf("requests = %d, want exactly the admission", rs.next)
			}
			if f.launches() != 0 {
				t.Fatal("the command was launched")
			}
		})
	}
}
