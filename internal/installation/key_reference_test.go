package installation

// Provider key-reference grammar at the installation boundary.
//
// Production Preflight owns the reference family and the AWS and GCP provider
// grammars. The public seam is Config.Preflight; these vectors keep the
// grammar next to the parser in this package, while
// test/contracts/adapters/kms_references_test.go proves the same corpus
// through both this seam and the keys provider constructors.

import (
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
)

// productionKeyConfig is a complete production composition with only the key
// adapter and reference varying.
func productionKeyConfig(adapter KeyAdapter, reference string) Config {
	return Config{
		Environment: EnvironmentProduction,
		State:       StatePostgres,
		Messaging:   MessagingNATS,
		Runtime:     RuntimeKubernetes,
		Keys:        adapter,
		DatabaseURL: "postgres://kodama@database:5432/kodama?sslmode=verify-full",
		NATSURL:     "tls://nat:4222",
		PublicURL:   "https://kodama.example",
		KeyRef:      reference,
	}
}

func TestAWSKeyReferencePreflight(t *testing.T) {
	accepted := []string{
		"aws-kms:arn:aws:kms:eu-west-1:123456789012:key/1234abcd-12ab-34cd-56ef-1234567890ab",
		"aws-kms:arn:aws:kms:us-east-1:111122223333:key/mrk-0123456789abcdef0123456789abcdef",
		"aws-kms:arn:aws-us-gov:kms:us-gov-west-1:111122223333:alias/kodama",
		"aws-kms:alias/kodama-kek",
		"aws-kms:alias/Kodama/Team_Kek-1",
	}
	for _, reference := range accepted {
		t.Run("accepted "+reference, func(t *testing.T) {
			if err := productionKeyConfig(KeyAWSKMS, reference).Preflight(); err != nil {
				t.Fatalf("Preflight refused %q: %v", reference, err)
			}
		})
	}

	refused := []string{
		"aws-kms:key-1",
		"aws-kms:1234abcd-12ab-34cd-56ef-1234567890ab",
		"aws-kms:arn:aws:kms:eu-west-1:123456789012:key/",
		"aws-kms:arn:aws:kms:eu-west-1:123456789012:key/1234abcd-12ab-34cd-56ef-1234567890ab/extra",
		"aws-kms:arn:aws:kms:eu-west-1:123456789012:alias/",
		"aws-kms:arn:aws:s3:eu-west-1:123456789012:key/1234abcd-12ab-34cd-56ef-1234567890ab",
		"aws-kms:arn:aws:kms:eu-west-1:12345:key/1234abcd-12ab-34cd-56ef-1234567890ab",
		"aws-kms:arn:aws:kms:eu-west-1:123456789012:key/not-a-uuid",
		"aws-kms:arn:aws:kms:EU-WEST-1:123456789012:key/1234abcd-12ab-34cd-56ef-1234567890ab",
		"aws-kms:arn:aws:kms::123456789012:key/1234abcd-12ab-34cd-56ef-1234567890ab",
		"aws-kms:arn:aws-other:kms:eu-west-1:123456789012:key/1234abcd-12ab-34cd-56ef-1234567890ab",
		"aws-kms:arn:aws:kms:eu-west-1:123456789012",
		"aws-kms:arn:aws:kms:eu-west-1:123456789012:key/1234abcd-12ab-34cd-56ef-1234567890ab?x=1",
		"aws-kms:arn:aws:kms:eu-west-1:123456789012:key/1234abcd-12ab-34cd-56ef-1234567890ab#frag",
		"aws-kms:alias/kodama kek",
		"aws-kms:alias/%2e%2e",
		"aws-kms:alias/..",
		"aws-kms:alias/alias/kodama",
		"aws-kms:",
	}
	for _, reference := range refused {
		t.Run("refused "+reference, func(t *testing.T) {
			err := productionKeyConfig(KeyAWSKMS, reference).Preflight()
			if err == nil {
				t.Fatalf("Preflight accepted the AWS reference %q", reference)
			}
			class, ok := fault.ClassOf(err)
			if !ok || class != fault.InvalidInput {
				t.Fatalf("refusal class = %v (%v); want InvalidInput", class, err)
			}
			if reference != "aws-kms:" && strings.Contains(err.Error(), reference) {
				t.Fatalf("refusal echoes the configured reference: %v", err)
			}
		})
	}
}

func TestGCPKeyReferencePreflight(t *testing.T) {
	accepted := []string{
		"gcp-kms:projects/demo-project/locations/europe-west1/keyRings/kodama/cryptoKeys/kek",
		"gcp-kms:projects/123456789012/locations/global/keyRings/kodama-ring/cryptoKeys/kek_1",
		"gcp-kms:projects/my-proj-1/locations/us/keyRings/ring-kodama/cryptoKeys/key-1",
	}
	for _, reference := range accepted {
		t.Run("accepted "+reference, func(t *testing.T) {
			if err := productionKeyConfig(KeyGCPKMS, reference).Preflight(); err != nil {
				t.Fatalf("Preflight refused %q: %v", reference, err)
			}
		})
	}

	refused := []string{
		"gcp-kms:projects/demo-project/locations/europe-west1/keyRings/kodama",
		"gcp-kms:projects/demo-project/locations/europe-west1/keyRings/kodama/cryptoKeys/",
		"gcp-kms:projects/demo-project/locations/europe-west1/keyRings/kodama/cryptoKeys/kek/cryptoKeyVersions/1",
		"gcp-kms:projects/demo-project/locations/europe-west1/keyRings/kodama/cryptoKeys/kek/extra",
		"gcp-kms:projects/demo-project/locations/europe-west1/keyRings//cryptoKeys/kek",
		"gcp-kms:/projects/demo-project/locations/europe-west1/keyRings/kodama/cryptoKeys/kek",
		"gcp-kms:projects/demo-project/locations/europe-west1/keyRings/kodama/cryptoKeys/kek/",
		"gcp-kms:projects/Demo-Project/locations/europe-west1/keyRings/kodama/cryptoKeys/kek",
		"gcp-kms:projects/demo_project/locations/europe-west1/keyRings/kodama/cryptoKeys/kek",
		"gcp-kms:projects/demo-project/locations/../keyRings/kodama/cryptoKeys/kek",
		"gcp-kms:projects/demo-project/locations/europe-west1/keyRings/kodama/cryptoKeys/%2e%2e",
		"gcp-kms:projects/demo-project/locations/europe-west1/keyRings/kodama/cryptoKeys/kek?x=1",
		"gcp-kms:projects/demo-project/locations/europe-west1/keyRings/kodama/cryptoKeys/kek#frag",
		"gcp-kms:projects/demo-project/locations/europe-west1/keyrings/kodama/cryptoKeys/kek",
		"gcp-kms:projects/demo-project/keyRings/kodama/cryptoKeys/kek",
		"gcp-kms:projects/demo-project/locations/europe-west1/cryptoKeys/kek",
		"gcp-kms:projects/demo-project/locations/europe-west1/keyRings/kodama.ring/cryptoKeys/kek",
		"gcp-kms:projects/demo-project/locations/europe-west1/keyRings/1ring/cryptoKeys/kek",
		"gcp-kms:projects/demo-project/locations/europe-west1/keyRings/kodama/cryptoKeys/ke k",
		"gcp-kms:",
	}
	for _, reference := range refused {
		t.Run("refused "+reference, func(t *testing.T) {
			err := productionKeyConfig(KeyGCPKMS, reference).Preflight()
			if err == nil {
				t.Fatalf("Preflight accepted the GCP reference %q", reference)
			}
			class, ok := fault.ClassOf(err)
			if !ok || class != fault.InvalidInput {
				t.Fatalf("refusal class = %v (%v); want InvalidInput", class, err)
			}
			if reference != "gcp-kms:" && strings.Contains(err.Error(), reference) {
				t.Fatalf("refusal echoes the configured reference: %v", err)
			}
		})
	}
}
