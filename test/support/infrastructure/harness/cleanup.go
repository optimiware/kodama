package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// ProcessAlive reports whether pid exists. EPERM still proves the process is
// alive (it belongs to another user).
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// CleanupOrphans removes containers, volumes and networks labelled
// kodama.test=1 whose kodama.owner-pid is no longer alive. It never removes a
// resource owned by a live process. Cleanup runs containers first, then
// volumes, then networks.
func (p *Podman) CleanupOrphans(ctx context.Context) error {
	var errs []error
	for _, kind := range []string{"container", "volume", "network"} {
		names, err := p.labeledNames(ctx, kind)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, name := range names {
			labels, err := p.labels(ctx, kind, name)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if !p.orphan(labels) {
				continue
			}
			p.logf("harness: removing orphaned %s %s (owner pid %s)", kind, name, labels[LabelOwnerPID])
			if _, err := p.run(ctx, p.operationLimit, orphanRemoveArgs(kind, name)...); err != nil {
				errs = append(errs, fmt.Errorf("harness: remove orphaned %s %s: %w", kind, name, err))
			}
		}
	}
	return errors.Join(errs...)
}

// CleanupOrphans is the package-level convenience used by TestMain helpers.
func CleanupOrphans(ctx context.Context) error {
	return NewPodman().CleanupOrphans(ctx)
}

func (p *Podman) orphan(labels map[string]string) bool {
	if labels[LabelTest] != "1" {
		return false
	}
	pidText := labels[LabelOwnerPID]
	if pidText == "" {
		// A labelled resource without an owner cannot be proven live.
		return true
	}
	pid, err := strconv.Atoi(pidText)
	if err != nil || pid <= 0 {
		return true
	}
	return !p.processAlive(pid)
}

func orphanRemoveArgs(kind, name string) []string {
	switch kind {
	case "container":
		return []string{"rm", "--force", "--volumes", name}
	case "volume":
		return []string{"volume", "rm", "--force", name}
	default:
		return []string{"network", "rm", "--force", name}
	}
}

func (p *Podman) labeledNames(ctx context.Context, kind string) ([]string, error) {
	var args []string
	switch kind {
	case "container":
		args = []string{"ps", "-a", "--filter", "label=" + LabelTest + "=1", "--format", "{{.Names}}"}
	case "volume":
		args = []string{"volume", "ls", "--filter", "label=" + LabelTest + "=1", "--format", "{{.Name}}"}
	case "network":
		args = []string{"network", "ls", "--filter", "label=" + LabelTest + "=1", "--format", "{{.Name}}"}
	default:
		return nil, fmt.Errorf("harness: unknown resource kind %q", kind)
	}
	res, err := p.run(ctx, p.operationLimit, args...)
	if err != nil {
		return nil, fmt.Errorf("harness: list %ss: %w", kind, err)
	}
	var names []string
	for line := range strings.SplitSeq(string(res.Stdout), "\n") {
		if name := strings.TrimSpace(line); name != "" {
			names = append(names, name)
		}
	}
	return names, nil
}

func (p *Podman) labels(ctx context.Context, kind, name string) (map[string]string, error) {
	var args []string
	switch kind {
	case "container":
		args = []string{"inspect", "--format", "{{json .Config.Labels}}", name}
	case "volume":
		args = []string{"volume", "inspect", "--format", "{{json .Labels}}", name}
	case "network":
		args = []string{"network", "inspect", "--format", "{{json .Labels}}", name}
	default:
		return nil, fmt.Errorf("harness: unknown resource kind %q", kind)
	}
	res, err := p.run(ctx, p.operationLimit, args...)
	if err != nil {
		return nil, fmt.Errorf("harness: inspect %s %s: %w", kind, name, err)
	}
	text := strings.TrimSpace(string(res.Stdout))
	if text == "" || text == "null" {
		return map[string]string{}, nil
	}
	labels := map[string]string{}
	if err := json.Unmarshal([]byte(text), &labels); err != nil {
		return nil, fmt.Errorf("harness: decode %s %s labels: %w", kind, name, err)
	}
	return labels, nil
}

// Main is the TestMain helper for component test binaries. It installs
// SIGINT/SIGTERM handling that cleans up active shard resources and orphaned
// containers before exiting, runs m, and sweeps dead-owner resources afterwards.
func Main(m *testing.M) int {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)

	done := make(chan struct{})
	var stopOnce sync.Once
	go func() {
		select {
		case sig := <-signals:
			code := HandleSignal(sig, cleanupForSignal)
			os.Exit(code)
		case <-done:
		}
	}()

	code := m.Run()
	stopOnce.Do(func() { close(done) })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := cleanupForSignal(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "harness: post-run cleanup: %v\n", err)
	}
	return code
}

// HandleSignal performs bounded cleanup for sig and returns the exit code the
// test binary should use. A nil signal is a no-op.
func HandleSignal(sig os.Signal, cleanup func(context.Context) error) int {
	if sig == nil {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if cleanup != nil {
		if err := cleanup(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "harness: signal %v cleanup: %v\n", sig, err)
		}
	}
	return 1
}

// cleanupForSignal removes every active shard's resources and then any
// dead-owner resources labelled kodama.test=1.
func cleanupForSignal(ctx context.Context) error {
	var errs []error
	for _, shard := range snapshotShards() {
		if err := shard.cleanup(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	if err := CleanupOrphans(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
