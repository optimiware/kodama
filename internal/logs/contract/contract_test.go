package contract_test

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/logs/contract"
)

type fixtureFile struct {
	Cases []struct {
		Name     string `json:"name"`
		Artifact struct {
			ID        domain.ArtifactID `json:"id"`
			Digest    string            `json:"digest"`
			SizeBytes int64             `json:"sizeBytes"`
		} `json:"artifact"`
		Metadata     json.RawMessage `json:"metadata"`
		Decode       string          `json:"decode"`
		Bind         string          `json:"bind"`
		Completeness string          `json:"completeness"`
	} `json:"cases"`
}

func loadFixtures(t *testing.T) fixtureFile {
	t.Helper()
	raw, err := os.ReadFile("../testdata/fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var f fixtureFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

// TestLogContractFixtures freezes the sidecar contract and the compatibility of
// the existing plain-text log artifacts with the runtime wire contract.
func TestLogContractFixtures(t *testing.T) {
	f := loadFixtures(t)
	if len(f.Cases) == 0 {
		t.Fatal("no fixtures")
	}
	for _, c := range f.Cases {
		t.Run(c.Name, func(t *testing.T) {
			if string(c.Metadata) == "null" {
				got, reasons := contract.Unverified()
				if string(got) != c.Completeness || got == contract.Complete || len(reasons) == 0 {
					t.Fatalf("a log without a sidecar must be unknown, not %q", got)
				}
				return
			}
			m, err := contract.Decode(c.Metadata)
			switch c.Decode {
			case "invalid":
				if !errors.Is(err, contract.ErrInvalid) {
					t.Fatalf("decode = %v, want ErrInvalid", err)
				}
				return
			case "ok":
				if err != nil {
					t.Fatalf("decode: %v", err)
				}
			default:
				t.Fatalf("bad fixture decode %q", c.Decode)
			}
			berr := m.Bind(c.Artifact.ID, c.Artifact.Digest, c.Artifact.SizeBytes)
			switch c.Bind {
			case "ok":
				if berr != nil {
					t.Fatalf("bind: %v", berr)
				}
				if string(m.Completeness) != c.Completeness {
					t.Fatalf("completeness = %q, want %q", m.Completeness, c.Completeness)
				}
			case "binding":
				if !errors.Is(berr, contract.ErrBinding) {
					t.Fatalf("bind = %v, want ErrBinding", berr)
				}
			default:
				t.Fatalf("bad fixture bind %q", c.Bind)
			}
			// Round trip: the canonical encoding decodes to the same statement.
			enc, err := contract.Encode(m)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			again, err := contract.Decode(enc)
			if err != nil {
				t.Fatalf("decode canonical: %v", err)
			}
			enc2, _ := contract.Encode(again)
			if string(enc) != string(enc2) {
				t.Fatalf("encoding is not deterministic")
			}
		})
	}
}

func TestDuplicateLinkageIsOneStatement(t *testing.T) {
	f := loadFixtures(t)
	var raw json.RawMessage
	for _, c := range f.Cases {
		if c.Name == "duplicate_linkage" {
			raw = c.Metadata
		}
	}
	a, err := contract.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := contract.Decode(raw)
	ea, _ := contract.Encode(a)
	eb, _ := contract.Encode(b)
	if string(ea) != string(eb) {
		t.Fatal("the same statement observed twice must be byte-identical")
	}
}

func TestDecodeBounds(t *testing.T) {
	if _, err := contract.Decode([]byte(strings.Repeat(" ", contract.MaxMetadataBytes+1))); !errors.Is(err, contract.ErrInvalid) {
		t.Fatalf("oversized document: %v", err)
	}
	for _, bad := range []string{``, `[]`, `{"version":1} {"version":1}`, `{"version":1}extra`, `null`} {
		if _, err := contract.Decode([]byte(bad)); !errors.Is(err, contract.ErrInvalid) {
			t.Fatalf("Decode(%q) = %v, want ErrInvalid", bad, err)
		}
	}
}

func TestNames(t *testing.T) {
	n, err := contract.MetadataName(contract.CommandLogName)
	if err != nil || n != "command.log.meta.json" || !contract.ValidName(n) {
		t.Fatalf("MetadataName = %q, %v", n, err)
	}
	if _, err := contract.MetadataName(strings.Repeat("a", 125)); err == nil {
		t.Fatal("a sidecar name beyond the safe-name bound must be refused")
	}
	if contract.ControllerLogName(2) == contract.CommandLogName || contract.ControllerLogName(2) == contract.EntrypointLogName {
		t.Fatal("controller captures must never collide with wrapper artifact names")
	}
}

// The unchanged runtime contract admits the plain-text logs and the sidecar as
// ordinary diagnostic files: no new route, action, role or field is needed.
func TestRuntimeCompatibility(t *testing.T) {
	size := int64(120)
	text, js := contract.LogMediaType, contract.MediaType
	digest := runtimev1.Digest("sha256:" + strings.Repeat("0f", 32))
	for _, req := range []runtimev1.ArtifactReserveRequest{
		{Name: contract.CommandLogName, Kind: runtimev1.KindFile, Role: runtimev1.RoleDiagnostic, Digest: digest, MediaType: &text, SizeBytes: &size},
		{Name: contract.EntrypointLogName, Kind: runtimev1.KindFile, Role: runtimev1.RoleDiagnostic, Digest: digest, MediaType: &text, SizeBytes: &size},
		{Name: "command.log" + contract.MetadataSuffix, Kind: runtimev1.KindFile, Role: runtimev1.RoleDiagnostic, Digest: digest, MediaType: &js, SizeBytes: &size},
		{Name: contract.ControllerLogName(1), Kind: runtimev1.KindFile, Role: runtimev1.RoleDiagnostic, Digest: digest, MediaType: &text, SizeBytes: &size},
	} {
		if err := req.Validate(); err != nil {
			t.Fatalf("%s: the frozen reserve request must accept it: %v", req.Name, err)
		}
	}
	res := runtimev1.Result{Outcome: runtimev1.OutcomeFailed, Outputs: map[string]runtimev1.Value{},
		DiagnosticArtifactIDs: []domain.ArtifactID{"art_wrapper01", "art_wrapper02"}}
	code := int64(1)
	reason := "command_failed"
	res.ExitCode, res.Reason = &code, &reason
	if err := res.Validate(); err != nil {
		t.Fatalf("a result listing log and sidecar as diagnostics must stay valid: %v", err)
	}
}
