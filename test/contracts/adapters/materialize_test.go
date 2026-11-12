package adaptercontract

import (
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/shell/log"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/workspace"
)

// TestWorkspacePathRules proves preparation is confined to the immutable
// /kodama roots, refuses traversal and reserved names, and never writes into
// the writable workspace.
func TestWorkspacePathRules(t *testing.T) {
	valid := []string{
		"/kodama/inputs/source",
		"/kodama/outputs/build/artifact",
		"/kodama/task.json",
		"/kodama/secrets/DB_PASSWORD",
	}
	for _, path := range valid {
		if err := workspace.ValidatePath(path); err != nil {
			t.Errorf("ValidatePath(%q) = %v; want nil", path, err)
		}
	}
	invalid := []string{
		"",
		"relative/path",
		"/workspace/scratch",
		"/kodama/inputs/../escape",
		"/kodama//inputs/source",
		"/kodama/inputs/",
		"/kodama/inputs/source/",
		"/kodama/inputs/source\x00",
		"/kodama/secrets/KODAMA_TOKEN",
		"/etc/passwd",
	}
	for _, path := range invalid {
		if err := workspace.ValidatePath(path); err == nil {
			t.Errorf("ValidatePath(%q) = nil; want refusal", path)
		}
	}
	if !workspace.IsReservedPath("/kodama/secrets/KODAMA_TOKEN") {
		t.Error("KODAMA_TOKEN mount path is not reserved")
	}
	if workspace.IsReservedPath("/kodama/secrets/DB_PASSWORD") {
		t.Error("ordinary secret mount path is reserved")
	}

	inputPath, err := workspace.InputPath(id.InputKey("source"))
	if err != nil || inputPath != "/kodama/inputs/source" {
		t.Errorf("InputPath = %q, %v; want /kodama/inputs/source", inputPath, err)
	}
	if _, err := workspace.InputPath(id.InputKey("../escape")); err == nil {
		t.Error("InputPath accepted an invalid key")
	}
	outputPath, err := workspace.OutputPath(id.ActionName("build"), id.OutputKey("artifact"))
	if err != nil || outputPath != "/kodama/outputs/build/artifact" {
		t.Errorf("OutputPath = %q, %v; want /kodama/outputs/build/artifact", outputPath, err)
	}
	if _, err := workspace.OutputPath(id.ActionName("../escape"), id.OutputKey("artifact")); err == nil {
		t.Error("OutputPath accepted an invalid action name")
	}
	secretPath, err := workspace.SecretPath(id.SecretName("DB_PASSWORD"))
	if err != nil || secretPath != "/kodama/secrets/DB_PASSWORD" {
		t.Errorf("SecretPath = %q, %v; want /kodama/secrets/DB_PASSWORD", secretPath, err)
	}
	if _, err := workspace.SecretPath(id.SecretName(id.SecretTokenName)); err == nil {
		t.Error("SecretPath accepted the reserved credential name")
	}
}

// TestWorkspaceMaterializationValidation pins the all-or-nothing preparation
// input: unique confined files, unique declared secrets, a valid workspace and
// a task file only when a task snapshot exists.
func TestWorkspaceMaterializationValidation(t *testing.T) {
	valid := workspace.Materialization{
		WorkspaceID: id.WorkspaceID("ws-1"),
		Files: []workspace.File{
			{Path: "/kodama/inputs/source", Data: []byte("input"), Mode: 0o444},
			{Path: "/kodama/task.json", Data: []byte(`{"id":"task-1"}`), Mode: 0o444},
		},
		Secrets: []engine.SecretValue{
			{Name: id.SecretName("DB_PASSWORD"), Value: "secret-value"},
		},
		Credential: "kodex.exec-1.mac",
		Task:       []byte(`{"id":"task-1"}`),
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid materialization refused: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*workspace.Materialization)
	}{
		{"empty workspace", func(m *workspace.Materialization) { m.WorkspaceID = "" }},
		{"duplicate file path", func(m *workspace.Materialization) {
			m.Files = append(m.Files, workspace.File{Path: "/kodama/inputs/source", Data: []byte("again")})
		}},
		{"workspace write", func(m *workspace.Materialization) {
			m.Files = append(m.Files, workspace.File{Path: "/workspace/scratch", Data: []byte("x")})
		}},
		{"credential path in files", func(m *workspace.Materialization) {
			m.Files = append(m.Files, workspace.File{Path: "/kodama/secrets/KODAMA_TOKEN", Data: []byte("x")})
		}},
		{"symlink file mode", func(m *workspace.Materialization) {
			m.Files[0].Mode = 0o444 | os.ModeSymlink
		}},
		{"device file mode", func(m *workspace.Materialization) {
			m.Files[0].Mode = 0o444 | os.ModeDevice | os.ModeCharDevice
		}},
		{"directory prefix collision", func(m *workspace.Materialization) {
			m.Files = append(m.Files, workspace.File{Path: "/kodama/inputs/source/child", Data: []byte("x")})
		}},
		{"file and secret collision", func(m *workspace.Materialization) {
			m.Files = append(m.Files, workspace.File{Path: "/kodama/secrets/DB_PASSWORD", Data: []byte("x")})
		}},
		{"empty credential", func(m *workspace.Materialization) {
			m.Credential = ""
		}},
		{"duplicate secret name", func(m *workspace.Materialization) {
			m.Secrets = append(m.Secrets, engine.SecretValue{Name: id.SecretName("DB_PASSWORD"), Value: "other"})
		}},
		{"reserved secret name", func(m *workspace.Materialization) {
			m.Secrets = append(m.Secrets, engine.SecretValue{Name: id.SecretName(id.SecretTokenName), Value: "x"})
		}},
		{"task file without task", func(m *workspace.Materialization) {
			m.Task = nil
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			candidate := valid
			candidate.Files = append([]workspace.File(nil), valid.Files...)
			candidate.Secrets = append([]engine.SecretValue(nil), valid.Secrets...)
			tc.mutate(&candidate)
			if err := candidate.Validate(); err == nil {
				t.Fatalf("materialization %q accepted; want refusal", tc.name)
			}
		})
	}
}

// TestPreparationStartAllowed proves partial preparation never authorizes a
// start: only a complete preparation with a stable workspace does.
func TestPreparationStartAllowed(t *testing.T) {
	if (workspace.Preparation{Complete: false, WorkspaceID: "ws-1"}).StartAllowed() {
		t.Error("incomplete preparation allowed start")
	}
	if (workspace.Preparation{Complete: true}).StartAllowed() {
		t.Error("preparation without workspace allowed start")
	}
	if !(workspace.Preparation{Complete: true, WorkspaceID: "ws-1"}).StartAllowed() {
		t.Error("complete preparation with workspace refused start")
	}
}

type envelopeDocument struct {
	Suite      string `json:"suite"`
	KeyRef     string `json:"key_ref"`
	KeyVersion uint32 `json:"key_version"`
	Associated struct {
		InstallationID string `json:"installation_id"`
		TenantID       string `json:"tenant_id"`
		ProjectID      string `json:"project_id"`
		SecretID       string `json:"secret_id"`
		Name           string `json:"name"`
		KeyVersion     uint32 `json:"key_version"`
	} `json:"associated_data"`
	AssociatedDataHex string `json:"associated_data_hex"`
	Envelope          struct {
		Suite         string `json:"suite"`
		KeyRef        string `json:"key_ref"`
		KeyVersion    uint32 `json:"key_version"`
		NonceHex      string `json:"nonce_hex"`
		CiphertextHex string `json:"ciphertext_hex"`
		WrappedKeyHex string `json:"wrapped_key_hex"`
		WrapNonceHex  string `json:"wrap_nonce_hex"`
	} `json:"envelope"`
	Tamper []struct {
		Field string `json:"field"`
		Valid bool   `json:"valid"`
	} `json:"tamper"`
}

// TestSecretSuites pins the one envelope suite. A different suite is a
// contract change, not an adapter-local choice.
func TestSecretSuites(t *testing.T) {
	suites := secret.Suites()
	if len(suites) != 1 || suites[0] != secret.SuiteAES256GCM {
		t.Fatalf("suites = %v; want [%s]", suites, secret.SuiteAES256GCM)
	}
	if !secret.SuiteAES256GCM.Valid() {
		t.Error("AES-256-GCM suite is not valid")
	}
	if secret.Suite("aes-128-gcm").Valid() {
		t.Error("unknown suite is valid")
	}
}

// TestSecretEnvelopeGoldenAndTamper pins the envelope associated-data byte
// layout and the structural tamper refusals. Cryptographic authentication is
// adapter behavior; this test fixes the bytes and the envelope shape it must
// authenticate.
func TestSecretEnvelopeGoldenAndTamper(t *testing.T) {
	var document envelopeDocument
	readTestJSON(t, "envelope.json", &document)
	if document.Suite != string(secret.SuiteAES256GCM) {
		t.Fatalf("vector suite = %q; want %q", document.Suite, secret.SuiteAES256GCM)
	}

	associated := secret.AssociatedData{
		InstallationID: document.Associated.InstallationID,
		TenantID:       id.TenantID(document.Associated.TenantID),
		ProjectID:      id.ProjectID(document.Associated.ProjectID),
		SecretID:       secret.SecretID(document.Associated.SecretID),
		Name:           id.SecretName(document.Associated.Name),
		KeyVersion:     secret.KeyVersion(document.Associated.KeyVersion),
	}
	bytes, err := associated.Bytes()
	if err != nil {
		t.Fatalf("AssociatedData.Bytes: %v", err)
	}
	if got := hex.EncodeToString(bytes); got != document.AssociatedDataHex {
		t.Fatalf("associated data = %s; want %s", got, document.AssociatedDataHex)
	}

	envelope := secret.Envelope{
		Suite:      secret.Suite(document.Envelope.Suite),
		KeyRef:     secret.KeyRef(document.Envelope.KeyRef),
		KeyVersion: secret.KeyVersion(document.Envelope.KeyVersion),
		Nonce:      mustHex(t, document.Envelope.NonceHex),
		Ciphertext: mustHex(t, document.Envelope.CiphertextHex),
		WrappedKey: mustHex(t, document.Envelope.WrappedKeyHex),
		WrapNonce:  mustHex(t, document.Envelope.WrapNonceHex),
	}
	if err := envelope.Validate(); err != nil {
		t.Fatalf("golden envelope refused: %v", err)
	}
	if err := envelope.ValidateFor(associated); err != nil {
		t.Fatalf("golden envelope refused for associated data: %v", err)
	}

	mismatched := associated
	mismatched.KeyVersion++
	if err := envelope.ValidateFor(mismatched); err == nil {
		t.Fatal("envelope accepted a mismatched associated-data key version")
	}
	otherScope := associated
	otherScope.ProjectID = id.ProjectID("project-2")
	otherBytes, err := otherScope.Bytes()
	if err != nil {
		t.Fatalf("AssociatedData.Bytes(other scope): %v", err)
	}
	if hex.EncodeToString(otherBytes) == document.AssociatedDataHex {
		t.Fatal("associated data did not change with the scoped project")
	}

	for _, tamper := range document.Tamper {
		if tamper.Valid {
			t.Errorf("tamper vector %q is marked valid", tamper.Field)
			continue
		}
		candidate := envelope
		candidate.Nonce = append([]byte(nil), envelope.Nonce...)
		candidate.Ciphertext = append([]byte(nil), envelope.Ciphertext...)
		candidate.WrappedKey = append([]byte(nil), envelope.WrappedKey...)
		candidate.WrapNonce = append([]byte(nil), envelope.WrapNonce...)
		switch tamper.Field {
		case "ciphertext":
			candidate.Ciphertext = nil
		case "nonce":
			candidate.Nonce = []byte{1, 2, 3}
		case "wrap_nonce":
			candidate.WrapNonce = nil
		case "wrapped_key":
			candidate.WrappedKey = make([]byte, 32)
		case "key_version":
			candidate.KeyVersion = 0
		case "suite":
			candidate.Suite = secret.Suite("aes-128-gcm")
		case "key_ref":
			candidate.KeyRef = ""
		default:
			t.Errorf("unknown tamper field %q", tamper.Field)
			continue
		}
		if err := candidate.Validate(); err == nil {
			t.Errorf("tampered envelope %q accepted; want refusal", tamper.Field)
		}
	}
}

// TestSecretResolutionRules pins declared-name resolution inputs and the
// resolved-value scope/size rules. Project-before-tenant fallback is resolver
// behavior; this test fixes the shapes it consumes.
func TestSecretResolutionRules(t *testing.T) {
	scope := tenancy.Scope{TenantID: "tenant-1", ProjectID: "project-1"}
	request := secret.ResolveRequest{
		Scope: scope,
		Names: []id.SecretName{"DB_PASSWORD", "API_KEY"},
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("valid resolve request refused: %v", err)
	}
	invalidRequests := []secret.ResolveRequest{
		{Scope: scope},
		{Scope: scope, Names: []id.SecretName{"DB_PASSWORD", "DB_PASSWORD"}},
		{Scope: scope, Names: []id.SecretName{"db_password"}},
		{Scope: tenancy.Scope{}, Names: []id.SecretName{"DB_PASSWORD"}},
	}
	for _, candidate := range invalidRequests {
		if err := candidate.Validate(); err == nil {
			t.Errorf("resolve request %+v accepted; want refusal", candidate)
		}
	}

	resolved := secret.Resolved{
		Name:  "DB_PASSWORD",
		Value: "value",
		Scope: scope,
	}
	if err := resolved.Validate(); err != nil {
		t.Fatalf("valid resolved secret refused: %v", err)
	}
	oversized := resolved
	oversized.Value = strings.Repeat("x", secret.DefaultBounds.MaxValueBytes+1)
	if err := oversized.Validate(); err == nil {
		t.Error("resolved secret above the value bound accepted")
	}
	tenantFallback := resolved
	tenantFallback.Scope.ProjectID = ""
	if err := tenantFallback.Validate(); err != nil {
		t.Errorf("tenant-scoped fallback value refused: %v", err)
	}

	path, err := secret.MountPath(id.SecretName("DB_PASSWORD"))
	if err != nil || path != "/kodama/secrets/DB_PASSWORD" {
		t.Errorf("MountPath = %q, %v; want /kodama/secrets/DB_PASSWORD", path, err)
	}
	if _, err := secret.MountPath(id.SecretName(id.SecretTokenName)); err == nil {
		t.Error("MountPath accepted the reserved credential name")
	}
}

// TestSecretKeyWrappingRules pins the 32-byte data key, 12-byte wrap nonce and
// 48-byte wrapped-key contract shared by every key adapter.
func TestSecretKeyWrappingRules(t *testing.T) {
	wrap := secret.WrapRequest{
		KeyRef:         "kms/key-1",
		KeyVersion:     7,
		Plaintext:      make([]byte, 32),
		AssociatedData: []byte("aad"),
	}
	if err := wrap.Validate(); err != nil {
		t.Fatalf("valid wrap request refused: %v", err)
	}
	invalidWraps := []secret.WrapRequest{
		{KeyRef: "", KeyVersion: 7, Plaintext: make([]byte, 32), AssociatedData: []byte("aad")},
		{KeyRef: "kms/key-1", KeyVersion: 0, Plaintext: make([]byte, 32), AssociatedData: []byte("aad")},
		{KeyRef: "kms/key-1", KeyVersion: 7, Plaintext: make([]byte, 16), AssociatedData: []byte("aad")},
		{KeyRef: "kms/key-1", KeyVersion: 7, Plaintext: make([]byte, 32)},
	}
	for _, candidate := range invalidWraps {
		if err := candidate.Validate(); err == nil {
			t.Errorf("wrap request %+v accepted; want refusal", candidate)
		}
	}

	wrapped := secret.WrappedKey{
		KeyRef:     "kms/key-1",
		KeyVersion: 7,
		Wrapped:    make([]byte, 48),
		Nonce:      make([]byte, 12),
	}
	if err := wrapped.Validate(); err != nil {
		t.Fatalf("valid wrapped key refused: %v", err)
	}
	invalidWrapped := []secret.WrappedKey{
		{KeyRef: "", KeyVersion: 7, Wrapped: make([]byte, 48), Nonce: make([]byte, 12)},
		{KeyRef: "kms/key-1", KeyVersion: 0, Wrapped: make([]byte, 48), Nonce: make([]byte, 12)},
		{KeyRef: "kms/key-1", KeyVersion: 7, Wrapped: make([]byte, 32), Nonce: make([]byte, 12)},
		{KeyRef: "kms/key-1", KeyVersion: 7, Wrapped: make([]byte, 48), Nonce: make([]byte, 8)},
	}
	for _, candidate := range invalidWrapped {
		if err := candidate.Validate(); err == nil {
			t.Errorf("wrapped key %+v accepted; want refusal", candidate)
		}
	}
}

// TestLogRetentionAndStreamBoundary pins the retained/live log split: a bounded
// chunk, 30-day retention and the two stream identities. Live publication is a
// separate seam and cannot become the retained source.
func TestLogRetentionAndStreamBoundary(t *testing.T) {
	retention := log.DefaultRetention()
	if retention.MaxChunkBytes != 32<<10 {
		t.Errorf("log chunk bound = %d; want %d", retention.MaxChunkBytes, 32<<10)
	}
	if retention.RetainFor != 30*24*time.Hour {
		t.Errorf("log retention = %v; want 30 days", retention.RetainFor)
	}
	if err := log.DefaultBounds.CheckChunk(32 << 10); err != nil {
		t.Errorf("chunk at the bound refused: %v", err)
	}
	if err := log.DefaultBounds.CheckChunk(32<<10 + 1); err == nil {
		t.Error("chunk above the bound accepted")
	}
	streams := log.Streams()
	if len(streams) != 2 || streams[0] != log.StreamStdout || streams[1] != log.StreamStderr {
		t.Fatalf("streams = %v; want [stdout stderr]", streams)
	}
}

func mustHex(t *testing.T, value string) []byte {
	t.Helper()
	decoded, err := hex.DecodeString(value)
	if err != nil {
		t.Fatalf("decode hex %q: %v", value, err)
	}
	return decoded
}
