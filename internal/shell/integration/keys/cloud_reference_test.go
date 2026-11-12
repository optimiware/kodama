package keys_test

// Concrete AWS/GCP key-reference parsing.
//
// The keys package is the only place a cloud provider reference becomes a
// request identity. These vectors pin the parsed fields the KMS clients need,
// and the AWS configured-region agreement rule that must fail before any
// credential is acquired or any request is built.

import (
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/integration/keys"
	"github.com/optimiware/kodama/internal/shell/secret"
)

func requireInvalidReference(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("malformed key reference was accepted")
	}
	class, ok := fault.ClassOf(err)
	if !ok || class != fault.InvalidInput {
		t.Fatalf("refusal class = %v (%v); want InvalidInput", class, err)
	}
}

func TestAWSKeyReferenceParsing(t *testing.T) {
	t.Run("arn key", func(t *testing.T) {
		parsed, err := keys.ParseAWSReference(secret.KeyRef("aws-kms:arn:aws:kms:eu-west-1:123456789012:key/1234abcd-12ab-34cd-56ef-1234567890ab"))
		if err != nil {
			t.Fatalf("ParseAWSReference: %v", err)
		}
		if !parsed.ARN || parsed.Partition != "aws" || parsed.Region != "eu-west-1" ||
			parsed.Account != "123456789012" || parsed.KeyID != "1234abcd-12ab-34cd-56ef-1234567890ab" || parsed.Alias != "" {
			t.Fatalf("parsed key reference = %+v", parsed)
		}
	})
	t.Run("arn multi-region key", func(t *testing.T) {
		parsed, err := keys.ParseAWSReference(secret.KeyRef("aws-kms:arn:aws-cn:kms:cn-north-1:111122223333:key/mrk-0123456789abcdef0123456789abcdef"))
		if err != nil {
			t.Fatalf("ParseAWSReference: %v", err)
		}
		if parsed.Partition != "aws-cn" || parsed.KeyID != "mrk-0123456789abcdef0123456789abcdef" {
			t.Fatalf("parsed multi-region key reference = %+v", parsed)
		}
	})
	t.Run("arn alias", func(t *testing.T) {
		parsed, err := keys.ParseAWSReference(secret.KeyRef("aws-kms:arn:aws:kms:ap-southeast-2:210987654321:alias/kodama-kek"))
		if err != nil {
			t.Fatalf("ParseAWSReference: %v", err)
		}
		if !parsed.ARN || parsed.Region != "ap-southeast-2" || parsed.Account != "210987654321" || parsed.Alias != "kodama-kek" || parsed.KeyID != "" {
			t.Fatalf("parsed alias reference = %+v", parsed)
		}
	})
	t.Run("configured-region alias", func(t *testing.T) {
		parsed, err := keys.ParseAWSReference(secret.KeyRef("aws-kms:alias/kodama-kek"))
		if err != nil {
			t.Fatalf("ParseAWSReference: %v", err)
		}
		if parsed.ARN || parsed.Region != "" || parsed.Alias != "kodama-kek" {
			t.Fatalf("parsed short alias reference = %+v", parsed)
		}
	})
	for _, reference := range []string{
		"aws-kms:key-1",
		"aws-kms:arn:aws:s3:eu-west-1:123456789012:key/1234abcd-12ab-34cd-56ef-1234567890ab",
		"aws-kms:arn:aws:kms:eu-west-1:12345:key/1234abcd-12ab-34cd-56ef-1234567890ab",
		"aws-kms:arn:aws:kms:eu-west-1:123456789012:key/1234abcd-12ab-34cd-56ef-1234567890ab/extra",
		"aws-kms:alias/..",
	} {
		t.Run("refused "+reference, func(t *testing.T) {
			_, err := keys.ParseAWSReference(secret.KeyRef(reference))
			requireInvalidReference(t, err)
		})
	}
}

func TestAWSReferenceConfiguredRegion(t *testing.T) {
	arn, err := keys.ParseAWSReference(secret.KeyRef("aws-kms:arn:aws:kms:eu-west-1:123456789012:key/1234abcd-12ab-34cd-56ef-1234567890ab"))
	if err != nil {
		t.Fatalf("ParseAWSReference: %v", err)
	}
	if err := arn.ValidateRegion("eu-west-1"); err != nil {
		t.Fatalf("matching configured region was refused: %v", err)
	}
	for _, configured := range []string{"", "us-east-1", "EU-WEST-1", "eu-west-1 ", "eu west 1"} {
		if err := arn.ValidateRegion(configured); err == nil {
			t.Fatalf("ARN region %q accepted configured region %q", arn.Region, configured)
		} else {
			requireInvalidReference(t, err)
		}
	}

	alias, err := keys.ParseAWSReference(secret.KeyRef("aws-kms:alias/kodama-kek"))
	if err != nil {
		t.Fatalf("ParseAWSReference: %v", err)
	}
	if err := alias.ValidateRegion("eu-west-1"); err != nil {
		t.Fatalf("configured region for the alias form was refused: %v", err)
	}
	if err := alias.ValidateRegion(""); err == nil {
		t.Fatal("alias form accepted an empty configured region")
	} else {
		requireInvalidReference(t, err)
	}
}

func TestGCPKeyReferenceParsing(t *testing.T) {
	parsed, err := keys.ParseGCPReference(secret.KeyRef("gcp-kms:projects/demo-project/locations/europe-west1/keyRings/kodama/cryptoKeys/kek"))
	if err != nil {
		t.Fatalf("ParseGCPReference: %v", err)
	}
	if parsed.Project != "demo-project" || parsed.Location != "europe-west1" ||
		parsed.KeyRing != "kodama" || parsed.CryptoKey != "kek" {
		t.Fatalf("parsed GCP reference = %+v", parsed)
	}

	for _, reference := range []string{
		"gcp-kms:projects/demo-project/locations/europe-west1/keyRings/kodama",
		"gcp-kms:projects/demo-project/locations/europe-west1/keyRings/kodama/cryptoKeys/kek/cryptoKeyVersions/1",
		"gcp-kms:projects/demo-project/locations/../keyRings/kodama/cryptoKeys/kek",
		"gcp-kms:projects/demo-project/locations/europe-west1/keyrings/kodama/cryptoKeys/kek",
	} {
		t.Run("refused "+reference, func(t *testing.T) {
			_, err := keys.ParseGCPReference(secret.KeyRef(reference))
			requireInvalidReference(t, err)
		})
	}
}
