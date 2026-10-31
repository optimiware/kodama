package runner_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/kernel/runtime"
	"github.com/optimiware/kodama/test/support/kernelcontracts"
)

// rootLocks serializes publication and revocation per directory, across every
// diskSecrets instance over it: the in-process stand-in for the atomicity a real
// resolver gets from its store.
var rootLocks sync.Map

// diskSecrets is a SecretResolver that really creates private material on disk and
// really removes it, so a fake cannot conceal what a recovering engine is able to
// revoke. Everything it knows is on disk under root, named by the durable identity
// of the attempt: material/<id> is published material, stage/ holds material being
// prepared, and tombstones/<id> is the durable record that Revoke closed the
// identity. Resolve prepares the material in stage/ and publishes it with one
// rename, under the same lock Revoke takes, after checking the tombstone: so a
// Resolve in flight at a revocation is refused at publication and publishes
// nothing, and a second instance over the same root (a replacement engine's)
// revokes, and honours the tombstone of, what the first did. It satisfies the
// shared resolver contract (see Test_unit_kernel_secrets_disk_contract). The
// fakeSecrets stay for fault injection only.
type diskSecrets struct {
	root string
	// lax makes the resolver ignore its tombstones, a resolver that breaks the
	// contract; it exists to prove the engine's own recheck after Resolve.
	lax bool
	// failRevokes fails that many Revoke calls before any succeeds.
	failRevokes int

	holdMu  sync.Mutex
	hold    chan struct{}
	entered chan struct{}

	// beforeResolve runs at the start of Resolve, before any material exists.
	beforeResolve func()
	// afterResolve runs once the material is published, before Resolve returns (a
	// panic stands for a crash between the resolver and the engine's next commit).
	afterResolve func()
	// onReturn runs whenever Resolve returns, published or refused (a panic stands
	// for a crash right after it).
	onReturn func()
	// resolveErr, when set, makes Resolve publish its material and then fail.
	resolveErr error
	// beforeRevoke runs at the start of every Revoke (a panic stands for a crash).
	beforeRevoke func()
}

func newDiskSecrets(t testing.TB) *diskSecrets {
	t.Helper()
	d := &diskSecrets{root: t.TempDir()}
	for _, sub := range []string{"material", "stage", "tombstones"} {
		if err := os.MkdirAll(filepath.Join(d.root, sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

// sibling is another resolver instance over the same directory, as a replacement
// worker's would be: it shares nothing with d but the disk.
func (d *diskSecrets) sibling() *diskSecrets { return &diskSecrets{root: d.root, lax: d.lax} }

var idPath = strings.NewReplacer(":", "_", "/", "_")

func (d *diskSecrets) dir(id string) string {
	return filepath.Join(d.root, "material", idPath.Replace(id))
}
func (d *diskSecrets) tomb(id string) string {
	return filepath.Join(d.root, "tombstones", idPath.Replace(id))
}

func (d *diskSecrets) lock() *sync.Mutex {
	m, _ := rootLocks.LoadOrStore(d.root, &sync.Mutex{})
	return m.(*sync.Mutex)
}

func (d *diskSecrets) tombstoned(id string) bool {
	_, err := os.Stat(d.tomb(id))
	return err == nil
}

func (d *diskSecrets) Resolve(_ context.Context, req runtime.ResolveRequest) (runtime.Materialization, error) {
	r := req.Ref
	if err := r.Validate(); err != nil {
		return runtime.Materialization{}, err
	}
	if d.onReturn != nil {
		defer d.onReturn()
	}
	if d.beforeResolve != nil {
		d.beforeResolve()
	}
	stage, err := os.MkdirTemp(filepath.Join(d.root, "stage"), "s-")
	if err != nil {
		return runtime.Materialization{}, err
	}
	if err := os.WriteFile(filepath.Join(stage, "TOKEN"), []byte("s3cret-"+string(r.StepKey)), 0o400); err != nil {
		_ = os.RemoveAll(stage)
		return runtime.Materialization{}, err
	}
	d.holdMu.Lock()
	hold, entered := d.hold, d.entered
	d.hold, d.entered = nil, nil
	d.holdMu.Unlock()
	if hold != nil { // prepared, not yet published
		close(entered)
		<-hold
	}
	dir := d.dir(r.ID)
	l := d.lock()
	l.Lock()
	if !d.lax && d.tombstoned(r.ID) {
		l.Unlock()
		_ = os.RemoveAll(stage)
		return runtime.Materialization{}, runtime.ErrMaterializationRevoked
	}
	// Idempotent on the identity: replace, never add a copy.
	err = os.RemoveAll(dir)
	if err == nil {
		err = os.Rename(stage, dir)
	}
	l.Unlock()
	if err != nil {
		_ = os.RemoveAll(stage)
		return runtime.Materialization{}, err
	}
	if d.afterResolve != nil {
		d.afterResolve()
	}
	if d.resolveErr != nil {
		return runtime.Materialization{}, d.resolveErr
	}
	return runtime.Materialization{Scope: r.Scope, RunID: r.RunID, StepKey: r.StepKey, Attempt: r.Attempt, ID: r.ID,
		Directory: dir, Refs: req.Refs, Revision: "rev-" + r.ID}, nil
}

func (d *diskSecrets) Revoke(_ context.Context, ref runtime.MaterializationRef) error {
	if d.beforeRevoke != nil {
		d.beforeRevoke()
	}
	if err := ref.Validate(); err != nil {
		return err
	}
	l := d.lock()
	l.Lock()
	defer l.Unlock()
	if d.failRevokes > 0 {
		d.failRevokes--
		return errors.New("vault unavailable")
	}
	if !d.lax {
		if err := os.WriteFile(d.tomb(ref.ID), nil, 0o600); err != nil {
			return err
		}
	}
	return os.RemoveAll(d.dir(ref.ID))
}

// material lists the private material published on disk.
func (d *diskSecrets) material() []string {
	entries, _ := os.ReadDir(filepath.Join(d.root, "material"))
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// Material and HoldNextResolve make diskSecrets a kernelcontracts.ResolverSubject.
func (d *diskSecrets) Material() []string { return d.material() }

func (d *diskSecrets) HoldNextResolve() (<-chan struct{}, func()) {
	hold, entered := make(chan struct{}), make(chan struct{})
	d.holdMu.Lock()
	d.hold, d.entered = hold, entered
	d.holdMu.Unlock()
	return entered, func() { close(hold) }
}

// Test_unit_kernel_secrets_disk_contract runs the shared resolver suite against
// the disk-backed test resolver the engine tests use, so what they rely on is
// proven to be the contract.
func Test_unit_kernel_secrets_disk_contract(t *testing.T) {
	kernelcontracts.RunResolver(t, func(tb kernelcontracts.TB) kernelcontracts.ResolverSubject {
		return newDiskSecrets(tb.(testing.TB))
	})
}
