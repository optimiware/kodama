//go:build e2e

package smoke_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/optimiware/kodama/api"

	"github.com/optimiware/kodama/test/e2e/internal/smoke"
)

// TestSmokeGitChecks proves the protected-Git and push-trigger checks refuse
// wrong candidate, wrong base, wrong actor, a second execution, a wrong push
// identity, a missing accepted report and a missing or duplicated push input.
// It is hermetic: no installation, network or clock.
func TestSmokeGitChecks(t *testing.T) {
	const (
		base      = "1111111111111111111111111111111111111111"
		candidate = "2222222222222222222222222222222222222222"
		other     = "3333333333333333333333333333333333333333"
		service   = "git"
	)

	t.Run("candidate and base", func(t *testing.T) {
		if err := smoke.CheckDistinctCommits(base, candidate, false); err != nil {
			t.Fatalf("a distinct base and candidate were refused: %v", err)
		}
		if err := smoke.CheckDistinctCommits(base, base, false); err == nil {
			t.Fatal("a candidate equal to the base was accepted")
		}
		if err := smoke.CheckDistinctCommits("", candidate, false); err == nil {
			t.Fatal("an empty base was accepted for a protected update")
		}
		if err := smoke.CheckDistinctCommits("", candidate, true); err != nil {
			t.Fatalf("a created ref with an absent base was refused: %v", err)
		}
		if err := smoke.CheckDistinctCommits("not-a-commit", candidate, false); err == nil {
			t.Fatal("a malformed base was accepted")
		}
		if err := smoke.CheckDistinctCommits(base, "ABCDEF", false); err == nil {
			t.Fatal("an uppercase/short candidate was accepted")
		}
	})

	t.Run("committed repository policy", func(t *testing.T) {
		policy := "repo admin\n    RW = @admin\n\nrepo smoke/abc\n" +
			"    R = @all\n" +
			"    RW+ refs/heads/work/ = @engineer @execution\n" +
			"    RW refs/heads/main = @execution\n" +
			"    option approve.refs/heads/main = reviewer\n"
		if err := smoke.CheckRepositoryPolicy([]byte(policy), "smoke/abc"); err != nil {
			t.Fatalf("the exact default policy was refused: %v", err)
		}
		for name, changed := range map[string]string{
			"engineer may write main": strings.Replace(policy, "RW refs/heads/main = @execution", "RW refs/heads/main = @engineer @execution", 1),
			"approval removed":        strings.Replace(policy, "    option approve.refs/heads/main = reviewer\n", "", 1),
			"approval widened":        strings.Replace(policy, "= reviewer", "= operator", 1),
			"read removed":            strings.Replace(policy, "    R = @all\n", "", 1),
			"work write narrowed":     strings.Replace(policy, "@engineer @execution", "@engineer", 1),
		} {
			if err := smoke.CheckRepositoryPolicy([]byte(changed), "smoke/abc"); err == nil {
				t.Fatalf("a weakened policy (%s) was accepted", name)
			}
		}
		if err := smoke.CheckRepositoryPolicy([]byte(policy), "smoke/missing"); err == nil {
			t.Fatal("a missing repository block was accepted")
		}
	})

	t.Run("hook refusal", func(t *testing.T) {
		if err := smoke.CheckHookRejected([]byte("remote: kodama git: update rejected\nfatal: failed to push")); err != nil {
			t.Fatalf("the hook refusal was refused: %v", err)
		}
		if err := smoke.CheckHookRejected([]byte("fatal: unable to access: TLS handshake timeout")); err == nil {
			t.Fatal("a transport failure was accepted as the hook refusal")
		}
		if err := smoke.CheckHookRejected(nil); err == nil {
			t.Fatal("an empty push output was accepted as the hook refusal")
		}
	})

	t.Run("exact review subject", func(t *testing.T) {
		want := map[string]string{
			"operation":     "git.ref.update",
			"resource":      "w49-repo-run",
			"ref":           "refs/heads/main",
			"expected_base": base,
			"candidate":     candidate,
		}
		subject := &api.Subject{Fields: []api.SubjectField{
			{Name: "operation", Kind: "string", Value: "git.ref.update"},
			{Name: "resource", Kind: "string", Value: "w49-repo-run"},
			{Name: "ref", Kind: "string", Value: "refs/heads/main"},
			{Name: "expected_base", Kind: "string", Value: base},
			{Name: "candidate", Kind: "string", Value: candidate},
		}}
		if err := smoke.CheckSubjectExact(subject, want); err != nil {
			t.Fatalf("the exact subject was refused: %v", err)
		}
		wrongCandidate := &api.Subject{Fields: append([]api.SubjectField(nil), subject.Fields...)}
		wrongCandidate.Fields[4].Value = other
		if err := smoke.CheckSubjectExact(wrongCandidate, want); err == nil {
			t.Fatal("a subject bound to another candidate was accepted")
		}
		wrongBase := &api.Subject{Fields: append([]api.SubjectField(nil), subject.Fields...)}
		wrongBase.Fields[3].Value = ""
		if err := smoke.CheckSubjectExact(wrongBase, want); err == nil {
			t.Fatal("a subject bound to another base was accepted")
		}
		extra := &api.Subject{Fields: append(append([]api.SubjectField(nil), subject.Fields...), api.SubjectField{Name: "extra", Kind: "string", Value: "x"})}
		if err := smoke.CheckSubjectExact(extra, want); err == nil {
			t.Fatal("a subject with an extra field was accepted")
		}
		if err := smoke.CheckSubjectExact(nil, want); err == nil {
			t.Fatal("a missing subject was accepted")
		}
	})

	t.Run("no execution before the human push", func(t *testing.T) {
		if err := smoke.CheckNoPipelineExecution(nil, "pipeline-1"); err != nil {
			t.Fatalf("the empty pre-human list was refused: %v", err)
		}
		if err := smoke.CheckNoPipelineExecution([]api.ExecutionSummary{{ExecutionID: "execution-1", PipelineID: "pipeline-1"}}, "pipeline-1"); err == nil {
			t.Fatal("a non-empty list before the human push was accepted")
		}
		if err := smoke.CheckNoPipelineExecution([]api.ExecutionSummary{{ExecutionID: "execution-1"}}, "pipeline-1"); err == nil {
			t.Fatal("an execution with no pipeline identity was accepted")
		}
		if err := smoke.CheckNoPipelineExecution(nil, ""); err == nil {
			t.Fatal("an empty pipeline identity was accepted")
		}
	})

	t.Run("one pipeline execution", func(t *testing.T) {
		one := []api.ExecutionSummary{{ExecutionID: "execution-1", PipelineID: "pipeline-1"}}
		id, err := smoke.CheckPipelineExecutions(one, "pipeline-1")
		if err != nil || id != "execution-1" {
			t.Fatalf("the single pipeline execution was refused: id=%q err=%v", id, err)
		}
		if _, err := smoke.CheckPipelineExecutions(one, "pipeline-2"); err == nil {
			t.Fatal("an execution of another pipeline was accepted")
		}
		if _, err := smoke.CheckPipelineExecutions(nil, "pipeline-1"); err == nil {
			t.Fatal("zero executions were accepted as exactly one")
		}
		two := append(append([]api.ExecutionSummary(nil), one...), api.ExecutionSummary{ExecutionID: "execution-2", PipelineID: "pipeline-1"})
		if _, err := smoke.CheckPipelineExecutions(two, "pipeline-1"); err == nil {
			t.Fatal("a second execution (replay or wrongly started push) was accepted")
		}
	})

	t.Run("push inputs", func(t *testing.T) {
		log := []byte("prefix\nw49-input-repository=w49-repo-run\nw49-input-ref=refs/heads/work/human\n" +
			"w49-input-commit=" + candidate + "\nw49-input-previous_commit=" + base + "\n")
		want := map[string]string{
			"repository":      "w49-repo-run",
			"ref":             "refs/heads/work/human",
			"commit":          candidate,
			"previous_commit": base,
		}
		if err := smoke.CheckInputValues(log, want); err != nil {
			t.Fatalf("the exact push inputs were refused: %v", err)
		}
		wrongCommit := map[string]string{"repository": "w49-repo-run", "ref": "refs/heads/work/human", "commit": other, "previous_commit": base}
		if err := smoke.CheckInputValues(log, wrongCommit); err == nil {
			t.Fatal("a push for another commit was accepted")
		}
		wrongRepo := map[string]string{"repository": "other", "ref": "refs/heads/work/human", "commit": candidate, "previous_commit": base}
		if err := smoke.CheckInputValues(log, wrongRepo); err == nil {
			t.Fatal("a push for another repository was accepted")
		}
		if err := smoke.CheckInputValues(log, map[string]string{"repository": "w49-repo-run"}); err == nil {
			t.Fatal("a log missing three expected inputs was accepted")
		}
		duplicated := append(append([]byte(nil), log...), []byte("w49-input-commit="+candidate+"\n")...)
		if err := smoke.CheckInputValues(duplicated, want); err == nil {
			t.Fatal("a duplicated push input line was accepted")
		}
		if err := smoke.CheckInputValues(append(append([]byte(nil), log...), []byte("w49-input-extra=x\n")...), want); err == nil {
			t.Fatal("an unexpected push input line was accepted")
		}
	})
}

// TestSmokeReportAttribution proves an accepted report is only the genuine
// verified Git update: wrong source, a synthetic event, a moved correlation, a
// changed candidate/base and a missing verification causation are refused.
func TestSmokeReportAttribution(t *testing.T) {
	const (
		base      = "1111111111111111111111111111111111111111"
		candidate = "2222222222222222222222222222222222222222"
	)
	want := smoke.ReportWant{
		Resource:      "w49-repo-run",
		Ref:           "refs/heads/main",
		ExpectedBase:  base,
		Candidate:     candidate,
		ServiceID:     "git",
		CorrelationID: "execution-1",
	}
	data, err := json.Marshal(map[string]string{
		"operation":     "git.ref.update",
		"resource":      "w49-repo-run",
		"ref":           "refs/heads/main",
		"expected_base": base,
		"candidate":     candidate,
	})
	if err != nil {
		t.Fatalf("marshal report data: %v", err)
	}
	event := api.Event{
		ID:            "event-1",
		Type:          "resource.verified",
		ReportType:    "git.ref.updated",
		Source:        api.EventSource{Kind: "service", PrincipalID: "git"},
		SubjectID:     "w49-repo-run",
		CorrelationID: "execution-1",
		CausationID:   "verification-1",
		Data:          data,
	}
	if err := smoke.CheckReportAttribution(event, want); err != nil {
		t.Fatalf("the genuine report was refused: %v", err)
	}
	if found, ok := smoke.FindReport([]api.Event{event}, want); !ok || found.ID != event.ID {
		t.Fatal("FindReport did not find the genuine report")
	}

	human := want
	human.CorrelationID = ""
	if err := smoke.CheckReportAttribution(event, human); err == nil {
		t.Fatal("an execution-correlated report was accepted as a human push")
	}
	execution := want
	execution.CorrelationID = ""
	humanEvent := event
	humanEvent.CorrelationID = ""
	if err := smoke.CheckReportAttribution(humanEvent, execution); err != nil {
		t.Fatalf("the human report with no correlation was refused: %v", err)
	}

	shell := event
	shell.Source = api.EventSource{Kind: "shell", PrincipalID: "shell"}
	if err := smoke.CheckReportAttribution(shell, want); err == nil {
		t.Fatal("a shell-sourced synthetic event was accepted")
	}
	wrongService := event
	wrongService.Source = api.EventSource{Kind: "service", PrincipalID: "other"}
	if err := smoke.CheckReportAttribution(wrongService, want); err == nil {
		t.Fatal("a report from another service was accepted")
	}
	noCausation := event
	noCausation.CausationID = ""
	if err := smoke.CheckReportAttribution(noCausation, want); err == nil {
		t.Fatal("a report without a verification causation was accepted")
	}
	wrongType := event
	wrongType.ReportType = "observation.custom"
	if err := smoke.CheckReportAttribution(wrongType, want); err == nil {
		t.Fatal("a non-Git report type was accepted")
	}

	changed := map[string]string{
		"operation":     "git.ref.update",
		"resource":      "w49-repo-run",
		"ref":           "refs/heads/main",
		"expected_base": "",
		"candidate":     candidate,
	}
	changedData, _ := json.Marshal(changed)
	wrongBase := event
	wrongBase.Data = changedData
	if err := smoke.CheckReportAttribution(wrongBase, want); err == nil {
		t.Fatal("a report for another base was accepted")
	}
	changed["expected_base"] = base
	changed["candidate"] = base
	wrongCandidateData, _ := json.Marshal(changed)
	wrongCandidate := event
	wrongCandidate.Data = wrongCandidateData
	if err := smoke.CheckReportAttribution(wrongCandidate, want); err == nil {
		t.Fatal("a report for another candidate was accepted")
	}
	deletedData, _ := json.Marshal(map[string]any{
		"operation":     "git.ref.update",
		"resource":      "w49-repo-run",
		"ref":           "refs/heads/main",
		"expected_base": base,
		"candidate":     "",
		"deleted":       true,
	})
	deleted := event
	deleted.Data = deletedData
	if err := smoke.CheckReportAttribution(deleted, want); err == nil {
		t.Fatal("a deletion report was accepted as a push")
	}
	if _, ok := smoke.FindReport([]api.Event{shell, wrongService}, want); ok {
		t.Fatal("FindReport found a report that does not match")
	}
}
