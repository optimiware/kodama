package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/images/harness-opencode/wrapper/spec"
	"github.com/optimiware/kodama/internal/domain"
)

// agentResult is the structured completion the agent writes to
// spec.ResultFile. It is the only thing that can complete a step: a prose
// reply, a zero exit status or a clean event stream do not. Unknown members,
// duplicate keys and trailing data are refused.
type agentResult struct {
	Outcome     string       `json:"outcome"`
	Summary     string       `json:"summary"`
	Question    string       `json:"question"`
	Reason      string       `json:"reason"`
	Conformance *agentReport `json:"conformance"`
}

type agentReport struct {
	Requirements []string         `json:"requirements"`
	Checks       []agentCheck     `json:"checks"`
	Deviations   []agentDeviation `json:"deviations"`
}

type agentCheck struct {
	ID       string `json:"id"`
	Outcome  string `json:"outcome"`
	Evidence string `json:"evidence"`
}

type agentDeviation struct {
	Requirement string `json:"requirement"`
	Reason      string `json:"reason"`
}

const maxResultBytes = 256 << 10

var reasonRE = regexp.MustCompile(runtimev1.Patterns["ReasonCode"])

// readAgentResult loads and validates the agent's result file from the
// workspace root, then removes it so it is never committed. A symlink,
// non-regular file, oversized file or malformed document is invalid; an absent
// file is missing.
func readAgentResult(workspace string) (res agentResult, state string, err error) {
	path := filepath.Join(workspace, spec.ResultFile)
	fi, lerr := os.Lstat(path)
	if lerr != nil {
		if errors.Is(lerr, os.ErrNotExist) {
			return res, spec.ReasonResultMissing, errors.New("the agent wrote no result file")
		}
		return res, spec.ReasonResultInvalid, lerr
	}
	defer os.Remove(path)
	if !fi.Mode().IsRegular() {
		return res, spec.ReasonResultInvalid, errors.New("the result file is not a regular file")
	}
	if fi.Size() > maxResultBytes {
		return res, spec.ReasonResultInvalid, errors.New("the result file is too large")
	}
	raw, rerr := os.ReadFile(path)
	if rerr != nil {
		return res, spec.ReasonResultInvalid, rerr
	}
	if _, cerr := runtimev1.Canonicalize(raw); cerr != nil {
		// Canonicalize rejects duplicate keys, floats and malformed JSON.
		return res, spec.ReasonResultInvalid, errors.New("the result file is not strict JSON")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if derr := dec.Decode(&res); derr != nil {
		return agentResult{}, spec.ReasonResultInvalid, fmt.Errorf("the result file does not match the result schema: %w", derr)
	}
	if _, terr := dec.Token(); terr != io.EOF {
		return agentResult{}, spec.ReasonResultInvalid, errors.New("the result file has trailing data")
	}
	switch res.Outcome {
	case "completed":
		if strings.TrimSpace(res.Summary) == "" || len(res.Summary) > 4096 {
			return agentResult{}, spec.ReasonResultInvalid, errors.New("a completed result needs a summary of at most 4096 bytes")
		}
	case "needs_input":
		if strings.TrimSpace(res.Question) == "" || len(res.Question) > 4096 {
			return agentResult{}, spec.ReasonResultInvalid, errors.New("a needs_input result needs a question of at most 4096 bytes")
		}
	case "failed":
		if res.Reason != "" && !reasonRE.MatchString(res.Reason) {
			return agentResult{}, spec.ReasonResultInvalid, errors.New("the failure reason is not a reason code")
		}
	default:
		return agentResult{}, spec.ReasonResultInvalid, errors.New("the outcome is not completed, needs_input or failed")
	}
	return res, "", nil
}

// manifest is the strict shape of the entrypoint's /kodama/results.json.
type manifest struct {
	Outcome  string `json:"outcome"`
	Reason   string `json:"reason,omitempty"`
	Question string `json:"question,omitempty"`
}

// writeManifest atomically replaces the results manifest.
func writeManifest(file string, m manifest) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, file); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// buildReport turns the baseline pinned for the attempt, the candidate commit
// and the agent's own account into the declared conformance artifact.
//
// The agent proposes requirements, check outcomes and deviations; the wrapper
// decides what can be believed:
//   - the baseline and its digest come from the pinned context, never the agent;
//   - the report always lists every required check of the baseline, and a check
//     the agent did not report is not_run;
//   - the contract demands evidence artifact IDs for a passed check and those IDs
//     are assigned by the artifact store only after the entrypoint uploads, which
//     happens after the wrapper exits. The wrapper cannot mint them, so an agent
//     claim of "passed" is recorded as not_run: the report is then blocked, never
//     a prose-only success. (Evidence binding is an open prerequisite, see README.)
//   - deviations are never dropped and always block.
func buildReport(base runtimev1.DesignBaseline, candidate string, ag *agentReport, redact func(string) string) (runtimev1.ConformanceReport, []string, error) {
	var notes []string
	reqs := map[string]bool{}
	for _, r := range base.Requirements {
		reqs[r] = true
	}
	claims := map[string]string{}
	var devs []runtimev1.Deviation
	if ag != nil {
		for _, r := range ag.Requirements {
			if !reqs[r] {
				notes = append(notes, "requirement "+r+" is not in the pinned baseline and was not added")
			}
		}
		for _, c := range ag.Checks {
			claims[c.ID] = c.Outcome
		}
		for _, d := range ag.Deviations {
			reason := strings.TrimSpace(redact(d.Reason))
			if len(reason) > 2048 {
				reason = reason[:2048]
			}
			devs = append(devs, runtimev1.Deviation{Requirement: d.Requirement, Reason: reason})
		}
	}
	checkIDs := append([]string{}, base.RequiredChecks...)
	if len(checkIDs) == 0 {
		// The contract requires at least one check; the wrapper's own structural
		// check stands in when the baseline requires none.
		checkIDs = []string{"structured-result"}
	}
	sort.Strings(checkIDs)
	var checks []runtimev1.ConformanceCheck
	for _, id := range checkIDs {
		out := runtimev1.CheckNotRun
		switch claims[id] {
		case "failed":
			out = runtimev1.CheckFailed
		case "passed":
			notes = append(notes, "check "+id+" was claimed passed but no evidence artifact can be bound by the wrapper; recorded not_run")
		}
		checks = append(checks, runtimev1.ConformanceCheck{ID: id, Outcome: out, EvidenceArtifactIDs: []domain.ArtifactID{}})
	}
	sort.Slice(devs, func(i, j int) bool {
		if devs[i].Requirement != devs[j].Requirement {
			return devs[i].Requirement < devs[j].Requirement
		}
		return devs[i].Reason < devs[j].Reason
	})
	devs = dedupe(devs)
	if devs == nil {
		devs = []runtimev1.Deviation{}
	}
	rep := runtimev1.ConformanceReport{
		BaselineDigest:       base.Digest,
		CandidateCommit:      runtimev1.GitCommit(candidate),
		Requirements:         append([]string{}, base.Requirements...),
		Checks:               checks,
		UnresolvedDeviations: devs,
		Disposition:          runtimev1.DispBlocked,
	}
	allPassed := true
	for _, c := range checks {
		allPassed = allPassed && c.Outcome == runtimev1.CheckPassed
	}
	if allPassed && len(devs) == 0 {
		rep.Disposition = runtimev1.Conforms
	}
	if err := rep.Validate(); err != nil {
		return rep, notes, err
	}
	return rep, notes, nil
}

func dedupe(in []runtimev1.Deviation) []runtimev1.Deviation {
	var out []runtimev1.Deviation
	for i, d := range in {
		if i > 0 && in[i-1] == d {
			continue
		}
		out = append(out, d)
	}
	return out
}
