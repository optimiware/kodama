package gitworker

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/optimiware/kodama/internal/domain"
)

// Object keys. Labels select objects and never grant authority: every adoption
// verifies the annotations, the namespace UID and the owner chain as well.
const (
	// LabelOwner marks an object this package owns; LabelTenantKey is the
	// tenant's label-safe key (the namespace suffix); LabelObject names the
	// object's role within the tenant's namespace.
	LabelOwner     = "kodama.optimi.com/git-hosting"
	ValueOwner     = "tenant-worker"
	LabelTenantKey = "kodama.optimi.com/git-tenant"
	LabelObject    = "kodama.optimi.com/git-object"
	// LabelApp is the conventional name label of the worker's Pods.
	LabelApp = "app.kubernetes.io/name"
	ValueApp = "kodama-git-worker"

	// AnnTenant carries the tenant ID verbatim (an ID does not fit label syntax).
	AnnTenant = "kodama.optimi.com/git-tenant-id"
	// AnnFingerprint is the digest of the object as rendered. An object that
	// carries it but differs in a controlled field is stale or tampered with, not
	// adopted as is.
	AnnFingerprint = "kodama.optimi.com/git-fingerprint"
	// AnnNamespaceUID is the instance UID of the tenant's namespace. Every
	// namespaced object carries it: an object copied from, or left over from,
	// another instance of the namespace does not verify.
	AnnNamespaceUID = "kodama.optimi.com/git-namespace-uid"

	labelPSAPrefix = "pod-security.kubernetes.io/"
	labelNSName    = "kubernetes.io/metadata.name"
)

// Roles of the tenant namespace's objects (LabelObject values).
const (
	RoleNamespace      = "namespace"
	RoleServiceAccount = "service-account"
	RoleClaim          = "volume-claim"
	RoleSecret         = "identity"
	RoleService        = "service"
	RoleDeployment     = "worker"
	RolePolicy         = "network-policy"
)

// Fixed object names. A tenant has its own namespace, so the names need no tenant
// part; two tenants never share a namespace.
const (
	NamespacePrefix    = "kodama-git-"
	DeploymentName     = "git-worker"
	ServiceName        = "git-worker"
	ServiceAccountName = "git-worker"
	ClaimName          = "git-data"
	SecretName         = "git-worker-identity"
	PolicyDenyAll      = "git-default-deny"
	PolicyIngress      = "git-allow-control-plane-ingress"
	PolicyEgress       = "git-allow-control-plane-egress"
	ContainerName      = "git-worker"
	PortName           = "git"
)

// Worker layout and the environment `kodama git serve` is configured with. The
// names are the contract this package renders; T-128 and T-132 read them.
const (
	DataRoot    = "/data"
	TmpDir      = "/tmp"
	IdentityDir = "/var/run/kodama/identity"

	// Keys of the identity Secret and the files they project under IdentityDir.
	KeyToken      = "token"
	KeyPublicKeys = "public-keys"
	KeyCA         = "ca.crt"

	EnvTenantID       = "KODAMA_GIT_TENANT_ID"
	EnvDataRoot       = "KODAMA_GIT_DATA_ROOT"
	EnvListen         = "KODAMA_GIT_LISTEN_ADDR"
	EnvControlURL     = "KODAMA_GIT_CONTROL_URL"
	EnvTokenFile      = "KODAMA_GIT_IDENTITY_TOKEN_FILE"
	EnvPublicKeys     = "KODAMA_GIT_PUBLIC_KEYS_FILE"
	EnvCAFile         = "KODAMA_GIT_CA_FILE"
	EnvWorkerInstance = "KODAMA_GIT_WORKER_INSTANCE"

	// DefaultCommand and DefaultArgs start the worker role of the Kodama binary.
	DefaultCommand = "/usr/local/bin/kodama"
)

// TenantKey is the label-safe key of a tenant: the first 24 hex characters of the
// framed digest of the tenant ID. Tenant IDs may carry upper case, underscores and
// up to 128 characters, none of which fit a namespace name.
func TenantKey(t domain.TenantID) string {
	h := sha256.New()
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(t)))
	h.Write([]byte("kodama-git-tenant"))
	h.Write(n[:])
	h.Write([]byte(t))
	return hex.EncodeToString(h.Sum(nil))[:24]
}

// NamespaceOf is the service namespace of a tenant.
func NamespaceOf(t domain.TenantID) string { return NamespacePrefix + TenantKey(t) }

// ErrInvalidSpec: the tenant or the desired worker is not acceptable. Nothing is
// created.
var ErrInvalidSpec = errors.New("gitworker: invalid worker specification")

func invalid(format string, args ...any) error {
	return &diagError{kind: ErrInvalidSpec, msg: fmt.Sprintf(format, args...)}
}

// Resources are the worker container's bounds. Every quantity is required: an
// unbounded worker would let one tenant starve the others.
type Resources struct {
	CPURequest       string
	CPULimit         string
	MemoryRequest    string
	MemoryLimit      string
	EphemeralRequest string
	EphemeralLimit   string
	// TmpSize bounds the scratch emptyDir.
	TmpSize string
}

// Storage is the tenant's durable volume.
type Storage struct {
	// Class is a StorageClass name. It must be named: the cluster default is never
	// used. Its reclaim policy must be Retain and its provisioner one the
	// installation declared ReadWriteOncePod-capable (preflight).
	Class string
	// Size is the claim's request, for example 50Gi.
	Size string
}

// ControlPlane identifies the only peer the worker talks to or is reached from.
type ControlPlane struct {
	// Namespace and PodLabels select the control plane's Pods: ingress is admitted
	// only from them (namespace and Pod selector together), and egress only to them.
	Namespace string
	PodLabels map[string]string
	// PodPort is the container port of the control plane's Pods that workers call.
	PodPort int32
	// URL is the origin workers call (a Service of the control plane), passed to
	// the worker as its control-plane URL.
	URL string
}

// WorkerSpec is the desired state of one tenant's Git worker.
type WorkerSpec struct {
	// Image is the worker image pinned by digest: repository[:tag]@sha256:<hex>.
	Image string
	// Command and Args replace the default worker command (the Kodama binary and
	// the arguments "git" "serve"); they exist for installations and tests that
	// wrap the worker, and never carry credentials.
	Command         []string
	Args            []string
	ImagePullPolicy string
	Resources       Resources
	Storage         Storage
	// ListenPort is the worker's container port and its Service port (>= 1024: the
	// worker is not root).
	ListenPort int32
	// Scheme is https (default) or http: the scheme of the worker's endpoint and of
	// its health probes.
	Scheme       string
	ControlPlane ControlPlane
	// PodSecurityVersion pins the Pod Security standard version of the namespace
	// (v1.NN). "latest" is refused: a cluster upgrade must not silently change what
	// the namespace enforces.
	PodSecurityVersion string
	// RunAsUser and RunAsGroup default to 65532; the group also owns the volume.
	RunAsUser  int64
	RunAsGroup int64
	// TerminationGraceSeconds defaults to 30.
	TerminationGraceSeconds int64
}

var (
	// imageRe is a digest-pinned OCI reference: an optional registry host (a DNS
	// name, localhost or an IPv4 address) with an optional port, a lower-case
	// repository path, an optional tag and the digest. The registry port is why
	// the host part is its own group: localhost:5000/scope/image@sha256:... is the
	// development registry's form.
	imageRe     = regexp.MustCompile(`^([a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?(:[0-9]{1,5})?/)?[a-z0-9]+([._-][a-z0-9]+)*(/[a-z0-9]+([._-][a-z0-9]+)*)*(:[A-Za-z0-9_][A-Za-z0-9._-]{0,127})?@sha256:[0-9a-f]{64}$`)
	versionRe   = regexp.MustCompile(`^v1\.[0-9]{1,3}$`)
	labelKeyRe  = regexp.MustCompile(`^([a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?/)?[A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?$`)
	labelValRe  = regexp.MustCompile(`^([A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?)?$`)
	dns1123Re   = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	classNameRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`)
)

// withDefaults fills the optional fields.
func (s WorkerSpec) withDefaults() WorkerSpec {
	if s.Scheme == "" {
		s.Scheme = "https"
	}
	if s.ImagePullPolicy == "" {
		s.ImagePullPolicy = "IfNotPresent"
	}
	if len(s.Command) == 0 && len(s.Args) == 0 {
		s.Command = []string{DefaultCommand}
		s.Args = []string{"git", "serve"}
	}
	if s.RunAsUser == 0 {
		s.RunAsUser = 65532
	}
	if s.RunAsGroup == 0 {
		s.RunAsGroup = 65532
	}
	if s.TerminationGraceSeconds == 0 {
		s.TerminationGraceSeconds = 30
	}
	return s
}

// validate checks the defaulted spec. Messages carry no values that could be
// secret: a spec holds none.
func (s WorkerSpec) validate() error {
	if len(s.Image) > 512 || !imageRe.MatchString(s.Image) {
		return invalid("the image must be pinned by digest")
	}
	switch s.ImagePullPolicy {
	case "IfNotPresent", "Always", "Never":
	default:
		return invalid("unknown image pull policy")
	}
	switch s.Scheme {
	case "https", "http":
	default:
		return invalid("the scheme must be https or http")
	}
	for _, q := range []struct{ name, v string }{
		{"cpu request", s.Resources.CPURequest}, {"cpu limit", s.Resources.CPULimit},
		{"memory request", s.Resources.MemoryRequest}, {"memory limit", s.Resources.MemoryLimit},
		{"ephemeral request", s.Resources.EphemeralRequest}, {"ephemeral limit", s.Resources.EphemeralLimit},
		{"tmp size", s.Resources.TmpSize}, {"storage size", s.Storage.Size},
	} {
		if m, err := parseQuantity(q.v); err != nil || m <= 0 {
			return invalid("%s must be a positive quantity", q.name)
		}
	}
	for _, p := range [][2]string{
		{s.Resources.CPURequest, s.Resources.CPULimit}, {s.Resources.MemoryRequest, s.Resources.MemoryLimit},
		{s.Resources.EphemeralRequest, s.Resources.EphemeralLimit},
	} {
		req, _ := parseQuantity(p[0])
		lim, _ := parseQuantity(p[1])
		if req > lim {
			return invalid("a resource request exceeds its limit")
		}
	}
	if !classNameRe.MatchString(s.Storage.Class) {
		return invalid("a named storage class is required: the cluster default is never used")
	}
	if s.ListenPort < 1024 || s.ListenPort > 65535 {
		return invalid("the listen port must be an unprivileged port")
	}
	if !versionRe.MatchString(s.PodSecurityVersion) {
		return invalid("the Pod Security version must be pinned (v1.NN)")
	}
	if s.RunAsUser < 1 || s.RunAsGroup < 1 {
		return invalid("the worker must not run as root")
	}
	if s.TerminationGraceSeconds < 1 || s.TerminationGraceSeconds > 600 {
		return invalid("the termination grace period is out of range")
	}
	for _, a := range append(append([]string(nil), s.Command...), s.Args...) {
		if a == "" || len(a) > 1024 || strings.ContainsRune(a, 0) {
			return invalid("a command argument is empty or unusable")
		}
	}
	return s.ControlPlane.validate()
}

func (c ControlPlane) validate() error {
	if !dns1123Re.MatchString(c.Namespace) {
		return invalid("the control-plane namespace is not a valid name")
	}
	if len(c.PodLabels) == 0 || len(c.PodLabels) > 8 {
		return invalid("the control plane must be selected by 1 to 8 Pod labels")
	}
	for k, v := range c.PodLabels {
		if !labelKeyRe.MatchString(k) || !labelValRe.MatchString(v) {
			return invalid("a control-plane Pod label is not valid")
		}
	}
	if c.PodPort < 1 || c.PodPort > 65535 {
		return invalid("the control-plane Pod port is out of range")
	}
	u, err := url.Parse(c.URL)
	if err != nil || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") {
		return invalid("the control-plane URL must be an http(s) origin")
	}
	return nil
}

// ---- quantities ----

var quantitySuffixes = []struct {
	suffix string
	mult   float64
}{
	{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40},
	{"m", 0.001}, {"k", 1e3}, {"M", 1e6}, {"G", 1e9}, {"T", 1e12},
}

// parseQuantity returns a quantity in milli-units for the plain, milli, decimal
// and binary forms; anything else is refused.
func parseQuantity(s string) (int64, error) {
	s = strings.TrimSpace(s)
	mult := 1.0
	for _, sf := range quantitySuffixes {
		if strings.HasSuffix(s, sf.suffix) {
			s, mult = strings.TrimSuffix(s, sf.suffix), sf.mult
			break
		}
	}
	if s == "" || strings.ContainsAny(s, "eE+- ") {
		return 0, errors.New("malformed quantity")
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 || math.IsInf(v, 0) || math.IsNaN(v) || v*mult*1000 > float64(math.MaxInt64/2) {
		return 0, errors.New("malformed quantity")
	}
	return int64(math.Round(v * mult * 1000)), nil
}

// canonicalQuantity maps equal quantities to one string so a value the server
// normalized (1000m to 1) compares equal to the one that was sent.
func canonicalQuantity(s string) string {
	if m, err := parseQuantity(s); err == nil {
		return "q" + strconv.FormatInt(m, 10)
	}
	return s
}
