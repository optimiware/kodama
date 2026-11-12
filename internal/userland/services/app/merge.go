package app

// The merge-coordinator composition entry.
//
// It builds the concrete Git merge/rebase effects over the injected native Git
// executor, the durable file coordination store, and the coordinator that
// serializes one merge per repository/ref. The adapter then consumes that
// coordinator through the existing public client boundary.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/optimiware/kodama/client"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/userland/services"
)

// DefaultMergeLease bounds one coordinator claim when the configuration leaves
// the lease unset.
const DefaultMergeLease = 2 * time.Minute

// MergeConfig is the complete merge-coordinator composition input.
type MergeConfig struct {
	ServiceID      id.PrincipalID
	TenantID       id.TenantID
	ProjectID      id.ProjectID
	StateDirectory string
	GitURL         string
	CredentialFile string
	// WorkDirectory is the scratch-clone parent. A zero value uses
	// <StateDirectory>/work.
	WorkDirectory string
	// Owner is this process's claim identity. A zero value derives one from
	// the host and process.
	Owner string
	// Lease bounds one claim. A zero value uses DefaultMergeLease.
	Lease          time.Duration
	RescanInterval time.Duration

	Transport client.Transport
	Events    services.EventSource
	Clock     services.Clock
	// Executor is the native Git boundary. A nil value builds the operating
	// system executor.
	Executor services.GitCommandExecutor
	// GitPath is the native git binary used by the default executor.
	GitPath string
	// NewOperationID generates durable operation identities. A nil value uses
	// a random identity.
	NewOperationID func() (id.OperationID, error)
}

// RunMergeCoordinator composes and runs the merge-coordinator adapter until ctx
// is canceled. It refuses an incomplete configuration before starting anything.
func RunMergeCoordinator(ctx context.Context, config MergeConfig) error {
	if strings.TrimSpace(config.StateDirectory) == "" {
		return fmt.Errorf("merge coordinator state directory is required")
	}
	if strings.TrimSpace(config.GitURL) == "" {
		return fmt.Errorf("merge coordinator Git URL is required")
	}
	if config.Clock == nil {
		config.Clock = SystemClock{}
	}
	stateDirectory, err := filepath.Abs(filepath.Clean(config.StateDirectory))
	if err != nil {
		return fmt.Errorf("resolve merge coordinator state directory: %w", err)
	}
	if err := os.MkdirAll(stateDirectory, 0o700); err != nil {
		return fmt.Errorf("create merge coordinator state directory: %w", err)
	}
	executor := config.Executor
	if executor == nil {
		built, err := NewOSGitExecutor(OSGitExecutorConfig{GitPath: config.GitPath})
		if err != nil {
			return err
		}
		executor = built
	}
	workDirectory := config.WorkDirectory
	if strings.TrimSpace(workDirectory) == "" {
		workDirectory = filepath.Join(stateDirectory, "work")
	}
	effects, err := services.NewGitMergeEffects(services.GitMergeEffectsConfig{
		RemoteBaseURL:  config.GitURL,
		StateDirectory: filepath.Join(stateDirectory, "effects"),
		WorkDirectory:  workDirectory,
		CredentialFile: config.CredentialFile,
		Executor:       executor,
	})
	if err != nil {
		return err
	}
	store, err := services.NewFileCoordinationStore(filepath.Join(stateDirectory, "coordination"))
	if err != nil {
		return err
	}
	lease := config.Lease
	if lease == 0 {
		lease = DefaultMergeLease
	}
	if lease < 0 {
		return fmt.Errorf("merge coordinator lease must be positive")
	}
	owner := strings.TrimSpace(config.Owner)
	if owner == "" {
		host, hostErr := os.Hostname()
		if hostErr != nil || strings.TrimSpace(host) == "" {
			host = "merge-coordinator"
		}
		owner = host + "-" + strconv.Itoa(os.Getpid())
	}
	newOperationID := config.NewOperationID
	if newOperationID == nil {
		newOperationID = randomOperationID
	}
	coordinator, err := services.NewCoordinator(services.CoordinatorConfig{
		TenantID:       config.TenantID,
		Owner:          owner,
		Lease:          lease,
		Store:          store,
		Effects:        effects,
		Clock:          config.Clock,
		NewOperationID: newOperationID,
	})
	if err != nil {
		return err
	}
	return Run(ctx, Config{
		Kind:           services.KindMergeCoordinator,
		ServiceID:      config.ServiceID,
		TenantID:       config.TenantID,
		ProjectID:      config.ProjectID,
		RescanInterval: config.RescanInterval,
		Transport:      config.Transport,
		Events:         config.Events,
		Clock:          config.Clock,
		Merges:         coordinator,
	})
}

// randomOperationID returns one random durable operation identity.
func randomOperationID() (id.OperationID, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("generate merge operation identity: %w", err)
	}
	return id.OperationID("merge-" + hex.EncodeToString(buffer)), nil
}
