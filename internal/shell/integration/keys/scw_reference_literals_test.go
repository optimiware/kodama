package keys_test

// Scaleway key-reference literal vectors.
//
// The other cloud-provider tests build every key reference from
// keys.ScalewayKeyRefPrefix, so a change that moved the constant and its
// callers together would pass unnoticed. These vectors spell the accepted,
// "scaleway-kms:" and foreign references as literals and assert the refusal
// class the constructor returns.
//
// The literals below are deliberately not derived from keys.ScalewayKeyRefPrefix
// on the accepted side: the point of the vector is to catch a changed constant,
// so it must fail when the literal bytes move.

import (
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/integration/keys"
	"github.com/optimiware/kodama/internal/shell/secret"
)

const (
	// scwKeyRefLiteral is the exact Scaleway key reference.
	scwKeyRefLiteral = "scw-kms:key-id"
	// scwUnsupportedKeyRefLiteral spells the family as "scaleway-kms:"; it must
	// not parse.
	scwUnsupportedKeyRefLiteral = "scaleway-kms:key-id"
	// scwAWSKeyRefLiteral and scwGCPKeyRefLiteral are other provider families;
	// NewScaleway must refuse them rather than accept any reference.
	scwAWSKeyRefLiteral = "aws-kms:key-id"
	scwGCPKeyRefLiteral = "gcp-kms:key-id"
)

// scwCloudConfig is a valid one-version Scaleway configuration apart from its
// key reference, so a refusal can only come from the reference family.
func scwCloudConfig(reference string) keys.CloudConfig {
	blob := []byte("scw-literal-encrypted-kek-v1")
	return keys.CloudConfig{
		KeyRef:   secret.KeyRef(reference),
		Current:  1,
		Versions: map[secret.KeyVersion][]byte{1: blob},
	}
}

// TestScalewayKeyRefPrefixLiteral pins the exported constant to its literal
// bytes rather than to itself.
func TestScalewayKeyRefPrefixLiteral(t *testing.T) {
	if keys.ScalewayKeyRefPrefix != "scw-kms:" {
		t.Fatalf("keys.ScalewayKeyRefPrefix = %q; want literal %q",
			keys.ScalewayKeyRefPrefix, "scw-kms:")
	}
}

// TestNewScalewayAcceptsLiteralReference pins that NewScaleway accepts the
// literal reference "scw-kms:key-id".
func TestNewScalewayAcceptsLiteralReference(t *testing.T) {
	client := newFakeKMS(map[string][]byte{
		string(scwCloudConfig(scwKeyRefLiteral).Versions[1]): testKEK(),
	})
	provider, err := keys.NewScaleway(client, scwCloudConfig(scwKeyRefLiteral), deterministicRandom(0x13))
	if err != nil {
		t.Fatalf("NewScaleway refused the literal reference %q: %v", scwKeyRefLiteral, err)
	}
	if provider == nil {
		t.Fatalf("NewScaleway returned a nil provider for the literal reference %q", scwKeyRefLiteral)
	}
	if got := string(provider.KeyRef()); got != scwKeyRefLiteral {
		t.Fatalf("provider key reference = %q; want literal %q", got, scwKeyRefLiteral)
	}
}

// TestNewScalewayRefusesForeignReferenceLiterals pins fail closed: the
// "scaleway-kms:" spelling and the AWS/GCP families are all refused as
// InvalidInput, never silently treated as a Scaleway reference.
func TestNewScalewayRefusesForeignReferenceLiterals(t *testing.T) {
	for _, reference := range []string{
		scwUnsupportedKeyRefLiteral,
		scwAWSKeyRefLiteral,
		scwGCPKeyRefLiteral,
	} {
		t.Run(reference, func(t *testing.T) {
			client := newFakeKMS(nil)
			provider, err := keys.NewScaleway(client, scwCloudConfig(reference), deterministicRandom(0x13))
			if err == nil {
				t.Fatalf("NewScaleway accepted the literal reference %q; want InvalidInput", reference)
			}
			class, ok := fault.ClassOf(err)
			if !ok {
				t.Fatalf("refusal of literal reference %q is not a fault: %v", reference, err)
			}
			if class != fault.InvalidInput {
				t.Fatalf("refusal of literal reference %q = %s; want InvalidInput (%v)",
					reference, class, err)
			}
			if provider != nil {
				t.Fatalf("refused literal reference %q returned a provider", reference)
			}
		})
	}
}
