package integration_test

import (
	"context"
	"errors"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/qa"
	"github.com/optimiware/kodama/internal/qa/qatest"
)

var scopeA = qatest.ScopeA

// stack is one environment with a succeeded deployment, a started QA attempt
// pinned to that deployment's environment and its uploaded screenshots.
type stack struct {
	*qatest.Env
	Dep  *qatest.Deployed
	Run  *qatest.QARun
	Shot []domain.ArtifactID
	Dig  []runtimev1.Digest
}

func newStack(t *testing.T, o qatest.Options) *stack {
	t.Helper()
	e := qatest.New(t, o)
	dep := e.Deploy(scopeA, "preview")
	run := e.StartQA(scopeA, dep, qatest.QAOptions{})
	ids, digs := run.UploadAllShots()
	return &stack{Env: e, Dep: dep, Run: run, Shot: ids, Dig: digs}
}

// finding builds an observation citing the i-th screenshot.
func (s *stack) finding(title, location string, shots ...int) qa.Observation {
	var ev []qa.EvidenceRef
	for _, i := range shots {
		ev = append(ev, qatest.Evidence(s.Shot[i], s.Dig[i], qa.EvidenceScreenshot, "step "+title))
	}
	return qatest.Observation(title, location, ev...)
}

// complete writes the report as the QA step's declared output and finishes the attempt.
func (s *stack) complete(obs ...qa.Observation) qa.Report {
	s.T.Helper()
	r := s.Run.ReportFor(s.Dep, obs...)
	s.Run.Complete(r)
	return r
}

func (s *stack) record() (qa.Result, error) {
	return s.Service.Record(context.Background(), s.Run.Command())
}

func (s *stack) mustRecord(t *testing.T) qa.Result {
	t.Helper()
	res, err := s.record()
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	return res
}

func is(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("got %v, want %v", err, want)
	}
}

// nothingFiled asserts a refused report left no defect, no pin and no work event.
func (s *stack) nothingFiled(t *testing.T, scope domain.Scope) {
	t.Helper()
	if n := s.DefectRows(scope); n != 0 {
		t.Fatalf("a refused report filed %d defects", n)
	}
	if n := s.Count(`SELECT count(*) FROM artifact_pins WHERE tenant_id = $1 AND owner_kind = 'qa_defect'`, string(scope.TenantID)); n != 0 {
		t.Fatalf("a refused report pinned %d artifacts", n)
	}
	if n := s.Count(`SELECT count(*) FROM work_item_history h JOIN work_items w ON w.tenant_id = h.tenant_id AND w.id = h.work_item_id WHERE h.tenant_id = $1 AND w.kind = 'defect'`, string(scope.TenantID)); n != 0 {
		t.Fatalf("a refused report wrote %d defect history rows", n)
	}
}
