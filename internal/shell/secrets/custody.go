package secret

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
)

// ErrUnavailable reports a declared reference that could not be materialized:
// the source does not know it, refused it, was unreachable or returned another
// version than the one pinned. The message names the reference, never a value,
// and never the source's own error text. The engine records it as
// runtime.ReasonSecretUnavailable.
var ErrUnavailable = errors.New("secret: reference unavailable")

// Secret is one resolved value and the version it was resolved at. The caller of
// Fetch owns Value; custody clears it as soon as it is written.
type Secret struct {
	Value   []byte
	Version string
}

// Source supplies the value of one declared reference for the attempt that owns
// the materialization. The attempt is passed so a source can refuse a run that
// ended (run-scoped Git credentials): Fetch is only asked about references the
// plan declared for that attempt, within the tenant's and project's authority.
type Source interface {
	Fetch(ctx context.Context, owner runtime.MaterializationRef, ref runtime.SecretRef) (Secret, error)
}

// EventKind is what happened to one materialization.
type EventKind string

// Observable custody events. They carry identity, revision and reference names,
// never a value.
const (
	// EventResolved: material was published for an identity.
	EventResolved EventKind = "resolved"
	// EventRotated: a repeated Resolve replaced material with another revision.
	EventRotated EventKind = "rotated"
	// EventRefused: a Resolve for a revoked identity published nothing.
	EventRefused EventKind = "refused"
	// EventRevoked: the identity was closed and its material removed.
	EventRevoked EventKind = "revoked"
)

// Event is an audit record of custody. It is safe to log and to retain.
type Event struct {
	Kind     EventKind
	ID       string
	Scope    domain.Scope
	RunID    domain.RunID
	StepKey  domain.StepID
	Attempt  int
	Revision string
	Names    []string
}

// CustodyConfig builds a Custody.
type CustodyConfig struct {
	// Root is the host directory holding every materialization. It must be
	// absolute and never inside a run's /workspace.
	Root   string
	Source Source
	// Observe receives every event after it took effect; nil observes nothing.
	Observe func(Event)
	Logger  *slog.Logger
}

// Custody is the shell's SecretResolver. Everything it knows lives on disk under
// Root, named by the hash of the durable identity of the attempt:
//
//	material/<h>/<NAME>  published material, one private file per declared reference
//	index/<h>            the published revision, for rotation
//	stage/<h>-*          material being prepared, never readable by a workload
//	tombstones/<h>       the durable record that Revoke closed the identity
//
// Publication and revocation of one root are serialized by a lock that holds
// across processes (flock) and across instances in one process, and Resolve
// checks the tombstone under it, so a Resolve in flight at a revocation is
// refused at publication and publishes nothing, and a replacement process over
// the same root honours the tombstones of the one it replaces.
type Custody struct {
	root    string
	source  Source
	observe func(Event)
	log     *slog.Logger

	// beforePublish runs after the material is prepared and before it is
	// published (tests hold a Resolve there).
	beforePublish func()
	// underLock runs while the publication lock is held and the identity was
	// confirmed open, before anything is published (tests hold a process there).
	underLock func()
}

// NewCustody opens the custody directory, creating its layout.
func NewCustody(cfg CustodyConfig) (*Custody, error) {
	if cfg.Source == nil {
		return nil, errors.New("secret: custody needs a source")
	}
	if cfg.Root == "" || !filepath.IsAbs(cfg.Root) {
		return nil, errors.New("secret: the custody root must be an absolute directory")
	}
	root := filepath.Clean(cfg.Root)
	if root == "/workspace" || strings.HasPrefix(root, "/workspace/") {
		return nil, errors.New("secret: the custody root must be outside /workspace")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	c := &Custody{root: root, source: cfg.Source, observe: cfg.Observe, log: log}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("secret: custody directory: %w", err)
	}
	if err := c.open(); err != nil {
		return nil, err
	}
	return c, nil
}

// initMarker is written, synced, once a root is initialised. Its presence is what
// tells reopening an initialised root from initialising a fresh one.
const initMarker = ".initialized"

// open initialises a fresh root or reopens an initialised one, under the root's
// lock so that two processes opening a fresh root agree.
//
// The tombstones are the revocation state and are the only thing here that
// cannot be recreated: an empty directory in their place would silently reopen
// every revoked identity. So an initialised root whose tombstone directory is
// gone is refused with ErrRevocationState, never repaired, and so is a root
// without a marker that holds published material but no tombstones. Only a root
// that never published anything is initialised, and a root from before the marker
// existed is adopted when its tombstone directory is there.
func (c *Custody) open() error {
	unlock, err := c.lock()
	if err != nil {
		return fmt.Errorf("secret: lock: %w", err)
	}
	defer unlock()

	tombs := filepath.Join(c.root, "tombstones")
	initialised, err := exists(filepath.Join(c.root, initMarker))
	if err != nil {
		return fmt.Errorf("%w: %w", ErrRevocationState, err)
	}
	hasTombs, err := isDir(tombs)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrRevocationState, err)
	}
	switch {
	case initialised && !hasTombs:
		return fmt.Errorf("%w: the tombstone directory of an initialised custody root is missing", ErrRevocationState)
	case !initialised && !hasTombs:
		for _, sub := range []string{"material", "index"} {
			entries, err := os.ReadDir(filepath.Join(c.root, sub))
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("%w: %w", ErrRevocationState, err)
			}
			if len(entries) > 0 {
				return fmt.Errorf("%w: the root holds published material but no tombstones", ErrRevocationState)
			}
		}
	}
	// The tombstones first: they are what the marker vouches for.
	for _, sub := range []string{"tombstones", "material", "index", "stage"} {
		if err := os.MkdirAll(filepath.Join(c.root, sub), 0o700); err != nil {
			return fmt.Errorf("secret: custody directory: %w", err)
		}
	}
	if initialised {
		return nil
	}
	for _, d := range []string{filepath.Join(c.root, "tombstones"), filepath.Join(c.root, "material"), filepath.Join(c.root, "index"), filepath.Join(c.root, "stage"), c.root} {
		if err := syncDir(d); err != nil {
			return fmt.Errorf("secret: custody directory: %w", err)
		}
	}
	if err := writeFileAtomic(filepath.Join(c.root, initMarker), []byte("kodama custody v1\n")); err != nil {
		return fmt.Errorf("secret: initialise custody: %w", err)
	}
	return nil
}

// exists reports whether name is there; any error but absence is returned.
func exists(name string) (bool, error) {
	_, err := os.Lstat(name)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, err
	}
}

// isDir reports whether name is a directory; a non-directory is an error.
func isDir(name string) (bool, error) {
	st, err := os.Lstat(name)
	switch {
	case err == nil && st.IsDir():
		return true, nil
	case err == nil:
		return false, fmt.Errorf("%s is not a directory", name)
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, err
	}
}

var _ runtime.SecretResolver = (*Custody)(nil)

// processLocks serializes the instances of one process over one root; flock
// serializes processes.
var processLocks sync.Map

func (c *Custody) lock() (unlock func(), err error) {
	m, _ := processLocks.LoadOrStore(c.root, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	f, err := os.OpenFile(filepath.Join(c.root, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		mu.Unlock()
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		mu.Unlock()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		mu.Unlock()
	}, nil
}

// key is the file name of an identity: a hash, so a step key never reaches the
// file system.
func key(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:16])
}

func (c *Custody) path(sub, id string) string { return filepath.Join(c.root, sub, key(id)) }

// ErrRevocationState reports that whether an identity is closed could not be
// determined. Custody fails closed on it: nothing is published while the answer
// is unknown, because a storage error must never reopen a revoked identity.
var ErrRevocationState = errors.New("secret: cannot read the revocation state")

// tombstoned reports whether the identity is closed. Only the confirmed absence
// of its tombstone, in a tombstone directory that itself exists, is "open"; a
// permission, I/O or any other error is returned and is never read as absence.
func (c *Custody) tombstoned(id string) (bool, error) {
	_, err := os.Lstat(c.path("tombstones", id))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		// An absent tombstone only counts when the directory that would hold it is
		// really there: a vanished directory is lost state, not an open identity.
		if _, derr := os.Stat(filepath.Join(c.root, "tombstones")); derr != nil {
			return false, fmt.Errorf("%w: %w", ErrRevocationState, derr)
		}
		return false, nil
	default:
		return false, fmt.Errorf("%w: %w", ErrRevocationState, err)
	}
}

func (c *Custody) emit(e Event) {
	c.log.Info("secret custody", "event", string(e.Kind), "id", e.ID, "revision", e.Revision, "refs", len(e.Names))
	if c.observe != nil {
		c.observe(e)
	}
}

func eventOf(ref runtime.MaterializationRef, kind EventKind, revision string, names []string) Event {
	return Event{Kind: kind, ID: ref.ID, Scope: ref.Scope, RunID: ref.RunID, StepKey: ref.StepKey, Attempt: ref.Attempt,
		Revision: revision, Names: names}
}

// checkRefs refuses a declaration the attempt may not receive: an invalid
// reference, one the scope cannot reach and a name that appears twice.
func checkRefs(scope domain.Scope, refs []runtime.SecretRef) error {
	seen := map[string]bool{}
	for _, r := range refs {
		if err := r.Validate(); err != nil {
			return err
		}
		if !r.Owner.AvailableTo(scope) {
			return fmt.Errorf("%w: reference %s is not available to %s", domain.ErrInvalidValue, r.Name, scope)
		}
		if seen[r.Name] {
			return fmt.Errorf("%w: reference %s is declared twice", domain.ErrInvalidValue, r.Name)
		}
		seen[r.Name] = true
	}
	return nil
}

// Resolve materializes the declared references under the identity req.Ref.
func (c *Custody) Resolve(ctx context.Context, req runtime.ResolveRequest) (runtime.Materialization, error) {
	ref := req.Ref
	if err := ref.Validate(); err != nil {
		return runtime.Materialization{}, err
	}
	if err := checkRefs(ref.Scope, req.Refs); err != nil {
		return runtime.Materialization{}, err
	}
	switch closed, err := c.tombstoned(ref.ID); {
	case err != nil:
		return runtime.Materialization{}, err
	case closed:
		c.emit(eventOf(ref, EventRefused, "", nil))
		return runtime.Materialization{}, runtime.ErrMaterializationRevoked
	}
	// refused turns a failure into the revocation that caused it, when there was one.
	refused := func(err error) error {
		switch closed, serr := c.tombstoned(ref.ID); {
		case serr != nil:
			return errors.Join(err, serr)
		case closed:
			c.emit(eventOf(ref, EventRefused, "", nil))
			return runtime.ErrMaterializationRevoked
		}
		return err
	}
	stage, err := os.MkdirTemp(filepath.Join(c.root, "stage"), key(ref.ID)+"-")
	if err != nil {
		return runtime.Materialization{}, refused(fmt.Errorf("secret: stage material: %w", err))
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(stage)
		}
	}()
	h := sha256.New()
	names := make([]string, 0, len(req.Refs))
	for _, r := range req.Refs {
		s, err := c.source.Fetch(ctx, ref, r)
		if err != nil {
			return runtime.Materialization{}, refused(fmt.Errorf("%w: %s", ErrUnavailable, r.Name))
		}
		if r.Version != "" && s.Version != r.Version {
			clear(s.Value)
			return runtime.Materialization{}, refused(fmt.Errorf("%w: %s is not at version %s", ErrUnavailable, r.Name, r.Version))
		}
		werr := os.WriteFile(filepath.Join(stage, r.Name), s.Value, 0o400)
		clear(s.Value)
		if werr != nil {
			return runtime.Materialization{}, refused(fmt.Errorf("secret: stage %s: %w", r.Name, werr))
		}
		fmt.Fprintf(h, "%s/%s/%s\x00%s\x00%s\n", r.Owner.TenantID, r.Owner.ProjectID, r.Name, r.Version, s.Version)
		names = append(names, r.Name)
	}
	sort.Strings(names)
	revision := hex.EncodeToString(h.Sum(nil))[:32]
	if c.beforePublish != nil {
		c.beforePublish()
	}

	unlock, err := c.lock()
	if err != nil {
		return runtime.Materialization{}, fmt.Errorf("secret: lock: %w", err)
	}
	// Under the lock the revocation and the publication are atomic. Publication
	// needs the confirmed absence of the tombstone; an unreadable one refuses.
	switch closed, err := c.tombstoned(ref.ID); {
	case err != nil:
		unlock()
		return runtime.Materialization{}, err
	case closed:
		unlock()
		c.emit(eventOf(ref, EventRefused, "", nil))
		return runtime.Materialization{}, runtime.ErrMaterializationRevoked
	}
	if c.underLock != nil {
		c.underLock()
	}
	dir := c.path("material", ref.ID)
	previous, _ := os.ReadFile(c.path("index", ref.ID))
	err = os.RemoveAll(dir) // idempotent on the identity: replace, never add a copy
	if err == nil {
		err = os.Rename(stage, dir)
	}
	if err == nil {
		published = true
		err = writeFileAtomic(c.path("index", ref.ID), []byte(revision))
	}
	unlock()
	if err != nil {
		if published {
			// The material is published without its index entry: take it back.
			_ = os.RemoveAll(dir)
		}
		return runtime.Materialization{}, fmt.Errorf("secret: publish material: %w", err)
	}
	kind := EventResolved
	if len(previous) > 0 && string(previous) != revision {
		kind = EventRotated
	}
	c.emit(eventOf(ref, kind, revision, names))
	return runtime.Materialization{Scope: ref.Scope, RunID: ref.RunID, StepKey: ref.StepKey, Attempt: ref.Attempt, ID: ref.ID,
		Directory: dir, Refs: req.Refs, Revision: revision}, nil
}

// Revoke closes the identity durably, then removes whatever it published or was
// preparing. It needs the identity alone.
func (c *Custody) Revoke(_ context.Context, ref runtime.MaterializationRef) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	unlock, err := c.lock()
	if err != nil {
		return fmt.Errorf("secret: lock: %w", err)
	}
	defer unlock()
	// Closing the identity comes first and is synced: from here on no Resolve
	// publishes, whatever happens to the removal below or to this process.
	if err := writeFileAtomic(c.path("tombstones", ref.ID), nil); err != nil {
		return fmt.Errorf("secret: close identity: %w", err)
	}
	errs := []error{
		os.RemoveAll(c.path("material", ref.ID)),
		os.RemoveAll(c.path("index", ref.ID)),
	}
	staged, err := filepath.Glob(filepath.Join(c.root, "stage", key(ref.ID)+"-*"))
	errs = append(errs, err)
	for _, s := range staged {
		errs = append(errs, os.RemoveAll(s))
	}
	for _, sub := range []string{"material", "index", "stage"} {
		errs = append(errs, syncDir(filepath.Join(c.root, sub)))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("secret: remove material: %w", err)
	}
	c.emit(eventOf(ref, EventRevoked, "", nil))
	return nil
}

// Published lists the identities (as file names, never values) whose material is
// currently readable by a workload.
func (c *Custody) Published() []string {
	entries, _ := os.ReadDir(filepath.Join(c.root, "material"))
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// Revoked reports whether the identity is closed. When that cannot be
// determined it returns the error and true: an unknown state is never "open".
func (c *Custody) Revoked(ref runtime.MaterializationRef) (bool, error) {
	closed, err := c.tombstoned(ref.ID)
	return closed || err != nil, err
}

// writeFileAtomic replaces name with data (mode 0600), synced, so a reader never
// sees a partial file and a crash leaves the old or the new content.
func writeFileAtomic(name string, data []byte) error {
	dir := filepath.Dir(name)
	f, err := os.CreateTemp(dir, ".tmp-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(data)
	serr := f.Sync()
	cerr := f.Close()
	if err := errors.Join(werr, serr, cerr, os.Chmod(tmp, 0o600)); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, name); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	// Some file systems refuse to sync a directory; that is not a failure.
	if errors.Is(err, fs.ErrInvalid) || errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) {
		return nil
	}
	return err
}
