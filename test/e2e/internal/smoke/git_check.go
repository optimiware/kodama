//go:build e2e

package smoke

// Checks for the protected-Git and push-trigger scenarios (S2, S5). They
// operate on public observations only: served policy bytes, the Git service's
// authenticated update refusal, accepted report events and admitted pipeline
// executions. Every negative branch is proved hermetically by the TestSmoke*
// tests in this package; no installation is needed to run them.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/optimiware/kodama/api"
)

// commitPattern accepts the lowercase object identity Git prints for a SHA-1
// or SHA-256 repository.
var commitPattern = regexp.MustCompile(`^([a-f0-9]{40}|[a-f0-9]{64})$`)

// hookRejection is the Git service's public update-hook refusal message. A
// failed push that does not carry it is a transport or authentication failure,
// never evidence of policy enforcement.
const hookRejection = "kodama git: update rejected"

// CheckCommitIdentity requires one lowercase full object identity.
func CheckCommitIdentity(label, value string) error {
	if !commitPattern.MatchString(value) {
		return fmt.Errorf("%s %q is not a full lowercase commit identity", label, value)
	}
	return nil
}

// CheckDistinctCommits requires a valid base and candidate that differ. An
// empty base is accepted only when absentBase is true (a created ref).
func CheckDistinctCommits(base, candidate string, absentBase bool) error {
	if base == "" && !absentBase {
		return fmt.Errorf("base commit is empty; a protected update needs the reviewed base")
	}
	if base != "" {
		if err := CheckCommitIdentity("base", base); err != nil {
			return err
		}
	}
	if err := CheckCommitIdentity("candidate", candidate); err != nil {
		return err
	}
	if candidate == base {
		return fmt.Errorf("candidate %q equals the base; a protected update must move the ref", candidate)
	}
	return nil
}

// CheckSubjectExact requires the immutable subject to carry exactly want, as
// string fields. A missing, additional or changed field is refused, so a
// resolution bound to another resource, ref, base or candidate cannot pass.
func CheckSubjectExact(subject *api.Subject, want map[string]string) error {
	if len(want) == 0 {
		return nil
	}
	if subject == nil {
		return fmt.Errorf("request subject is missing; want %v", want)
	}
	if len(subject.Fields) != len(want) {
		return fmt.Errorf("request subject has %d field(s) %v; want exactly %d %v", len(subject.Fields), subject.Fields, len(want), want)
	}
	seen := make(map[string]bool, len(want))
	for _, field := range subject.Fields {
		value, ok := want[field.Name]
		if !ok {
			return fmt.Errorf("request subject carries unexpected field %q", field.Name)
		}
		if field.Kind != "string" {
			return fmt.Errorf("request subject field %q kind = %q; want string", field.Name, field.Kind)
		}
		if field.Value != value {
			return fmt.Errorf("request subject field %q = %q; want %q", field.Name, field.Value, value)
		}
		seen[field.Name] = true
	}
	for name := range want {
		if !seen[name] {
			return fmt.Errorf("request subject is missing field %q", name)
		}
	}
	return nil
}

// CheckRepositoryPolicy requires the committed admin policy to carry the
// exact default block for one repository: read for @all, forced work-ref writes
// for engineers and executions, execution-only default-branch writes and
// reviewer approval for the default branch. A weakened or missing rule is
// refused; comments and blank lines are ignored.
func CheckRepositoryPolicy(policy []byte, repository string) error {
	if strings.TrimSpace(repository) == "" {
		return fmt.Errorf("repository name is empty")
	}
	want := []string{
		"repo " + repository,
		"R = @all",
		"RW+ refs/heads/work/ = @engineer @execution",
		"RW refs/heads/main = @execution",
		"option approve.refs/heads/main = reviewer",
	}
	var block []string
	found := false
	for raw := range strings.SplitSeq(string(policy), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "repo ") {
			if found {
				break
			}
			if line == want[0] {
				found = true
				block = append(block, line)
			}
			continue
		}
		if found {
			block = append(block, line)
		}
	}
	if !found {
		return fmt.Errorf("the committed policy has no %q block", want[0])
	}
	if len(block) != len(want) {
		return fmt.Errorf("the committed %q policy has %d rule(s) %v; want exactly %d %v", repository, len(block), block, len(want), want)
	}
	for index := range want {
		if block[index] != want[index] {
			return fmt.Errorf("the committed %q policy rule %d = %q; want %q", repository, index, block[index], want[index])
		}
	}
	return nil
}

// CheckHookRejected requires a failed Git push to carry the Git service's
// exact update-hook refusal. A transport, TLS or authentication failure never
// satisfies it.
func CheckHookRejected(output []byte) error {
	if !bytes.Contains(output, []byte(hookRejection)) {
		return fmt.Errorf("the push output does not carry the update-hook refusal %q", hookRejection)
	}
	return nil
}

// CheckInputValues requires one exact `w49-input-<name>=<value>` line for every
// wanted push input and no other w49-input line. It proves the subscribed
// pipeline was admitted with the human event's repository, ref, commit and
// previous commit, never the labels of another push.
func CheckInputValues(log []byte, want map[string]string) error {
	if len(want) == 0 {
		return fmt.Errorf("no expected push input was supplied")
	}
	counts := map[string]int{}
	for raw := range strings.SplitSeq(string(log), "\n") {
		line := strings.TrimSpace(raw)
		const prefix = "w49-input-"
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		name, value, ok := strings.Cut(strings.TrimPrefix(line, prefix), "=")
		if !ok || name == "" {
			return fmt.Errorf("malformed push input line %q", line)
		}
		counts[name]++
		if expected, wanted := want[name]; !wanted {
			return fmt.Errorf("push input %q was not expected", name)
		} else if value != expected {
			return fmt.Errorf("push input %q = %q; want %q", name, value, expected)
		}
	}
	for name, value := range want {
		if counts[name] != 1 {
			return fmt.Errorf("push input %q appears %d time(s); want exactly one %q line", name, counts[name], value)
		}
	}
	return nil
}

// CheckNoPipelineExecution requires zero executions of the named pipeline. It
// is the ordered exclusion control: after an execution-originated push has
// been reported, no subscribed pipeline execution may appear before the human
// push, so a late admission cannot hide behind a later non-empty list.
func CheckNoPipelineExecution(executions []api.ExecutionSummary, pipelineID string) error {
	if pipelineID == "" {
		return fmt.Errorf("pipeline identity is empty")
	}
	if len(executions) != 0 {
		ids := make([]string, 0, len(executions))
		for _, execution := range executions {
			ids = append(ids, execution.ExecutionID+"("+execution.PipelineID+")")
		}
		return fmt.Errorf("pipeline %s has %d execution(s) %v; want none before the human push", pipelineID, len(executions), ids)
	}
	return nil
}

// CheckPipelineExecutions requires exactly one execution of the named pipeline
// and returns its identity. A replay or a push that wrongly starts a second
// execution is refused.
func CheckPipelineExecutions(executions []api.ExecutionSummary, pipelineID string) (string, error) {
	if pipelineID == "" {
		return "", fmt.Errorf("pipeline identity is empty")
	}
	if len(executions) != 1 {
		ids := make([]string, 0, len(executions))
		for _, execution := range executions {
			ids = append(ids, execution.ExecutionID)
		}
		return "", fmt.Errorf("pipeline %s has %d execution(s) %v; want exactly one", pipelineID, len(executions), ids)
	}
	execution := executions[0]
	if execution.PipelineID != pipelineID {
		return "", fmt.Errorf("execution %s pipeline = %q; want %q", execution.ExecutionID, execution.PipelineID, pipelineID)
	}
	if execution.ExecutionID == "" {
		return "", fmt.Errorf("the pipeline execution has no identity")
	}
	return execution.ExecutionID, nil
}

// ReportWant names the accepted report facts one scenario expects.
type ReportWant struct {
	Resource     string
	Ref          string
	ExpectedBase string
	Candidate    string
	ServiceID    string
	// CorrelationID is the verified actor's execution. It is empty for a human
	// push and the execution identity for an execution-originated push.
	CorrelationID string
}

// reportData is the durable payload the verified-report ingress derives from
// the Git service's canonical subject.
type reportData struct {
	Operation    string `json:"operation"`
	Resource     string `json:"resource"`
	Ref          string `json:"ref"`
	ExpectedBase string `json:"expected_base"`
	Candidate    string `json:"candidate"`
	Deleted      bool   `json:"deleted"`
}

// CheckReportAttribution requires one genuine accepted verified report: the
// lifecycle type and report type, the Git service as source, the exact
// resource/ref/base/candidate payload, a non-empty verification causation and
// the expected execution correlation. A synthesised event, a shell-sourced
// event, a deletion or a report for another subject is refused.
func CheckReportAttribution(event api.Event, want ReportWant) error {
	if event.Type != "resource.verified" {
		return fmt.Errorf("event %s type = %q; want resource.verified", event.ID, event.Type)
	}
	if event.ReportType != "git.ref.updated" {
		return fmt.Errorf("event %s report type = %q; want git.ref.updated", event.ID, event.ReportType)
	}
	if event.Source.Kind != "service" {
		return fmt.Errorf("event %s source kind = %q; want service", event.ID, event.Source.Kind)
	}
	if want.ServiceID == "" || event.Source.PrincipalID != want.ServiceID {
		return fmt.Errorf("event %s source principal = %q; want the Git service %q", event.ID, event.Source.PrincipalID, want.ServiceID)
	}
	if want.Resource == "" || event.SubjectID != want.Resource {
		return fmt.Errorf("event %s subject = %q; want the resource %q", event.ID, event.SubjectID, want.Resource)
	}
	if event.CorrelationID != want.CorrelationID {
		return fmt.Errorf("event %s correlation = %q; want %q", event.ID, event.CorrelationID, want.CorrelationID)
	}
	if event.CausationID == "" {
		return fmt.Errorf("event %s has no verification causation", event.ID)
	}
	if event.ID == "" {
		return fmt.Errorf("accepted report has no event identity")
	}
	var data reportData
	if err := json.Unmarshal(event.Data, &data); err != nil {
		return fmt.Errorf("event %s report data is not the canonical payload: %v", event.ID, err)
	}
	if data.Operation != "git.ref.update" {
		return fmt.Errorf("event %s operation = %q; want git.ref.update", event.ID, data.Operation)
	}
	if data.Resource != want.Resource {
		return fmt.Errorf("event %s report resource = %q; want %q", event.ID, data.Resource, want.Resource)
	}
	if data.Ref != want.Ref {
		return fmt.Errorf("event %s report ref = %q; want %q", event.ID, data.Ref, want.Ref)
	}
	if data.ExpectedBase != want.ExpectedBase {
		return fmt.Errorf("event %s report expected base = %q; want %q", event.ID, data.ExpectedBase, want.ExpectedBase)
	}
	if data.Candidate != want.Candidate {
		return fmt.Errorf("event %s report candidate = %q; want %q", event.ID, data.Candidate, want.Candidate)
	}
	if data.Deleted {
		return fmt.Errorf("event %s reports a deletion; a push trigger needs a live commit", event.ID)
	}
	return nil
}

// FindReport returns the first accepted report event that matches want exactly.
func FindReport(events []api.Event, want ReportWant) (api.Event, bool) {
	for _, event := range events {
		if CheckReportAttribution(event, want) == nil {
			return event, true
		}
	}
	return api.Event{}, false
}
