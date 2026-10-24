package memdb_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/memdb"
)

const (
	acme   domain.TenantID = "acme"
	globex domain.TenantID = "globex"
)

type item struct {
	ID, Slug, Body string
}

type event struct {
	ID, Kind string
}

var (
	items  = memdb.NewTable("items", func(i item) string { return i.ID }, func(i item) string { return i.Slug })
	audits = memdb.NewTable("audits", func(e event) string { return e.ID })
	outbox = memdb.NewTable("outbox", func(e event) string { return e.ID })
)

var errBoom = errors.New("boom")

func update(s *memdb.Store, tn domain.TenantID, fn func(memdb.Tx) error) error {
	return s.Update(context.Background(), tn, fn)
}

func mustUpdate(t *testing.T, s *memdb.Store, tn domain.TenantID, fn func(memdb.Tx) error) {
	t.Helper()
	if err := update(s, tn, fn); err != nil {
		t.Fatalf("update: %v", err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// snapshot lists every row of the three tables for a tenant.
func snapshot(t *testing.T, s *memdb.Store, tn domain.TenantID) (is []item, as, os []event) {
	t.Helper()
	err := s.View(context.Background(), tn, func(tx memdb.Tx) error {
		items.Scan(tx, func(i item) bool { is = append(is, i); return true })
		audits.Scan(tx, func(e event) bool { as = append(as, e); return true })
		outbox.Scan(tx, func(e event) bool { os = append(os, e); return true })
		return nil
	})
	must(t, err)
	return
}

func writeAll(tx memdb.Tx, suffix string) error {
	if err := items.Put(tx, item{ID: "i" + suffix, Slug: "s" + suffix}); err != nil {
		return err
	}
	if err := audits.Put(tx, event{ID: "a" + suffix, Kind: "created"}); err != nil {
		return err
	}
	return outbox.Put(tx, event{ID: "o" + suffix, Kind: "created"})
}

func TestUnitMemdbAtomicUpdate(t *testing.T) {
	s := memdb.New()
	mustUpdate(t, s, acme, func(tx memdb.Tx) error { return writeAll(tx, "0") })

	t.Run("failure after several writes changes nothing", func(t *testing.T) {
		err := update(s, acme, func(tx memdb.Tx) error {
			must(t, writeAll(tx, "1"))
			must(t, items.Put(tx, item{ID: "i0", Slug: "s0", Body: "changed"}))
			audits.Delete(tx, "a0")
			if _, ok := items.Get(tx, "i1"); !ok {
				t.Error("update does not read its own writes")
			}
			return errBoom
		})
		if !errors.Is(err, errBoom) {
			t.Fatalf("err = %v, want errBoom", err)
		}
		is, as, os := snapshot(t, s, acme)
		if len(is) != 1 || is[0].Body != "" || len(as) != 1 || len(os) != 1 {
			t.Fatalf("rows changed after failed update: %v %v %v", is, as, os)
		}
	})

	t.Run("panic discards and releases the writer", func(t *testing.T) {
		func() {
			defer func() { _ = recover() }()
			_ = update(s, acme, func(tx memdb.Tx) error {
				must(t, writeAll(tx, "p"))
				panic("boom")
			})
		}()
		if is, _, _ := snapshot(t, s, acme); len(is) != 1 {
			t.Fatalf("panicking update leaked rows: %v", is)
		}
		mustUpdate(t, s, acme, func(memdb.Tx) error { return nil })
	})

	t.Run("cancelled context discards", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		err := s.Update(ctx, acme, func(tx memdb.Tx) error {
			must(t, writeAll(tx, "c"))
			cancel()
			return nil
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if is, _, _ := snapshot(t, s, acme); len(is) != 1 {
			t.Fatalf("cancelled update leaked rows: %v", is)
		}
	})

	t.Run("success publishes all tables at once", func(t *testing.T) {
		started, release := make(chan struct{}), make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- update(s, acme, func(tx memdb.Tx) error {
				if err := writeAll(tx, "2"); err != nil {
					return err
				}
				close(started)
				<-release
				return nil
			})
		}()
		<-started
		// A concurrent reader sees none of the in-flight writes.
		is, as, os := snapshot(t, s, acme)
		if len(is) != 1 || len(as) != 1 || len(os) != 1 {
			t.Fatalf("uncommitted rows visible: %v %v %v", is, as, os)
		}
		close(release)
		must(t, <-done)
		is, as, os = snapshot(t, s, acme)
		if len(is) != 2 || len(as) != 2 || len(os) != 2 || is[1].ID != "i2" {
			t.Fatalf("rows not all published: %v %v %v", is, as, os)
		}
	})

	t.Run("delete and replace commit", func(t *testing.T) {
		mustUpdate(t, s, acme, func(tx memdb.Tx) error {
			items.Delete(tx, "i0")
			return items.Put(tx, item{ID: "i2", Slug: "s2", Body: "new"})
		})
		is, _, _ := snapshot(t, s, acme)
		if len(is) != 1 || is[0].ID != "i2" || is[0].Body != "new" {
			t.Fatalf("items = %v", is)
		}
	})

	t.Run("writers are serialized", func(t *testing.T) {
		c := memdb.New()
		counter := memdb.NewTable("counter", func(n [2]int) string { return "n" })
		var wg sync.WaitGroup
		for range 50 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				err := c.Update(context.Background(), acme, func(tx memdb.Tx) error {
					cur, _ := counter.Get(tx, "n")
					return counter.Put(tx, [2]int{cur[0] + 1})
				})
				if err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		_ = c.View(context.Background(), acme, func(tx memdb.Tx) error {
			if cur, _ := counter.Get(tx, "n"); cur[0] != 50 {
				t.Errorf("lost updates: counter = %d", cur[0])
			}
			return nil
		})
	})
}

func TestUnitMemdbTenantScope(t *testing.T) {
	s := memdb.New()
	mustUpdate(t, s, acme, func(tx memdb.Tx) error {
		return items.Put(tx, item{ID: "shared", Slug: "slug", Body: "acme"})
	})

	t.Run("reads never cross tenants", func(t *testing.T) {
		for name, run := range map[string]func(context.Context, domain.TenantID, func(memdb.Tx) error) error{
			"update": s.Update, "view": s.View,
		} {
			err := run(context.Background(), globex, func(tx memdb.Tx) error {
				if tx.Tenant() != globex {
					t.Errorf("%s: Tx tenant = %q", name, tx.Tenant())
				}
				if v, ok := items.Get(tx, "shared"); ok {
					t.Errorf("%s: read acme row %v", name, v)
				}
				items.Scan(tx, func(i item) bool { t.Errorf("%s: scan saw %v", name, i); return true })
				return nil
			})
			must(t, err)
		}
	})

	t.Run("same key and unique value are independent per tenant", func(t *testing.T) {
		mustUpdate(t, s, globex, func(tx memdb.Tx) error {
			return items.Put(tx, item{ID: "shared", Slug: "slug", Body: "globex"})
		})
		for tn, want := range map[domain.TenantID]string{acme: "acme", globex: "globex"} {
			is, _, _ := snapshot(t, s, tn)
			if len(is) != 1 || is[0].Body != want {
				t.Fatalf("%s items = %v", tn, is)
			}
		}
	})

	t.Run("writes and deletes touch only the Tx tenant", func(t *testing.T) {
		mustUpdate(t, s, globex, func(tx memdb.Tx) error {
			items.Delete(tx, "shared")
			return items.Put(tx, item{ID: "g2", Slug: "g2"})
		})
		if is, _, _ := snapshot(t, s, acme); len(is) != 1 || is[0].Body != "acme" {
			t.Fatalf("acme rows disturbed: %v", is)
		}
		if is, _, _ := snapshot(t, s, globex); len(is) != 1 || is[0].ID != "g2" {
			t.Fatalf("globex rows = %v", is)
		}
	})

	t.Run("invalid tenant is refused", func(t *testing.T) {
		called := false
		err := update(s, "", func(memdb.Tx) error { called = true; return nil })
		if !errors.Is(err, domain.ErrInvalidValue) || called {
			t.Fatalf("err = %v called = %v", err, called)
		}
		err = s.View(context.Background(), "bad id", func(memdb.Tx) error { called = true; return nil })
		if !errors.Is(err, domain.ErrInvalidValue) || called {
			t.Fatalf("view err = %v called = %v", err, called)
		}
	})

	t.Run("view cannot write and a closed Tx is refused", func(t *testing.T) {
		var kept memdb.Tx
		must(t, s.View(context.Background(), acme, func(tx memdb.Tx) error {
			kept = tx
			if err := items.Put(tx, item{ID: "x", Slug: "x"}); !errors.Is(err, memdb.ErrReadOnly) {
				t.Errorf("Put in View = %v", err)
			}
			return nil
		}))
		func() {
			defer func() {
				if recover() == nil {
					t.Error("closed Tx was accepted")
				}
			}()
			items.Get(kept, "shared")
		}()
	})
}

func TestUnitMemdbUniqueConflict(t *testing.T) {
	s := memdb.New()
	mustUpdate(t, s, acme, func(tx memdb.Tx) error {
		return items.Put(tx, item{ID: "a", Slug: "one", Body: "first"})
	})

	t.Run("duplicate unique key conflicts and changes nothing", func(t *testing.T) {
		err := update(s, acme, func(tx memdb.Tx) error {
			err := items.Put(tx, item{ID: "b", Slug: "one", Body: "second"})
			if !errors.Is(err, memdb.ErrConflict) {
				t.Errorf("Put = %v, want ErrConflict", err)
			}
			if _, ok := items.Get(tx, "b"); ok {
				t.Error("conflicting row visible in its own Tx")
			}
			return err
		})
		if !errors.Is(err, memdb.ErrConflict) {
			t.Fatalf("Update = %v", err)
		}
		is, _, _ := snapshot(t, s, acme)
		if len(is) != 1 || is[0].ID != "a" || is[0].Body != "first" {
			t.Fatalf("items = %v", is)
		}
	})

	t.Run("a conflict the caller absorbs leaves earlier writes intact and index sound", func(t *testing.T) {
		mustUpdate(t, s, acme, func(tx memdb.Tx) error {
			must(t, items.Put(tx, item{ID: "c", Slug: "three"}))
			if err := items.Put(tx, item{ID: "d", Slug: "three"}); !errors.Is(err, memdb.ErrConflict) {
				t.Errorf("Put = %v", err)
			}
			return nil
		})
		is, _, _ := snapshot(t, s, acme)
		if len(is) != 2 || is[1].ID != "c" {
			t.Fatalf("items = %v", is)
		}
	})

	t.Run("replacing a row with its own unique value is not a conflict", func(t *testing.T) {
		mustUpdate(t, s, acme, func(tx memdb.Tx) error {
			return items.Put(tx, item{ID: "a", Slug: "one", Body: "edited"})
		})
	})

	t.Run("changing, deleting and reusing a unique value", func(t *testing.T) {
		mustUpdate(t, s, acme, func(tx memdb.Tx) error {
			must(t, items.Put(tx, item{ID: "a", Slug: "renamed"}))
			// "one" was released by the rename.
			must(t, items.Put(tx, item{ID: "e", Slug: "one"}))
			items.Delete(tx, "c")
			return items.Put(tx, item{ID: "f", Slug: "three"})
		})
		err := update(s, acme, func(tx memdb.Tx) error {
			return items.Put(tx, item{ID: "g", Slug: "renamed"})
		})
		if !errors.Is(err, memdb.ErrConflict) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("empty unique value opts out and empty key is invalid", func(t *testing.T) {
		mustUpdate(t, s, acme, func(tx memdb.Tx) error {
			must(t, items.Put(tx, item{ID: "n1"}))
			return items.Put(tx, item{ID: "n2"})
		})
		err := update(s, acme, func(tx memdb.Tx) error { return items.Put(tx, item{Slug: "k"}) })
		if !errors.Is(err, domain.ErrInvalidValue) {
			t.Fatalf("err = %v", err)
		}
	})
}

// peer is a module adapter that joins the caller's transaction.
type peer struct{}

func (peer) record(tx memdb.Tx, id string) error {
	return audits.Put(tx, event{ID: id, Kind: "peer"})
}

// ownUpdate is a peer that opens its own Update.
func (peer) ownUpdate(ctx context.Context, s *memdb.Store, tn domain.TenantID, id string) error {
	return s.Update(ctx, tn, func(tx memdb.Tx) error { return audits.Put(tx, event{ID: id, Kind: "own"}) })
}

func TestUnitMemdbJoinedPeer(t *testing.T) {
	p := peer{}

	t.Run("a peer given the caller's Tx rolls back with it", func(t *testing.T) {
		s := memdb.New()
		err := update(s, acme, func(tx memdb.Tx) error {
			must(t, items.Put(tx, item{ID: "i", Slug: "i"}))
			must(t, p.record(tx, "joined"))
			if _, ok := audits.Get(tx, "joined"); !ok {
				t.Error("caller cannot see the peer's write")
			}
			return errBoom
		})
		if !errors.Is(err, errBoom) {
			t.Fatal(err)
		}
		is, as, _ := snapshot(t, s, acme)
		if len(is) != 0 || len(as) != 0 {
			t.Fatalf("rows survived: %v %v", is, as)
		}
	})

	t.Run("a peer given the caller's Tx commits with it", func(t *testing.T) {
		s := memdb.New()
		mustUpdate(t, s, acme, func(tx memdb.Tx) error {
			must(t, items.Put(tx, item{ID: "i", Slug: "i"}))
			return p.record(tx, "joined")
		})
		is, as, _ := snapshot(t, s, acme)
		if len(is) != 1 || len(as) != 1 {
			t.Fatalf("rows: %v %v", is, as)
		}
	})

	t.Run("a peer that opens its own Update does not roll back", func(t *testing.T) {
		s := memdb.New()
		peerDone := make(chan error, 1)
		err := update(s, acme, func(tx memdb.Tx) error {
			must(t, items.Put(tx, item{ID: "i", Slug: "i"}))
			// The peer escapes the caller's transaction: it is its own
			// writer and queues behind the caller's.
			go func() { peerDone <- p.ownUpdate(context.Background(), s, acme, "own") }()
			return errBoom
		})
		if !errors.Is(err, errBoom) {
			t.Fatal(err)
		}
		must(t, <-peerDone)
		is, as, _ := snapshot(t, s, acme)
		if len(is) != 0 {
			t.Fatalf("caller's row survived: %v", is)
		}
		if len(as) != 1 || as[0].ID != "own" {
			t.Fatalf("peer's own commit was undone: %v", as)
		}
	})

	t.Run("an independent reader does not see the caller's uncommitted rows", func(t *testing.T) {
		s := memdb.New()
		seen := make(chan bool, 1)
		mustUpdate(t, s, acme, func(tx memdb.Tx) error {
			must(t, items.Put(tx, item{ID: "i", Slug: "i"}))
			go func() {
				_ = s.View(context.Background(), acme, func(v memdb.Tx) error {
					_, ok := items.Get(v, "i")
					seen <- ok
					return nil
				})
			}()
			if <-seen {
				t.Error("independent reader saw uncommitted row")
			}
			return nil
		})
	})

	t.Run("a same-goroutine nested Update is not a join and ends with its context", func(t *testing.T) {
		s := memdb.New()
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		err := update(s, acme, func(memdb.Tx) error {
			return p.ownUpdate(ctx, s, acme, "nested")
		})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestUnitMemdbCommitFault(t *testing.T) {
	var fail error
	calls := 0
	s := memdb.New(memdb.WithCommitFault(func() error { calls++; return fail }))

	mustUpdate(t, s, acme, func(tx memdb.Tx) error { return writeAll(tx, "0") })

	fail = errors.New("disk full")
	err := update(s, acme, func(tx memdb.Tx) error {
		must(t, writeAll(tx, "1"))
		must(t, items.Put(tx, item{ID: "i0", Slug: "s0", Body: "changed"}))
		return nil
	})
	if !errors.Is(err, fail) {
		t.Fatalf("err = %v, want the injected fault", err)
	}
	is, as, os := snapshot(t, s, acme)
	if len(is) != 1 || is[0].Body != "" || len(as) != 1 || len(os) != 1 {
		t.Fatalf("faulted commit leaked: %v %v %v", is, as, os)
	}

	t.Run("the store keeps working once the fault clears", func(t *testing.T) {
		fail = nil
		mustUpdate(t, s, acme, func(tx memdb.Tx) error { return writeAll(tx, "2") })
		if is, _, _ := snapshot(t, s, acme); len(is) != 2 {
			t.Fatalf("items = %v", is)
		}
	})

	t.Run("fault is not consulted when fn fails", func(t *testing.T) {
		fail = errors.New("unused")
		before := calls
		_ = update(s, acme, func(memdb.Tx) error { return errBoom })
		if calls != before {
			t.Fatal("commit fault ran for a failed update")
		}
	})
}
