package artifactstest_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/artifacts/artifactstest"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/memdb"
)

// TestUnitMemoryArtifactsContract runs the shared artifact persistence contract
// on the memory adapter: no database.
func TestUnitMemoryArtifactsContract(t *testing.T) {
	artifactstest.RepoContract(t, artifactstest.OpenMemRepo)
}

// TestUnitMemoryArtifactObjectsContract runs the shared object-store contract on
// the in-memory store and its loopback transfer endpoint.
func TestUnitMemoryArtifactObjectsContract(t *testing.T) {
	artifactstest.ObjectContract(t, artifactstest.OpenMemObjects)
}

func TestUnitMemoryArtifactsAreDevelopmentOnly(t *testing.T) {
	e := artifactstest.NewMemEnv(t)
	for _, v := range []any{e.Store, e.Objects} {
		d, ok := v.(interface{ DevelopmentOnly() bool })
		if !ok || !d.DevelopmentOnly() {
			t.Fatalf("%T is not flagged development-only", v)
		}
	}
	if _, err := artifactstest.NewMemStore(nil); err == nil {
		t.Fatal("a store without a kernel was accepted")
	}
}

// The whole artifact module runs on memory: a producing attempt reserves a file,
// uploads through the issued instruction, completes (the service reads, hashes
// and seals the bytes) and a reader downloads through a fresh instruction.
func TestUnitMemoryArtifactsServiceRoundtrip(t *testing.T) {
	ctx := context.Background()
	e := artifactstest.NewMemEnv(t)
	caller := e.Attempt(artifactstest.ScopeA, "att_t088", artifacts.OutputDecl{Name: "report", Kind: artifacts.KindFile})
	data := []byte("the report bytes")

	out, err := e.Service.Reserve(ctx, caller, "att_t088", artifactstest.FileInput("report", artifacts.RoleOutput, data), "req_1")
	if err != nil || out.Upload == nil {
		t.Fatalf("reserve: %+v %v", out, err)
	}
	if code := uploadVia(t, *out.Upload, data); code != 200 {
		t.Fatalf("upload: %d", code)
	}
	if code := uploadVia(t, *out.Upload, []byte("different bytes!")); code == 200 {
		t.Fatal("an instruction accepted content other than the declaration")
	}
	done, err := e.Service.Complete(ctx, caller, "att_t088", out.ID, artifacts.CompleteInput{Digest: artifacts.DigestOf(data), Size: int64(len(data))}, "req_2")
	if err != nil || done.State != artifacts.StateUsable {
		t.Fatalf("complete: %+v %v", done, err)
	}
	if again, err := e.Service.Complete(ctx, caller, "att_t088", out.ID, artifacts.CompleteInput{Digest: artifacts.DigestOf(data), Size: int64(len(data))}, "req_3"); err != nil || again != done {
		t.Fatalf("a repeated completion: %+v %v", again, err)
	}
	a, err := e.Store.Get(ctx, artifactstest.ScopeA, out.ID)
	if err != nil || !e.Objects.Exists(a.SealedKey()) {
		t.Fatalf("sealed object: %v %v", a, err)
	}
	e.Reader.Allow("usr_reader", artifactstest.ScopeA)
	view, err := e.Service.Metadata(ctx, artifacts.Caller{Subject: "usr_reader"}, artifactstest.ScopeA, out.ID, true)
	if err != nil || view.Download == nil {
		t.Fatalf("metadata: %+v %v", view, err)
	}
	if code, body := downloadVia(t, *view.Download); code != 200 || !bytes.Equal(body, data) {
		t.Fatalf("download: %d %q", code, body)
	}
	// Another tenant's reader learns nothing about the artifact.
	e.Reader.Allow("usr_other", artifactstest.ScopeB)
	if _, err := e.Service.Metadata(ctx, artifacts.Caller{Subject: "usr_other"}, artifactstest.ScopeA, out.ID, false); !errors.Is(err, artifacts.ErrNotFound) {
		t.Fatalf("a foreign reader: %v", err)
	}
	// Transfer instructions end with their lifetime.
	e.Clock.Advance(30 * time.Minute)
	if code, _ := downloadVia(t, *view.Download); code != 403 {
		t.Fatalf("an expired download instruction was honored: %d", code)
	}
}

// A failed commit leaves nothing behind: no artifact and no staging object
// record, so the reservation can be repeated.
func TestUnitMemoryArtifactsCommitFault(t *testing.T) {
	var fail atomic.Bool
	e := artifactstest.NewMemRepoEnv(t, memdb.WithCommitFault(func() error {
		if fail.Load() {
			return errors.New("commit refused")
		}
		return nil
	}))
	ctx := context.Background()
	size := int64(1)
	id := domain.ArtifactID("art_fault")
	a, err := artifacts.NewFile(id, artifacts.Reservation{Scope: artifactstest.ScopeA, Attempt: "att_1", Run: "run_1", Step: "build", Name: "r",
		Kind: artifacts.KindFile, Role: artifacts.RoleOutput, Digest: artifacts.DigestOf([]byte("x")), MediaType: "text/plain", Size: &size},
		artifacts.ObjectKey(artifactstest.ScopeA, id, artifacts.SegmentStaging, "n1"), time.Now().Add(time.Hour), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cmd := artifacts.ReserveCommand{Artifact: a, Limits: artifacts.Limits{OutputBytes: 10, OutputFiles: 1}, Actor: artifacts.Actor{Type: "workload", ID: "w"}, Now: time.Now()}
	fail.Store(true)
	if _, err := e.Store.Reserve(ctx, cmd); err == nil {
		t.Fatal("the commit fault was ignored")
	}
	fail.Store(false)
	if _, err := e.Store.Get(ctx, artifactstest.ScopeA, id); !errors.Is(err, artifacts.ErrNotFound) {
		t.Fatalf("the failed reservation left an artifact: %v", err)
	}
	if states := e.ObjectStates(t, artifactstest.ScopeA, id); len(states) != 0 {
		t.Fatalf("the failed reservation left object records: %v", states)
	}
	res, err := e.Store.Reserve(ctx, cmd)
	if err != nil || !res.Created {
		t.Fatalf("retry: %+v %v", res, err)
	}
}

func uploadVia(t *testing.T, tr artifacts.Transfer, data []byte) int {
	t.Helper()
	req, err := http.NewRequest(tr.Method, tr.URL, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = int64(len(data))
	for k, v := range tr.Headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func downloadVia(t *testing.T, tr artifacts.Transfer) (int, []byte) {
	t.Helper()
	resp, err := http.Get(tr.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}
