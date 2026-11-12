//go:build component

package kubernetesconformance

// Deterministic concurrency regression for kind deletion/cleanup isolation.
//
// The suite allows up to four concurrent kind clusters. This test drives
// four independent Kind clients over one shared fake provider at a barrier, so
// cluster closes and dead-owner sweeps overlap. It proves the deletion path
// never selects another client's cluster, never touches the protected
// kodama-probe cluster or a live foreign cluster, and always names a private
// kubeconfig instead of the ambient host kubeconfig.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/optimiware/kodama/test/support/infrastructure/harness"
	kubesupport "github.com/optimiware/kodama/test/support/infrastructure/kubernetes"
)

const isolationKindVersionOutput = "kind v0.33.0 go1.26.7 linux/amd64\n"

// isolationDelete is one recorded destructive command.
type isolationDelete struct {
	kubeconfig string
	dirExisted bool
}

// isolationProvider is the shared kind provider state the fake runners read
// and mutate under one mutex.
type isolationProvider struct {
	mu      sync.Mutex
	names   map[string]struct{}
	deletes map[string][]isolationDelete
}

func newIsolationProvider(names ...string) *isolationProvider {
	provider := &isolationProvider{
		names:   make(map[string]struct{}, len(names)),
		deletes: map[string][]isolationDelete{},
	}
	for _, name := range names {
		provider.names[name] = struct{}{}
	}
	return provider
}

func (p *isolationProvider) runner() harness.Runner { return &isolationRunner{provider: p} }

func (p *isolationProvider) listing() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	names := make([]string, 0, len(p.names))
	for name := range p.names {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (p *isolationProvider) deleteLog() map[string][]isolationDelete {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string][]isolationDelete, len(p.deletes))
	for name, entries := range p.deletes {
		out[name] = append([]isolationDelete(nil), entries...)
	}
	return out
}

type isolationRunner struct {
	provider *isolationProvider
}

// Run implements harness.Runner.
func (r *isolationRunner) Run(_ context.Context, command harness.Command) (harness.Result, error) {
	switch {
	case len(command.Args) == 1 && command.Args[0] == "version":
		return harness.Result{Stdout: []byte(isolationKindVersionOutput)}, nil
	case isolationHasArgs(command.Args, "create", "cluster"):
		name := isolationArgValue(command.Args, "--name")
		kubeconfig := isolationArgValue(command.Args, "--kubeconfig")
		if name == "" || kubeconfig == "" {
			return harness.Result{}, fmt.Errorf("isolation fixture: create without name or kubeconfig: %v", command.Args)
		}
		if err := os.WriteFile(kubeconfig, []byte("apiVersion: v1\nkind: Config\n"), 0o600); err != nil {
			return harness.Result{}, err
		}
		r.provider.mu.Lock()
		r.provider.names[name] = struct{}{}
		r.provider.mu.Unlock()
		return harness.Result{}, nil
	case isolationHasArgs(command.Args, "get", "clusters"):
		return harness.Result{Stdout: []byte(strings.Join(r.provider.listing(), "\n") + "\n")}, nil
	case isolationHasArgs(command.Args, "delete", "cluster"):
		name := isolationArgValue(command.Args, "--name")
		kubeconfig := isolationArgValue(command.Args, "--kubeconfig")
		_, statErr := os.Stat(filepath.Dir(kubeconfig))
		r.provider.mu.Lock()
		r.provider.deletes[name] = append(r.provider.deletes[name], isolationDelete{
			kubeconfig: kubeconfig,
			dirExisted: kubeconfig != "" && statErr == nil,
		})
		delete(r.provider.names, name)
		r.provider.mu.Unlock()
		return harness.Result{}, nil
	default:
		return harness.Result{}, nil
	}
}

// TestConcurrentClusterCleanupIsolation proves concurrent closes and
// dead-owner sweeps keep the protected cluster and live foreign clusters
// intact, delete only their own names, and never fall back to the ambient
// host kubeconfig.
func TestConcurrentClusterCleanupIsolation(t *testing.T) {
	livePID := 424242
	deadPID := 999999
	liveForeign := fmt.Sprintf("%s%d-abcdef01", kubesupport.ClusterPrefix, livePID)
	deadForeign := fmt.Sprintf("%s%d-abcdef02", kubesupport.ClusterPrefix, deadPID)
	provider := newIsolationProvider(kubesupport.ProtectedCluster, liveForeign, deadForeign)

	home := t.TempDir()
	sentinel := filepath.Join(home, ".kube", "config")
	if err := os.MkdirAll(filepath.Dir(sentinel), 0o700); err != nil {
		t.Fatalf("create sentinel kubeconfig directory: %v", err)
	}
	const sentinelBody = "apiVersion: v1\nkind: Config\ncurrent-context: host-sentinel\nclusters: []\n"
	if err := os.WriteFile(sentinel, []byte(sentinelBody), 0o600); err != nil {
		t.Fatalf("write sentinel kubeconfig: %v", err)
	}
	env := []string{
		"PATH=/usr/bin:/bin",
		"HOME=" + home,
		"KUBECONFIG=" + sentinel,
		kubesupport.ProviderEnv,
	}
	alive := func(pid int) bool { return pid == livePID || pid == os.Getpid() }

	const clients = 4
	clusters := make([]*kubesupport.Cluster, clients)
	kinds := make([]*kubesupport.Kind, clients)
	for index := 0; index < clients; index++ {
		kind, err := kubesupport.NewKind(
			kubesupport.WithBinary("/fake/kind"),
			kubesupport.WithRunner(provider.runner()),
			kubesupport.WithEnv(env),
			kubesupport.WithProcessAlive(alive),
		)
		if err != nil {
			t.Fatalf("NewKind client %d: %v", index, err)
		}
		cluster, err := kind.CreateCluster(context.Background(), kubesupport.ClusterOptions{Dir: t.TempDir()})
		if err != nil {
			t.Fatalf("CreateCluster client %d: %v", index, err)
		}
		t.Cleanup(func() { _ = cluster.Close() })
		clusters[index] = cluster
		kinds[index] = kind
	}

	// Barrier: every close and sweep starts together. No sleeps.
	start := make(chan struct{})
	errs := make(chan error, clients*2)
	var wait sync.WaitGroup
	for index := range clusters {
		cluster := clusters[index]
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			if err := cluster.Close(); err != nil {
				errs <- fmt.Errorf("close %s: %w", cluster.Name, err)
			}
		}()
	}
	for index := range kinds {
		kind := kinds[index]
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			if _, err := kind.SweepDeadOwners(context.Background()); err != nil {
				errs <- fmt.Errorf("concurrent dead-owner sweep: %w", err)
			}
		}()
	}
	close(start)
	wait.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if t.Failed() {
		return
	}

	remaining := provider.listing()
	for _, want := range []string{kubesupport.ProtectedCluster, liveForeign} {
		if !isolationContains(remaining, want) {
			t.Fatalf("protected or live cluster %q was removed by a concurrent cleanup: %v", want, remaining)
		}
	}
	if isolationContains(remaining, deadForeign) {
		t.Fatalf("dead-owner cluster %q survived the sweep: %v", deadForeign, remaining)
	}
	for _, cluster := range clusters {
		if isolationContains(remaining, cluster.Name) {
			t.Fatalf("own cluster %q survived its close: %v", cluster.Name, remaining)
		}
	}

	deletes := provider.deleteLog()
	for _, forbidden := range []string{kubesupport.ProtectedCluster, liveForeign} {
		if entries := deletes[forbidden]; len(entries) != 0 {
			t.Fatalf("concurrent cleanup deleted %q: %+v", forbidden, entries)
		}
	}
	for _, cluster := range clusters {
		entries := deletes[cluster.Name]
		if len(entries) != 1 {
			t.Fatalf("cluster %q delete count = %d; want 1", cluster.Name, len(entries))
		}
		if entries[0].kubeconfig != cluster.KubeconfigPath {
			t.Fatalf("cluster %q delete used kubeconfig %q; want %q", cluster.Name, entries[0].kubeconfig, cluster.KubeconfigPath)
		}
		if !entries[0].dirExisted {
			t.Fatalf("cluster %q delete used a kubeconfig directory that did not exist: %q", cluster.Name, entries[0].kubeconfig)
		}
	}
	deadEntries := deletes[deadForeign]
	if len(deadEntries) == 0 {
		t.Fatalf("dead-owner cluster %q was never swept", deadForeign)
	}
	for name, entries := range deletes {
		for _, entry := range entries {
			if entry.kubeconfig == "" {
				t.Fatalf("destructive command for %q has no --kubeconfig", name)
			}
			if filepath.Clean(entry.kubeconfig) == filepath.Clean(sentinel) {
				t.Fatalf("destructive command for %q used the host kubeconfig %s", name, sentinel)
			}
			if strings.HasPrefix(entry.kubeconfig, home+string(os.PathSeparator)) {
				t.Fatalf("destructive command for %q used a path under the host HOME: %s", name, entry.kubeconfig)
			}
			if !entry.dirExisted {
				t.Fatalf("destructive command for %q used a kubeconfig directory that did not exist: %s", name, entry.kubeconfig)
			}
			for _, cluster := range clusters {
				if name != cluster.Name && entry.kubeconfig == cluster.KubeconfigPath {
					t.Fatalf("destructive command for %q used cluster %q's kubeconfig", name, cluster.Name)
				}
			}
		}
	}

	body, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatalf("read sentinel kubeconfig: %v", err)
	}
	if string(body) != sentinelBody {
		t.Fatalf("host kubeconfig was modified by concurrent cleanup:\n%s", body)
	}
}

func isolationArgValue(args []string, flag string) string {
	for index := 0; index+1 < len(args); index++ {
		if args[index] == flag {
			return args[index+1]
		}
	}
	return ""
}

func isolationHasArgs(args []string, want ...string) bool {
	for index := 0; index+len(want) <= len(args); index++ {
		matched := true
		for offset, candidate := range want {
			if args[index+offset] != candidate {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func isolationContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
