package adaptercontract

// Scaleway key adapter literal vector.
//
// The Scaleway key adapter and key-reference prefix are spelled scw-kms, not
// scaleway-kms. The policy tests name the key adapter and key reference
// through the exported Go constants, so a rename that moved a constant and its
// consumers together would pass unnoticed. This vector spells every accepted
// and refused value as a literal string and parses a complete production
// installation document through installation.ParseConfig.
//
// The literals below are deliberately not derived from
// installation.KeyScalewayKMS or installation.KeyRefScalewayKMSPrefix: the
// point of the vector is to catch a renamed constant, so it must fail when the
// literal bytes move.

import (
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/installation"
)

// The exact literal spellings the installation document accepts.
const (
	scwKeyAdapterLiteral = "scw-kms"
	scwKeyRefLiteral     = "scw-kms:key-id"

	// scwLegacyAdapterLiteral and scwLegacyKeyRefLiteral are the long-form
	// spellings. They must not parse.
	scwLegacyAdapterLiteral = "scaleway-kms"
	scwLegacyKeyRefLiteral  = "scaleway-kms:key-id"

	scwForeignKeyRefLiteral = "gcp-kms:projects/kodama/locations/europe-west1/keyRings/kodama"
)

// scwProductionDocument renders a complete production composition with the
// literal adapter and key reference under test. Everything else is valid so a
// refusal can only come from the pair under test.
func scwProductionDocument(keys, keyRef string) string {
	return strings.Join([]string{
		"environment = production",
		"state = postgres",
		"messaging = nats",
		"runtime = kubernetes",
		"keys = " + keys,
		"database_url = postgres://kodama@database:5432/kodama?sslmode=verify-full",
		"nats_url = tls://nats:4222",
		"public_url = https://kodama.example",
		"key_ref = " + keyRef,
	}, "\n") + "\n"
}

// TestScalewayKeyLiteralsAccepted pins the literal accepted pair: the
// production environment accepts the key adapter "scw-kms" with the key
// reference "scw-kms:key-id".
func TestScalewayKeyLiteralsAccepted(t *testing.T) {
	config, err := installation.ParseConfig(strings.NewReader(
		scwProductionDocument(scwKeyAdapterLiteral, scwKeyRefLiteral)))
	if err != nil {
		t.Fatalf("ParseConfig refused the literal production document (adapter %q, reference %q): %v",
			scwKeyAdapterLiteral, scwKeyRefLiteral, err)
	}
	if string(config.Keys) != scwKeyAdapterLiteral {
		t.Errorf("parsed key adapter = %q, want literal %q", config.Keys, scwKeyAdapterLiteral)
	}
	if config.KeyRef != scwKeyRefLiteral {
		t.Errorf("parsed key reference = %q, want literal %q", config.KeyRef, scwKeyRefLiteral)
	}
}

// TestScalewayLegacyKeyLiteralsRefused pins fail-closed parsing: the long-form
// adapter spelling, the long-form reference prefix and a mismatched key family
// are all refused as InvalidInput. No legacy alias is accepted.
func TestScalewayLegacyKeyLiteralsRefused(t *testing.T) {
	cases := []struct {
		name   string
		keys   string
		keyRef string
	}{
		{
			name:   "legacy adapter and legacy prefix",
			keys:   scwLegacyAdapterLiteral,
			keyRef: scwLegacyKeyRefLiteral,
		},
		{
			name:   "legacy adapter with the new prefix",
			keys:   scwLegacyAdapterLiteral,
			keyRef: scwKeyRefLiteral,
		},
		{
			name:   "new adapter with the legacy prefix",
			keys:   scwKeyAdapterLiteral,
			keyRef: scwLegacyKeyRefLiteral,
		},
		{
			name:   "new adapter with a foreign key family",
			keys:   scwKeyAdapterLiteral,
			keyRef: scwForeignKeyRefLiteral,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := installation.ParseConfig(strings.NewReader(
				scwProductionDocument(testCase.keys, testCase.keyRef)))
			if err == nil {
				t.Fatalf("ParseConfig accepted adapter %q with reference %q; want InvalidInput",
					testCase.keys, testCase.keyRef)
			}
			class, ok := fault.ClassOf(err)
			if !ok {
				t.Fatalf("refusal of adapter %q reference %q is not a fault: %v",
					testCase.keys, testCase.keyRef, err)
			}
			if class != fault.InvalidInput {
				t.Fatalf("refusal of adapter %q reference %q = %s; want InvalidInput (%v)",
					testCase.keys, testCase.keyRef, class, err)
			}
		})
	}
}
