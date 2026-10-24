package gate

import (
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/optimiware/kodama/internal/cli/client"
	"github.com/optimiware/kodama/internal/cli/root"
	"github.com/optimiware/kodama/internal/domain"
)

// decisionRequest is the GateDecisionRequest of api/gates.yaml and nothing
// else: the actor and the subject are server-derived and never sent.
type decisionRequest struct {
	Decision      string `json:"decision"`
	SubjectDigest string `json:"subjectDigest"`
	Reason        string `json:"reason"`
}

const maxReasonRunes = 2048

var (
	digestPattern  = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	keyPattern     = regexp.MustCompile(`^[A-Za-z0-9._~:-]{1,128}$`)
	versionPattern = regexp.MustCompile(`^[1-9][0-9]{0,15}$`)
)

func getRequest(scope client.Scope, id string) client.Request {
	return client.Request{Method: http.MethodGet, Scope: scope, Path: []string{"gates", id}}
}

func decideRequest(scope client.Scope, id string, v domain.Version, key string, body decisionRequest) client.Request {
	return client.Request{
		Method: http.MethodPost, Scope: scope, Path: []string{"gates", id, "decisions"},
		IfVersion: v, IdempotencyKey: key, Body: body,
	}
}

// decisionInputs checks that every explicit input is present and well formed
// before any request. It only enforces wire syntax; nothing is defaulted,
// fetched or recomputed, so the values sent are exactly the ones supplied.
func decisionInputs(set map[string]bool, decision, digest, reason, ifVersion, key string) (decisionRequest, domain.Version, string, error) {
	var zero decisionRequest
	for _, name := range []string{"decision", "subject-digest", "reason", "if-version", "idempotency-key"} {
		if !set[name] {
			return zero, 0, "", root.Usagef("--%s is required: a decision is never inferred", name)
		}
	}
	if decision != "approve" && decision != "reject" {
		return zero, 0, "", root.Usagef("--decision must be approve or reject")
	}
	if !digestPattern.MatchString(digest) {
		return zero, 0, "", root.Usagef("--subject-digest must be sha256: followed by 64 lowercase hex digits")
	}
	if strings.TrimSpace(reason) == "" {
		return zero, 0, "", root.Usagef("--reason must not be empty")
	}
	if !utf8.ValidString(reason) || utf8.RuneCountInString(reason) > maxReasonRunes {
		return zero, 0, "", root.Usagef("--reason must be valid UTF-8 text of at most %d characters", maxReasonRunes)
	}
	if !keyPattern.MatchString(key) {
		return zero, 0, "", root.Usagef("--idempotency-key must be 1-128 characters of letters, digits and . _ ~ : -")
	}
	version, err := parseVersion("if-version", ifVersion)
	if err != nil {
		return zero, 0, "", err
	}
	return decisionRequest{Decision: decision, SubjectDigest: digest, Reason: reason}, version, key, nil
}

// parseVersion reads the observed gate version exactly as written.
func parseVersion(flagName, v string) (domain.Version, error) {
	if !versionPattern.MatchString(v) {
		return 0, root.Usagef("--%s must be a positive integer gate version", flagName)
	}
	var n int64
	for _, c := range v {
		n = n*10 + int64(c-'0')
	}
	ver := domain.Version(n)
	if err := ver.Validate(); err != nil {
		return 0, root.Usagef("--%s must be between 1 and %d", flagName, int64(domain.MaxSafeInteger))
	}
	return ver, nil
}
