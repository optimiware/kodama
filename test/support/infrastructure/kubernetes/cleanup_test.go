package kubesupport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/optimiware/kodama/test/support/infrastructure/harness"
)

// argValue returns the value following flag in args.
func argValue(args []string, flag string) string {
	for index := 0; index+1 < len(args); index++ {
		if args[index] == flag {
			return args[index+1]
		}
	}
	return ""
}

// removeName returns names without one occurrence of name.
func removeName(names []string, name string) []string {
	out := make([]string, 0, len(names))
	removed := false
	for _, candidate := range names {
		if !removed && candidate == name {
			removed = true
			continue
		}
		out = append(out, candidate)
	}
	return out
}

// containsCluster reports whether clusters contains want.
func containsCluster(clusters []*Cluster, want *Cluster) bool {
	return slices.Contains(clusters, want)
}

// digestOf returns the lowercase hex SHA-256 of data.
func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// writeKindCache installs data as the pinned kind cache file.
func writeKindCache(t *testing.T, dir string, data []byte) string {
	t.Helper()
	path := filepath.Join(dir, "kind-"+KindVersion+"-linux-amd64")
	if err := os.WriteFile(path, data, 0o755); err != nil {
		t.Fatalf("write kind cache: %v", err)
	}
	return path
}

// TestClusterCloseRetriesTransientDeleteFailure proves a transient kind delete
// failure is retried and the cluster is unregistered only after it succeeds.
func TestClusterCloseRetriesTransientDeleteFailure(t *testing.T) {
	deleteCalls := 0
	kind, _ := newTestKind(t, func(command harness.Command) (harness.Result, error) {
		switch {
		case hasArgs(command.Args, "create", "cluster"):
			return createHandler(t, nil)(command)
		case hasArgs(command.Args, "delete", "cluster"):
			deleteCalls++
			if deleteCalls == 1 {
				return harness.Result{}, errors.New("podman: transient delete failure")
			}
			return harness.Result{}, nil
		default:
			return harness.Result{}, nil
		}
	})
	cluster, err := kind.CreateCluster(context.Background(), ClusterOptions{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	if err := cluster.Close(); err != nil {
		t.Fatalf("Close after one transient delete failure: %v", err)
	}
	if deleteCalls != 2 {
		t.Fatalf("delete attempts = %d; want 2 (first failed, second succeeded)", deleteCalls)
	}
	if len(ActiveClusters()) != 0 {
		t.Fatalf("successfully deleted cluster is still registered")
	}
}

// TestClusterCloseSurfacesPersistentFailureAndKeepsRegistration proves a delete
// that keeps failing is retried a bounded number of times, the cluster stays
// registered for the post-run sweep, Close returns the error (so the fixture
// fails instead of logging only), and PostRunCleanup returns an error while the
// own-pid cluster is still listed (so Main exits non-zero).
func TestClusterCloseSurfacesPersistentFailureAndKeepsRegistration(t *testing.T) {
	deleteCalls := 0
	var listed []string
	handler := func(command harness.Command) (harness.Result, error) {
		switch {
		case hasArgs(command.Args, "version"):
			return harness.Result{Stdout: []byte(kindVersionOutput)}, nil
		case hasArgs(command.Args, "create", "cluster"):
			result, err := createHandler(t, nil)(command)
			if err == nil {
				listed = append(listed, argValue(command.Args, "--name"))
			}
			return result, err
		case hasArgs(command.Args, "get", "clusters"):
			out := strings.Join(listed, "\n")
			if out != "" {
				out += "\n"
			}
			return harness.Result{Stdout: []byte(out)}, nil
		case hasArgs(command.Args, "delete", "cluster"):
			deleteCalls++
			return harness.Result{}, errors.New("podman: delete failed")
		default:
			return harness.Result{}, nil
		}
	}
	kind, runner := newTestKind(t, handler)
	cluster, err := kind.CreateCluster(context.Background(), ClusterOptions{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	t.Cleanup(func() { unregisterCluster(cluster) })

	if err := cluster.Close(); err == nil {
		t.Fatal("Close returned nil after every delete attempt failed; want the failure surfaced")
	}
	if deleteCalls != deleteAttempts {
		t.Fatalf("delete attempts = %d; want %d", deleteCalls, deleteAttempts)
	}
	if !containsCluster(ActiveClusters(), cluster) {
		t.Fatal("cluster was unregistered after a failed delete; want it kept for the post-run sweep")
	}
	if err := cluster.Close(); err == nil {
		t.Fatal("second Close returned nil; want the cached failure")
	}
	if deleteCalls != deleteAttempts {
		t.Fatalf("second Close ran %d deletes; want the cached result without another attempt", deleteCalls)
	}

	err = PostRunCleanup(context.Background(),
		WithBinary("/fake/kind"),
		WithRunner(runner),
		WithEnv([]string{"PATH=/usr/bin:/bin", "HOME=/tmp", ProviderEnv}),
		WithLogger(t.Logf),
	)
	if err == nil {
		t.Fatal("PostRunCleanup returned nil while the own-pid cluster still exists")
	}
	if !strings.Contains(err.Error(), "remain after cleanup") {
		t.Fatalf("PostRunCleanup error %q does not report the remaining own-pid cluster", err)
	}
}

// TestPostRunCleanupSweepsOwnPIDOrphansAndProtectsOthers proves the post-run
// sweep deletes an own-pid cluster that is no longer registered, deletes a
// dead-owner cluster, and never touches kodama-probe, a foreign name, or a live
// other-pid cluster.
func TestPostRunCleanupSweepsOwnPIDOrphansAndProtectsOthers(t *testing.T) {
	otherLivePID := 424242
	ownOrphan := fmt.Sprintf("%s%d-abcdef01", ClusterPrefix, os.Getpid())
	deadOther := fmt.Sprintf("%s%d-abcdef02", ClusterPrefix, 999999)
	liveOther := fmt.Sprintf("%s%d-abcdef03", ClusterPrefix, otherLivePID)
	names := []string{ProtectedCluster, ownOrphan, deadOther, liveOther, "foreign-cluster"}

	var deleted []string
	runner := &fakeRunner{}
	runner.handler = func(command harness.Command) (harness.Result, error) {
		switch {
		case hasArgs(command.Args, "version"):
			return harness.Result{Stdout: []byte(kindVersionOutput)}, nil
		case hasArgs(command.Args, "get", "clusters"):
			return harness.Result{Stdout: []byte(strings.Join(names, "\n") + "\n")}, nil
		case hasArgs(command.Args, "delete", "cluster"):
			name := argValue(command.Args, "--name")
			deleted = append(deleted, name)
			names = removeName(names, name)
			return harness.Result{}, nil
		default:
			return harness.Result{}, nil
		}
	}

	if active := ActiveClusters(); len(active) != 0 {
		t.Fatalf("%d cluster(s) registered before the post-run sweep test", len(active))
	}
	err := PostRunCleanup(context.Background(),
		WithBinary("/fake/kind"),
		WithRunner(runner),
		WithEnv([]string{"PATH=/usr/bin:/bin", "HOME=/tmp", ProviderEnv}),
		WithProcessAlive(func(pid int) bool { return pid == otherLivePID || pid == os.Getpid() }),
		WithLogger(t.Logf),
	)
	if err != nil {
		t.Fatalf("PostRunCleanup: %v", err)
	}
	want := []string{ownOrphan, deadOther}
	if strings.Join(deleted, ",") != strings.Join(want, ",") {
		t.Fatalf("deleted clusters = %v; want %v", deleted, want)
	}
	for _, forbidden := range []string{ProtectedCluster, liveOther, "foreign-cluster"} {
		if containsString(deleted, forbidden) {
			t.Fatalf("post-run sweep touched %q: %v", forbidden, deleted)
		}
	}
	for _, command := range runner.snapshot() {
		if !hasArgs(command.Args, "delete", "cluster") {
			continue
		}
		if name := argValue(command.Args, "--name"); name == ProtectedCluster || name == liveOther || name == "foreign-cluster" {
			t.Fatalf("post-run sweep issued a delete for %q", name)
		}
	}
}

// TestCreateClusterFailureDeletesExactGeneratedName proves a failed create
// best-effort deletes the exact generated name it attempted, so a timed-out or
// killed create cannot leak an unregistered cluster.
func TestCreateClusterFailureDeletesExactGeneratedName(t *testing.T) {
	kind, runner := newTestKind(t, func(command harness.Command) (harness.Result, error) {
		if hasArgs(command.Args, "create", "cluster") {
			return harness.Result{}, errors.New("kind: create timed out")
		}
		return harness.Result{}, nil
	})
	if _, err := kind.CreateCluster(context.Background(), ClusterOptions{Dir: t.TempDir()}); err == nil {
		t.Fatal("CreateCluster returned nil for a failed create")
	}

	createName := ""
	var deletes []recordedCommand
	for _, command := range runner.snapshot() {
		switch {
		case hasArgs(command.Args, "create", "cluster"):
			createName = argValue(command.Args, "--name")
		case hasArgs(command.Args, "delete", "cluster"):
			deletes = append(deletes, command)
		}
	}
	if createName == "" {
		t.Fatal("no create command recorded")
	}
	if _, ok := clusterOwner(createName); !ok {
		t.Fatalf("create name %q is not a generated name", createName)
	}
	if len(deletes) != 1 {
		t.Fatalf("delete commands after failed create = %d; want 1", len(deletes))
	}
	if name := argValue(deletes[0].Args, "--name"); name != createName {
		t.Fatalf("cleanup deleted %q; want the exact generated name %q", name, createName)
	}
}

// TestSweepDeadOwnersBestEffortResolvesVerifiedCacheWithoutDownload proves the
// cleanup path reuses the already verified pinned cache file when no
// KODAMA_KIND_BIN or PATH kind exists, refuses a wrong-digest cache file
// without downloading, and silently skips only when no binary exists anywhere.
func TestSweepDeadOwnersBestEffortResolvesVerifiedCacheWithoutDownload(t *testing.T) {
	emptyPath := t.TempDir()
	t.Setenv(KindBinaryEnv, "")
	t.Setenv(KindCacheEnv, "")
	t.Setenv("PATH", emptyPath)

	data := []byte("kind-binary-fixture")
	digest := digestOf(data)
	cache := t.TempDir()
	cachePath := writeKindCache(t, cache, data)

	deadOwner := fmt.Sprintf("%s%d-abcdef01", ClusterPrefix, 999999)
	runner := &fakeRunner{}
	runner.handler = func(command harness.Command) (harness.Result, error) {
		switch {
		case hasArgs(command.Args, "version"):
			return harness.Result{Stdout: []byte(kindVersionOutput)}, nil
		case hasArgs(command.Args, "get", "clusters"):
			return harness.Result{Stdout: []byte(deadOwner + "\n")}, nil
		case hasArgs(command.Args, "delete", "cluster"):
			return harness.Result{}, nil
		default:
			return harness.Result{}, nil
		}
	}
	fetched := 0
	fetcher := func(context.Context, string) ([]byte, error) {
		fetched++
		return nil, errors.New("download is forbidden in cleanup")
	}

	err := SweepDeadOwnersBestEffort(context.Background(),
		WithCacheDir(cache),
		WithRunner(runner),
		WithEnv([]string{"PATH=" + emptyPath, "HOME=/tmp", ProviderEnv}),
		WithFetcher(fetcher),
		WithProcessAlive(func(int) bool { return false }),
		withExpectedDigest(digest),
	)
	if err != nil {
		t.Fatalf("SweepDeadOwnersBestEffort with a verified cache file: %v", err)
	}
	if fetched != 0 {
		t.Fatalf("cleanup downloaded %d times; want 0", fetched)
	}
	commands := runner.snapshot()
	sawVersionFromCache := false
	sawList := false
	sawDelete := false
	for _, command := range commands {
		if command.Name == cachePath && hasArgs(command.Args, "version") {
			sawVersionFromCache = true
		}
		if hasArgs(command.Args, "get", "clusters") {
			sawList = true
		}
		if hasArgs(command.Args, "delete", "cluster") && argValue(command.Args, "--name") == deadOwner {
			sawDelete = true
		}
	}
	if !sawVersionFromCache {
		t.Fatalf("cleanup did not resolve the verified cache file %s: %v", cachePath, commands)
	}
	if !sawList || !sawDelete {
		t.Fatalf("cleanup did not list and delete the dead-owner cluster (list=%v delete=%v)", sawList, sawDelete)
	}

	// A wrong-digest cache file is refused, not used, and nothing is downloaded.
	badCache := t.TempDir()
	writeKindCache(t, badCache, []byte("tampered"))
	fetched = 0
	err = SweepDeadOwnersBestEffort(context.Background(),
		WithCacheDir(badCache),
		WithRunner(runner),
		WithEnv([]string{"PATH=" + emptyPath, "HOME=/tmp", ProviderEnv}),
		WithFetcher(fetcher),
		withExpectedDigest(digest),
	)
	if !errors.Is(err, ErrKindUnavailable) {
		t.Fatalf("wrong-digest cache error = %v; want ErrKindUnavailable", err)
	}
	if errors.Is(err, errKindBinaryAbsent) {
		t.Fatalf("wrong-digest cache was reported as no binary anywhere: %v", err)
	}
	if fetched != 0 {
		t.Fatalf("wrong-digest cleanup downloaded %d times; want 0", fetched)
	}

	// No binary anywhere is the only silent skip.
	emptyCache := t.TempDir()
	before := runner.count()
	fetched = 0
	err = SweepDeadOwnersBestEffort(context.Background(),
		WithCacheDir(emptyCache),
		WithRunner(runner),
		WithEnv([]string{"PATH=" + emptyPath, "HOME=/tmp", ProviderEnv}),
		WithFetcher(fetcher),
		withExpectedDigest(digest),
	)
	if err != nil {
		t.Fatalf("no binary anywhere should be a silent skip: %v", err)
	}
	if runner.count() != before {
		t.Fatalf("no-binary cleanup ran %d command(s); want none", runner.count()-before)
	}
	if fetched != 0 {
		t.Fatalf("no-binary cleanup downloaded %d times; want 0", fetched)
	}
}

// newCleanupRunner builds a fake kind runner that answers the pinned version
// check and delegates every other command to handler.
func newCleanupRunner(handler func(harness.Command) (harness.Result, error)) *fakeRunner {
	runner := &fakeRunner{}
	runner.handler = func(command harness.Command) (harness.Result, error) {
		if hasArgs(command.Args, "version") {
			return harness.Result{Stdout: []byte(kindVersionOutput)}, nil
		}
		return handler(command)
	}
	return runner
}

// requireNoActiveClusters proves the post-run tests start from a clean registry.
func requireNoActiveClusters(t *testing.T) {
	t.Helper()
	if active := ActiveClusters(); len(active) != 0 {
		t.Fatalf("%d cluster(s) registered before the post-run cleanup test", len(active))
	}
}

// TestPostRunCleanupIgnoresForeignSweepRace proves a dead-owner cluster that a
// concurrent lane removes between the sweep's list and its delete cannot fail
// this run. The foreign cluster belongs to no live process of this run, so its
// disappearance is not this run's leak.
func TestPostRunCleanupIgnoresForeignSweepRace(t *testing.T) {
	requireNoActiveClusters(t)

	livePID := 424242
	deadPID := 999999
	liveForeign := fmt.Sprintf("%s%d-abcdef01", ClusterPrefix, livePID)
	deadForeign := fmt.Sprintf("%s%d-abcdef02", ClusterPrefix, deadPID)
	names := []string{ProtectedCluster, liveForeign, deadForeign}

	var deleted []string
	runner := newCleanupRunner(func(command harness.Command) (harness.Result, error) {
		switch {
		case hasArgs(command.Args, "get", "clusters"):
			return harness.Result{Stdout: []byte(strings.Join(names, "\n") + "\n")}, nil
		case hasArgs(command.Args, "delete", "cluster"):
			name := argValue(command.Args, "--name")
			deleted = append(deleted, name)
			if name == deadForeign {
				// Another lane removed the foreign cluster between our list
				// and this delete; the provider reports it missing.
				names = removeName(names, name)
				return harness.Result{}, errors.New("kind: no such cluster")
			}
			names = removeName(names, name)
			return harness.Result{}, nil
		default:
			return harness.Result{}, nil
		}
	})

	err := PostRunCleanup(context.Background(),
		WithBinary("/fake/kind"),
		WithRunner(runner),
		WithEnv([]string{"PATH=/usr/bin:/bin", "HOME=/tmp", ProviderEnv}),
		WithProcessAlive(func(pid int) bool { return pid == livePID }),
		WithLogger(t.Logf),
	)
	if err != nil {
		t.Fatalf("PostRunCleanup failed on a foreign sweep race: %v", err)
	}
	if !containsString(deleted, deadForeign) {
		t.Fatalf("no delete was attempted for the racing foreign cluster: %v", deleted)
	}
	for _, forbidden := range []string{ProtectedCluster, liveForeign} {
		if containsString(deleted, forbidden) {
			t.Fatalf("post-run sweep deleted %q: %v", forbidden, deleted)
		}
	}
}

// TestPostRunCleanupFailsOwnLeak proves the post-run verdict stays fatal for a
// cluster this process (this pid) owns and for an unverifiable own cleanup.
func TestPostRunCleanupFailsOwnLeak(t *testing.T) {
	t.Run("own leak remains fatal", func(t *testing.T) {
		requireNoActiveClusters(t)

		ownOrphan := fmt.Sprintf("%s%d-abcdef01", ClusterPrefix, os.Getpid())
		names := []string{ProtectedCluster, ownOrphan}
		var deleted []string
		runner := newCleanupRunner(func(command harness.Command) (harness.Result, error) {
			switch {
			case hasArgs(command.Args, "get", "clusters"):
				return harness.Result{Stdout: []byte(strings.Join(names, "\n") + "\n")}, nil
			case hasArgs(command.Args, "delete", "cluster"):
				name := argValue(command.Args, "--name")
				deleted = append(deleted, name)
				if name == ownOrphan {
					return harness.Result{}, errors.New("podman: delete failed")
				}
				names = removeName(names, name)
				return harness.Result{}, nil
			default:
				return harness.Result{}, nil
			}
		})

		err := PostRunCleanup(context.Background(),
			WithBinary("/fake/kind"),
			WithRunner(runner),
			WithEnv([]string{"PATH=/usr/bin:/bin", "HOME=/tmp", ProviderEnv}),
			WithProcessAlive(func(int) bool { return true }),
			WithLogger(t.Logf),
		)
		if err == nil {
			t.Fatal("PostRunCleanup returned nil while an own-pid cluster remains")
		}
		if !strings.Contains(err.Error(), "remain after cleanup") || !strings.Contains(err.Error(), ownOrphan) {
			t.Fatalf("own leak error %q does not name the remaining own-pid cluster", err)
		}
		if containsString(deleted, ProtectedCluster) {
			t.Fatalf("post-run sweep deleted the protected cluster: %v", deleted)
		}
	})

	t.Run("unverifiable own cleanup stays fatal", func(t *testing.T) {
		requireNoActiveClusters(t)

		runner := newCleanupRunner(func(command harness.Command) (harness.Result, error) {
			if hasArgs(command.Args, "get", "clusters") {
				return harness.Result{}, errors.New("podman: cannot list clusters")
			}
			return harness.Result{}, nil
		})
		err := PostRunCleanup(context.Background(),
			WithBinary("/fake/kind"),
			WithRunner(runner),
			WithEnv([]string{"PATH=/usr/bin:/bin", "HOME=/tmp", ProviderEnv}),
			WithLogger(t.Logf),
		)
		if err == nil {
			t.Fatal("PostRunCleanup returned nil when the own-cluster probe could not list clusters")
		}
		if !strings.Contains(err.Error(), "list clusters") {
			t.Fatalf("unverifiable cleanup error %q does not report the list failure", err)
		}
	})
}
