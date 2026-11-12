package recovery

import "github.com/optimiware/kodama/internal/installation"

// manifestFormat is the only on-disk backup manifest format this driver reads
// or writes.
const manifestFormat = 1

// backupManifest is the documented on-disk evidence format. It is written last,
// so a set without a manifest is never a usable backup.
type backupManifest struct {
	Format        int               `json:"format"`
	Identity      string            `json:"identity"`
	Scope         manifestScope     `json:"scope"`
	Sources       map[string]int    `json:"sources"`
	DatabaseBytes int               `json:"database_bytes"`
	Preserved     manifestPreserved `json:"preserved"`
	Workspaces    []string          `json:"workspaces"`
}

type manifestScope struct {
	Database       bool `json:"database"`
	RetainedLogs   bool `json:"retained_logs"`
	EncryptionKeys bool `json:"encryption_keys"`
	JetStream      bool `json:"jetstream"`
	Workspaces     bool `json:"workspaces"`
}

func manifestScopeFrom(scope installation.BackupScope) manifestScope {
	return manifestScope{
		Database:       scope.Database,
		RetainedLogs:   scope.RetainedLogs,
		EncryptionKeys: scope.EncryptionKeys,
		JetStream:      scope.JetStream,
		Workspaces:     scope.Workspaces,
	}
}

func (scope manifestScope) installationScope() installation.BackupScope {
	return installation.BackupScope{
		Database:       scope.Database,
		RetainedLogs:   scope.RetainedLogs,
		EncryptionKeys: scope.EncryptionKeys,
		JetStream:      scope.JetStream,
		Workspaces:     scope.Workspaces,
	}
}

type manifestPreserved struct {
	Receipts         int `json:"receipts"`
	Decisions        int `json:"decisions"`
	QueuedExecutions int `json:"queued_executions"`
}

func manifestPreservedFrom(preserved installation.PreservedState) manifestPreserved {
	return manifestPreserved{
		Receipts:         preserved.Receipts,
		Decisions:        preserved.Decisions,
		QueuedExecutions: preserved.QueuedExecutions,
	}
}

func (preserved manifestPreserved) installationState() installation.PreservedState {
	return installation.PreservedState{
		Receipts:         preserved.Receipts,
		Decisions:        preserved.Decisions,
		QueuedExecutions: preserved.QueuedExecutions,
	}
}
