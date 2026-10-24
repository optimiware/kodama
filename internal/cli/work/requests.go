package work

import (
	"net/http"
	"net/url"

	"github.com/optimiware/kodama/internal/cli/client"
	"github.com/optimiware/kodama/internal/cli/root"
	"github.com/optimiware/kodama/internal/domain"
)

// Wire shapes of api/work.yaml for the structured input files. They carry no
// validation beyond the contract's member names; the server owns every rule.
type criterion struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

type evidenceRef struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Note string `json:"note,omitempty"`
}

type finding struct {
	ID      string `json:"id"`
	Summary string `json:"summary,omitempty"`
}

type reviewClaim struct {
	CandidateCommit string        `json:"candidateCommit"`
	Evidence        []evidenceRef `json:"evidence"`
}

type completionClaim struct {
	ContentRevision      int64     `json:"contentRevision"`
	CandidateCommit      string    `json:"candidateCommit,omitempty"`
	AcceptedCriteria     []string  `json:"acceptedCriteria"`
	AcknowledgedFindings []finding `json:"acknowledgedFindings,omitempty"`
}

var (
	workKinds  = map[string]bool{"epic": true, "story": true, "task": true, "defect": true}
	workStates = map[string]bool{"backlog": true, "ready": true, "active": true, "review": true, "done": true, "cancelled": true}
)

func workPath(id string, rest ...string) []string {
	return append([]string{"work-items", id}, rest...)
}

func createRequest(scope client.Scope, body map[string]any) client.Request {
	return client.Request{Method: http.MethodPost, Scope: scope, Path: []string{"work-items"}, Body: body}
}

func getRequest(scope client.Scope, id string) client.Request {
	return client.Request{Method: http.MethodGet, Scope: scope, Path: workPath(id)}
}

func updateRequest(scope client.Scope, id string, v domain.Version, body map[string]any) client.Request {
	return client.Request{Method: http.MethodPatch, Scope: scope, Path: workPath(id), IfVersion: v, Body: body}
}

func transitionRequest(scope client.Scope, id string, v domain.Version, body map[string]any) client.Request {
	return client.Request{Method: http.MethodPost, Scope: scope, Path: workPath(id, "transitions"), IfVersion: v, Body: body}
}

func historyRequest(scope client.Scope, id string, q url.Values) client.Request {
	return client.Request{Method: http.MethodGet, Scope: scope, Path: workPath(id, "history"), Query: q}
}

func listRequest(scope client.Scope, q url.Values) client.Request {
	return client.Request{Method: http.MethodGet, Scope: scope, Path: []string{"work-items"}, Query: q}
}

// checkEnum rejects a value outside the contract's closed set before any call.
func checkEnum(flagName, v string, allowed map[string]bool) error {
	if !allowed[v] {
		return root.Usagef("--%s %q is not one of the accepted values", flagName, v)
	}
	return nil
}
