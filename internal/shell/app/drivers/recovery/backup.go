package recovery

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/installation"
)

// requiredBackupSources is the exact durable source set every backup set must
// declare. The names are also what Preflight and Restore resolve, so a set that
// omits one is not a complete backup.
var requiredBackupSources = []string{"retained-logs", "keys", "jetstream"}

// BackupConfig binds one backup driver to its container command executor, its
// host file boundary and the exact durable sources it captures.
type BackupConfig struct {
	Executor    Executor
	Files       Files
	Container   string
	Database    string
	Root        string
	Sources     []BackupSource
	Workspaces  []Workspace
	Preserved   func(context.Context) (installation.PreservedState, error)
	NewIdentity func() string
}

// BackupDriver performs one backup of the configured durable scope. It never
// includes disposable workspaces and never returns an identity for an
// incomplete set.
type BackupDriver struct {
	executor    Executor
	files       Files
	container   string
	database    string
	root        string
	sources     []BackupSource
	workspaces  []Workspace
	preserved   func(context.Context) (installation.PreservedState, error)
	newIdentity func() string
}

// NewBackupDriver validates one backup configuration. It contacts no
// database, broker or filesystem.
func NewBackupDriver(config BackupConfig) (*BackupDriver, error) {
	if config.Executor == nil {
		return nil, fault.New(fault.InvalidInput, "backup driver requires an executor")
	}
	if strings.TrimSpace(config.Container) == "" {
		return nil, fault.New(fault.InvalidInput, "backup driver requires a container")
	}
	if strings.TrimSpace(config.Database) == "" {
		return nil, fault.New(fault.InvalidInput, "backup driver requires a database name")
	}
	if strings.TrimSpace(config.Root) == "" || !filepath.IsAbs(config.Root) {
		return nil, fault.New(fault.InvalidInput, "backup driver requires an absolute backup root")
	}
	if config.NewIdentity == nil {
		return nil, fault.New(fault.InvalidInput, "backup driver requires an identity source")
	}
	sources := make([]BackupSource, 0, len(config.Sources))
	for _, source := range config.Sources {
		if !validName(source.Name) {
			return nil, fault.New(fault.InvalidInput, "backup source name is not a single path component")
		}
		if strings.TrimSpace(source.Root) == "" {
			return nil, fault.New(fault.InvalidInput, "backup source root is empty")
		}
		sources = append(sources, source)
	}
	// A backup that does not capture every durable family is incomplete, so the
	// driver refuses to be constructed with a missing or extra source name.
	if len(sources) != len(requiredBackupSources) {
		return nil, fault.New(fault.InvalidInput,
			"backup driver requires exactly the retained-logs, keys and jetstream sources")
	}
	declared := make(map[string]bool, len(sources))
	for _, source := range sources {
		declared[source.Name] = true
	}
	for _, name := range requiredBackupSources {
		if !declared[name] {
			return nil, fault.New(fault.InvalidInput, "backup driver requires the "+name+" source")
		}
	}
	workspaces := make([]Workspace, 0, len(config.Workspaces))
	for _, workspace := range config.Workspaces {
		if strings.TrimSpace(workspace.ID) == "" {
			return nil, fault.New(fault.InvalidInput, "backup workspace identity is empty")
		}
		workspaces = append(workspaces, workspace)
	}
	files := config.Files
	if files == nil {
		files = OSFiles{}
	}
	return &BackupDriver{
		executor:    config.Executor,
		files:       files,
		container:   config.Container,
		database:    config.Database,
		root:        filepath.Clean(config.Root),
		sources:     sources,
		workspaces:  workspaces,
		preserved:   config.Preserved,
		newIdentity: config.NewIdentity,
	}, nil
}

// Backup runs exactly one pg_dump, captures every ready source and writes the
// manifest last. Any failure returns no identity, so the caller has nothing it
// can preflight.
func (d *BackupDriver) Backup(ctx context.Context, plan installation.BackupPlan) (installation.BackupResult, error) {
	if err := ctx.Err(); err != nil {
		return installation.BackupResult{}, err
	}
	if err := plan.Validate(); err != nil {
		return installation.BackupResult{}, err
	}
	identity := d.newIdentity()
	if !validName(identity) {
		return installation.BackupResult{}, fault.New(fault.InvalidInput, "backup identity is not a single path component")
	}
	setDir := filepath.Join(d.root, identity)

	// The dump keeps object ownership and table/column ACLs. Stripping them
	// (--no-owner/--no-privileges) would restore a database owned by the
	// restoring administrator where kodama_runtime, kodama_controller,
	// kodama_publisher and kodama_auth hold none of their declared grants.
	// pg_dump never creates roles, so a restore requires the target to be
	// bootstrapped by `kodama migrate` first.
	dump, err := d.executor.Exec(ctx, ExecRequest{
		Container: d.container,
		Argv:      []string{"pg_dump", "--format=custom", "--dbname", d.database},
	})
	if err != nil {
		return installation.BackupResult{}, fault.Wrap(fault.DependencyUnavailable, "database dump failed", err)
	}
	if dump.ExitCode != 0 {
		return installation.BackupResult{}, fault.New(fault.DependencyUnavailable, "database dump reported a non-zero exit")
	}
	if err := d.files.WriteFile(ctx, filepath.Join(setDir, "database.dump"), dump.Stdout); err != nil {
		return installation.BackupResult{}, err
	}

	sources := make(map[string]int, len(d.sources))
	for _, source := range d.sources {
		if err := ctx.Err(); err != nil {
			return installation.BackupResult{}, err
		}
		if source.Ready != nil {
			if err := source.Ready(ctx); err != nil {
				return installation.BackupResult{}, fault.Wrap(fault.DependencyUnavailable,
					"backup source is not ready: "+source.Name, err)
			}
		}
		count, err := d.files.Snapshot(ctx, source.Root, filepath.Join(setDir, source.Name))
		if err != nil {
			return installation.BackupResult{}, err
		}
		sources[source.Name] = count
	}
	// A set without key material cannot be restored and must not be reported as
	// a complete backup: return no identity and write no manifest.
	if sources["keys"] <= 0 {
		return installation.BackupResult{}, fault.New(fault.NotFound, "backup captured no encryption key material")
	}

	preserved := installation.PreservedState{}
	if d.preserved != nil {
		preserved, err = d.preserved(ctx)
		if err != nil {
			return installation.BackupResult{}, err
		}
	}

	workspaces := make([]string, 0, len(d.workspaces))
	for _, workspace := range d.workspaces {
		workspaces = append(workspaces, workspace.ID)
	}

	manifest := backupManifest{
		Format:        manifestFormat,
		Identity:      identity,
		Scope:         manifestScopeFrom(plan.Scope),
		Sources:       sources,
		DatabaseBytes: len(dump.Stdout),
		Preserved:     manifestPreservedFrom(preserved),
		Workspaces:    workspaces,
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return installation.BackupResult{}, fault.Wrap(fault.InternalFailure, "backup manifest is not encodable", err)
	}
	if err := d.files.WriteFile(ctx, filepath.Join(setDir, "manifest.json"), encoded); err != nil {
		return installation.BackupResult{}, err
	}
	return installation.BackupResult{
		Identity: installation.BackupIdentity{ID: identity},
		Scope:    plan.Scope,
		Detail: fmt.Sprintf("captured %d database bytes and %d durable sources into backup %s",
			len(dump.Stdout), len(sources), identity),
	}, nil
}
