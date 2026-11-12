package adaptercontract

import (
	"reflect"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/installation"
)

// TestEnvironmentCompositions pins explicit composition: development uses
// memory/process/ephemeral, test uses PostgreSQL/NATS/Podman/keyfile, and
// production uses PostgreSQL/NATS/Kubernetes/one cloud KMS. Production refuses
// every development or test adapter.
func TestEnvironmentCompositions(t *testing.T) {
	cases := []struct {
		name    string
		config  installation.Config
		wantErr bool
	}{
		{"development", validConfig(installation.EnvironmentDevelopment), false},
		{"test", validConfig(installation.EnvironmentTest), false},
		{"production aws", validConfig(installation.EnvironmentProduction), false},
		{"production gcp", withKeys(validConfig(installation.EnvironmentProduction), installation.KeyGCPKMS), false},
		{"production scaleway", withKeys(validConfig(installation.EnvironmentProduction), installation.KeyScalewayKMS), false},

		{"development postgres", withState(validConfig(installation.EnvironmentDevelopment), installation.StatePostgres), true},
		{"development nats", withMessaging(validConfig(installation.EnvironmentDevelopment), installation.MessagingNATS), true},
		{"development podman", withRuntime(validConfig(installation.EnvironmentDevelopment), installation.RuntimePodman), true},
		{"development keyfile", withKeys(validConfig(installation.EnvironmentDevelopment), installation.KeyKeyfile), true},

		{"test memory", withState(validConfig(installation.EnvironmentTest), installation.StateMemory), true},
		{"test process", withRuntime(validConfig(installation.EnvironmentTest), installation.RuntimeProcess), true},
		{"test kubernetes", withRuntime(validConfig(installation.EnvironmentTest), installation.RuntimeKubernetes), false},
		{"test ephemeral", withKeys(validConfig(installation.EnvironmentTest), installation.KeyEphemeral), true},
		{"test memory messaging", withMessaging(validConfig(installation.EnvironmentTest), installation.MessagingMemory), true},

		{"production memory", withState(validConfig(installation.EnvironmentProduction), installation.StateMemory), true},
		{"production process", withRuntime(validConfig(installation.EnvironmentProduction), installation.RuntimeProcess), true},
		{"production podman", withRuntime(validConfig(installation.EnvironmentProduction), installation.RuntimePodman), true},
		{"production keyfile", withKeys(validConfig(installation.EnvironmentProduction), installation.KeyKeyfile), true},
		{"production ephemeral", withKeys(validConfig(installation.EnvironmentProduction), installation.KeyEphemeral), true},
		{"production memory messaging", withMessaging(validConfig(installation.EnvironmentProduction), installation.MessagingMemory), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.config.Preflight()
			if tc.wantErr && err == nil {
				t.Fatalf("config %+v accepted; want refusal", tc.config)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("config %+v refused: %v", tc.config, err)
			}
		})
	}
}

// TestProductionRequiredSettings proves production refuses incomplete
// bootstrap/API/broker/key settings rather than silently selecting weaker
// adapters.
func TestProductionRequiredSettings(t *testing.T) {
	base := validConfig(installation.EnvironmentProduction)
	cases := []struct {
		name   string
		mutate func(*installation.Config)
	}{
		{"empty database URL", func(c *installation.Config) { c.DatabaseURL = "" }},
		{"empty NATS URL", func(c *installation.Config) { c.NATSURL = "" }},
		{"empty key ref", func(c *installation.Config) { c.KeyRef = "" }},
		{"empty public URL", func(c *installation.Config) { c.PublicURL = "" }},
		{"http database URL", func(c *installation.Config) { c.DatabaseURL = "http://db/kodama" }},
		{"http NATS URL", func(c *installation.Config) { c.NATSURL = "http://nats:4222" }},
		{"plaintext production NATS URL", func(c *installation.Config) { c.NATSURL = "nats://nats.internal:4222" }},
		{"production database without verify-full", func(c *installation.Config) { c.DatabaseURL = "postgres://db.internal/kodama" }},
		{"http public URL", func(c *installation.Config) { c.PublicURL = "http://kodama.example" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			candidate := base
			tc.mutate(&candidate)
			if err := candidate.Preflight(); err == nil {
				t.Fatalf("production config with %s accepted; want refusal", tc.name)
			}
		})
	}

	unknown := base
	unknown.Environment = installation.Environment("staging")
	if err := unknown.Preflight(); err == nil {
		t.Error("unknown environment accepted")
	}
}

// TestBackupScope pins the backup boundary: database, retained logs,
// encryption keys and JetStream state are included; disposable workspaces are
// excluded.
func TestBackupScope(t *testing.T) {
	want := installation.BackupScope{
		Database:       true,
		RetainedLogs:   true,
		EncryptionKeys: true,
		JetStream:      true,
		Workspaces:     false,
	}
	if got := installation.DefaultBackupScope(); !reflect.DeepEqual(got, want) {
		t.Fatalf("default backup scope = %+v; want %+v", got, want)
	}
	if err := (installation.BackupPlan{Scope: want}).Validate(); err != nil {
		t.Fatalf("valid backup plan refused: %v", err)
	}
	if err := (installation.BackupPlan{Scope: installation.BackupScope{Workspaces: true}}).Validate(); err == nil {
		t.Error("backup plan including workspaces accepted")
	}
	if err := (installation.BackupPlan{}).Validate(); err == nil {
		t.Error("empty backup plan accepted")
	}
}

// TestRestoreOrderAndFencing pins restore sequencing: fence old actors, restore
// state, reconcile external effects, then enable dispatch. Dispatch before
// fencing/reconciliation is refused.
func TestRestoreOrderAndFencing(t *testing.T) {
	wantOrder := []installation.RestoreStep{
		installation.RestoreFence,
		installation.RestoreState,
		installation.RestoreReconcile,
		installation.RestoreEnableDispatch,
	}
	if got := installation.RestoreOrder(); !reflect.DeepEqual(got, wantOrder) {
		t.Fatalf("restore order = %v; want %v", got, wantOrder)
	}
	scope := installation.DefaultBackupScope()
	cases := []struct {
		name    string
		plan    installation.RestorePlan
		wantErr bool
	}{
		{"quiescent", installation.RestorePlan{Scope: scope}, false},
		{"fenced only", installation.RestorePlan{Scope: scope, Fenced: true}, false},
		{"dispatch before fencing", installation.RestorePlan{Scope: scope, EnableDispatch: true}, true},
		{"dispatch before reconciliation", installation.RestorePlan{Scope: scope, Fenced: true, EnableDispatch: true}, true},
		{"dispatch after fencing and reconciliation", installation.RestorePlan{Scope: scope, Fenced: true, Reconciled: true, EnableDispatch: true}, false},
		{"workspaces included", installation.RestorePlan{Scope: installation.BackupScope{Workspaces: true}, Fenced: true, Reconciled: true}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.plan.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("restore plan %+v accepted; want refusal", tc.plan)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("restore plan %+v refused: %v", tc.plan, err)
			}
		})
	}
}

// TestPreflightKeyNameRule pins the key-name rule for every declared key
// family: after an exact case-sensitive family prefix the key name must be
// non-empty and free of whitespace, control bytes and DEL, and the refusal
// never echoes the configured name. The provider grammars form the prefix and
// remainder around the name under test.
func TestPreflightKeyNameRule(t *testing.T) {
	families := []struct {
		name     string
		adapter  installation.KeyAdapter
		ref      func(string) string
		template installation.Config
	}{
		{name: "ephemeral", adapter: installation.KeyEphemeral, ref: func(name string) string { return installation.KeyRefEphemeralPrefix + name }, template: validConfig(installation.EnvironmentDevelopment)},
		{name: "keyfile", adapter: installation.KeyKeyfile, ref: func(name string) string { return installation.KeyRefKeyfilePrefix + name }, template: validConfig(installation.EnvironmentTest)},
		{name: "aws", adapter: installation.KeyAWSKMS, ref: func(name string) string { return installation.KeyRefAWSKMSPrefix + "alias/" + name }, template: validConfig(installation.EnvironmentProduction)},
		{name: "gcp", adapter: installation.KeyGCPKMS, ref: func(name string) string {
			return installation.KeyRefGCPKMSPrefix + "projects/kodama/locations/global/keyRings/kodama/cryptoKeys/" + name
		}, template: validConfig(installation.EnvironmentProduction)},
		{name: "scaleway", adapter: installation.KeyScalewayKMS, ref: func(name string) string { return installation.KeyRefScalewayKMSPrefix + name }, template: validConfig(installation.EnvironmentProduction)},
	}
	invalidNames := []struct {
		name  string
		value string
	}{
		{name: "empty name", value: ""},
		{name: "interior space", value: "alias/kodama key"},
		{name: "interior tab", value: "alias/kodama\tkey"},
		{name: "control byte", value: "alias/kodama\x01key"},
		{name: "nul byte", value: "alias/kodama\x00key"},
		{name: "del byte", value: "alias/kodama\x7fkey"},
	}
	for _, family := range families {
		// The template must start from the family it names.
		valid := family.template
		valid.Keys = family.adapter
		valid.KeyRef = family.ref("key-1")
		if err := valid.Preflight(); err != nil {
			t.Fatalf("%s family template was not valid: %v", family.name, err)
		}
		for _, invalid := range invalidNames {
			t.Run(family.name+" refused "+invalid.name, func(t *testing.T) {
				config := family.template
				config.Keys = family.adapter
				config.KeyRef = family.ref(invalid.value)
				err := config.Preflight()
				if err == nil {
					t.Fatalf("Preflight accepted the %s reference with %s", family.name, invalid.name)
				}
				class, ok := fault.ClassOf(err)
				if !ok || class != fault.InvalidInput {
					t.Fatalf("%s %s refusal = %v; want fault.InvalidInput", family.name, invalid.name, err)
				}
				if invalid.value != "" && strings.Contains(err.Error(), invalid.value) {
					t.Fatalf("refusal echoes the key name: %v", err)
				}
			})
		}
	}
}

func validConfig(environment installation.Environment) installation.Config {
	switch environment {
	case installation.EnvironmentDevelopment:
		return installation.Config{
			Environment: environment,
			State:       installation.StateMemory,
			Messaging:   installation.MessagingMemory,
			Runtime:     installation.RuntimeProcess,
			Keys:        installation.KeyEphemeral,
		}
	case installation.EnvironmentTest:
		return installation.Config{
			Environment: environment,
			State:       installation.StatePostgres,
			Messaging:   installation.MessagingNATS,
			Runtime:     installation.RuntimePodman,
			Keys:        installation.KeyKeyfile,
			DatabaseURL: "postgres://localhost/kodama",
			NATSURL:     "nats://localhost:4222",
			KeyRef:      "keyfile:kodama",
			PublicURL:   "https://kodama.example",
		}
	case installation.EnvironmentProduction:
		return installation.Config{
			Environment: environment,
			State:       installation.StatePostgres,
			Messaging:   installation.MessagingNATS,
			Runtime:     installation.RuntimeKubernetes,
			Keys:        installation.KeyAWSKMS,
			DatabaseURL: "postgres://db.internal/kodama?sslmode=verify-full",
			NATSURL:     "tls://nats.internal:4222",
			KeyRef:      "aws-kms:alias/kodama",
			PublicURL:   "https://kodama.example",
		}
	default:
		return installation.Config{Environment: environment}
	}
}

func withState(config installation.Config, state installation.StateAdapter) installation.Config {
	config.State = state
	return config
}

func withMessaging(config installation.Config, messaging installation.MessagingAdapter) installation.Config {
	config.Messaging = messaging
	return config
}

func withRuntime(config installation.Config, runtime installation.RuntimeAdapter) installation.Config {
	config.Runtime = runtime
	return config
}

func withKeys(config installation.Config, keys installation.KeyAdapter) installation.Config {
	config.Keys = keys
	// Preflight refuses a key reference outside the declared key adapter
	// family, so keep the reference consistent with the adapter under test.
	// These cases still vary the adapter itself.
	switch keys {
	case installation.KeyEphemeral:
		config.KeyRef = installation.KeyRefEphemeralPrefix + "dev"
	case installation.KeyKeyfile:
		config.KeyRef = installation.KeyRefKeyfilePrefix + "kodama"
	case installation.KeyAWSKMS:
		config.KeyRef = installation.KeyRefAWSKMSPrefix + "alias/kodama"
	case installation.KeyGCPKMS:
		config.KeyRef = installation.KeyRefGCPKMSPrefix + "projects/kodama/locations/global/keyRings/kodama/cryptoKeys/kek"
	case installation.KeyScalewayKMS:
		config.KeyRef = installation.KeyRefScalewayKMSPrefix + "fr-par/kodama"
	}
	return config
}
