package gitservice

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// Limits bound what one worker accepts (GR-16). A zero value takes the default.
type Limits struct {
	// MaxRequestBytes bounds the body of a non-push request (an upload-pack
	// request); MaxPushBytes the (decompressed) body of a receive-pack request
	// and Git's own receive.maxInputSize.
	MaxRequestBytes int64
	MaxPushBytes    int64
	// MaxRefUpdates is the installation's bound on the ref updates of one
	// receive, at most gitv1.MaxPushUpdates. A receive above it is refused
	// explicitly with too_many_updates.
	MaxRefUpdates int
	// MaxDuration bounds one Git request, whatever its receive deadline.
	MaxDuration time.Duration
	// BodyTimeout bounds receiving a request body from a slow client.
	BodyTimeout time.Duration
	// MaxConcurrent bounds concurrent Git requests of the tenant; a request
	// above it is refused with 429 and Retry-After, never queued.
	MaxConcurrent int
	// GateWait bounds how long a receive waits for a running maintenance.
	GateWait time.Duration
	// ControlTimeout bounds one control-plane call made for a hook.
	ControlTimeout time.Duration
	// OperationTimeout bounds one worker RPC; MaintenanceTimeout a maintenance.
	OperationTimeout   time.Duration
	MaintenanceTimeout time.Duration
	// MaxBundleBytes bounds a bundle the worker creates or accepts for replay;
	// BundleTimeout bounds one bundle request.
	MaxBundleBytes int64
	BundleTimeout  time.Duration
	// MaxConcurrentBundles bounds the bundle creations and replays running at
	// once (they spool or stream large files); one above it is refused with 429
	// and Retry-After, never queued.
	MaxConcurrentBundles int
	// LockWait bounds how long a starting worker waits for the data root's
	// generation lock, which a previous worker's surviving Git processes hold;
	// after it New fails with ErrDataRootBusy and touches nothing.
	LockWait time.Duration
}

// Defaults of Limits.
const (
	DefaultMaxRequestBytes    = 8 << 20
	DefaultMaxPushBytes       = 1 << 30
	DefaultMaxDuration        = 30 * time.Minute
	DefaultBodyTimeout        = 5 * time.Minute
	DefaultMaxConcurrent      = 32
	DefaultGateWait           = 30 * time.Second
	DefaultControlTimeout     = 30 * time.Second
	DefaultOperationTimeout   = 2 * time.Minute
	DefaultMaintenanceTimeout = 30 * time.Minute
	DefaultLockWait           = 30 * time.Second
	DefaultMaxBundleBytes     = 8 << 30
	DefaultBundleTimeout      = 30 * time.Minute
	DefaultMaxBundles         = 2
)

func (l Limits) withDefaults() Limits {
	if l.MaxRequestBytes <= 0 {
		l.MaxRequestBytes = DefaultMaxRequestBytes
	}
	if l.MaxPushBytes <= 0 {
		l.MaxPushBytes = DefaultMaxPushBytes
	}
	if l.MaxRefUpdates <= 0 || l.MaxRefUpdates > gitv1.MaxPushUpdates {
		l.MaxRefUpdates = gitv1.MaxPushUpdates
	}
	if l.MaxDuration <= 0 {
		l.MaxDuration = DefaultMaxDuration
	}
	if l.BodyTimeout <= 0 {
		l.BodyTimeout = DefaultBodyTimeout
	}
	if l.MaxConcurrent <= 0 {
		l.MaxConcurrent = DefaultMaxConcurrent
	}
	if l.GateWait <= 0 {
		l.GateWait = DefaultGateWait
	}
	if l.ControlTimeout <= 0 {
		l.ControlTimeout = DefaultControlTimeout
	}
	if l.OperationTimeout <= 0 {
		l.OperationTimeout = DefaultOperationTimeout
	}
	if l.MaintenanceTimeout <= 0 {
		l.MaintenanceTimeout = DefaultMaintenanceTimeout
	}
	if l.MaxBundleBytes <= 0 {
		l.MaxBundleBytes = DefaultMaxBundleBytes
	}
	if l.BundleTimeout <= 0 {
		l.BundleTimeout = DefaultBundleTimeout
	}
	if l.MaxConcurrentBundles <= 0 {
		l.MaxConcurrentBundles = DefaultMaxBundles
	}
	if l.LockWait <= 0 {
		l.LockWait = DefaultLockWait
	}
	return l
}

// Config configures a Service (and the `kodama git serve` command).
type Config struct {
	// TenantID is the one tenant this worker serves; a forwarding assertion of
	// another tenant is forbidden.
	TenantID domain.TenantID
	// DataRoot is the worker-owned volume root. Repositories live at
	// <DataRoot>/repositories/<repositoryId>.git, the shared hooks at
	// <DataRoot>/hooks, request spools at <DataRoot>/spool and the worker-owned
	// Git configuration at <DataRoot>/config.
	DataRoot string
	// ListenAddr is the address ServeCommand listens on (for example ":8443").
	ListenAddr string

	ControlPlane ControlPlane
	Assertions   AssertionVerifier
	Callers      ControlCallerVerifier

	Limits Limits

	// GitBinary is the Git executable ("git" resolved through PATH when empty).
	GitBinary string
	// HookBinary is the executable the hook shims run as
	// `<HookBinary> git-hook <name>`; the running executable when empty. It must
	// route `git-hook` to HookMain.
	HookBinary string

	// TLSCertFile and TLSKeyFile enable TLS on ServeCommand's listener when both
	// are set (the worker is reached through the tenant's Service).
	TLSCertFile string
	TLSKeyFile  string

	// PollInterval is how often the worker polls the control plane's
	// assignments; zero disables polling (the control plane then calls the RPC).
	PollInterval time.Duration

	WorkerInstance string
	Logger         *slog.Logger
	Clock          func() time.Time
}

// resolve validates the configuration and fills the defaults.
func (c Config) resolve() (Config, error) {
	if err := c.TenantID.Validate(); err != nil {
		return c, err
	}
	if c.DataRoot == "" || !filepath.IsAbs(c.DataRoot) {
		return c, errors.New("gitservice: DataRoot must be an absolute path")
	}
	c.DataRoot = filepath.Clean(c.DataRoot)
	if c.ControlPlane == nil {
		return c, errors.New("gitservice: ControlPlane is required")
	}
	if c.Assertions == nil {
		return c, errors.New("gitservice: Assertions verifier is required (default deny)")
	}
	if c.Callers == nil {
		return c, errors.New("gitservice: Callers verifier is required (default deny)")
	}
	c.Limits = c.Limits.withDefaults()
	if c.GitBinary == "" {
		c.GitBinary = "git"
	}
	path, err := exec.LookPath(c.GitBinary)
	if err != nil {
		return c, fmt.Errorf("gitservice: git is not available: %w", err)
	}
	if c.GitBinary, err = filepath.Abs(path); err != nil {
		return c, err
	}
	if c.HookBinary == "" {
		if c.HookBinary, err = os.Executable(); err != nil {
			return c, fmt.Errorf("gitservice: locating the hook binary: %w", err)
		}
	}
	if !filepath.IsAbs(c.HookBinary) {
		return c, errors.New("gitservice: HookBinary must be an absolute path")
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.DiscardHandler)
	}
	if c.Clock == nil {
		c.Clock = time.Now
	}
	return c, nil
}
