package secret

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
	"github.com/optimiware/kodama/test/support/kernelcontracts"
)

// vault is a Source over fixed values. Its Fetch is the only place a value is
// produced; the tests then look for that value on disk, in logs, events and errors.
type vault struct {
	mu       sync.Mutex
	values   map[string]string // reference name -> value
	versions map[string]string
	missing  map[string]bool
	fetches  int
}

func newVault() *vault {
	return &vault{values: map[string]string{"TOKEN": "tok-s3cret-value"}, versions: map[string]string{"TOKEN": "7"}, missing: map[string]bool{}}
}

func (v *vault) Fetch(_ context.Context, owner runtime.MaterializationRef, ref runtime.SecretRef) (Secret, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.fetches++
	val, ok := v.values[ref.Name]
	if !ok || v.missing[ref.Name] {
		return Secret{}, errors.New("backend says: no such secret, token was tok-s3cret-value")
	}
	return Secret{Value: []byte(val), Version: v.versions[ref.Name]}, nil
}

func (v *vault) set(name, value, version string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.values[name], v.versions[name] = value, version
}

type harness struct {
	t      *testing.T
	root   string
	vault  *vault
	logs   *bytes.Buffer
	mu     sync.Mutex
	events []Event
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return &harness{t: t, root: t.TempDir(), vault: newVault(), logs: &bytes.Buffer{}}
}

// open returns a custody over the harness root; every call is a "process" that
// shares nothing with the others but the disk.
func (h *harness) open() *Custody {
	h.t.Helper()
	c, err := NewCustody(CustodyConfig{Root: h.root, Source: h.vault,
		Logger:  slog.New(slog.NewTextHandler(syncWriter{h}, nil)),
		Observe: func(e Event) { h.mu.Lock(); h.events = append(h.events, e); h.mu.Unlock() }})
	if err != nil {
		h.t.Fatal(err)
	}
	return c
}

type syncWriter struct{ h *harness }

func (w syncWriter) Write(p []byte) (int, error) {
	w.h.mu.Lock()
	defer w.h.mu.Unlock()
	return w.h.logs.Write(p)
}

var (
	scopeA = domain.Scope{TenantID: "t1", ProjectID: "p1"}
	owner  = domain.Owner{TenantID: "t1", ProjectID: "p1"}
)

func ident(t testing.TB, step domain.StepID, attempt int) runtime.MaterializationRef {
	t.Helper()
	ref, err := runtime.NewMaterializationRef(scopeA, "run1", step, attempt)
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func request(ref runtime.MaterializationRef, names ...string) runtime.ResolveRequest {
	req := runtime.ResolveRequest{Ref: ref}
	for _, n := range names {
		req.Refs = append(req.Refs, runtime.SecretRef{Owner: owner, Name: n})
	}
	return req
}

// everything returns the content of every file under root, with its path.
func everything(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			out[p] = string(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// subject adapts a Custody to the shared resolver suite.
type subject struct {
	*Custody
	mu      sync.Mutex
	hold    chan struct{}
	entered chan struct{}
}

func newSubject(t kernelcontracts.TB) kernelcontracts.ResolverSubject {
	tt := t.(*testing.T)
	h := newHarness(tt)
	s := &subject{Custody: h.open()}
	s.Custody.beforePublish = func() {
		s.mu.Lock()
		hold, entered := s.hold, s.entered
		s.hold, s.entered = nil, nil
		s.mu.Unlock()
		if hold != nil {
			close(entered)
			<-hold
		}
	}
	return s
}

func (s *subject) Material() []string { return s.Published() }

func (s *subject) HoldNextResolve() (<-chan struct{}, func()) {
	hold, entered := make(chan struct{}), make(chan struct{})
	s.mu.Lock()
	s.hold, s.entered = hold, entered
	s.mu.Unlock()
	return entered, func() { close(hold) }
}

// Test_unit_shell_secret_revocation proves the custody adapter keeps the
// resolver contract on a real disk: material is created and removed, a revoked
// identity is closed for good across restarts, and no value escapes into
// anything but the private material.
func Test_unit_shell_secret_revocation(t *testing.T) {
	t.Run("resolver conformance suite", func(t *testing.T) {
		kernelcontracts.RunResolver(t, newSubject)
	})

	t.Run("material is really created outside the workspace and really removed", func(t *testing.T) {
		h := newHarness(t)
		c := h.open()
		h.vault.set("DEPLOY_KEY", "deploy-s3cret", "1")
		ref := ident(t, "build", 1)
		m, err := c.Resolve(context.Background(), request(ref, "TOKEN", "DEPLOY_KEY"))
		if err != nil {
			t.Fatal(err)
		}
		if !filepath.IsAbs(m.Directory) || strings.HasPrefix(m.Directory, "/workspace") || !strings.HasPrefix(m.Directory, h.root) {
			t.Fatalf("the material is not private and outside /workspace: %s", m.Directory)
		}
		for name, want := range map[string]string{"TOKEN": "tok-s3cret-value", "DEPLOY_KEY": "deploy-s3cret"} {
			p := filepath.Join(m.Directory, name)
			b, err := os.ReadFile(p)
			if err != nil || string(b) != want {
				t.Fatalf("%s holds %q, %v", name, b, err)
			}
			if st, _ := os.Stat(p); st.Mode().Perm() != 0o400 {
				t.Fatalf("%s is %v, not private", name, st.Mode().Perm())
			}
		}
		if st, _ := os.Stat(m.Directory); st.Mode().Perm() != 0o700 {
			t.Fatalf("the directory is %v", st.Mode().Perm())
		}
		if m.Revision == "" || len(m.Refs) != 2 {
			t.Fatalf("the materialization misses its revision or references: %+v", m)
		}
		stage, _ := os.ReadDir(filepath.Join(h.root, "stage"))
		if len(stage) != 0 {
			t.Fatalf("staging left behind: %v", stage)
		}

		if err := c.Revoke(context.Background(), ref); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(m.Directory); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("the material survived its revocation: %v", err)
		}
		for p, content := range everything(t, h.root) {
			if strings.Contains(content, "s3cret") {
				t.Fatalf("%s still holds a value after revocation", p)
			}
		}
	})

	t.Run("a revoked identity is closed across restarts and instances", func(t *testing.T) {
		h := newHarness(t)
		first := h.open()
		ref := ident(t, "build", 1)
		if _, err := first.Resolve(context.Background(), request(ref, "TOKEN")); err != nil {
			t.Fatal(err)
		}
		// A replacement engine opens the same disk and revokes what it never saw resolved.
		second := h.open()
		if err := second.Revoke(context.Background(), ref); err != nil {
			t.Fatal(err)
		}
		if len(first.Published()) != 0 {
			t.Fatalf("the material outlived the replacement's revocation: %v", first.Published())
		}
		// The original, a brand-new process and the replacement all refuse it.
		for name, c := range map[string]*Custody{"first": first, "second": second, "restarted": h.open()} {
			fetches := h.vault.fetches
			_, err := c.Resolve(context.Background(), request(ref, "TOKEN"))
			if !errors.Is(err, runtime.ErrMaterializationRevoked) {
				t.Fatalf("%s resolved a revoked identity: %v", name, err)
			}
			if h.vault.fetches != fetches {
				t.Fatalf("%s asked the vault for a revoked identity", name)
			}
			if closed, err := c.Revoked(ref); !closed || err != nil {
				t.Fatalf("%s does not report the identity closed: %v %v", name, closed, err)
			}
		}
		if len(first.Published()) != 0 {
			t.Fatalf("material appeared: %v", first.Published())
		}
	})

	t.Run("a resolve in flight in another instance is refused at publication", func(t *testing.T) {
		h := newHarness(t)
		worker := h.open()
		entered, release := make(chan struct{}), make(chan struct{})
		worker.beforePublish = func() { close(entered); <-release }
		ref := ident(t, "build", 1)
		done := make(chan error, 1)
		go func() {
			_, err := worker.Resolve(context.Background(), request(ref, "TOKEN"))
			done <- err
		}()
		<-entered
		// The staged copy exists but is not published.
		if len(worker.Published()) != 0 {
			t.Fatalf("staged material is already readable: %v", worker.Published())
		}
		// The worker outlived a takeover: the replacement revokes and finishes cleanup.
		if err := h.open().Revoke(context.Background(), ref); err != nil {
			t.Fatal(err)
		}
		close(release)
		if err := <-done; !errors.Is(err, runtime.ErrMaterializationRevoked) {
			t.Fatalf("the late worker published: %v", err)
		}
		if len(worker.Published()) != 0 {
			t.Fatalf("the late worker left material behind: %v", worker.Published())
		}
		stage, _ := os.ReadDir(filepath.Join(h.root, "stage"))
		if len(stage) != 0 {
			t.Fatalf("the late worker left staged material behind: %v", stage)
		}
		for p, content := range everything(t, h.root) {
			if strings.Contains(content, "s3cret") {
				t.Fatalf("%s holds a value", p)
			}
		}
	})

	t.Run("revoking removes material left by a crash before the resolver returned", func(t *testing.T) {
		h := newHarness(t)
		c := h.open()
		ref := ident(t, "build", 1)
		// A resolver that died after staging and before publishing leaves a stage directory.
		orphan := filepath.Join(h.root, "stage", key(ref.ID)+"-crashed")
		if err := os.MkdirAll(orphan, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(orphan, "TOKEN"), []byte("tok-s3cret-value"), 0o400); err != nil {
			t.Fatal(err)
		}
		other := filepath.Join(h.root, "stage", key(ident(t, "other", 1).ID)+"-crashed")
		if err := os.MkdirAll(other, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := c.Revoke(context.Background(), ref); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(orphan); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("the crashed staging survived: %v", err)
		}
		if _, err := os.Stat(other); err != nil {
			t.Fatalf("another identity's staging was removed: %v", err)
		}
	})

	t.Run("a failed removal is reported and the identity stays closed", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("file permissions do not bind root")
		}
		h := newHarness(t)
		c := h.open()
		ref := ident(t, "build", 1)
		m, err := c.Resolve(context.Background(), request(ref, "TOKEN"))
		if err != nil {
			t.Fatal(err)
		}
		// A read-only subdirectory makes the removal fail.
		stuck := filepath.Join(m.Directory, "stuck")
		if err := os.Mkdir(stuck, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(stuck, "f"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(stuck, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(stuck, 0o700) })
		if err := c.Revoke(context.Background(), ref); err == nil {
			t.Fatal("a failed removal was discarded")
		}
		if _, err := c.Resolve(context.Background(), request(ref, "TOKEN")); !errors.Is(err, runtime.ErrMaterializationRevoked) {
			t.Fatalf("the identity is not closed after a failed revoke: %v", err)
		}
		if err := os.Chmod(stuck, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := c.Revoke(context.Background(), ref); err != nil {
			t.Fatalf("the retry: %v", err)
		}
		if len(c.Published()) != 0 {
			t.Fatalf("material left: %v", c.Published())
		}
	})

	t.Run("rotation replaces material and is observable without a value", func(t *testing.T) {
		h := newHarness(t)
		c := h.open()
		ref := ident(t, "build", 1)
		first, err := c.Resolve(context.Background(), request(ref, "TOKEN"))
		if err != nil {
			t.Fatal(err)
		}
		again, err := c.Resolve(context.Background(), request(ref, "TOKEN"))
		if err != nil || again.Revision != first.Revision {
			t.Fatalf("an unchanged repeat changed the revision: %v %q %q", err, first.Revision, again.Revision)
		}
		h.vault.set("TOKEN", "tok-rotated-s3cret", "8")
		rotated, err := c.Resolve(context.Background(), request(ref, "TOKEN"))
		if err != nil {
			t.Fatal(err)
		}
		if rotated.Revision == first.Revision || len(c.Published()) != 1 {
			t.Fatalf("the rotation did not replace the material: %q %q %v", first.Revision, rotated.Revision, c.Published())
		}
		if b, _ := os.ReadFile(filepath.Join(rotated.Directory, "TOKEN")); string(b) != "tok-rotated-s3cret" {
			t.Fatalf("the file holds %q", b)
		}
		if err := c.Revoke(context.Background(), ref); err != nil {
			t.Fatal(err)
		}
		var kinds []EventKind
		for _, e := range h.events {
			kinds = append(kinds, e.Kind)
		}
		want := []EventKind{EventResolved, EventResolved, EventRotated, EventRevoked}
		if fmt.Sprint(kinds) != fmt.Sprint(want) {
			t.Fatalf("events %v, want %v", kinds, want)
		}
		if h.events[2].Revision != rotated.Revision || h.events[2].ID != ref.ID || h.events[2].Names[0] != "TOKEN" {
			t.Fatalf("the rotation event: %+v", h.events[2])
		}
	})

	t.Run("an unavailable reference publishes nothing and names no value", func(t *testing.T) {
		h := newHarness(t)
		c := h.open()
		ref := ident(t, "build", 1)
		_, err := c.Resolve(context.Background(), request(ref, "TOKEN", "MISSING"))
		if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "MISSING") {
			t.Fatalf("a missing reference: %v", err)
		}
		if strings.Contains(err.Error(), "s3cret") {
			t.Fatalf("the error carries the source's text: %v", err)
		}
		// A pinned version the source does not hold.
		pinned := runtime.ResolveRequest{Ref: ref, Refs: []runtime.SecretRef{{Owner: owner, Name: "TOKEN", Version: "3"}}}
		if _, err := c.Resolve(context.Background(), pinned); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("a version the source does not hold: %v", err)
		}
		pinned.Refs[0].Version = "7"
		if _, err := c.Resolve(context.Background(), pinned); err != nil {
			t.Fatalf("the pinned version: %v", err)
		}
		if err := c.Revoke(context.Background(), ref); err != nil {
			t.Fatal(err)
		}
		stage, _ := os.ReadDir(filepath.Join(h.root, "stage"))
		if len(stage) != 0 {
			t.Fatalf("a failed resolution left staged material: %v", stage)
		}
	})

	t.Run("a failed resolution of a revoked identity reports the revocation", func(t *testing.T) {
		h := newHarness(t)
		c := h.open()
		ref := ident(t, "build", 1)
		c.beforePublish = func() {}
		// The vault fails while the identity gets revoked: the cause that matters is the revocation.
		blocking := &revokingSource{inner: h.vault, revoke: func() { _ = h.open().Revoke(context.Background(), ref) }}
		c.source = blocking
		_, err := c.Resolve(context.Background(), request(ref, "MISSING"))
		if !errors.Is(err, runtime.ErrMaterializationRevoked) {
			t.Fatalf("%v", err)
		}
	})

	t.Run("references outside the attempt's authority are refused before the vault is asked", func(t *testing.T) {
		h := newHarness(t)
		c := h.open()
		ref := ident(t, "build", 1)
		for name, refs := range map[string][]runtime.SecretRef{
			"another tenant":  {{Owner: domain.Owner{TenantID: "t2", ProjectID: "p1"}, Name: "TOKEN"}},
			"another project": {{Owner: domain.Owner{TenantID: "t1", ProjectID: "p2"}, Name: "TOKEN"}},
			"a bad name":      {{Owner: owner, Name: "../TOKEN"}},
			"declared twice":  {{Owner: owner, Name: "TOKEN"}, {Owner: domain.Owner{TenantID: "t1"}, Name: "TOKEN"}},
		} {
			if _, err := c.Resolve(context.Background(), runtime.ResolveRequest{Ref: ref, Refs: refs}); !errors.Is(err, domain.ErrInvalidValue) {
				t.Errorf("%s: %v", name, err)
			}
		}
		// A tenant-owned reference is shared by its projects.
		shared := runtime.ResolveRequest{Ref: ref, Refs: []runtime.SecretRef{{Owner: domain.Owner{TenantID: "t1"}, Name: "TOKEN"}}}
		if _, err := c.Resolve(context.Background(), shared); err != nil {
			t.Errorf("a tenant-owned reference: %v", err)
		}
		if h.vault.fetches != 1 {
			t.Errorf("the vault was asked %d times", h.vault.fetches)
		}
	})

	t.Run("no value reaches logs, events, errors or retained metadata", func(t *testing.T) {
		h := newHarness(t)
		c := h.open()
		ref := ident(t, "build", 1)
		var errs []string
		if _, err := c.Resolve(context.Background(), request(ref, "TOKEN", "MISSING")); err != nil {
			errs = append(errs, err.Error())
		}
		if _, err := c.Resolve(context.Background(), request(ref, "TOKEN")); err != nil {
			t.Fatal(err)
		}
		h.vault.set("TOKEN", "tok-s3cret-rotated", "9")
		if _, err := c.Resolve(context.Background(), request(ref, "TOKEN")); err != nil {
			t.Fatal(err)
		}
		// Everything retained besides the material itself: the index and tombstones.
		for p, content := range everything(t, h.root) {
			if !strings.Contains(p, string(filepath.Separator)+"material"+string(filepath.Separator)) && strings.Contains(content, "s3cret") {
				t.Errorf("retained metadata %s holds a value", p)
			}
		}
		if err := c.Revoke(context.Background(), ref); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Resolve(context.Background(), request(ref, "TOKEN")); err != nil {
			errs = append(errs, err.Error())
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		surfaces := append([]string{h.logs.String(), fmt.Sprintf("%+v", h.events)}, errs...)
		if len(h.events) == 0 || h.logs.Len() == 0 {
			t.Fatal("custody is not observable")
		}
		for _, s := range surfaces {
			if strings.Contains(s, "s3cret") || strings.Contains(s, "tok-") {
				t.Errorf("a value escaped: %s", s)
			}
		}
		for p, content := range everything(t, h.root) {
			if strings.Contains(content, "s3cret") {
				t.Errorf("%s holds a value after revocation", p)
			}
		}
	})

	t.Run("an unreadable revocation state never reopens a closed identity", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("file permissions do not bind root")
		}
		h := newHarness(t)
		c := h.open()
		tombs := filepath.Join(h.root, "tombstones")
		t.Cleanup(func() { _ = os.Chmod(tombs, 0o700) })

		closed := ident(t, "build", 1)
		if _, err := c.Resolve(context.Background(), request(closed, "TOKEN")); err != nil {
			t.Fatal(err)
		}
		if err := c.Revoke(context.Background(), closed); err != nil {
			t.Fatal(err)
		}
		// The tombstone exists, and cannot be read.
		if err := os.Chmod(tombs, 0); err != nil {
			t.Fatal(err)
		}
		fetches := h.vault.fetches
		_, err := c.Resolve(context.Background(), request(closed, "TOKEN"))
		if !errors.Is(err, ErrRevocationState) {
			t.Fatalf("a Resolve with an unreadable tombstone: %v", err)
		}
		if len(c.Published()) != 0 || h.vault.fetches != fetches {
			t.Fatalf("material was published or fetched for a closed identity: %v", c.Published())
		}
		if isClosed, err := c.Revoked(closed); !isClosed || err == nil {
			t.Fatalf("an unknown state must read as closed, with its error: %v %v", isClosed, err)
		}
		// Whatever is unknown stays unpublished: other identities too.
		if _, err := c.Resolve(context.Background(), request(ident(t, "deploy", 1), "TOKEN")); !errors.Is(err, ErrRevocationState) {
			t.Fatalf("an identity whose state cannot be read was resolved: %v", err)
		}
		if len(c.Published()) != 0 {
			t.Fatalf("material was published: %v", c.Published())
		}
		if err := os.Chmod(tombs, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Resolve(context.Background(), request(closed, "TOKEN")); !errors.Is(err, runtime.ErrMaterializationRevoked) {
			t.Fatalf("the identity was reopened: %v", err)
		}

		// The same at the locked publication check: the identity is revoked while a
		// Resolve is in flight, and the state becomes unreadable before it publishes.
		inflight := ident(t, "build", 2)
		entered, release := make(chan struct{}), make(chan struct{})
		c.beforePublish = func() { close(entered); <-release }
		done := make(chan error, 1)
		go func() {
			_, err := c.Resolve(context.Background(), request(inflight, "TOKEN"))
			done <- err
		}()
		<-entered
		if err := h.open().Revoke(context.Background(), inflight); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(tombs, 0); err != nil {
			t.Fatal(err)
		}
		close(release)
		if err := <-done; err == nil {
			t.Fatal("the in-flight Resolve published while the tombstone was unreadable")
		}
		if len(c.Published()) != 0 {
			t.Fatalf("the in-flight Resolve left material: %v", c.Published())
		}
		stage, _ := os.ReadDir(filepath.Join(h.root, "stage"))
		if len(stage) != 0 {
			t.Fatalf("staged material left behind: %v", stage)
		}
		c.beforePublish = nil
		if err := os.Chmod(tombs, 0o700); err != nil {
			t.Fatal(err)
		}

		// A vanished tombstone directory is lost state, not an open identity.
		moved := tombs + ".gone"
		if err := os.Rename(tombs, moved); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Resolve(context.Background(), request(ident(t, "fresh", 1), "TOKEN")); !errors.Is(err, ErrRevocationState) {
			t.Fatalf("a missing tombstone directory was read as open: %v", err)
		}
		if err := os.Rename(moved, tombs); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Resolve(context.Background(), request(ident(t, "fresh", 1), "TOKEN")); err != nil {
			t.Fatalf("an open identity is refused once the state is readable: %v", err)
		}
	})

	t.Run("the custody root must be absolute and outside /workspace", func(t *testing.T) {
		for _, root := range []string{"", "relative/dir", "/workspace", "/workspace/secrets"} {
			if _, err := NewCustody(CustodyConfig{Root: root, Source: newVault()}); err == nil {
				t.Errorf("root %q was accepted", root)
			}
		}
		if _, err := NewCustody(CustodyConfig{Root: t.TempDir()}); err == nil {
			t.Error("a custody without a source was accepted")
		}
	})
}

// revokingSource revokes the identity as its inner source fails, as a takeover would.
type revokingSource struct {
	inner  Source
	revoke func()
}

func (r *revokingSource) Fetch(ctx context.Context, owner runtime.MaterializationRef, ref runtime.SecretRef) (Secret, error) {
	r.revoke()
	return r.inner.Fetch(ctx, owner, ref)
}
