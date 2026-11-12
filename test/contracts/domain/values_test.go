package domaincontract

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/gateway"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/log"
	"github.com/optimiware/kodama/internal/shell/mcpproxy"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/resource"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/task"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/token"
	"github.com/optimiware/kodama/internal/shell/verify"
)

// TestAdmissionSnapshotExactBytes pins S03's snapshot rule: the hash is
// SHA-256 of the exact admitted bytes, never a canonicalized YAML hash.
func TestAdmissionSnapshotExactBytes(t *testing.T) {
	yamlA := []byte("actions:\n  - name: test\n    image: registry.example.com/test:1\n")
	yamlB := []byte("actions:\n- name: test\n  image: registry.example.com/test:1\n")
	if bytes.Equal(yamlA, yamlB) {
		t.Fatal("test fixture is not byte-distinct")
	}

	inputs := map[id.InputKey]string{"ref": "refs/heads/main"}
	images := []pipeline.PinnedImage{{Reference: "registry.example.com/test:1", Digest: "sha256:abc"}}
	secretNames := []id.SecretName{"DEPLOY_TOKEN"}
	requirements := []string{"network"}

	snapA, err := pipeline.NewSnapshot(pipeline.SnapshotInput{
		YAML:         yamlA,
		Inputs:       inputs,
		Images:       images,
		SecretNames:  secretNames,
		Requirements: requirements,
	})
	if err != nil {
		t.Fatalf("NewSnapshot refused valid YAML: %v", err)
	}
	snapB, err := pipeline.NewSnapshot(pipeline.SnapshotInput{YAML: yamlB})
	if err != nil {
		t.Fatalf("NewSnapshot refused second YAML: %v", err)
	}
	if snapA.YAMLSHA256 == snapB.YAMLSHA256 {
		t.Fatal("byte-distinct equivalent YAML produced the same hash")
	}
	wantHash := sha256.Sum256(yamlA)
	if snapA.YAMLSHA256 != wantHash {
		t.Fatalf("snapshot hash = %x; want %x", snapA.YAMLSHA256, wantHash)
	}
	if snapA.HashHex() != hex.EncodeToString(wantHash[:]) {
		t.Fatalf("HashHex = %q; want %q", snapA.HashHex(), hex.EncodeToString(wantHash[:]))
	}

	// The snapshot owns copies: a later edit to the caller's YAML, inputs,
	// images, secret names or requirements cannot mutate admitted data.
	yamlA[0] = 'X'
	inputs["ref"] = "refs/heads/other"
	images[0].Digest = "sha256:changed"
	secretNames[0] = "OTHER"
	requirements[0] = "other"
	if got := snapA.YAMLBytes(); !bytes.Equal(got, []byte("actions:\n  - name: test\n    image: registry.example.com/test:1\n")) {
		t.Fatalf("snapshot YAML was mutated through the caller slice: %q", got)
	}
	if snapA.Inputs["ref"] != "refs/heads/main" {
		t.Fatalf("snapshot inputs were mutated: %v", snapA.Inputs)
	}
	if snapA.Images[0].Digest != "sha256:abc" {
		t.Fatalf("snapshot images were mutated: %v", snapA.Images)
	}
	if snapA.SecretNames[0] != "DEPLOY_TOKEN" {
		t.Fatalf("snapshot secret names were mutated: %v", snapA.SecretNames)
	}
	if snapA.Requirements[0] != "network" {
		t.Fatalf("snapshot requirements were mutated: %v", snapA.Requirements)
	}

	// YAMLBytes hands out a copy, not the internal slice.
	got := snapA.YAMLBytes()
	got[0] = 'Y'
	if snapA.YAMLBytes()[0] == 'Y' {
		t.Fatal("YAMLBytes returned the internal slice")
	}

	if _, err := pipeline.NewSnapshot(pipeline.SnapshotInput{}); err == nil {
		t.Fatal("NewSnapshot accepted empty YAML")
	} else if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
		t.Fatalf("empty YAML class = %v; want invalid_input", class)
	}
}

func TestAdmissionInputRequiredness(t *testing.T) {
	specs := []pipeline.InputSpec{
		{Key: id.InputKey("repository"), Kind: pipeline.InputString, Required: true},
		{Key: id.InputKey("ref"), Kind: pipeline.InputString, Required: true},
		{Key: id.InputKey("retries"), Kind: pipeline.InputNumber, Required: false},
		{Key: id.InputKey("dry_run"), Kind: pipeline.InputBoolean, Required: false},
	}
	valid := map[id.InputKey]string{
		"repository": "repository-1",
		"ref":        "refs/heads/main",
		"retries":    "3",
		"dry_run":    "true",
	}
	if err := pipeline.ValidateInputs(specs, valid); err != nil {
		t.Fatalf("valid admission inputs refused: %v", err)
	}

	invalid := []struct {
		name   string
		values map[id.InputKey]string
	}{
		{"missing required", map[id.InputKey]string{"repository": "repository-1"}},
		{"extra undeclared", map[id.InputKey]string{"repository": "repository-1", "ref": "refs/heads/main", "unknown": "x"}},
		{"invalid number", map[id.InputKey]string{"repository": "repository-1", "ref": "refs/heads/main", "retries": "many"}},
		{"invalid boolean", map[id.InputKey]string{"repository": "repository-1", "ref": "refs/heads/main", "dry_run": "yes"}},
	}
	for _, tc := range invalid {
		if err := pipeline.ValidateInputs(specs, tc.values); err == nil {
			t.Errorf("%s inputs accepted", tc.name)
		} else if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
			t.Errorf("%s class = %v; want invalid_input", tc.name, class)
		}
	}
}

func TestResponseRequiredness(t *testing.T) {
	schema := request.ResponseSchema{
		Keys:     []id.OutputKey{id.OutputKey("approved"), id.OutputKey("note")},
		Required: []id.OutputKey{id.OutputKey("approved")},
	}
	if err := schema.Validate(kernel.Outputs{"approved": "true"}); err != nil {
		t.Fatalf("valid response refused: %v", err)
	}
	if err := schema.Validate(kernel.Outputs{"approved": "true", "note": "ok"}); err != nil {
		t.Fatalf("valid response with optional field refused: %v", err)
	}
	if err := schema.Validate(kernel.Outputs{"note": "ok"}); err == nil {
		t.Fatal("response missing a required field accepted")
	} else if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
		t.Fatalf("missing response class = %v; want invalid_input", class)
	}
	if err := schema.Validate(kernel.Outputs{"approved": "true", "extra": "x"}); err == nil {
		t.Fatal("response with an undeclared field accepted")
	}

	open := request.ResponseSchema{}
	if err := open.Validate(nil); err != nil {
		t.Fatalf("empty response schema refused an empty response: %v", err)
	}
	if err := open.Validate(kernel.Outputs{"anything": "x"}); err == nil {
		t.Fatal("empty response schema accepted an undeclared field")
	}
}

func TestAdmissionLimits(t *testing.T) {
	limits := pipeline.DefaultLimits
	if limits.MaxYAMLBytes <= 0 || limits.MaxDepth <= 0 || limits.MaxAliases < 0 {
		t.Fatalf("invalid default limits: %+v", limits)
	}
	if err := limits.CheckYAMLBytes(limits.MaxYAMLBytes); err != nil {
		t.Fatalf("max YAML bytes refused: %v", err)
	}
	if err := limits.CheckYAMLBytes(limits.MaxYAMLBytes + 1); err == nil {
		t.Fatal("oversized YAML accepted")
	} else if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
		t.Fatalf("oversized YAML class = %v; want invalid_input", class)
	}
	if err := limits.CheckDepth(limits.MaxDepth); err != nil {
		t.Fatalf("max depth refused: %v", err)
	}
	if err := limits.CheckDepth(limits.MaxDepth + 1); err == nil {
		t.Fatal("over-deep YAML accepted")
	}
	if err := limits.CheckAliases(limits.MaxAliases); err != nil {
		t.Fatalf("max aliases refused: %v", err)
	}
	if err := limits.CheckAliases(limits.MaxAliases + 1); err == nil {
		t.Fatal("alias expansion accepted")
	}
}

func TestAssignmentGrammar(t *testing.T) {
	valid := []struct {
		input   string
		kind    request.AssignmentKind
		role    tenancy.Role
		service service.ServiceID
	}{
		{"role/operator", request.AssignmentRole, tenancy.RoleOperator, ""},
		{"role/reviewer", request.AssignmentRole, tenancy.RoleReviewer, ""},
		{"role/admin", request.AssignmentRole, tenancy.RoleAdmin, ""},
		{"service/svc-1", request.AssignmentService, "", service.ServiceID("svc-1")},
		{"service/svc_1", request.AssignmentService, "", service.ServiceID("svc_1")},
	}
	for _, tc := range valid {
		assignment, err := request.ParseAssignment(tc.input)
		if err != nil {
			t.Errorf("ParseAssignment(%q) refused: %v", tc.input, err)
			continue
		}
		if assignment.Kind != tc.kind || assignment.Role != tc.role || assignment.ServiceID != tc.service {
			t.Errorf("ParseAssignment(%q) = %+v; want kind=%q role=%q service=%q",
				tc.input, assignment, tc.kind, tc.role, tc.service)
		}
		if !assignment.Valid() {
			t.Errorf("ParseAssignment(%q) produced an invalid assignment", tc.input)
		}
		if assignment.String() != tc.input {
			t.Errorf("Assignment.String() = %q; want %q", assignment.String(), tc.input)
		}
	}

	invalid := []string{
		"",
		"role/",
		"service/",
		"role/owner",
		"human/reviewer",
		"role/reviewer/extra",
		" role/reviewer",
		"role/reviewer ",
		"service/../escape",
		"role",
		"/reviewer",
	}
	for _, input := range invalid {
		assignment, err := request.ParseAssignment(input)
		if err == nil {
			t.Errorf("ParseAssignment(%q) accepted %+v", input, assignment)
			continue
		}
		if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
			t.Errorf("ParseAssignment(%q) class = %v; want invalid_input", input, class)
		}
	}
}

type tokenVectorFile struct {
	TenantKeyHex string        `json:"tenant_key_hex"`
	Cases        []tokenVector `json:"cases"`
}

type tokenVector struct {
	Kind        string `json:"kind"`
	TenantID    string `json:"tenant_id"`
	PrincipalID string `json:"principal_id"`
	NonceHex    string `json:"nonce_hex"`
	ExpiryUnix  int64  `json:"expiry_unix"`
	MACInputHex string `json:"mac_input_hex"`
	Token       string `json:"token"`
}

// TestTokenEncodingGoldenVectors pins the unambiguous canonical binary
// encoding and exact token strings from S05. The HMAC itself is standard
// library; the pinned part is the byte layout and base64url representation.
func TestTokenEncodingGoldenVectors(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "tokens.json"))
	if err != nil {
		t.Fatalf("read token vectors: %v", err)
	}
	var vectors tokenVectorFile
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatalf("decode token vectors: %v", err)
	}
	tenantKey, err := hex.DecodeString(vectors.TenantKeyHex)
	if err != nil || len(tenantKey) != 32 {
		t.Fatalf("tenant key = %d bytes, err=%v; want 32", len(tenantKey), err)
	}
	if len(vectors.Cases) != 2 {
		t.Fatalf("token vectors = %d; want 2", len(vectors.Cases))
	}

	for _, vector := range vectors.Cases {
		nonce, err := hex.DecodeString(vector.NonceHex)
		if err != nil {
			t.Fatalf("%s nonce: %v", vector.Kind, err)
		}
		expiry := time.Unix(vector.ExpiryUnix, 0).UTC()
		var input []byte
		var prefix string
		switch vector.Kind {
		case "execution":
			input, err = token.ExecutionMACInput(token.ExecutionClaims{
				TenantID:    id.TenantID(vector.TenantID),
				ExecutionID: id.ExecutionID(vector.PrincipalID),
				Nonce:       nonce,
				Expiry:      expiry,
			})
			prefix = token.ExecutionPrefix
		case "service":
			input, err = token.ServiceMACInput(token.ServiceClaims{
				TenantID:  id.TenantID(vector.TenantID),
				ServiceID: service.ServiceID(vector.PrincipalID),
				Nonce:     nonce,
				Expiry:    expiry,
			})
			prefix = token.ServicePrefix
		default:
			t.Fatalf("unknown token vector kind %q", vector.Kind)
		}
		if err != nil {
			t.Fatalf("%s MAC input refused: %v", vector.Kind, err)
		}
		if got := hex.EncodeToString(input); got != vector.MACInputHex {
			t.Errorf("%s MAC input = %s; want %s", vector.Kind, got, vector.MACInputHex)
		}

		mac := hmac.New(sha256.New, tenantKey)
		mac.Write(input)
		wantToken := prefix + vector.PrincipalID + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
		if wantToken != vector.Token {
			t.Errorf("%s token = %q; want %q", vector.Kind, wantToken, vector.Token)
		}
	}

	// Missing fields, short nonces and zero expiry are refused rather than
	// encoded ambiguously.
	validNonce := make([]byte, token.MinNonceBytes)
	validExpiry := time.Unix(1_700_000_000, 0).UTC()
	if _, err := token.ExecutionMACInput(token.ExecutionClaims{}); err == nil {
		t.Fatal("empty execution claims accepted")
	}
	if _, err := token.ExecutionMACInput(token.ExecutionClaims{
		TenantID:    id.TenantID("tenant-1"),
		ExecutionID: id.ExecutionID("exec-1"),
		Nonce:       make([]byte, token.MinNonceBytes-1),
		Expiry:      validExpiry,
	}); err == nil {
		t.Fatal("short execution nonce accepted")
	}
	if _, err := token.ServiceMACInput(token.ServiceClaims{
		TenantID:  id.TenantID("tenant-1"),
		ServiceID: service.ServiceID("service-1"),
		Nonce:     validNonce,
	}); err == nil {
		t.Fatal("service claims with zero expiry accepted")
	}

	bounds := token.DefaultBounds
	if bounds.MinNonceBytes != 32 {
		t.Fatalf("MinNonceBytes = %d; want 32", bounds.MinNonceBytes)
	}
	if !bounds.ValidTTL(bounds.MaxTTL) || bounds.ValidTTL(bounds.MaxTTL+time.Nanosecond) {
		t.Fatalf("MaxTTL boundary is not exclusive: %+v", bounds)
	}
	if !bounds.ValidTTL(bounds.MinTTL) || bounds.ValidTTL(bounds.MinTTL-time.Nanosecond) {
		t.Fatalf("MinTTL boundary is not inclusive: %+v", bounds)
	}
	if bounds.MaxTTL > 8*time.Hour {
		t.Fatalf("MaxTTL = %s; S05 caps execution credentials at 8 hours", bounds.MaxTTL)
	}
}

func TestTokenCredentialState(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	if got := (token.Credential{}).State(now); got != token.StateAbsent {
		t.Fatalf("empty credential state = %q; want absent", got)
	}
	current := token.Credential{Nonce: make([]byte, token.MinNonceBytes), Expiry: now.Add(time.Hour)}
	if got := current.State(now); got != token.StateCurrent {
		t.Fatalf("live credential state = %q; want current", got)
	}
	expired := token.Credential{Nonce: make([]byte, token.MinNonceBytes), Expiry: now}
	if got := expired.State(now); got != token.StateExpired {
		t.Fatalf("expired credential state = %q; want expired", got)
	}
	for _, state := range []token.State{token.StateAbsent, token.StateCurrent, token.StateExpired} {
		if !state.Valid() {
			t.Errorf("token state %q reported invalid", state)
		}
	}
	if token.State("revoked").Valid() {
		t.Fatal("unknown token state reported valid")
	}
}

func TestIdempotencyContentRules(t *testing.T) {
	jsonOrdered := receipt.Content{MediaType: "application/json", Bytes: []byte(`{"b":2,"a":1}`)}
	jsonReordered := receipt.Content{MediaType: "application/json", Bytes: []byte(`{ "a": 1, "b": 2 }`)}
	first, err := receipt.Fingerprint(jsonOrdered)
	if err != nil {
		t.Fatalf("Fingerprint refused JSON: %v", err)
	}
	second, err := receipt.Fingerprint(jsonReordered)
	if err != nil {
		t.Fatalf("Fingerprint refused reordered JSON: %v", err)
	}
	if first != second {
		t.Fatal("JSON key order/whitespace changed the content fingerprint")
	}

	distinct := []struct {
		name string
		a    receipt.Content
		b    receipt.Content
	}{
		{
			name: "absent vs null",
			a:    receipt.Content{MediaType: "application/json", Bytes: []byte(`{"a":1}`)},
			b:    receipt.Content{MediaType: "application/json", Bytes: []byte(`{"a":1,"b":null}`)},
		},
		{
			name: "integer vs float literal",
			a:    receipt.Content{MediaType: "application/json", Bytes: []byte(`{"a":1}`)},
			b:    receipt.Content{MediaType: "application/json", Bytes: []byte(`{"a":1.0}`)},
		},
		{
			name: "binary whitespace",
			a:    receipt.Content{MediaType: "application/octet-stream", Bytes: []byte("a b")},
			b:    receipt.Content{MediaType: "application/octet-stream", Bytes: []byte("a  b")},
		},
	}
	for _, tc := range distinct {
		left, err := receipt.Fingerprint(tc.a)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		right, err := receipt.Fingerprint(tc.b)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if left == right {
			t.Errorf("%s: distinct content produced the same fingerprint", tc.name)
		}
	}

	if _, err := receipt.Normalize(receipt.Content{MediaType: "application/json", Bytes: []byte(`{`)}); err == nil {
		t.Fatal("invalid JSON accepted")
	} else if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
		t.Fatalf("invalid JSON class = %v; want invalid_input", class)
	}

	key := receipt.Key{
		TenantID:    id.TenantID("tenant-1"),
		ProjectID:   id.ProjectID("project-1"),
		PrincipalID: id.PrincipalID("principal-1"),
		Operation:   "task.start",
		CallerKey:   "caller-key-1",
	}
	record := receipt.Record{
		ID:          id.ReceiptID("receipt-1"),
		Key:         key,
		ContentHash: first,
		Result:      []byte(`{"execution_id":"exec-1"}`),
		ExecutionID: id.ExecutionID("exec-1"),
	}
	if record.Key.TenantID != "tenant-1" || record.Key.ProjectID != "project-1" ||
		record.Key.PrincipalID != "principal-1" || record.Key.Operation != "task.start" ||
		record.Key.CallerKey != "caller-key-1" {
		t.Fatalf("receipt identity lost scope or caller key: %+v", record.Key)
	}
	if record.ExecutionID != "exec-1" {
		t.Fatalf("receipt did not retain the committed execution id: %+v", record)
	}
}

func TestVerificationSubjectMatching(t *testing.T) {
	required := verify.Subject{Fields: []verify.SubjectField{
		{Name: "ref", Kind: verify.KindString, Value: "refs/heads/main"},
		{Name: "candidate", Kind: verify.KindString, Value: "abc123"},
	}}
	// Display context is exactly the recorded fields the enforcing service did
	// not supply as required: no flag is needed and the extra fields are
	// ignored even when their values or names differ.
	recorded := verify.Subject{Fields: []verify.SubjectField{
		{Name: "image", Kind: verify.KindString, Value: "registry/app:latest"},
		{Name: "candidate", Kind: verify.KindString, Value: "abc123"},
		{Name: "ref", Kind: verify.KindString, Value: "refs/heads/main"},
	}}
	if !verify.SubjectMatches(required, recorded) {
		t.Fatal("matching subject with display context was refused")
	}

	otherDisplay := verify.Subject{Fields: []verify.SubjectField{
		{Name: "ref", Kind: verify.KindString, Value: "refs/heads/main"},
		{Name: "candidate", Kind: verify.KindString, Value: "abc123"},
		{Name: "ui_hint", Kind: verify.KindString, Value: "different"},
		{Name: "note", Kind: verify.KindNumber, Value: "42"},
	}}
	if !verify.SubjectMatches(required, otherDisplay) {
		t.Fatal("different display context changed verification matching")
	}

	// Typed equality: "1" as a string is not the number 1.
	typeMismatch := verify.Subject{Fields: []verify.SubjectField{
		{Name: "ref", Kind: verify.KindString, Value: "refs/heads/main"},
		{Name: "candidate", Kind: verify.KindNumber, Value: "abc123"},
	}}
	if verify.SubjectMatches(required, typeMismatch) {
		t.Fatal("a different value kind matched")
	}

	// A required field is never ignorable: the recorded field with the same
	// name must carry the same value.
	valueMismatch := verify.Subject{Fields: []verify.SubjectField{
		{Name: "ref", Kind: verify.KindString, Value: "refs/heads/main"},
		{Name: "candidate", Kind: verify.KindString, Value: "def456"},
		{Name: "image", Kind: verify.KindString, Value: "registry/app:latest"},
	}}
	if verify.SubjectMatches(required, valueMismatch) {
		t.Fatal("a required field with a different value matched")
	}

	// A missing required field refuses.
	missing := verify.Subject{Fields: []verify.SubjectField{
		{Name: "ref", Kind: verify.KindString, Value: "refs/heads/main"},
		{Name: "image", Kind: verify.KindString, Value: "registry/app:latest"},
	}}
	if verify.SubjectMatches(required, missing) {
		t.Fatal("a missing required field matched")
	}

	// Ambiguous duplicate names refuse in the required and recorded subjects.
	duplicateRequired := verify.Subject{Fields: []verify.SubjectField{
		{Name: "ref", Kind: verify.KindString, Value: "refs/heads/main"},
		{Name: "ref", Kind: verify.KindString, Value: "refs/heads/main"},
		{Name: "candidate", Kind: verify.KindString, Value: "abc123"},
	}}
	if verify.SubjectMatches(duplicateRequired, recorded) {
		t.Fatal("a duplicate required name matched")
	}
	duplicateRecorded := verify.Subject{Fields: []verify.SubjectField{
		{Name: "ref", Kind: verify.KindString, Value: "refs/heads/main"},
		{Name: "candidate", Kind: verify.KindString, Value: "abc123"},
		{Name: "image", Kind: verify.KindString, Value: "one"},
		{Name: "image", Kind: verify.KindString, Value: "two"},
	}}
	if verify.SubjectMatches(required, duplicateRecorded) {
		t.Fatal("a duplicate recorded name matched")
	}

	// Unknown kinds refuse even on otherwise ignored display fields.
	unknownKind := verify.Subject{Fields: []verify.SubjectField{
		{Name: "ref", Kind: verify.KindString, Value: "refs/heads/main"},
		{Name: "candidate", Kind: verify.KindString, Value: "abc123"},
		{Name: "extra", Kind: verify.ValueKind("object"), Value: "{}"},
	}}
	if verify.SubjectMatches(required, unknownKind) {
		t.Fatal("an unknown recorded kind matched")
	}

	// Empty required subjects match only an empty recorded subject; they must
	// not be a wildcard.
	if !verify.SubjectMatches(verify.Subject{}, verify.Subject{}) {
		t.Fatal("two empty subjects did not match")
	}
	if verify.SubjectMatches(verify.Subject{}, recorded) {
		t.Fatal("empty required subject acted as a wildcard")
	}

	if _, ok := required.Field("ref"); !ok {
		t.Fatal("Subject.Field did not find a present field")
	}
	if _, ok := required.Field("missing"); ok {
		t.Fatal("Subject.Field found a missing field")
	}

	record := verify.Record{
		ID:                verify.VerificationID("verification-1"),
		CallingService:    service.ServiceID("service-1"),
		Actor:             identityPrincipal(),
		ExecutionID:       id.ExecutionID("exec-1"),
		ActionID:          id.ActionID("action-1"),
		ResourceID:        resourceID(),
		Operation:         "git.ref.update",
		Subject:           required,
		RequestedApproval: mustAssignment(t, "role/reviewer"),
		Result:            verify.ResultApproved,
		Approved:          true,
		At:                time.Unix(1_700_000_000, 0).UTC(),
	}
	if !record.Approved || record.Result != verify.ResultApproved {
		t.Fatalf("verification record lost approval result: %+v", record)
	}
	// The forwarded actor credential is never part of the durable record.
	recordType := reflect.TypeOf(record)
	for index := 0; index < recordType.NumField(); index++ {
		name := recordType.Field(index).Name
		if name == "Credential" || name == "Token" || name == "ActorCredential" {
			t.Fatalf("verification record exposes credential field %q", name)
		}
	}
}

func TestEventEnvelopeAndAuditBounds(t *testing.T) {
	if got := event.Types(); len(got) == 0 {
		t.Fatal("event.Types() is empty")
	}
	required := []event.Type{
		event.TypeTaskCreated,
		event.TypeTaskQuestion,
		event.TypeTaskReply,
		event.TypeTaskCancelled,
		event.TypeExecutionQueued,
		event.TypeExecutionStarted,
		event.TypeExecutionSucceeded,
		event.TypeExecutionFailed,
		event.TypeExecutionCancelled,
		event.TypeActionTransition,
		event.TypeRequestActivated,
		event.TypeActionRequested,
		event.TypeRequestResolved,
		event.TypeConfigChanged,
		event.TypeServiceChanged,
		event.TypeVerifiedReport,
		event.TypeCustomObservation,
		event.TypeCleanup,
		event.TypePipelineStartRefused,
	}
	for _, want := range required {
		if !want.Valid() {
			t.Errorf("required event type %q is not valid", want)
		}
	}
	for _, typ := range event.Types() {
		if !typ.Valid() {
			t.Errorf("event.Types() returned invalid type %q", typ)
		}
	}
	if event.Type("execution.teleported").Valid() {
		t.Fatal("unknown event type reported valid")
	}

	if got := event.SourceKinds(); !reflect.DeepEqual(got, []event.SourceKind{event.SourceShell, event.SourceService, event.SourceExecution}) {
		t.Fatalf("SourceKinds() = %v", got)
	}
	for _, kind := range event.SourceKinds() {
		if !kind.Valid() {
			t.Errorf("source kind %q invalid", kind)
		}
	}

	envelope := event.Envelope{
		ID:             id.EventID("event-1"),
		Type:           event.TypeTaskCreated,
		Source:         event.Source{Kind: event.SourceShell, PrincipalID: id.PrincipalID("human-1")},
		At:             time.Unix(1_700_000_000, 0).UTC(),
		Scope:          tenancy.Scope{TenantID: id.TenantID("tenant-1"), ProjectID: id.ProjectID("project-1")},
		SubjectID:      "task-1",
		SubjectVersion: 1,
		Data:           make([]byte, event.DefaultBounds.MaxDataBytes),
	}
	if err := envelope.Validate(); err != nil {
		t.Fatalf("valid envelope refused: %v", err)
	}
	oversized := envelope
	oversized.Data = make([]byte, event.DefaultBounds.MaxDataBytes+1)
	if err := oversized.Validate(); err == nil {
		t.Fatal("oversized event data accepted")
	} else if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
		t.Fatalf("oversized event class = %v; want invalid_input", class)
	}
	unknownType := envelope
	unknownType.Type = event.Type("execution.teleported")
	if err := unknownType.Validate(); err == nil {
		t.Fatal("unknown event type accepted by Envelope.Validate")
	}

	// A binding-declared report type is not a lifecycle type. A verified report
	// carries it in the dedicated trusted field under resource.verified, while a
	// custom observation can never set that field.
	if event.Type("git.ref.updated").Valid() {
		t.Fatal("a binding-declared report type became a lifecycle event type")
	}
	reportEnvelope := envelope
	reportEnvelope.Type = event.TypeVerifiedReport
	reportEnvelope.ReportType = event.ReportType("git.ref.updated")
	if err := reportEnvelope.Validate(); err != nil {
		t.Fatalf("verified report envelope refused: %v", err)
	}
	if reportEnvelope.ReportType != event.ReportType("git.ref.updated") {
		t.Fatalf("verified report envelope lost its declared type: %+v", reportEnvelope)
	}
	missingReportType := envelope
	missingReportType.Type = event.TypeVerifiedReport
	if err := missingReportType.Validate(); err == nil {
		t.Fatal("verified report envelope without a declared type accepted")
	}
	customForge := envelope
	customForge.Type = event.TypeCustomObservation
	customForge.ReportType = event.ReportType("git.ref.updated")
	if err := customForge.Validate(); err == nil {
		t.Fatal("custom observation accepted a trusted report type")
	}
	badReportType := reportEnvelope
	badReportType.ReportType = event.ReportType("Git Ref Updated")
	if err := badReportType.Validate(); err == nil {
		t.Fatal("invalid declared report type accepted")
	}

	audit := event.Audit{
		ID:        id.EventID("audit-1"),
		Scope:     envelope.Scope,
		Actor:     id.PrincipalID("human-1"),
		Operation: "task.start",
		At:        envelope.At,
		Detail:    "started",
	}
	if audit.Operation != "task.start" || audit.Scope.TenantID != "tenant-1" {
		t.Fatalf("audit lost operation or scope: %+v", audit)
	}
}

func TestSecretLogAndSurfaceBounds(t *testing.T) {
	if secret.DefaultBounds.CheckValue(secret.DefaultBounds.MaxValueBytes) != nil {
		t.Fatal("max secret value refused")
	}
	if err := secret.DefaultBounds.CheckValue(secret.DefaultBounds.MaxValueBytes + 1); err == nil {
		t.Fatal("oversized secret value accepted")
	} else if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
		t.Fatalf("oversized secret class = %v; want invalid_input", class)
	}
	metadata := secret.Metadata{
		ID:        secret.SecretID("secret-1"),
		Scope:     tenancy.Scope{TenantID: id.TenantID("tenant-1"), ProjectID: id.ProjectID("project-1")},
		Name:      id.SecretName("DEPLOY_TOKEN"),
		Version:   2,
		UpdatedAt: time.Unix(1_700_000_000, 0).UTC(),
	}
	if metadata.Name != "DEPLOY_TOKEN" || metadata.Version != 2 {
		t.Fatalf("secret metadata lost fields: %+v", metadata)
	}

	if log.DefaultBounds.CheckChunk(log.DefaultBounds.MaxChunkBytes) != nil {
		t.Fatal("max log chunk refused")
	}
	if err := log.DefaultBounds.CheckChunk(log.DefaultBounds.MaxChunkBytes + 1); err == nil {
		t.Fatal("oversized log chunk accepted")
	}
	chunk := log.Chunk{
		ExecutionID: id.ExecutionID("exec-1"),
		ActionID:    id.ActionID("action-1"),
		Stream:      log.StreamStdout,
		Offset:      10,
		Sequence:    3,
		Data:        []byte("line\n"),
	}
	if chunk.Stream != log.StreamStdout || chunk.Offset != 10 {
		t.Fatalf("log chunk lost fields: %+v", chunk)
	}

	modelSurface := gateway.Surface{Operation: "model.invoke", AllowedModels: []string{"small-model"}, MaxRequestBytes: 1024}
	if modelSurface.Operation != "model.invoke" || len(modelSurface.AllowedModels) != 1 {
		t.Fatalf("model surface lost fields: %+v", modelSurface)
	}
	if gateway.DefaultBounds.MaxRequestBytes <= 0 || gateway.DefaultBounds.MaxResponseBytes <= 0 {
		t.Fatalf("invalid model gateway bounds: %+v", gateway.DefaultBounds)
	}
	toolSurface := mcpproxy.Surface{Operation: "tool.invoke", AllowedTools: []string{"read-file"}, MaxRequestBytes: 1024}
	if toolSurface.Operation != "tool.invoke" || len(toolSurface.AllowedTools) != 1 {
		t.Fatalf("tool surface lost fields: %+v", toolSurface)
	}
	if mcpproxy.DefaultBounds.MaxRequestBytes <= 0 || mcpproxy.DefaultBounds.MaxResponseBytes <= 0 {
		t.Fatalf("invalid MCP proxy bounds: %+v", mcpproxy.DefaultBounds)
	}
}

// TestTaskContentAndLifecycleVersions pins S04/S06's separation: content edits
// advance ContentVersion only; lifecycle operations advance LifecycleVersion.
func TestTaskContentAndLifecycleVersions(t *testing.T) {
	value := task.Task{
		ID:               id.TaskID("task-1"),
		Scope:            tenancy.Scope{TenantID: id.TenantID("tenant-1"), ProjectID: id.ProjectID("project-1")},
		ContentVersion:   3,
		LifecycleVersion: 7,
		State:            task.StateOpen,
		SelectedPipeline: pipeline.PipelineID("pipeline-1"),
		Inputs:           map[id.InputKey]string{"ref": "refs/heads/main"},
		Initiator:        id.PrincipalID("human-1"),
		CurrentExecution: id.ExecutionID("exec-1"),
	}
	if value.ContentVersion != 3 || value.LifecycleVersion != 7 {
		t.Fatalf("task versions = %d/%d; want 3/7", value.ContentVersion, value.LifecycleVersion)
	}
	if value.State != task.StateOpen {
		t.Fatalf("task state = %q; want open", value.State)
	}
}

func identityPrincipal() identity.Principal {
	return identity.Principal{
		ID:      id.PrincipalID("human-1"),
		Kind:    identity.KindHuman,
		Scope:   tenancy.Scope{TenantID: id.TenantID("tenant-1"), ProjectID: id.ProjectID("project-1")},
		Roles:   []tenancy.Role{tenancy.RoleReviewer},
		Enabled: true,
		Version: 1,
	}
}

func resourceID() resource.ResourceID {
	return resource.ResourceID("resource-1")
}

func mustAssignment(t *testing.T, value string) request.Assignment {
	t.Helper()
	assignment, err := request.ParseAssignment(value)
	if err != nil {
		t.Fatalf("ParseAssignment(%q): %v", value, err)
	}
	return assignment
}
