package gitservice

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// Service is one tenant's Git worker. It is safe for concurrent use.
type Service struct {
	cfg      Config
	layout   layout
	instance string

	gates  gates
	initMu sync.Mutex
	slots  chan struct{}
	// bundleSlots bounds the concurrent bundle creations and replays.
	bundleSlots chan struct{}

	mu       sync.Mutex
	receives map[receiveKey]*receive
	tokens   map[[sha256.Size]byte]*receive
	// acks is the per-repository acknowledgement state of reconcile.go.
	acks map[string]*ackState

	lock     *os.File // the data root's single-writer lock
	closed   sync.Once
	closeErr error

	hookLn   net.Listener
	hookSrv  *http.Server
	hookAddr string

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// maintainHook, when set (tests), runs inside the exclusive gate instead of
	// Git maintenance, to observe the serialization.
	maintainHook func(ctx context.Context, repositoryID string) error
	// bundleHook, when set (tests), runs inside CreateBundle once the refs were
	// captured and before the bundle is written, with the shared gate held.
	bundleHook atomic.Pointer[func(ctx context.Context, repositoryID string)]
	// sweepEntries and snapshotEntries bound a reconcile page and the evidence of
	// its receives (reconcile.go).
	sweepEntries, snapshotEntries int
	// sweepHook, when set (tests), runs in a reconcile sweep after its page of
	// candidate receives was read and before their liveness is checked.
	sweepHook func(repositoryID string)
	// groupGoneHook, when set (tests), replaces the check that a terminated
	// process group is gone.
	groupGoneHook func(pgid int) bool
}

var repositoryIDPattern = regexp.MustCompile(domain.IDPattern)

// New prepares the data root (layout, worker-owned Git configuration, hooks
// directory), starts the loopback listener the hooks use and returns the
// Service. Close releases it.
func New(cfg Config) (*Service, error) {
	cfg, err := cfg.resolve()
	if err != nil {
		return nil, err
	}
	s := &Service{
		cfg:          cfg,
		layout:       newLayout(cfg.DataRoot),
		instance:     cfg.WorkerInstance,
		slots:        make(chan struct{}, cfg.Limits.MaxConcurrent),
		bundleSlots:  make(chan struct{}, cfg.Limits.MaxConcurrentBundles),
		receives:     map[receiveKey]*receive{},
		tokens:       map[[sha256.Size]byte]*receive{},
		acks:         map[string]*ackState{},
		sweepEntries: defaultSweepEntries, snapshotEntries: defaultSnapshotEntries,
	}
	if s.instance == "" {
		s.instance = randomID("wi", 8)
	}
	if !repositoryIDPattern.MatchString(s.instance) {
		return nil, errors.New("gitservice: WorkerInstance must be an opaque ID")
	}
	if err := os.MkdirAll(cfg.DataRoot, 0o700); err != nil {
		return nil, err
	}
	// The generation fence (GR-04): nothing is touched before the lock of the
	// data root is held, which also proves that no process of an earlier worker
	// generation is left.
	if s.lock, err = lockDataRoot(cfg.DataRoot, cfg.Limits.LockWait); err != nil {
		return nil, err
	}
	for _, dir := range []string{s.layout.repositories, s.layout.hooks, s.layout.spool, s.layout.tmp, s.layout.home, s.layout.fences} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			_ = s.lock.Close()
			return nil, err
		}
	}
	fail := func(err error) (*Service, error) {
		_ = s.lock.Close()
		return nil, err
	}
	if err := writeFileAtomic(s.layout.globalConfig, globalConfigFile(serverConfig(s.layout.hooks, cfg.Limits.MaxPushBytes)), 0o600); err != nil {
		return fail(err)
	}
	if err := InstallHooks(s.layout.hooks, cfg.HookBinary); err != nil {
		return fail(err)
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.cleanStale()
	s.recoverStartup()
	if err := s.loadRestoreFences(); err != nil {
		s.cancel()
		return fail(err)
	}
	if err := s.startHookServer(); err != nil {
		s.cancel()
		return fail(err)
	}
	return s, nil
}

// Close stops the hook listener, terminates every running receive's process
// tree and waits for background work.
func (s *Service) Close() error {
	s.closed.Do(func() { s.closeErr = s.close() })
	return s.closeErr
}

func (s *Service) close() error {
	s.cancel()
	s.mu.Lock()
	running := make([]*receive, 0, len(s.receives))
	for _, r := range s.receives {
		running = append(running, r)
	}
	s.mu.Unlock()
	for _, r := range running {
		r.terminate("worker shutting down")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := s.hookSrv.Shutdown(ctx)
	s.wg.Wait()
	_ = s.lock.Close() // releases the data root
	return err
}

// WorkerInstance identifies this process to the control plane.
func (s *Service) WorkerInstance() string { return s.instance }

// cleanStale removes spool files and half-initialized repositories a crash left.
func (s *Service) cleanStale() {
	for _, dir := range []string{s.layout.spool, s.layout.tmp} {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			_ = os.RemoveAll(filepath.Join(dir, e.Name()))
		}
	}
	entries, _ := os.ReadDir(s.layout.repositories)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".init-") {
			_ = os.RemoveAll(filepath.Join(s.layout.repositories, e.Name()))
		}
	}
}

// repoDir derives the repository path from the repository ID only. The ID must
// match the opaque-ID syntax, which cannot contain a separator or a dot, so the
// result is always a direct child of the repositories directory.
func (s *Service) repoDir(repositoryID string) (string, error) {
	if !repositoryIDPattern.MatchString(repositoryID) {
		return "", contractError(domain.CodeInvalidRequest, "repositoryId is not a valid ID")
	}
	return filepath.Join(s.layout.repositories, repositoryID+".git"), nil
}

// existingRepo resolves the path of an initialized repository: an unknown one
// is not_found.
func (s *Service) existingRepo(repositoryID string) (string, error) {
	dir, err := s.repoDir(repositoryID)
	if err != nil {
		return "", err
	}
	if !isRepository(dir) {
		return "", contractError(domain.CodeNotFound, "the repository was not found")
	}
	return dir, nil
}

// repoPath is the path of a repository whose ID is already validated.
func (s *Service) repoPath(repositoryID string) string {
	return filepath.Join(s.layout.repositories, repositoryID+".git")
}

func isRepository(dir string) bool {
	st, err := os.Stat(filepath.Join(dir, "HEAD"))
	return err == nil && st.Mode().IsRegular()
}

func (s *Service) now() time.Time { return s.cfg.Clock().UTC() }

func randomID(prefix string, n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("gitservice: no randomness: %v", err))
	}
	return prefix + "_" + hex.EncodeToString(b)
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// ---- receive registry ----

type receiveKey struct{ repositoryID, receiveID string }

// receive is one running receive-pack request: the identity every hook reports
// under, its authorization state and the means to terminate its process tree.
type receive struct {
	id, repositoryID, grantID string
	token                     string
	dir                       string
	service                   *Service

	mu         sync.Mutex
	pushID     string
	authorized map[string]gitv1.RefUpdate
	deadline   time.Time
	timer      *time.Timer
	kill       func()
	terminated string
	idShown    bool
	done       chan struct{}
}

// takePushIDForClient reports true exactly once per receive.
func (r *receive) takePushIDForClient() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.idShown {
		return false
	}
	r.idShown = true
	return true
}

// startReceive registers a receive and returns it with its per-receive token.
func (s *Service) startReceive(repositoryID, grantID, dir string) *receive {
	r := &receive{
		id:           gitv1.NewReceiveID(grantID),
		repositoryID: repositoryID,
		grantID:      grantID,
		token:        randomID("hk", 32),
		dir:          dir,
		service:      s,
		done:         make(chan struct{}),
	}
	s.mu.Lock()
	s.receives[receiveKey{repositoryID, r.id}] = r
	s.tokens[sha256.Sum256([]byte(r.token))] = r
	s.mu.Unlock()
	return r
}

// finish removes the receive: it is no longer running, its token is void and
// its deadline timer is cancelled.
func (r *receive) finish() {
	s := r.service
	s.mu.Lock()
	delete(s.receives, receiveKey{r.repositoryID, r.id})
	delete(s.tokens, sha256.Sum256([]byte(r.token)))
	s.mu.Unlock()
	r.mu.Lock()
	if r.timer != nil {
		r.timer.Stop()
	}
	r.mu.Unlock()
	close(r.done)
}

func (r *receive) setKill(kill func()) {
	r.mu.Lock()
	r.kill = kill
	r.mu.Unlock()
}

// terminate kills the receive's whole process group (once; idempotent).
func (r *receive) terminate(reason string) {
	r.mu.Lock()
	kill := r.kill
	if r.terminated == "" {
		r.terminated = reason
	}
	r.mu.Unlock()
	if kill != nil {
		kill()
	}
}

func (r *receive) terminatedFor() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.terminated
}

// arm records the authorization (push ID and the exact updates it allowed) and
// schedules the termination of the process tree at the control plane's deadline.
func (r *receive) arm(pushID string, updates []gitv1.RefUpdate, deadline time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pushID = pushID
	r.authorized = make(map[string]gitv1.RefUpdate, len(updates))
	for _, u := range updates {
		r.authorized[u.Ref] = u
	}
	r.deadline = deadline
	if r.timer != nil {
		r.timer.Stop()
	}
	r.timer = time.AfterFunc(time.Until(deadline), func() { r.terminate("receive deadline passed") })
}

func (r *receive) snapshot() (pushID string, deadline time.Time, authorized map[string]gitv1.RefUpdate) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pushID, r.deadline, r.authorized
}

func (s *Service) receiveByToken(token string) (*receive, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.tokens[sha256.Sum256([]byte(token))]
	return r, ok
}

// receiveRunning reports whether the worker still runs the receive.
func (s *Service) receiveRunning(repositoryID, receiveID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.receives[receiveKey{repositoryID, receiveID}]
	return ok || s.gates.of(repositoryID).receiveFenced(receiveID)
}

// ErrDataRootBusy is returned by New when a process of a previous worker
// generation (or another worker) still holds the data root's lock.
var ErrDataRootBusy = errors.New("gitservice: the data root is held by another Git worker or a process of a previous one")

// lockDataRoot takes the exclusive generation lock of the data root, waiting up
// to wait for it. Every Git process the worker starts inherits the descriptor
// (see execGitTracked), and so do their descendants, the hooks included: the
// lock is therefore released only when the last process of the generation that
// held it has exited. Acquiring it proves that nothing of any earlier
// generation can still hold a repository lock (GR-04).
func lockDataRoot(root string, wait time.Duration) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(root, "worker.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return f, nil
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EINTR {
			f.Close()
			return nil, err
		}
		if !time.Now().Before(deadline) {
			f.Close()
			return nil, fmt.Errorf("%w (%s)", ErrDataRootBusy, root)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// recoverStartup clears what an earlier generation left in the repositories:
// ref and packed-refs locks and incoming-object quarantines. It runs
// synchronously before any handler is served and only after the generation lock
// was acquired, which proves every process that could own such a file is gone,
// so each one is abandoned. Refs and the journal are never modified: the
// prepared entries of interrupted transactions are the evidence settlement
// reads.
func (s *Service) recoverStartup() {
	entries, _ := os.ReadDir(s.layout.repositories)
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".git")
		if !ok || !e.IsDir() || !repositoryIDPattern.MatchString(id) {
			continue
		}
		dir := filepath.Join(s.layout.repositories, e.Name())
		if removed := removeStaleLocks(dir); len(removed) > 0 {
			s.cfg.Logger.Warn("removed what an earlier worker generation left", "repositoryId", id, "paths", removed)
		}
	}
}
