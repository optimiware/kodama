// Package installation — conformance rules for environment composition,
// preflight and the backup/restore boundary.
//
// These functions are pure value rules. They never open a database, broker,
// container runtime, cloud client or filesystem path.
package installation

import (
	"github.com/optimiware/kodama/internal/fault"
)

// Preflight refuses an environment whose adapter composition or required
// settings are incomplete. Production refuses every development or test
// adapter rather than silently selecting a weaker one, and requires TLS on the
// declared transport. Preflight also owns the key-reference family check and
// the AWS and GCP provider grammars, so a Config refused here never needs the
// later key client to detect it.
func (config Config) Preflight() error {
	if !config.Environment.Valid() {
		return fault.New(fault.InvalidInput, "installation environment is not one of the declared environments")
	}
	switch config.Environment {
	case EnvironmentDevelopment:
		if config.State != StateMemory {
			return fault.New(fault.InvalidInput, "development requires the memory state adapter")
		}
		if config.Messaging != MessagingMemory {
			return fault.New(fault.InvalidInput, "development requires the memory messaging adapter")
		}
		if config.Runtime != RuntimeProcess {
			return fault.New(fault.InvalidInput, "development requires the process runtime adapter")
		}
		if config.Keys != KeyEphemeral {
			return fault.New(fault.InvalidInput, "development requires the ephemeral key adapter")
		}
		// Development may omit the reference, but an explicit reference must
		// still name the declared key adapter family.
		return config.validateKeyReference()
	case EnvironmentTest:
		if config.State != StatePostgres {
			return fault.New(fault.InvalidInput, "test requires the postgres state adapter")
		}
		if config.Messaging != MessagingNATS {
			return fault.New(fault.InvalidInput, "test requires the NATS messaging adapter")
		}
		if config.Runtime != RuntimePodman && config.Runtime != RuntimeKubernetes {
			return fault.New(fault.InvalidInput, "test requires the podman runtime adapter")
		}
		if config.Keys != KeyKeyfile {
			return fault.New(fault.InvalidInput, "test requires the keyfile key adapter")
		}
		if err := config.validateEndpoints(); err != nil {
			return err
		}
		return config.validateKeyReference()
	case EnvironmentProduction:
		if config.State != StatePostgres {
			return fault.New(fault.InvalidInput, "production requires the postgres state adapter")
		}
		if config.Messaging != MessagingNATS {
			return fault.New(fault.InvalidInput, "production requires the NATS messaging adapter")
		}
		if config.Runtime != RuntimeKubernetes {
			return fault.New(fault.InvalidInput, "production requires the kubernetes runtime adapter")
		}
		if !config.Keys.CloudKMS() {
			return fault.New(fault.InvalidInput, "production requires exactly one explicitly selected cloud KMS key adapter")
		}
		if err := config.validateEndpoints(); err != nil {
			return err
		}
		if err := validateProductionNATSURL(config.NATSURL); err != nil {
			return err
		}
		if err := validateProductionPostgresURL(config.DatabaseURL, "production database URL"); err != nil {
			return err
		}
		return config.validateKeyReference()
	default:
		return fault.New(fault.InvalidInput, "installation environment is not one of the declared environments")
	}
}

// DefaultBackupScope returns the declared backup boundary: every durable family
// is included and disposable workspaces are excluded.
func DefaultBackupScope() BackupScope {
	return BackupScope{
		Database:       true,
		RetainedLogs:   true,
		EncryptionKeys: true,
		JetStream:      true,
		Workspaces:     false,
	}
}

// Validate refuses a backup scope that omits a durable family or includes
// disposable workspaces.
func (plan BackupPlan) Validate() error {
	return plan.Scope.validate()
}

func (scope BackupScope) validate() error {
	if !scope.Database {
		return fault.New(fault.InvalidInput, "backup scope omits the database")
	}
	if !scope.RetainedLogs {
		return fault.New(fault.InvalidInput, "backup scope omits retained logs")
	}
	if !scope.EncryptionKeys {
		return fault.New(fault.InvalidInput, "backup scope omits encryption keys")
	}
	if !scope.JetStream {
		return fault.New(fault.InvalidInput, "backup scope omits JetStream state")
	}
	if scope.Workspaces {
		return fault.New(fault.InvalidInput, "backup scope includes disposable workspaces")
	}
	return nil
}

// RestoreOrder returns the declared restore sequence.
func RestoreOrder() []RestoreStep {
	return []RestoreStep{
		RestoreFence,
		RestoreState,
		RestoreReconcile,
		RestoreEnableDispatch,
	}
}

// Validate refuses a restore plan with an invalid scope, or one that would
// enable dispatch before old actors are fenced and external effects are
// reconciled.
func (plan RestorePlan) Validate() error {
	if err := plan.Scope.validate(); err != nil {
		return err
	}
	if plan.EnableDispatch && !plan.Fenced {
		return fault.New(fault.InvalidInput, "restore enables dispatch before fencing old actors")
	}
	if plan.EnableDispatch && !plan.Reconciled {
		return fault.New(fault.InvalidInput, "restore enables dispatch before reconciling external effects")
	}
	return nil
}
