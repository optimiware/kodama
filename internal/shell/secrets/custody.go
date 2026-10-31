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
	for _, sub := range []string{"material", "index", "stage", "tombstones"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o700); err != nil {
			return nil, fmt.Errorf("secret: custody directory: %w", err)
		}
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Custody{root: root, source: cfg.Source, observe: cfg.Observe, log: log}, nil
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

func (c *Custody) tombstoned(id string) bool {
	_, err := os.Lstat(c.path("tombstones", id))
	return err == nil
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
	if c.tombstoned(ref.ID) {
		c.emit(eventOf(ref, EventRefused, "", nil))
		return runtime.Materialization{}, runtime.ErrMaterializationRevoked
	}
	// refused turns a failure into the revocation that caused it, when there was one.
	refused := func(err error) error {
		if c.tombstoned(ref.ID) {
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
	if c.tombstoned(ref.ID) { // under the lock: the revocation and the publication are atomic
		unlock()
		c.emit(eventOf(ref, EventRefused, "", nil))
		return runtime.Materialization{}, runtime.ErrMaterializationRevoked
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

// Revoked reports whether the identity is closed.
func (c *Custody) Revoked(ref runtime.MaterializationRef) bool { return c.tombstoned(ref.ID) }

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
