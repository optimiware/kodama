package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/userland/git"
	"github.com/optimiware/kodama/internal/userland/git/app/native"
)

// Recovery sweep bounds. The interval is a fixed production default: there is
// no existing interval configuration surface for `git serve`, so the sweep
// does not invent one. The batch bounds the expensive work (an actual
// Service.Recover attempt) per pass. The scan limit is a separate, generous
// bound on cheap journal loads a periodic pass may examine: settled entries do
// not consume the batch, so without it one tick could walk a very large
// journal. The first pass after startup is not scan-bounded, so every pending
// entry left by a crash is reached at startup.
const (
	defaultRecoveryInterval  = 30 * time.Second
	defaultRecoveryBatch     = 128
	defaultRecoveryScanLimit = 4096
	terminalJournalMaxAge    = 24 * time.Hour
)

// ErrJournalInRepository reports a --journal-dir that is a served repository
// path or lies inside one. The receive journal is service state, not
// repository content: creating it below a repository would wedge provisioning
// and mix journal files into a served path.
var ErrJournalInRepository = errors.New("receive journal directory is inside a served repository")

// StartRecovery starts the startup-and-periodic report recovery sweep. The
// first pass runs immediately, before the first interval elapses, and examines
// the whole journal directory (bounded only by the recovery batch and
// cancellation), so an accepted entry left by a crash between acceptance and
// reporting is reached and retried at startup, however many settled entries sit
// ahead of it. Later passes are bounded. It is idempotent and safe for
// concurrent use, and a canceled context stops the loop. CloseRecovery joins
// the goroutine.
func (server *Server) StartRecovery(ctx context.Context) error {
	server.recoveryMutex.Lock()
	defer server.recoveryMutex.Unlock()
	if server.recoveryLive {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	interval := server.recoveryInterval
	if interval <= 0 {
		interval = defaultRecoveryInterval
	}
	sweepCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	server.recoveryCancel = cancel
	server.recoveryDone = done
	server.recoveryLive = true
	go func() {
		defer close(done)
		server.recoveryLoop(sweepCtx, interval)
	}()
	return nil
}

// CloseRecovery stops the recovery sweep and joins its goroutine. It is safe to
// call more than once, including before StartRecovery.
func (server *Server) CloseRecovery() error {
	server.recoveryMutex.Lock()
	if !server.recoveryLive {
		server.recoveryMutex.Unlock()
		return nil
	}
	cancel := server.recoveryCancel
	done := server.recoveryDone
	server.recoveryLive = false
	server.recoveryCancel = nil
	server.recoveryDone = nil
	server.recoveryMutex.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
	return nil
}

// recoveryLoop runs one immediate pass and then one bounded pass per interval
// until the context is canceled.
func (server *Server) recoveryLoop(ctx context.Context, interval time.Duration) {
	_ = server.RecoverPending(ctx)
	if ctx.Err() != nil {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = server.RecoverPending(ctx)
		}
	}
}

// RecoverPending performs one recovery sweep synchronously. It reclaims at most
// recoveryBatch accepted entries through the existing Service.Recover
// report-only path and its idempotent report identity, so a large backlog
// cannot turn one tick into an unbounded sequence of report attempts. Cheap
// skips are bounded separately and generously: a periodic pass examines at most
// recoveryScanLimit journal entries, while the first pass after startup
// examines the whole directory so an accepted entry behind any amount of
// settled history is reached at startup. Settled (reported or refused) entries
// never consume the recovery batch.
//
// Intended entries are reconciled only while no git-receive-pack exchange is
// active for their repository: a fresh intent can belong to an in-flight
// receive-pack transaction, and the reconciliation observes the ref to decide
// between not-applied and applied. No age bound overrides that, because a slow
// push can outlast any fixed bound. Uncertain entries are left untouched on a
// timer. A reported or refused entry is a settled no-op and no effect is
// ever replayed. Per-entry failures are logged by fault class and do not stop
// the pass; the next pass retries them. The context is checked between entries,
// so cancellation stops even the unbounded startup pass promptly.
func (server *Server) RecoverPending(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	keys, err := server.journalKeys()
	if err != nil {
		server.logRecoveryFailure(ctx, err)
		return nil
	}
	server.recoveryMutex.Lock()
	if len(keys) == 0 {
		server.recoveryOffset = 0
		server.recoveryFirstSweep = false
		server.recoveryMutex.Unlock()
		return nil
	}
	batch := server.recoveryBatch
	if batch <= 0 {
		batch = defaultRecoveryBatch
	}
	scanLimit := server.recoveryScanLimit
	if scanLimit <= 0 {
		scanLimit = defaultRecoveryScanLimit
	}
	if server.recoveryFirstSweep {
		// The startup pass reaches every entry: only the recovery batch and
		// cancellation bound it.
		scanLimit = len(keys)
	}
	start := server.recoveryOffset % len(keys)
	server.recoveryMutex.Unlock()

	now := server.currentTime()
	var pruneCutoff time.Time
	if !now.IsZero() {
		pruneCutoff = now.Add(-terminalJournalMaxAge)
	}

	scanned := 0
	recovered := 0
	pruned := 0
	for index := range keys {
		if recovered >= batch || scanned >= scanLimit {
			break
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		key := keys[(start+index)%len(keys)]
		scanned++
		entry, ok, loadErr := server.journal.Load(ctx, key)
		if loadErr != nil {
			server.logRecoveryFailure(ctx, loadErr)
			continue
		}
		if !ok {
			// The entry disappeared between the directory scan and the load.
			continue
		}
		if (entry.State == git.JournalReported || entry.State == git.JournalRefused) &&
			!pruneCutoff.IsZero() && !entry.At.IsZero() && entry.At.Before(pruneCutoff) {
			removed, pruneErr := server.journal.PruneTerminalEntry(ctx, key, pruneCutoff)
			if pruneErr != nil {
				server.logRecoveryFailure(ctx, pruneErr)
			} else if removed {
				pruned++
			}
			continue
		}
		if !server.recoveryPending(entry) {
			continue
		}
		recovered++
		if _, err := server.service.Recover(ctx, key); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			// An uncertain effect is a durable, expected state: the entry stays
			// recoverable and is retried on a later pass. Any other failure is
			// logged by class without its private cause.
			if !errors.Is(err, git.ErrUncertain) {
				server.logRecoveryFailure(ctx, err)
			}
		}
	}
	server.recoveryMutex.Lock()
	remaining := len(keys) - pruned
	if remaining <= 0 {
		server.recoveryOffset = 0
	} else {
		advanced := start + scanned - pruned
		if advanced < 0 {
			advanced = 0
		}
		server.recoveryOffset = advanced % remaining
	}
	server.recoveryFirstSweep = false
	server.recoveryMutex.Unlock()
	return nil
}

// recoveryPending reports whether the sweep may reconcile a durable entry.
// Accepted entries always retry their report. An intended entry is reconciled
// only while its repository has no git-receive-pack exchange in flight: an age
// bound must never override that, because a slow push can run for longer than
// any fixed bound, and settling its intent first would mark a push refused that
// receive-pack then applies. The active count is in-process state that a
// deferred call releases when the exchange returns, so it cannot outlive the
// exchange, and a restart begins from zero: the first sweep after a crash
// reconciles every intended entry. Uncertain entries stay untouched on a timer.
func (server *Server) recoveryPending(entry git.JournalEntry) bool {
	switch entry.State {
	case git.JournalAccepted:
		return true
	case git.JournalIntended:
		return !server.hasActiveReceivePack(entry.Repository)
	default:
		return false
	}
}

func (server *Server) currentTime() time.Time {
	if server.clock != nil {
		return server.clock.Now()
	}
	return time.Now()
}

// beginReceivePack marks one repository's git-receive-pack exchange as active
// and returns the callback that releases the active repository count.
func (server *Server) beginReceivePack(repository string) func() {
	server.receiveMutex.Lock()
	if server.activeReceives == nil {
		server.activeReceives = make(map[string]int)
	}
	if repository != "" {
		server.activeReceives[repository]++
	}
	server.receiveMutex.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			if repository == "" {
				return
			}
			server.receiveMutex.Lock()
			server.activeReceives[repository]--
			if server.activeReceives[repository] <= 0 {
				delete(server.activeReceives, repository)
			}
			server.receiveMutex.Unlock()
		})
	}
}

func (server *Server) hasActiveReceivePack(repository string) bool {
	server.receiveMutex.Lock()
	defer server.receiveMutex.Unlock()
	return server.activeReceives[repository] > 0
}

// journalKeys lists the durable source-event keys by scanning the journal
// directory this service owns. The format is the concrete FileJournal layout:
// one <source-event-id>.json file directly under the directory. A load still
// validates every entry, so a foreign or malformed file is a logged error, not
// a guessed entry.
func (server *Server) journalKeys() ([]string, error) {
	if server.journal == nil || strings.TrimSpace(server.journalDirectory) == "" {
		return nil, fmt.Errorf("git serve recovery journal is not configured")
	}
	entries, err := os.ReadDir(server.journalDirectory)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		key, ok := strings.CutSuffix(entry.Name(), ".json")
		if !ok || key == "" {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys, nil
}

// logRecoveryFailure records one recovery failure by its public fault class.
// The private cause is never rendered: it may carry a path, a response body or
// credential material. A canceled context is shutdown, not a fault.
func (server *Server) logRecoveryFailure(ctx context.Context, err error) {
	if err == nil || ctx.Err() != nil {
		return
	}
	class := fault.InternalFailure
	if faultClass, ok := fault.ClassOf(err); ok {
		class = faultClass
	}
	logger := server.logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Warn("git receive recovery failed", "fault", string(class))
}

// requireJournalOutsideRepositories enforces the journal placement rule: the
// receive journal may neither equal nor lie inside any served repository path.
// The admin policy repository is always served; the configured resource
// repositories are the rest. Paths are resolved through their longest existing
// ancestor so a symbolic link cannot hide the containment.
func requireJournalOutsideRepositories(journalDirectory string, storage git.Storage, tenant id.TenantID, resourceIDs map[string]string) error {
	repositories := make(map[string]string, len(resourceIDs)+1)
	for repository := range resourceIDs {
		repositoryPath, err := storage.RepositoryPath(tenant, repository)
		if err != nil {
			// An invalid configured repository name is refused by the smart-HTTP
			// resolver; it cannot be a served repository root here.
			continue
		}
		repositories[repository] = repositoryPath
	}
	adminPath, err := storage.RepositoryPath(tenant, native.AdminRepository)
	if err == nil {
		repositories[native.AdminRepository] = adminPath
	}
	return requireJournalOutsidePaths(journalDirectory, repositories)
}

// requireJournalOutsidePaths enforces the placement rule for one concrete set
// of served repository paths. It is shared by the serving and provisioning
// entry points so both refuse the same configuration.
func requireJournalOutsidePaths(journalDirectory string, repositories map[string]string) error {
	journalPath, err := filepath.Abs(journalDirectory)
	if err != nil {
		return fmt.Errorf("resolve git journal directory: %w", err)
	}
	resolvedJournal := resolveExistingPath(journalPath)
	for repository, repositoryPath := range repositories {
		if pathAtOrInside(resolveExistingPath(repositoryPath), resolvedJournal) {
			return fmt.Errorf("%w: %q is at or inside served repository %q", ErrJournalInRepository, journalDirectory, repository)
		}
	}
	return nil
}

// pathAtOrInside reports whether path is base itself or stays inside base.
func pathAtOrInside(base, path string) bool {
	relative, err := filepath.Rel(base, path)
	if err != nil {
		return false
	}
	if relative == "." {
		return true
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// resolveExistingPath resolves symbolic links for the longest existing
// ancestor of path and keeps the remaining segments. It never requires the full
// path to exist, so a not-yet-created journal directory is still compared.
func resolveExistingPath(path string) string {
	cleaned := filepath.Clean(path)
	var suffix []string
	current := cleaned
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for _, s := range slices.Backward(suffix) {
				resolved = filepath.Join(resolved, s)
			}
			return resolved
		}
		if !os.IsNotExist(err) {
			return cleaned
		}
		parent := filepath.Dir(current)
		if parent == current {
			return cleaned
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}
