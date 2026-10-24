package qa

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// DefectKey is the deterministic deduplication key of one finding. It binds the
// tenant and project, the work item under test, the candidate commit and the
// finding's fingerprint. Two reports of the same finding on the same candidate
// (a retry after a lost response, a second pass, a different screenshot) have
// one key; the same finding on a different candidate is a different key, so a
// regression is never hidden behind an old, closed defect. The key never
// depends on the report's artifact identities, the attempt or the clock.
func DefectKey(scope domain.Scope, subject domain.WorkItemID, candidate runtimev1.GitCommit, fingerprint string) string {
	raw, err := json.Marshal(struct {
		Format      string `json:"format"`
		Tenant      string `json:"tenantId"`
		Project     string `json:"projectId"`
		Subject     string `json:"workItemId"`
		Candidate   string `json:"candidateCommit"`
		Fingerprint string `json:"fingerprint"`
	}{"kodama/qa-defect-key/v1", string(scope.TenantID), string(scope.ProjectID), string(subject), string(candidate), fingerprint})
	if err != nil {
		panic(fmt.Sprintf("qa: marshal defect key: %v", err)) // plain strings; cannot fail
	}
	canonical, err := runtimev1.Canonicalize(raw)
	if err != nil {
		panic(fmt.Sprintf("qa: canonicalize defect key: %v", err))
	}
	sum := sha256.Sum256(canonical)
	return KeyPrefix + hex.EncodeToString(sum[:])
}

// KeyPrefix starts every defect key.
const KeyPrefix = "qa1_"

// DefectIDFor derives the work item identity of a defect key. The defect has
// this identity from creation, so the identity is known before any write: a
// retry after an unknown commit outcome and a concurrent identical request
// address the same row, and the work store's primary key admits one.
func DefectIDFor(key string) domain.WorkItemID {
	if len(key) != len(KeyPrefix)+64 {
		return ""
	}
	return domain.WorkItemID("wi_qa_" + key[len(KeyPrefix):len(KeyPrefix)+32])
}

// payloadMarker matches the marker line of a defect specification, which the
// server writes as the exact final line of the creation specification. It is
// part of the immutable creation record in the work history, which is what a
// later report of the same finding is compared against. The expression is
// anchored to the whole line: report-controlled text (which may span lines) is
// rendered before the marker and can never be mistaken for it.
var payloadMarker = regexp.MustCompile("^- Finding payload digest: `([0-9a-f]{64})`$")

// PayloadMarker renders the marker line for a payload digest (no trailing newline).
func PayloadMarker(digest string) string {
	return "- Finding payload digest: `" + digest + "`"
}

// ParsePayloadMarker extracts the payload digest recorded in a defect creation
// specification. Only the exact final line is consulted (after trimming
// trailing newlines and nothing else); marker-like text anywhere earlier is
// report-controlled content and is ignored. ok is false, so callers fail closed,
// when the final line is not the server marker.
func ParsePayloadMarker(spec string) (digest string, ok bool) {
	spec = strings.TrimRight(spec, "\n")
	last := spec
	if i := strings.LastIndexByte(spec, '\n'); i >= 0 {
		last = spec[i+1:]
	}
	m := payloadMarker.FindStringSubmatch(last)
	if m == nil {
		return "", false
	}
	return m[1], true
}
