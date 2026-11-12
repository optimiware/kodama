package adaptercontract

// Key-reference grammar corpus.
//
// One JSON corpus drives both real seams for the AWS and GCP KMS families:
// installation.Config.Preflight (what production accepts) and the keys package
// cloud-provider constructors (what the runtime can build). A reference the two
// seams disagree about, a reference that is normalized on the way through, or a
// malformed reference that reaches the KMS client is a contract failure.
//
// The Scaleway family has no structural grammar here: only the exact scw-kms
// family prefix and a non-empty, whitespace/control-free remainder are pinned.
// Ephemeral and keyfile references follow the same prefix-only rule.

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/installation"
	"github.com/optimiware/kodama/internal/shell/integration/keys"
	"github.com/optimiware/kodama/internal/shell/secret"
)

// kmsReferenceVector is one accepted or refused reference.
type kmsReferenceVector struct {
	Name              string `json:"name"`
	Reference         string `json:"reference"`
	ReferenceRegion   string `json:"reference_region"`
	ReferenceAccount  string `json:"reference_account"`
	ReferenceProject  string `json:"reference_project"`
	ReferenceLocation string `json:"reference_location"`
	ReferenceKeyRing  string `json:"reference_key_ring"`
	ReferenceCrypto   string `json:"reference_crypto_key"`
}

// kmsReferenceProvider is one cloud provider's vectors.
type kmsReferenceProvider struct {
	Adapter     string               `json:"adapter"`
	Environment string               `json:"environment"`
	Accepted    []kmsReferenceVector `json:"accepted"`
	Refused     []kmsReferenceVector `json:"refused"`
}

// kmsReferencePreserved is a family with a prefix-only rule.
type kmsReferencePreserved struct {
	Name        string               `json:"name"`
	Provider    string               `json:"provider"`
	Environment string               `json:"environment"`
	Adapter     string               `json:"adapter"`
	Accepted    []kmsReferenceVector `json:"accepted"`
	Refused     []kmsReferenceVector `json:"refused"`
}

// kmsReferenceCorpus is the whole vector file.
type kmsReferenceCorpus struct {
	Providers map[string]kmsReferenceProvider `json:"providers"`
	Preserved []kmsReferencePreserved         `json:"preserved"`
}

// loadKMSReferences reads and shape-checks the corpus.
func loadKMSReferences(t *testing.T) kmsReferenceCorpus {
	t.Helper()
	var corpus kmsReferenceCorpus
	readTestJSON(t, "kms_references.json", &corpus)
	for _, name := range []string{"aws", "gcp", "scaleway"} {
		provider, ok := corpus.Providers[name]
		if !ok {
			t.Fatalf("kms reference corpus has no %s provider section", name)
		}
		if len(provider.Accepted) == 0 || len(provider.Refused) == 0 {
			t.Fatalf("kms reference corpus %s section has no accepted/refused vectors", name)
		}
	}
	for _, preserved := range corpus.Preserved {
		if len(preserved.Accepted) == 0 || len(preserved.Refused) == 0 {
			t.Fatalf("kms reference corpus preserved family %s has no accepted/refused vectors", preserved.Provider)
		}
	}
	return corpus
}

// kmsReferenceConfig builds a complete installation config for one vector. Only
// the key adapter and reference vary.
func kmsReferenceConfig(t *testing.T, environment, adapter, reference string) installation.Config {
	t.Helper()
	switch environment {
	case "production":
		return installation.Config{
			Environment: installation.EnvironmentProduction,
			State:       installation.StatePostgres,
			Messaging:   installation.MessagingNATS,
			Runtime:     installation.RuntimeKubernetes,
			Keys:        installation.KeyAdapter(adapter),
			DatabaseURL: "postgres://kodama@database:5432/kodama?sslmode=verify-full",
			NATSURL:     "tls://nats:4222",
			PublicURL:   "https://kodama.example",
			KeyRef:      reference,
		}
	case "development":
		return installation.Config{
			Environment: installation.EnvironmentDevelopment,
			State:       installation.StateMemory,
			Messaging:   installation.MessagingMemory,
			Runtime:     installation.RuntimeProcess,
			Keys:        installation.KeyAdapter(adapter),
			KeyRef:      reference,
		}
	case "test":
		return installation.Config{
			Environment: installation.EnvironmentTest,
			State:       installation.StatePostgres,
			Messaging:   installation.MessagingNATS,
			Runtime:     installation.RuntimePodman,
			Keys:        installation.KeyAdapter(adapter),
			DatabaseURL: "postgres://localhost/kodama",
			NATSURL:     "nats://localhost:4222",
			PublicURL:   "https://kodama.example",
			KeyRef:      reference,
		}
	default:
		t.Fatalf("kms reference vector names unknown environment %q", environment)
		return installation.Config{}
	}
}

// countingKMS records decrypt calls and panics if construction ever reaches it.
type countingKMS struct {
	calls int
}

func (client *countingKMS) Decrypt(context.Context, secret.KeyRef, []byte) ([]byte, error) {
	client.calls++
	panic("KMS decrypt must not run during reference validation")
}

// buildKMSProvider constructs the real cloud provider for one vector.
func buildKMSProvider(t *testing.T, provider, reference string, client keys.KMSClient) (keys.Provider, error) {
	t.Helper()
	config := keys.CloudConfig{
		KeyRef:   secret.KeyRef(reference),
		Current:  secret.MinKeyVersion,
		Versions: map[secret.KeyVersion][]byte{secret.MinKeyVersion: []byte("synthetic-encrypted-kek")},
	}
	switch provider {
	case "aws":
		return keys.NewAWS(client, config, nil)
	case "gcp":
		return keys.NewGCP(client, config, nil)
	case "scaleway":
		return keys.NewScaleway(client, config, nil)
	default:
		t.Fatalf("kms reference vector names unknown cloud provider %q", provider)
		return nil, nil
	}
}

// requireInvalidInput asserts the stable refusal class.
func requireInvalidInput(t *testing.T, err error, context string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s was accepted; want fault.InvalidInput", context)
	}
	class, ok := fault.ClassOf(err)
	if !ok || class != fault.InvalidInput {
		t.Fatalf("%s refusal = %v (class %v); want fault.InvalidInput", context, err, class)
	}
}

// runKMSGrammar drives both real seams for one cloud provider.
func runKMSGrammar(t *testing.T, providerName string) {
	t.Helper()
	corpus := loadKMSReferences(t)
	provider := corpus.Providers[providerName]
	for _, vector := range provider.Accepted {
		vector := vector
		t.Run("accepted "+vector.Name, func(t *testing.T) {
			config := kmsReferenceConfig(t, provider.Environment, provider.Adapter, vector.Reference)
			if err := config.Preflight(); err != nil {
				t.Fatalf("Preflight refused the %s reference %q: %v", providerName, vector.Name, err)
			}
			client := &countingKMS{}
			built, err := buildKMSProvider(t, providerName, vector.Reference, client)
			if err != nil {
				t.Fatalf("provider construction refused the %s reference %q: %v", providerName, vector.Name, err)
			}
			if built == nil {
				t.Fatalf("provider construction returned a nil provider for %q", vector.Name)
			}
			// References are persisted identities: they are never normalized.
			if got := string(built.KeyRef()); got != vector.Reference {
				t.Fatalf("provider key reference = %q; want the exact accepted reference %q", got, vector.Reference)
			}
			switch providerName {
			case "aws":
				parsed, err := keys.ParseAWSReference(secret.KeyRef(vector.Reference))
				if err != nil {
					t.Fatalf("ParseAWSReference refused an accepted vector: %v", err)
				}
				if vector.ReferenceRegion != "" && parsed.Region != vector.ReferenceRegion {
					t.Fatalf("parsed AWS region = %q; corpus pins %q", parsed.Region, vector.ReferenceRegion)
				}
				if vector.ReferenceAccount != "" && parsed.Account != vector.ReferenceAccount {
					t.Fatalf("parsed AWS account = %q; corpus pins %q", parsed.Account, vector.ReferenceAccount)
				}
				if vector.ReferenceRegion != "" {
					if err := parsed.ValidateRegion(vector.ReferenceRegion); err != nil {
						t.Fatalf("parsed AWS reference rejected its own configured region %q: %v", vector.ReferenceRegion, err)
					}
				}
			case "gcp":
				parsed, err := keys.ParseGCPReference(secret.KeyRef(vector.Reference))
				if err != nil {
					t.Fatalf("ParseGCPReference refused an accepted vector: %v", err)
				}
				if parsed.Project != vector.ReferenceProject || parsed.Location != vector.ReferenceLocation ||
					parsed.KeyRing != vector.ReferenceKeyRing || parsed.CryptoKey != vector.ReferenceCrypto {
					t.Fatalf("parsed GCP reference = %+v; corpus pins %+v", parsed, vector)
				}
			}
			if client.calls != 0 {
				t.Fatalf("reference validation contacted the KMS %d times", client.calls)
			}
		})
	}
	for _, vector := range provider.Refused {
		vector := vector
		t.Run("refused "+vector.Name, func(t *testing.T) {
			config := kmsReferenceConfig(t, provider.Environment, provider.Adapter, vector.Reference)
			requireInvalidInput(t, config.Preflight(), "Preflight "+providerName+" reference "+vector.Name)
			client := &countingKMS{}
			built, err := buildKMSProvider(t, providerName, vector.Reference, client)
			requireInvalidInput(t, err, "provider construction "+providerName+" reference "+vector.Name)
			if built != nil {
				t.Fatalf("refused %s reference %q returned a provider", providerName, vector.Name)
			}
			if client.calls != 0 {
				t.Fatalf("refused %s reference %q contacted the KMS", providerName, vector.Name)
			}
		})
	}
}

// TestAWSKeyReferenceGrammar pins the accepted and refused AWS forms through
// both real seams.
func TestAWSKeyReferenceGrammar(t *testing.T) {
	runKMSGrammar(t, "aws")
}

// TestGCPKeyReferenceGrammar pins the accepted and refused GCP forms through
// both real seams.
func TestGCPKeyReferenceGrammar(t *testing.T) {
	runKMSGrammar(t, "gcp")
}

// TestKMSReferencePreflightProviderParity proves the two independent parsers
// agree on every vector: a reference Preflight accepts is exactly one the
// provider constructor accepts, and neither seam normalizes it.
func TestKMSReferencePreflightProviderParity(t *testing.T) {
	corpus := loadKMSReferences(t)
	for name, provider := range corpus.Providers {
		name, provider := name, provider
		for _, group := range []struct {
			accepted bool
			vectors  []kmsReferenceVector
		}{
			{accepted: true, vectors: provider.Accepted},
			{accepted: false, vectors: provider.Refused},
		} {
			for _, vector := range group.vectors {
				vector := vector
				t.Run(name+" "+vector.Name, func(t *testing.T) {
					preflightErr := kmsReferenceConfig(t, provider.Environment, provider.Adapter, vector.Reference).Preflight()
					client := &countingKMS{}
					built, buildErr := buildKMSProvider(t, name, vector.Reference, client)
					preflightAccepted := preflightErr == nil
					providerAccepted := buildErr == nil
					if preflightAccepted != providerAccepted {
						t.Fatalf("seams disagree: Preflight accepted=%t (%v), provider accepted=%t (%v)", preflightAccepted, preflightErr, providerAccepted, buildErr)
					}
					if preflightAccepted != group.accepted {
						t.Fatalf("both seams accepted=%t; corpus says accepted=%t", preflightAccepted, group.accepted)
					}
					if providerAccepted {
						if built == nil {
							t.Fatal("accepted reference returned a nil provider")
						}
						if got := string(built.KeyRef()); got != vector.Reference {
							t.Fatalf("accepted reference was normalized: %q -> %q", vector.Reference, got)
						}
					} else if built != nil {
						t.Fatal("refused reference returned a provider")
					}
					if client.calls != 0 {
						t.Fatalf("reference validation contacted the KMS %d times", client.calls)
					}
				})
			}
		}
	}
}

// TestMalformedKMSReferenceHasNoProviderEffects proves a malformed reference
// fails before any provider, cache entry or KMS call exists.
func TestMalformedKMSReferenceHasNoProviderEffects(t *testing.T) {
	corpus := loadKMSReferences(t)
	for name, provider := range corpus.Providers {
		for _, vector := range provider.Refused {
			vector := vector
			t.Run(name+" "+vector.Name, func(t *testing.T) {
				client := &countingKMS{}
				built, err := buildKMSProvider(t, name, vector.Reference, client)
				requireInvalidInput(t, err, name+" malformed reference")
				if built != nil {
					t.Fatal("malformed reference produced a provider")
				}
				if client.calls != 0 {
					t.Fatalf("malformed reference reached the KMS client %d times", client.calls)
				}
			})
		}
	}
}

// TestPreservedKeyReferenceFamilies pins the prefix-only ephemeral, keyfile
// and Scaleway families.
func TestPreservedKeyReferenceFamilies(t *testing.T) {
	corpus := loadKMSReferences(t)
	scaleway := corpus.Providers["scaleway"]
	for _, vector := range scaleway.Accepted {
		if err := kmsReferenceConfig(t, scaleway.Environment, scaleway.Adapter, vector.Reference).Preflight(); err != nil {
			t.Errorf("scaleway reference %q was refused: %v", vector.Name, err)
		}
	}
	for _, vector := range scaleway.Refused {
		requireInvalidInput(t, kmsReferenceConfig(t, scaleway.Environment, scaleway.Adapter, vector.Reference).Preflight(), "scaleway reference "+vector.Name)
	}
	for _, preserved := range corpus.Preserved {
		for _, vector := range preserved.Accepted {
			if err := kmsReferenceConfig(t, preserved.Environment, preserved.Adapter, vector.Reference).Preflight(); err != nil {
				t.Errorf("%s reference %q was refused: %v", preserved.Provider, vector.Name, err)
			}
		}
		for _, vector := range preserved.Refused {
			requireInvalidInput(t, kmsReferenceConfig(t, preserved.Environment, preserved.Adapter, vector.Reference).Preflight(), preserved.Provider+" reference "+vector.Name)
		}
	}
}
