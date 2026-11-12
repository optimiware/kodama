package harness

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Shard is one isolated test environment: a unique network, uniquely named
// containers and volumes, and a bounded lifetime. NewShard registers cleanup
// with t.Cleanup.
type Shard struct {
	ID      string
	Podman  *Podman
	Timeout time.Duration

	t       testing.TB
	ctx     context.Context
	cancel  context.CancelFunc
	require bool

	probeOptions WaitOptions

	mu         sync.Mutex
	netOnce    sync.Once
	network    string
	netErr     error
	containers []*Container
	volumes    []string

	// imageMu serializes per-shard image selections and guards images, the
	// retained tag-to-identity map shared by RequireImage and Run.
	imageMu sync.Mutex
	images  map[string]ImageRecord

	closeOnce sync.Once
}

type shardConfig struct {
	podman  *Podman
	timeout time.Duration
}

// ShardOption configures a shard.
type ShardOption func(*shardConfig)

// WithPodman injects a Podman client (unit tests use a fake runner).
func WithPodman(p *Podman) ShardOption {
	return func(c *shardConfig) {
		if p != nil {
			c.podman = p
		}
	}
}

// WithShardTimeout overrides the 15-minute shard lifetime.
func WithShardTimeout(d time.Duration) ShardOption {
	return func(c *shardConfig) {
		if d > 0 {
			c.timeout = d
		}
	}
}

// NewShard builds an isolated shard for t. If Podman is missing the test is
// skipped with "unqualified: infrastructure unavailable" unless
// KODAMA_REQUIRE_INFRA=1, in which case it fails. A skip is never a pass claim.
func NewShard(t testing.TB, opts ...ShardOption) *Shard {
	t.Helper()
	cfg := shardConfig{timeout: DefaultShardTimeout}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	require := requireInfra()
	podman := cfg.podman
	if podman == nil {
		podman = NewPodman(WithLogger(t.Logf), WithEvidenceDir(os.Getenv("KODAMA_EVIDENCE_DIR")))
	}
	ctx, cancel := context.WithTimeout(t.Context(), cfg.timeout)
	shard := &Shard{
		ID:           newShardID(t.Name()),
		Podman:       podman,
		Timeout:      cfg.timeout,
		t:            t,
		ctx:          ctx,
		cancel:       cancel,
		require:      require,
		probeOptions: DefaultWaitOptions(),
		images:       map[string]ImageRecord{},
	}
	if err := podman.Version(ctx); err != nil {
		cancel()
		SkipOrFail(t, require, fmt.Errorf("podman is unavailable: %w", err))
	}
	registerShard(shard)
	t.Cleanup(shard.Close)
	return shard
}

// Context is the shard's bounded context. It is cancelled at the shard
// deadline or when the test ends.
func (s *Shard) Context() context.Context { return s.ctx }

// requireInfra reports whether missing infrastructure must fail the test.
func requireInfra() bool { return os.Getenv("KODAMA_REQUIRE_INFRA") == "1" }

// SkipOrFail applies the infrastructure policy: fail when require is true,
// otherwise skip with the exact unqualified reason. It never returns.
func SkipOrFail(t testing.TB, require bool, err error) {
	t.Helper()
	reason := "unqualified: infrastructure unavailable: " + err.Error()
	if require {
		t.Fatalf("%s (KODAMA_REQUIRE_INFRA=1)", reason)
	}
	t.Skip(reason)
}

// RequireImage returns the image digest, pulling when absent. A pull failure
// follows the shard's skip/fail policy instead of passing. The tag is resolved
// once per shard: later calls and Run reuse the same retained identity, so the
// recorded evidence always names the image that actually starts.
func (s *Shard) RequireImage(ctx context.Context, tag string) string {
	s.t.Helper()
	record, err := s.resolveImage(ctx, tag)
	if err != nil {
		SkipOrFail(s.t, s.require, err)
	}
	return record.Digest
}

// resolveImage returns the shard's retained identity for tag, inspecting and
// recording it exactly once even when callers race. It holds imageMu for the
// whole resolution so a concurrent selection cannot observe a retargeted tag.
func (s *Shard) resolveImage(ctx context.Context, tag string) (ImageRecord, error) {
	key := strings.TrimSpace(tag)
	s.imageMu.Lock()
	defer s.imageMu.Unlock()
	if record, ok := s.images[key]; ok {
		return record, nil
	}
	record, err := s.Podman.ensureImageRecord(ctx, key)
	if err != nil {
		return ImageRecord{}, err
	}
	if s.images == nil {
		s.images = map[string]ImageRecord{}
	}
	s.images[key] = record
	return record, nil
}

// Name returns a unique, shard-scoped container or volume name.
func (s *Shard) Name(prefix string) string {
	return s.ID + "-" + sanitizeName(prefix)
}

// Network returns the shard's private network, creating it on first use.
func (s *Shard) Network(ctx context.Context) (string, error) {
	s.netOnce.Do(func() {
		name := "kodama-" + s.ID
		_, err := s.Podman.run(ctx, s.Podman.operationLimit, "network", "create",
			"--label", LabelTest+"=1",
			"--label", LabelShard+"="+s.ID,
			"--label", LabelOwnerPID+"="+strconv.Itoa(os.Getpid()),
			name)
		s.mu.Lock()
		defer s.mu.Unlock()
		if err != nil {
			s.netErr = fmt.Errorf("harness: create network %s: %w", name, err)
			return
		}
		s.network = name
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.network, s.netErr
}

// CreateVolume creates a labelled named volume and tracks it for cleanup.
func (s *Shard) CreateVolume(ctx context.Context, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("harness: volume needs a name")
	}
	if _, err := s.Podman.run(ctx, s.Podman.operationLimit, "volume", "create",
		"--label", LabelTest+"=1",
		"--label", LabelShard+"="+s.ID,
		"--label", LabelOwnerPID+"="+strconv.Itoa(os.Getpid()),
		name); err != nil {
		return "", fmt.Errorf("harness: create volume %s: %w", name, err)
	}
	s.mu.Lock()
	s.volumes = append(s.volumes, name)
	s.mu.Unlock()
	return name, nil
}

// Run starts a container in the shard. Name and Network default to unique
// shard-scoped values. The container is tracked for cleanup. The launch uses
// the shard's retained immutable identity for spec.Image's tag, never the
// mutable tag, so another pull cannot retarget what starts.
func (s *Shard) Run(ctx context.Context, spec RunSpec) (*Container, error) {
	image := spec.Image
	if spec.Name == "" {
		spec.Name = s.Name(image)
	}
	reference, err := s.resolveRunImage(ctx, image)
	if err != nil {
		return nil, err
	}
	spec.Image = reference
	if spec.Network == "" {
		network, err := s.Network(ctx)
		if err != nil {
			return nil, err
		}
		spec.Network = network
	}
	args, err := runArgs(spec, s.ID, os.Getpid())
	if err != nil {
		return nil, err
	}
	if _, err := s.Podman.run(ctx, s.Podman.operationLimit, args...); err != nil {
		return nil, fmt.Errorf("harness: run %s: %w", image, err)
	}
	container := &Container{
		Name:    spec.Name,
		ShardID: s.ID,
		podman:  s.Podman,
		ports:   map[int]int{},
	}
	s.mu.Lock()
	s.containers = append(s.containers, container)
	s.mu.Unlock()
	return container, nil
}

// resolveRunImage returns the immutable reference Run must start. A tag is
// resolved through the shard's single recorded selection; an already
// digest- or ID-pinned reference is used verbatim. An image that cannot be
// resolved is an error: the mutable tag is never a fallback.
func (s *Shard) resolveRunImage(ctx context.Context, image string) (string, error) {
	image = strings.TrimSpace(image)
	if image == "" {
		return "", errors.New("harness: run spec needs an image")
	}
	if isImmutableImageReference(image) {
		return image, nil
	}
	record, err := s.resolveImage(ctx, image)
	if err != nil {
		return "", fmt.Errorf("harness: resolve image %s: %w", image, err)
	}
	return pinnedImageReference(image, record.Digest), nil
}

// isImmutableImageReference reports whether image already names exactly one
// image: a digest reference (repository@sha256:...) or a local image ID.
func isImmutableImageReference(image string) bool {
	return strings.Contains(image, "@") || strings.HasPrefix(image, "sha256:")
}

// pinnedImageReference turns a tag into the immutable repository@digest
// reference for that tag.
func pinnedImageReference(tag, digest string) string {
	repository := tag
	if index := strings.LastIndex(tag, ":"); index > strings.LastIndex(tag, "/") {
		repository = tag[:index]
	}
	return repository + "@" + digest
}

// WaitReady runs a readiness barrier with the shard's default options.
func (s *Shard) WaitReady(ctx context.Context, name string, probe Probe) error {
	return WaitReady(ctx, s.Podman.clock, name, probe, s.probeOptions)
}

// Close cancels the shard and removes its containers, volumes and network in
// that order. It is idempotent and safe to call from a signal handler.
func (s *Shard) Close() {
	s.closeOnce.Do(func() {
		s.cancel()
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		if s.t != nil && s.t.Failed() {
			for _, container := range s.snapshotContainers() {
				if logs, err := container.Logs(ctx); err == nil {
					s.t.Logf("harness: logs for failed container %s:\n%s", container.Name, logs)
				} else {
					s.t.Logf("harness: logs for failed container %s: %v", container.Name, err)
				}
			}
		}
		if err := s.cleanup(ctx); err != nil {
			if s.t != nil {
				s.t.Logf("harness: shard %s cleanup: %v", s.ID, err)
			}
		}
		unregisterShard(s)
	})
}

func (s *Shard) snapshotContainers() []*Container {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Container, len(s.containers))
	copy(out, s.containers)
	return out
}

// cleanup removes tracked resources in dependency order: containers, then
// volumes, then the network. Every step is attempted even after a failure.
func (s *Shard) cleanup(ctx context.Context) error {
	var errs []error
	containers := s.snapshotContainers()
	for _, container := range containers {
		if err := container.Cleanup(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	s.mu.Lock()
	volumes := append([]string(nil), s.volumes...)
	network := s.network
	s.mu.Unlock()
	for _, volume := range volumes {
		if _, err := s.Podman.run(ctx, s.Podman.stopLimit, "volume", "rm", "--force", volume); err != nil {
			errs = append(errs, fmt.Errorf("harness: remove volume %s: %w", volume, err))
		}
	}
	if network != "" {
		if _, err := s.Podman.run(ctx, s.Podman.stopLimit, "network", "rm", "--force", network); err != nil {
			errs = append(errs, fmt.Errorf("harness: remove network %s: %w", network, err))
		}
	}
	return joinErrors(errs)
}

func joinErrors(errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	message := make([]string, 0, len(errs))
	for _, err := range errs {
		message = append(message, err.Error())
	}
	return fmt.Errorf("%s", strings.Join(message, "; "))
}

// newShardID derives a unique, DNS-safe shard identifier from the test name.
func newShardID(testName string) string {
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		suffix = []byte{byte(os.Getpid()), 0, 0, 0}
	}
	return sanitizeName(testName) + "-" + strconv.Itoa(os.Getpid()) + "-" + hex.EncodeToString(suffix)
}

func sanitizeName(value string) string {
	var builder strings.Builder
	for _, r := range strings.ToLower(value) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			builder.WriteRune(r)
		default:
			builder.WriteByte('-')
		}
	}
	out := strings.Trim(builder.String(), "-")
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	if out == "" {
		out = "shard"
	}
	if len(out) > 40 {
		out = strings.TrimRight(out[:40], "-")
	}
	return out
}
