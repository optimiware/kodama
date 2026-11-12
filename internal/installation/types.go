// Package installation declares the explicit environment composition and the
// backup/restore boundary.
//
// Composition is explicit, never auto-detected: development uses memory state,
// memory messaging, the Process runtime and ephemeral keys; test uses
// PostgreSQL, NATS, Podman and a keyfile; production uses PostgreSQL, NATS,
// Kubernetes Jobs and exactly one explicitly selected cloud KMS. Production
// refuses every development or test adapter and incomplete required settings.
//
// Backup covers PostgreSQL, retained logs, encryption keys and JetStream state;
// disposable workspaces are excluded. Restore fences old actors, restores state,
// reconciles external effects from stable identities and only then enables
// dispatch. Missing fencing or unresolved effect evidence blocks dispatch
// rather than triggering replay.
//
// These are declaration-only values and pure preflight rules. No command,
// database, broker, container runtime or cloud client is created here.
package installation

// Environment is one explicit installation environment.
type Environment string

const (
	// EnvironmentDevelopment is the local development composition.
	EnvironmentDevelopment Environment = "development"
	// EnvironmentTest is the shared test composition.
	EnvironmentTest Environment = "test"
	// EnvironmentProduction is the production composition.
	EnvironmentProduction Environment = "production"
)

// Valid reports whether the environment is one of the declared environments.
func (environment Environment) Valid() bool {
	return environment == EnvironmentDevelopment ||
		environment == EnvironmentTest ||
		environment == EnvironmentProduction
}

// StateAdapter is one explicit durable-state adapter.
type StateAdapter string

const (
	// StateMemory is the development in-memory adapter. It is never production
	// evidence.
	StateMemory StateAdapter = "memory"
	// StatePostgres is the PostgreSQL adapter.
	StatePostgres StateAdapter = "postgres"
)

// Valid reports whether the adapter is one of the declared adapters.
func (adapter StateAdapter) Valid() bool {
	return adapter == StateMemory || adapter == StatePostgres
}

// MessagingAdapter is one explicit event/log messaging adapter.
type MessagingAdapter string

const (
	// MessagingMemory is the development in-memory adapter.
	MessagingMemory MessagingAdapter = "memory"
	// MessagingNATS is the NATS adapter.
	MessagingNATS MessagingAdapter = "nats"
)

// Valid reports whether the adapter is one of the declared adapters.
func (adapter MessagingAdapter) Valid() bool {
	return adapter == MessagingMemory || adapter == MessagingNATS
}

// RuntimeAdapter is one explicit workload runtime adapter.
type RuntimeAdapter string

const (
	// RuntimeProcess is the development host-process adapter.
	RuntimeProcess RuntimeAdapter = "process"
	// RuntimePodman is the test Podman adapter.
	RuntimePodman RuntimeAdapter = "podman"
	// RuntimeKubernetes is the production Kubernetes Job adapter.
	RuntimeKubernetes RuntimeAdapter = "kubernetes"
)

// Valid reports whether the adapter is one of the declared adapters.
func (adapter RuntimeAdapter) Valid() bool {
	return adapter == RuntimeProcess || adapter == RuntimePodman || adapter == RuntimeKubernetes
}

// KeyAdapter is one explicit secret-custody key adapter.
type KeyAdapter string

const (
	// KeyEphemeral is the development ephemeral adapter.
	KeyEphemeral KeyAdapter = "ephemeral"
	// KeyKeyfile is the test keyfile adapter.
	KeyKeyfile KeyAdapter = "keyfile"
	// KeyAWSKMS is the production AWS KMS adapter.
	KeyAWSKMS KeyAdapter = "aws-kms"
	// KeyGCPKMS is the production GCP KMS adapter.
	KeyGCPKMS KeyAdapter = "gcp-kms"
	// KeyScalewayKMS is the production Scaleway KMS adapter.
	KeyScalewayKMS KeyAdapter = "scw-kms"
)

// Valid reports whether the adapter is one of the declared adapters.
func (adapter KeyAdapter) Valid() bool {
	return adapter == KeyEphemeral || adapter == KeyKeyfile ||
		adapter == KeyAWSKMS || adapter == KeyGCPKMS || adapter == KeyScalewayKMS
}

// CloudKMS reports whether the adapter is an explicitly selected production
// cloud KMS. Exactly one is required in production.
func (adapter KeyAdapter) CloudKMS() bool {
	return adapter == KeyAWSKMS || adapter == KeyGCPKMS || adapter == KeyScalewayKMS
}

// Config is one installation configuration. URLs and the key reference are
// bootstrap configuration, never recursive use of the secret store.
type Config struct {
	Environment Environment
	State       StateAdapter
	Messaging   MessagingAdapter
	Runtime     RuntimeAdapter
	Keys        KeyAdapter
	DatabaseURL string
	NATSURL     string
	KeyRef      string
	PublicURL   string
	// Optional enables optional companions explicitly. A missing entry is
	// disabled.
	Optional map[OptionalCapability]bool
}

// BackupScope is the exact set of durable state a backup includes. Workspaces
// are disposable and always excluded.
type BackupScope struct {
	Database       bool
	RetainedLogs   bool
	EncryptionKeys bool
	JetStream      bool
	Workspaces     bool
}

// BackupPlan is one backup request. Its scope must include every durable family
// and exclude workspaces.
type BackupPlan struct {
	Scope BackupScope
}

// BackupResult is the evidence returned for one completed backup.
type BackupResult struct {
	Identity BackupIdentity
	Scope    BackupScope
	Detail   string
}

// RestoreStep is one ordered restore phase.
type RestoreStep string

const (
	// RestoreFence fences old controllers, enforcing services and consumers.
	RestoreFence RestoreStep = "fence"
	// RestoreState restores database, key, log and event state.
	RestoreState RestoreStep = "restore_state"
	// RestoreReconcile reconciles existing workloads and external effects.
	RestoreReconcile RestoreStep = "reconcile"
	// RestoreEnableDispatch enables dispatch and report retries.
	RestoreEnableDispatch RestoreStep = "enable_dispatch"
)

// RestorePlan is one restore request. Dispatch cannot be enabled before old
// actors are fenced and external effects are reconciled.
type RestorePlan struct {
	Scope          BackupScope
	Fenced         bool
	Reconciled     bool
	EnableDispatch bool
}

// RestoreResult is the evidence returned for one completed restore.
type RestoreResult struct {
	Fenced                  bool
	Reconciled              bool
	Detail                  string
	Preserved               PreservedState
	Services                []string
	UnrecoverableWorkspaces []string
}
