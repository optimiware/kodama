// Package runprofile is the bounded P1 qualification driver for the rootless
// run-resource profile (ADR 0010, T-111) as revised by O-30 (T-124): storage,
// ko and Buildah as ordinary steps in the S3 `step` role, PostgreSQL and the
// tenant network model. BuildKit and browser are out of scope. It probes a named Kubernetes target
// with task-owned, uniquely labelled, quota-bounded namespaces and records
// retained receipts. It never changes node, runtime, storage, CNI or global
// policy configuration, never uses host privilege and never reports a
// combination qualified without a passing real probe.
//
// It is intentionally separate from the legacy kind-only attempt tests in
// test/integration/kube: it assumes no local kind container, imports no image
// into a node and needs no host callback address.
package runprofile

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

//go:embed pins.json
var pinsJSON []byte

// LeasePath is the single manual integration lease shared with T-112.
const LeasePath = "/tmp/opencode/kodama-run-resources-integration.lock"

// Pins are the exact image inputs.
type Pins struct {
	Images map[string]PinImage `json:"images"`
}

// PinImage is one digest-pinned image.
type PinImage struct {
	Ref  string `json:"ref"`
	Tag  string `json:"tag"`
	Role string `json:"role"`
}

// LoadPins parses the embedded pins and refuses mutable references.
func LoadPins() (*Pins, error) {
	var p Pins
	if err := json.Unmarshal(pinsJSON, &p); err != nil {
		return nil, err
	}
	for _, name := range []string{"ko", "buildah", "registry", "httpd", "postgres"} {
		img, ok := p.Images[name]
		if !ok || !strings.Contains(img.Ref, "@sha256:") || len(img.Ref[strings.Index(img.Ref, "@sha256:")+8:]) != 64 {
			return nil, fmt.Errorf("pins: image %q missing or not digest-pinned", name)
		}
	}
	return &p, nil
}

// Config selects the target. Every field is explicit; nothing is guessed from
// the ambient environment, so a missing prerequisite fails closed.
type Config struct {
	Kubectl      string
	Kubeconfig   string
	Target       string
	StorageClass string
	ReportDir    string
	Commit       string
}

// ConfigFromEnv reads the explicit configuration and refuses to continue
// when anything required is absent.
func ConfigFromEnv(getenv func(string) string) (*Config, error) {
	var missing []string
	need := func(name string) string {
		v := strings.TrimSpace(getenv(name))
		if v == "" {
			missing = append(missing, name)
		}
		return v
	}
	cfg := &Config{
		Kubeconfig:   need("KODAMA_RUNPROFILE_KUBECONFIG"),
		Target:       need("KODAMA_RUNPROFILE_TARGET"),
		StorageClass: need("KODAMA_RUNPROFILE_STORAGE_CLASS"),
		ReportDir:    strings.TrimSpace(getenv("KODAMA_RUNPROFILE_REPORT_DIR")),
		Kubectl:      strings.TrimSpace(getenv("KODAMA_RUNPROFILE_KUBECTL")),
		Commit:       strings.TrimSpace(getenv("KODAMA_RUNPROFILE_COMMIT")),
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("run-resource profile qualification has no target: set %s (the legacy kind scope does not qualify this profile)", strings.Join(missing, ", "))
	}
	if cfg.Kubectl == "" {
		cfg.Kubectl = "kubectl"
	}
	path, err := exec.LookPath(cfg.Kubectl)
	if err != nil {
		return nil, fmt.Errorf("kubectl is required: %w", err)
	}
	cfg.Kubectl = path
	if _, err := os.Stat(cfg.Kubeconfig); err != nil {
		return nil, fmt.Errorf("kubeconfig is not readable: %w", err)
	}
	return cfg, nil
}

// AcquireLease takes the shared manual integration lease, waiting up to
// wait. The lock is held for the life of the returned release function.
func AcquireLease(path string, wait time.Duration) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) || time.Now().After(deadline) {
			_ = f.Close()
			return nil, fmt.Errorf("integration lease %s unavailable: %w", path, err)
		}
		time.Sleep(2 * time.Second)
	}
}

// Qualifier carries one run's state.
type Qualifier struct {
	cfg  *Config
	pins *Pins
	c    *Cluster
	rep  *Report
	dir  string

	nsMain, nsNetA, nsNetB string
	reg                    *registryFixture
}

// Run executes the whole qualification and returns the report. It always
// attempts cleanup and always writes the report, even after a failed phase.
func Run(ctx context.Context, cfg *Config) (*Report, error) {
	pins, err := LoadPins()
	if err != nil {
		return nil, err
	}
	runID, err := NewRunID()
	if err != nil {
		return nil, err
	}
	dir := cfg.ReportDir
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "kodama-run-profile-"+runID)
	}
	dir = filepath.Join(dir, "krp-t111-"+runID)
	rec, err := NewRecorder(dir)
	if err != nil {
		return nil, err
	}
	defer rec.Close()
	q := &Qualifier{cfg: cfg, pins: pins, dir: dir}
	q.c = &Cluster{K: &Kubectl{Binary: cfg.Kubectl, Kubeconfig: cfg.Kubeconfig, Rec: rec}, RunID: runID}
	q.nsMain = NamespacePrefix + runID
	q.nsNetA = NamespacePrefix + runID + "-na"
	q.nsNetB = NamespacePrefix + runID + "-nb"
	q.rep = &Report{Target: cfg.Target, RunID: runID, Commit: cfg.Commit, Started: time.Now().UTC(),
		Pins: map[string]string{}, ImageIDs: map[string]string{}, Inventory: map[string]any{}}
	for k, v := range pins.Images {
		q.rep.Pins[k] = v.Ref
	}

	phases := []struct {
		name string
		fn   func(context.Context) error
	}{
		{"provision", q.provision},
		{"inventory", q.phaseInventory},
		{"storage", q.phaseStorage},
		{"registry", q.phaseRegistry},
		{"ko", q.phaseKo},
		{"buildah", q.phaseBuildah},
		{"postgres", q.phasePostgres},
		{"network", q.phaseNetwork},
	}
	for i, ph := range phases {
		if ctx.Err() != nil {
			var left []string
			for _, p := range phases[i:] {
				left = append(left, p.name)
			}
			q.rep.Add(Check{ID: "run.interrupted", Capability: "complete run", Required: true, Status: Fail,
				Detail: "run interrupted (" + ctx.Err().Error() + ") before phases: " + strings.Join(left, ", ")})
			break
		}
		if err := q.runPhase(ctx, ph.name, ph.fn); err != nil && ph.name == "provision" {
			break // nothing else can run without namespaces
		}
	}
	q.teardown()
	q.rep.Finalize()
	if err := q.rep.Write(dir); err != nil {
		return q.rep, err
	}
	return q.rep, nil
}

// runPhase isolates a phase: a panic or error becomes a failed check, and
// the remaining phases still produce evidence.
func (q *Qualifier) runPhase(ctx context.Context, name string, fn func(context.Context) error) (err error) {
	pctx, cancel := context.WithTimeout(ctx, 25*time.Minute)
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("phase %s panicked: %v", name, r)
		}
		if err != nil {
			q.rep.Add(Check{ID: "phase." + name, Capability: "phase " + name, Required: true, Status: Fail, Detail: q.c.K.Rec.Redact(err.Error())})
		}
	}()
	return fn(pctx)
}

func (q *Qualifier) img(name string) string { return q.pins.Images[name].Ref }

// provision creates the task-owned namespaces. The main namespace and the
// network-test namespaces carry no Pod Security enforcement of their own:
// the builder profile's PSA requirement is probed separately by dry-run.
func (q *Qualifier) provision(ctx context.Context) error {
	if _, err := q.c.K.Run(ctx, nil, "version", "--output=json"); err != nil {
		return fmt.Errorf("target unreachable: %w", err)
	}
	for _, ns := range []string{q.nsMain, q.nsNetA, q.nsNetB} {
		if err := q.c.CreateNamespace(ctx, ns, nil); err != nil {
			return err
		}
	}
	return nil
}
