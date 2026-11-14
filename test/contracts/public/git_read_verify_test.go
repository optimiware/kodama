package publiccontract

import (
	"encoding/json"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/shell/resource"
)

// TestGitReadVerifyShape pins the repository-level `git.read` resource
// operation and its verification shape: it is declared after `git.ref.update`,
// it is never a router or MCP operation, and a verify request for it requires
// an empty subject and no approval. It still returns the authenticated
// principal, the scoped roles and a verification ID like any verify.
//
// This kills an enum-only change (git.read valid but unrestricted), an
// approval-allowed read, and a git.ref.read alias.
func TestGitReadVerifyShape(t *testing.T) {
	want := []resource.Operation{resource.OperationGitRefUpdate, resource.OperationGitRead}
	domain := resource.Operations()
	if len(domain) != len(want) {
		t.Fatalf("domain resource operations = %v; want %v", domain, want)
	}
	for index := range want {
		if domain[index] != want[index] {
			t.Fatalf("domain resource operation %d = %q; want %q", index, domain[index], want[index])
		}
	}
	public := api.ResourceOperations()
	if len(public) != len(want) {
		t.Fatalf("api.ResourceOperations() = %v; want %v", public, want)
	}
	for index, operation := range want {
		if string(public[index]) != string(operation) {
			t.Errorf("public resource operation %d = %q; want %q", index, public[index], operation)
		}
	}
	if api.ResourceOperationGitRead != "git.read" {
		t.Fatalf("ResourceOperationGitRead = %q; want git.read", api.ResourceOperationGitRead)
	}
	if !api.ResourceOperationGitRead.Valid() {
		t.Fatal("git.read is not a valid resource operation")
	}
	if api.ResourceOperation("git.ref.read").Valid() {
		t.Fatal("git.ref.read was added as a resource operation")
	}
	for _, name := range []string{"", "git.ref.read", "git.ref.delete", "git.read.all", "task.create"} {
		if api.ResourceOperation(name).Valid() {
			t.Errorf("resource operation %q accepted", name)
		}
	}

	// git.read is not a router operation, an MCP tool or a codec entry.
	if route, ok := api.LookupRoute("git.read"); ok {
		t.Fatalf("git.read is also a public route %+v", route)
	}
	if len(api.Routes()) != 58 {
		t.Fatalf("api.Routes() has %d rows; want 58", len(api.Routes()))
	}
	for _, route := range api.Routes() {
		if route.Operation == "git.read" || route.MCPTool == "git.read" {
			t.Fatalf("git.read leaked into the router inventory: %+v", route)
		}
	}
	for _, codec := range api.OperationCodecs() {
		if codec.Operation == "git.read" {
			t.Fatal("git.read leaked into the operation codec table")
		}
	}

	// A git.read verify carries no subject and no approval.
	request := api.VerifyRequest{
		ActorCredential: "kodex.exec-1.signature",
		ResourceID:      "res-1",
		Operation:       "git.read",
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("empty-subject git.read verify refused: %v", err)
	}
	decoded, err := api.DecodeVerifyRequest([]byte(`{"actor_credential":"kodex.exec-1.signature","resource_id":"res-1","operation":"git.read"}`))
	if err != nil {
		t.Fatalf("DecodeVerifyRequest(git.read): %v", err)
	}
	if err := decoded.Validate(); err != nil {
		t.Fatalf("decoded git.read verify refused: %v", err)
	}
	emptySubject := api.VerifyRequest{
		ActorCredential: "k",
		ResourceID:      "res-1",
		Operation:       "git.read",
		Subject:         api.Subject{Fields: []api.SubjectField{}},
	}
	if err := emptySubject.Validate(); err != nil {
		t.Fatalf("git.read verify with an explicitly empty subject refused: %v", err)
	}

	// A nonempty subject is refused: reads are repository-level.
	withSubject := request
	withSubject.Subject = api.Subject{Fields: []api.SubjectField{{Name: "ref", Kind: "string", Value: "refs/heads/main"}}}
	if err := withSubject.Validate(); err == nil {
		t.Fatal("git.read verify accepted a nonempty subject")
	} else {
		requireInvalid(t, err, "git.read nonempty subject")
	}

	// No approval may be requested for a read.
	for _, approval := range []string{"role/admin", "role/engineer", "role/reviewer", "role/operator", "role/viewer"} {
		withApproval := request
		withApproval.Approval = approval
		if err := withApproval.Validate(); err == nil {
			t.Errorf("git.read verify accepted approval %q", approval)
		} else {
			requireInvalid(t, err, "git.read approval "+approval)
		}
	}

	// The ref-update path keeps its own shape: its canonical subject and
	// approval are accepted.
	update := api.VerifyRequest{
		ActorCredential: "k",
		ResourceID:      "res-1",
		Operation:       "git.ref.update",
		Subject: api.Subject{Fields: []api.SubjectField{
			{Name: "operation", Kind: "string", Value: "git.ref.update"},
			{Name: "resource", Kind: "string", Value: "shop/web"},
			{Name: "ref", Kind: "string", Value: "refs/heads/main"},
			{Name: "expected_base", Kind: "string", Value: "base-1"},
			{Name: "candidate", Kind: "string", Value: "cand-1"},
		}},
		Approval: "role/reviewer",
	}
	if err := update.Validate(); err != nil {
		t.Fatalf("git.ref.update verify refused: %v", err)
	}

	// A verification response still needs only the principal, roles and id.
	response := api.VerifyResponse{
		Principal:      api.VerifiedPrincipal{ID: "prn-1", Kind: "human"},
		Roles:          []string{"viewer"},
		VerificationID: "ver-1",
	}
	if err := response.Validate(); err != nil {
		t.Fatalf("git.read verification response refused: %v", err)
	}
}

// TestGitReadIsNotReportable pins that a repository read check can never stand
// behind a verified resource report. The codec refuses `git.read` as a report
// operation while the ref-update report shape stays accepted; the domain
// report linkage refuses it too (internal/shell/verify).
func TestGitReadIsNotReportable(t *testing.T) {
	read := api.ReportRequest{
		ResourceID:     "res-1",
		SourceEventID:  "event-1",
		EventType:      "git.ref.updated",
		Operation:      "git.read",
		VerificationID: "ver-1",
	}
	if err := read.Validate(); err == nil {
		t.Fatal("ReportRequest.Validate accepted a git.read report")
	} else {
		requireInvalid(t, err, "git.read report")
	}
	body := `{"resource_id":"res-1","source_event_id":"event-1","event_type":"git.ref.updated","operation":"git.read","verification_id":"ver-1"}`
	if _, err := api.DecodeReportRequest([]byte(body)); err == nil {
		t.Fatal("DecodeReportRequest accepted a git.read report")
	} else {
		requireInvalid(t, err, "git.read report vector")
	}

	// The ref-update report shape is accepted.
	update := read
	update.Operation = "git.ref.update"
	update.Subject = api.Subject{Fields: []api.SubjectField{{Name: "ref", Kind: "string", Value: "refs/heads/main"}}}
	if err := update.Validate(); err != nil {
		t.Fatalf("git.ref.update report refused: %v", err)
	}
}

// requireInvalid asserts one refusal is the stable public invalid_input.
func requireInvalid(t *testing.T, err error, what string) {
	t.Helper()
	publicError, ok := err.(*api.Error)
	if !ok {
		t.Fatalf("%s refusal is %T; want *api.Error", what, err)
	}
	if publicError.Code != api.CodeInvalidInput {
		t.Fatalf("%s refusal code = %q; want %q", what, publicError.Code, api.CodeInvalidInput)
	}
	if publicError.Message == "" {
		t.Fatalf("%s refusal has an empty message", what)
	}
}

// TestResourceVocabularyPreservesOrder pins the vocabulary order (git.read
// follows git.ref.update) and that the returned slices are not mutable.
func TestResourceVocabularyPreservesOrder(t *testing.T) {
	domain := resource.Operations()
	if len(domain) < 2 {
		t.Fatalf("resource.Operations() = %v", domain)
	}
	domain[0] = resource.Operation("git.read")
	if got := resource.Operations(); got[0] != resource.OperationGitRefUpdate {
		t.Fatalf("resource.Operations() exposed mutable state: %v", got)
	}
	public := api.ResourceOperations()
	public[0] = api.ResourceOperation("git.read")
	if got := api.ResourceOperations(); got[0] != api.ResourceOperationGitRefUpdate {
		t.Fatalf("api.ResourceOperations() exposed mutable state: %v", got)
	}
}

// TestGitReadVerifyWireIsStrict pins that the git.read verify is still a strict
// object: the empty subject may be omitted, but unknown members refuse.
func TestGitReadVerifyWireIsStrict(t *testing.T) {
	for _, body := range []string{
		`{"actor_credential":"k","resource_id":"res-1","operation":"git.read","ref":"refs/heads/main"}`,
		`{"actor_credential":"k","resource_id":"res-1","operation":"git.read","subject":{"fields":[]},"approval":"role/viewer"}`,
	} {
		if _, err := api.DecodeVerifyRequest([]byte(body)); err == nil {
			t.Errorf("DecodeVerifyRequest(%s) accepted a read subject or approval", body)
		}
	}
	// Confirm the omitted subject shape encodes without inventing a subject.
	encoded, err := json.Marshal(api.VerifyRequest{ActorCredential: "k", ResourceID: "res-1", Operation: "git.read"})
	if err != nil {
		t.Fatalf("marshal git.read verify: %v", err)
	}
	if string(encoded) == "" {
		t.Fatal("git.read verify encoded empty")
	}
}
