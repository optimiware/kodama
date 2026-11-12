// Package kubedeps provides a bounded in-cluster dependency fixture: one
// pinned PostgreSQL server behind a Kubernetes Service in a namespace it owns.
//
// Scope rules:
//
//   - it reuses an already-owned kubesupport.Cluster (Apply, Kubectl, GetJSON)
//     and never creates or deletes the cluster itself;
//   - every fixture owns exactly one namespace and one generated password;
//   - the server image is digest-pinned, never a mutable tag;
//   - the credential exists only in the generated Secret payload; the Pod and
//     the SQL probe read it through a Secret-backed environment, never through
//     command arguments or a literal environment value;
//   - readiness is proven by a real SQL query that travels from a probe Pod to
//     the Service, not by Pod phase or an open TCP port alone;
//   - Close deletes only the fixture namespace and reports a failed or
//     unverified deletion instead of reducing it to a log line.
//
// This is infrastructure only. It deliberately does not duplicate the product
// migration or role bootstrap, and it makes no production TLS, RLS or workload
// identity claim.
package kubedeps

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/optimiware/kodama/test/support/infrastructure/harness"
)

const (
	// PostgresImage is the pinned official PostgreSQL image tag.
	PostgresImage = "docker.io/library/postgres:18-alpine"
	// PostgresImageDigest is the immutable manifest list digest of that tag.
	PostgresImageDigest = "sha256:123b17998ccaf47e53b502e8c6a1e79afd8f579c6f3eec83c563da026f957395"
	// PostgresImageReference is the digest-pinned reference applied to every
	// fixture object. A tag alone is refused.
	PostgresImageReference = PostgresImage + "@" + PostgresImageDigest

	// PostgresPort is the server and Service port.
	PostgresPort = 5432

	// LabelDependency names the dependency kind on the fixture objects.
	LabelDependency = "kodama.test/dependency"
	// LabelInstance scopes the Service selector and Pod labels to one fixture.
	LabelInstance = "kodama.test/dependency-instance"
	// DependencyPostgres is the LabelDependency value.
	DependencyPostgres = "postgres"

	// DefaultName is the fixture object name when none is given.
	DefaultName = "postgres"
	// DefaultUser is the bootstrap superuser when none is given.
	DefaultUser = "kodama"
	// DefaultDatabase is the bootstrap database when none is given.
	DefaultDatabase = "kodama"
)

const (
	namespacePrefix = "kodama-dep-"

	// DefaultReadyTimeout bounds "the Pod exists and its TCP readiness probe
	// passes".
	DefaultReadyTimeout = 5 * time.Minute
	// DefaultProbeTimeout bounds one SQL probe Job and its log read.
	DefaultProbeTimeout = 2 * time.Minute
	// DefaultDeleteTimeout bounds one namespace deletion and its verification.
	DefaultDeleteTimeout = 2 * time.Minute

	probeRetries = 90
)

var (
	dnsLabelPattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	sqlNamePattern  = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)
	digestPattern   = regexp.MustCompile(`^[^\s@]+@sha256:[0-9a-f]{64}$`)

	reservedNamespaces = map[string]struct{}{
		"default":         {},
		"kube-system":     {},
		"kube-public":     {},
		"kube-node-lease": {},
	}
)

// Cluster is the subset of kubesupport.Cluster this fixture uses. It is an
// interface so unit tests can exercise readiness and cleanup without a real
// cluster; *kubesupport.Cluster satisfies it.
type Cluster interface {
	Apply(ctx context.Context, manifest string) (harness.Result, error)
	Kubectl(ctx context.Context, args ...string) (harness.Result, error)
	GetJSON(ctx context.Context, args ...string) (map[string]any, error)
}

// PostgresOptions configures one PostgreSQL dependency fixture. Empty fields
// select the pinned defaults; Namespace and Password are generated per fixture
// when omitted.
type PostgresOptions struct {
	Namespace     string
	Name          string
	User          string
	Database      string
	Password      string
	Image         string
	ReadyTimeout  time.Duration
	ProbeTimeout  time.Duration
	DeleteTimeout time.Duration
	WaitOptions   harness.WaitOptions
	Clock         harness.Clock
}

// Connection is the private connection material for in-cluster consumers. It
// is never logged by this package.
type Connection struct {
	Host     string
	Port     int
	User     string
	Database string
	Password string
	SSLMode  string
}

// DSN renders the connection as a PostgreSQL URL. The password is escaped for
// the URL; callers must still treat the result as a secret.
func (c Connection) DSN() string {
	sslMode := c.SSLMode
	if sslMode == "" {
		sslMode = "disable"
	}
	dsn := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(c.User, c.Password),
		Host:   net.JoinHostPort(c.Host, strconv.Itoa(c.Port)),
		Path:   "/" + c.Database,
	}
	query := url.Values{}
	query.Set("sslmode", sslMode)
	dsn.RawQuery = query.Encode()
	return dsn.String()
}

// Postgres is one PostgreSQL dependency fixture: a namespace, a Secret, a Pod
// and a Service behind which the server is reachable.
type Postgres struct {
	cluster       Cluster
	namespace     string
	name          string
	user          string
	database      string
	password      string
	image         string
	clock         harness.Clock
	waitOptions   harness.WaitOptions
	readyTimeout  time.Duration
	probeTimeout  time.Duration
	deleteTimeout time.Duration

	mu     sync.Mutex
	closed bool
}

// StartPostgres renders and applies one dependency fixture to an already-owned
// cluster. It applies the namespace, the Secret (through the caller's private
// staged manifest path), the Pod and the Service, in that order, and returns
// without waiting for readiness. Call WaitReady before using the fixture.
func StartPostgres(ctx context.Context, cluster Cluster, options PostgresOptions) (*Postgres, error) {
	fixture, err := newPostgres(cluster, options)
	if err != nil {
		return nil, err
	}
	manifests, err := renderManifests(fixture)
	if err != nil {
		return nil, err
	}
	// A fixture owns only a namespace it created. Adopting a namespace that
	// already exists (a sibling fixture's, or any caller-supplied one) would
	// make Close delete something this fixture never created, so refuse before
	// the apply and never issue a deletion for it.
	exists, err := fixture.namespaceExists(ctx)
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, fmt.Errorf("kubedeps: refusing to use existing namespace %s; each fixture must create and own its namespace", fixture.namespace)
	}
	if _, err := cluster.Apply(ctx, manifests.Namespace); err != nil {
		return nil, fmt.Errorf("kubedeps: create dependency namespace %s: %w", fixture.namespace, err)
	}
	// Any later apply failure leaves a namespace this fixture owns. Delete it
	// best-effort and report both causes, so a partial fixture cannot leak.
	cleanup := func(cause error) error {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fixture.deleteTimeout)
		defer cancel()
		if _, err := cluster.Kubectl(cleanupCtx, "delete", "namespace", fixture.namespace,
			"--wait=true", "--ignore-not-found=true"); err != nil {
			return errors.Join(cause, fmt.Errorf("kubedeps: clean up dependency namespace %s: %w", fixture.namespace, err))
		}
		return cause
	}
	// The Secret is staged by Cluster.Apply into a private 0600 file; its
	// payload is never logged.
	if _, err := cluster.Apply(ctx, manifests.Secret); err != nil {
		return nil, cleanup(fmt.Errorf("kubedeps: create dependency Secret %s: %w", fixture.SecretName(), err))
	}
	if _, err := cluster.Apply(ctx, manifests.Pod); err != nil {
		return nil, cleanup(fmt.Errorf("kubedeps: create dependency Pod %s: %w", fixture.name, err))
	}
	if _, err := cluster.Apply(ctx, manifests.Service); err != nil {
		return nil, cleanup(fmt.Errorf("kubedeps: create dependency Service %s: %w", fixture.name, err))
	}
	return fixture, nil
}

// newPostgres validates options and builds the fixture without touching the
// cluster. Tests use it to inspect the rendered manifests.
func newPostgres(cluster Cluster, options PostgresOptions) (*Postgres, error) {
	if cluster == nil {
		return nil, errors.New("kubedeps: nil cluster")
	}
	name := options.Name
	if name == "" {
		name = DefaultName
	}
	if err := validateDNSLabel("name", name); err != nil {
		return nil, err
	}
	namespace := options.Namespace
	if namespace == "" {
		generated, err := generatedNamespace()
		if err != nil {
			return nil, err
		}
		namespace = generated
	}
	if err := validateNamespace(namespace); err != nil {
		return nil, err
	}
	user := options.User
	if user == "" {
		user = DefaultUser
	}
	if err := validateSQLName("user", user); err != nil {
		return nil, err
	}
	database := options.Database
	if database == "" {
		database = DefaultDatabase
	}
	if err := validateSQLName("database", database); err != nil {
		return nil, err
	}
	password := options.Password
	if password == "" {
		generated, err := generatedPassword()
		if err != nil {
			return nil, err
		}
		password = generated
	}
	image := options.Image
	if image == "" {
		image = PostgresImageReference
	}
	if !digestPattern.MatchString(image) {
		return nil, fmt.Errorf("kubedeps: image %q is not an immutable digest reference; want <name>@sha256:<64 hex>", image)
	}
	fixture := &Postgres{
		cluster:       cluster,
		namespace:     namespace,
		name:          name,
		user:          user,
		database:      database,
		password:      password,
		image:         image,
		clock:         options.Clock,
		waitOptions:   options.WaitOptions,
		readyTimeout:  options.ReadyTimeout,
		probeTimeout:  options.ProbeTimeout,
		deleteTimeout: options.DeleteTimeout,
	}
	if fixture.clock == nil {
		fixture.clock = harness.RealClock{}
	}
	if fixture.waitOptions == (harness.WaitOptions{}) {
		fixture.waitOptions = harness.DefaultWaitOptions()
	}
	if fixture.readyTimeout <= 0 {
		fixture.readyTimeout = DefaultReadyTimeout
	}
	if fixture.probeTimeout <= 0 {
		fixture.probeTimeout = DefaultProbeTimeout
	}
	if fixture.deleteTimeout <= 0 {
		fixture.deleteTimeout = DefaultDeleteTimeout
	}
	return fixture, nil
}

func validateDNSLabel(field, value string) error {
	if len(value) == 0 || len(value) > 63 || !dnsLabelPattern.MatchString(value) {
		return fmt.Errorf("kubedeps: %s %q is not a DNS label", field, value)
	}
	return nil
}

func validateNamespace(namespace string) error {
	if err := validateDNSLabel("namespace", namespace); err != nil {
		return err
	}
	if _, reserved := reservedNamespaces[namespace]; reserved {
		return fmt.Errorf("kubedeps: refusing reserved namespace %q", namespace)
	}
	return nil
}

func validateSQLName(field, value string) error {
	if len(value) == 0 || len(value) > 63 || !sqlNamePattern.MatchString(value) {
		return fmt.Errorf("kubedeps: %s %q is not a simple SQL identifier", field, value)
	}
	return nil
}

func generatedNamespace() (string, error) {
	suffix, err := randomHex(4)
	if err != nil {
		return "", err
	}
	return namespacePrefix + suffix, nil
}

func generatedPassword() (string, error) {
	return randomHex(24)
}

func randomHex(size int) (string, error) {
	buffer := make([]byte, size)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("kubedeps: generate random value: %w", err)
	}
	return hex.EncodeToString(buffer), nil
}

// Namespace returns the namespace this fixture owns.
func (p *Postgres) Namespace() string { return p.namespace }

// Name returns the fixture object name (also its Pod and Service name).
func (p *Postgres) Name() string { return p.name }

// ServiceName returns the Kubernetes Service name the fixture owns.
func (p *Postgres) ServiceName() string { return p.name }

// SecretName returns the generated Secret name that holds the credential.
func (p *Postgres) SecretName() string { return p.name + "-credentials" }

// ServiceHost returns the in-cluster DNS name of the Service.
func (p *Postgres) ServiceHost() string { return p.name + "." + p.namespace + ".svc" }

// Endpoint returns the in-cluster host:port consumers connect to. It is the
// Service path, never the Pod IP or localhost.
func (p *Postgres) Endpoint() string {
	return net.JoinHostPort(p.ServiceHost(), strconv.Itoa(PostgresPort))
}

// Connection returns the private connection material for in-cluster consumers.
func (p *Postgres) Connection() Connection {
	return Connection{
		Host:     p.ServiceHost(),
		Port:     PostgresPort,
		User:     p.user,
		Database: p.database,
		Password: p.password,
		SSLMode:  "disable",
	}
}

// WaitReady blocks until the server Pod reports Ready and a real SQL query
// through the Service returns its expected result. A Running Pod or an open
// TCP port alone is not readiness.
func (p *Postgres) WaitReady(ctx context.Context) error {
	if err := p.waitServerReady(ctx); err != nil {
		return err
	}
	return p.Query(ctx)
}

func (p *Postgres) waitServerReady(ctx context.Context) error {
	options := p.waitOptions
	options.Timeout = p.readyTimeout
	probe := func(ctx context.Context) error {
		document, err := p.cluster.GetJSON(ctx, "get", "pod", p.name, "-n", p.namespace)
		if err != nil {
			return fmt.Errorf("observe dependency Pod %s: %w", p.name, err)
		}
		return podReady(document)
	}
	return harness.WaitReady(ctx, p.clock, "postgres dependency "+p.name+" Pod", probe, options)
}

func podReady(document map[string]any) error {
	status, _ := document["status"].(map[string]any)
	phase, _ := status["phase"].(string)
	if phase != "Running" {
		return fmt.Errorf("pod phase is %q; want Running", phase)
	}
	conditions, _ := status["conditions"].([]any)
	ready := false
	for _, entry := range conditions {
		condition, _ := entry.(map[string]any)
		if condition["type"] == "Ready" && condition["status"] == "True" {
			ready = true
		}
	}
	if !ready {
		return errors.New("pod is Running but not Ready; its TCP readiness probe has not passed")
	}
	return nil
}

// Query runs one bounded SQL query through the Service from a probe Pod. The
// only credential it passes is a Secret-backed PGPASSWORD environment value;
// the command arguments never contain it. Query succeeds only when the query
// really returned its expected row.
func (p *Postgres) Query(ctx context.Context) error {
	jobName, err := p.probeJobName()
	if err != nil {
		return err
	}
	manifest, err := renderProbeJob(p, jobName)
	if err != nil {
		return err
	}
	if _, err := p.cluster.Apply(ctx, manifest); err != nil {
		return fmt.Errorf("kubedeps: create SQL probe Job %s: %w", jobName, err)
	}
	options := p.waitOptions
	options.Timeout = p.probeTimeout
	probe := func(ctx context.Context) error {
		document, err := p.cluster.GetJSON(ctx, "get", "job", jobName, "-n", p.namespace)
		if err != nil {
			return fmt.Errorf("observe SQL probe Job %s: %w", jobName, err)
		}
		return jobComplete(document)
	}
	if err := harness.WaitReady(ctx, p.clock, "postgres dependency "+p.name+" SQL probe", probe, options); err != nil {
		if detail := p.probeDiagnostics(ctx, jobName); detail != "" {
			return fmt.Errorf("kubedeps: SQL probe through Service %s did not succeed: %w (%s)", p.ServiceHost(), err, detail)
		}
		return fmt.Errorf("kubedeps: SQL probe through Service %s did not succeed: %w", p.ServiceHost(), err)
	}
	result, err := p.cluster.Kubectl(ctx, "logs", "-n", p.namespace, "job/"+jobName, "-c", "probe")
	if err != nil {
		return fmt.Errorf("kubedeps: read SQL probe logs for Job %s: %w", jobName, err)
	}
	output := strings.TrimSpace(string(result.Stdout))
	if output != "1" {
		return fmt.Errorf("kubedeps: SQL probe through Service %s returned %q; want 1",
			p.ServiceHost(), truncate(output, 200))
	}
	return nil
}

func (p *Postgres) probeDiagnostics(ctx context.Context, jobName string) string {
	diagnosticCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	result, err := p.cluster.Kubectl(diagnosticCtx, "logs", "-n", p.namespace, "job/"+jobName, "-c", "probe")
	if err != nil {
		return ""
	}
	return truncate(strings.TrimSpace(string(result.Stdout)), 400)
}

func jobComplete(document map[string]any) error {
	status, _ := document["status"].(map[string]any)
	conditions, _ := status["conditions"].([]any)
	for _, entry := range conditions {
		condition, _ := entry.(map[string]any)
		if condition["status"] != "True" {
			continue
		}
		switch condition["type"] {
		case "Complete":
			return nil
		case "Failed":
			return errors.New("SQL probe Job failed")
		}
	}
	if intValue(status["succeeded"]) > 0 {
		return nil
	}
	if intValue(status["failed"]) > 0 {
		return errors.New("SQL probe Job failed")
	}
	return errors.New("SQL probe Job has not completed")
}

func intValue(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int32:
		return int(typed)
	case int64:
		return int(typed)
	case float32:
		return int(typed)
	case float64:
		return int(typed)
	default:
		return 0
	}
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "..."
}

// NamespaceExists reports whether this fixture's namespace still exists.
func (p *Postgres) NamespaceExists(ctx context.Context) (bool, error) {
	return p.namespaceExists(ctx)
}

func (p *Postgres) namespaceExists(ctx context.Context) (bool, error) {
	result, err := p.cluster.Kubectl(ctx, "get", "namespace", p.namespace, "--ignore-not-found", "-o", "name")
	if err != nil {
		return false, fmt.Errorf("kubedeps: observe dependency namespace %s: %w", p.namespace, err)
	}
	return strings.TrimSpace(string(result.Stdout)) != "", nil
}

// Close deletes exactly this fixture's namespace and waits for the deletion.
// It verifies the namespace is gone; a failed or unverified deletion is
// returned, never only logged. Close is idempotent after a verified deletion.
func (p *Postgres) Close(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	deleteCtx, cancel := context.WithTimeout(ctx, p.deleteTimeout)
	defer cancel()
	if _, err := p.cluster.Kubectl(deleteCtx, "delete", "namespace", p.namespace,
		"--wait=true", "--ignore-not-found=true"); err != nil {
		return fmt.Errorf("kubedeps: delete dependency namespace %s: %w", p.namespace, err)
	}
	exists, err := p.namespaceExists(deleteCtx)
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("kubedeps: dependency namespace %s still exists after deletion", p.namespace)
	}
	p.closed = true
	return nil
}

func (p *Postgres) probeJobName() (string, error) {
	suffix, err := randomHex(4)
	if err != nil {
		return "", err
	}
	name := p.name + "-probe-" + suffix
	if len(name) > 63 {
		name = "probe-" + suffix
	}
	return name, nil
}

// manifests is the rendered object set for one fixture.
type manifests struct {
	Namespace string
	Secret    string
	Pod       string
	Service   string
}

// renderManifests renders the fixture objects. Every object carries the
// fixture namespace and its unique instance label.
func renderManifests(fixture *Postgres) (manifests, error) {
	labels := map[string]string{
		LabelDependency: DependencyPostgres,
		LabelInstance:   fixture.name,
	}
	namespaceDocument := namespaceObject{
		APIVersion: "v1",
		Kind:       "Namespace",
		Metadata: objectMeta{
			Name:   fixture.namespace,
			Labels: labels,
		},
	}
	secretDocument := secretObject{
		APIVersion: "v1",
		Kind:       "Secret",
		Metadata: objectMeta{
			Name:      fixture.SecretName(),
			Namespace: fixture.namespace,
			Labels:    labels,
		},
		Type:       "Opaque",
		StringData: map[string]string{"password": fixture.password},
	}
	podDocument := podObject{
		APIVersion: "v1",
		Kind:       "Pod",
		Metadata: objectMeta{
			Name:      fixture.name,
			Namespace: fixture.namespace,
			Labels:    labels,
		},
		Spec: podSpec{
			AutomountServiceAccountToken: false,
			RestartPolicy:                "Always",
			SecurityContext: &podSecurityContext{
				RunAsNonRoot:   true,
				RunAsUser:      70,
				RunAsGroup:     70,
				FSGroup:        70,
				SeccompProfile: &seccompProfile{Type: "RuntimeDefault"},
			},
			Containers: []containerSpec{{
				Name:            "postgres",
				Image:           fixture.image,
				ImagePullPolicy: "IfNotPresent",
				Ports: []containerPort{{
					Name:          "postgres",
					ContainerPort: PostgresPort,
					Protocol:      "TCP",
				}},
				Env: []envVar{
					{Name: "POSTGRES_USER", Value: fixture.user},
					{Name: "POSTGRES_DB", Value: fixture.database},
					{Name: "PGDATA", Value: "/var/lib/postgresql/data"},
					{
						Name: "POSTGRES_PASSWORD",
						ValueFrom: &envValueFrom{SecretKeyRef: &keyReference{
							Name: fixture.SecretName(),
							Key:  "password",
						}},
					},
				},
				Resources: resourceRequirements{
					Requests: resourceList{"cpu": "100m", "memory": "128Mi"},
					Limits:   resourceList{"cpu": "1", "memory": "512Mi"},
				},
				ReadinessProbe: &probeSpec{
					TCPSocket:           &tcpSocket{Port: "postgres"},
					InitialDelaySeconds: 2,
					PeriodSeconds:       2,
					TimeoutSeconds:      1,
					FailureThreshold:    30,
				},
				SecurityContext: &containerSecurityContext{
					AllowPrivilegeEscalation: new(false),
					Capabilities:             &capabilities{Drop: []string{"ALL"}},
				},
				VolumeMounts: []volumeMount{{
					// Mount the parent so the postgres user can create and own
					// PGDATA; an emptyDir mounted directly at PGDATA is root-owned
					// even with fsGroup, and initdb then cannot chmod it.
					Name:      "data",
					MountPath: "/var/lib/postgresql",
				}},
			}},
			Volumes: []volumeSpec{{
				Name:     "data",
				EmptyDir: map[string]any{},
			}},
		},
	}
	serviceDocument := serviceObject{
		APIVersion: "v1",
		Kind:       "Service",
		Metadata: objectMeta{
			Name:      fixture.name,
			Namespace: fixture.namespace,
			Labels:    labels,
		},
		Spec: serviceSpec{
			Type:     "ClusterIP",
			Selector: labels,
			Ports: []servicePort{{
				Name:       "postgres",
				Protocol:   "TCP",
				Port:       PostgresPort,
				TargetPort: "postgres",
			}},
		},
	}
	namespaceYAML, err := marshalManifest(namespaceDocument)
	if err != nil {
		return manifests{}, err
	}
	secretYAML, err := marshalManifest(secretDocument)
	if err != nil {
		return manifests{}, err
	}
	podYAML, err := marshalManifest(podDocument)
	if err != nil {
		return manifests{}, err
	}
	serviceYAML, err := marshalManifest(serviceDocument)
	if err != nil {
		return manifests{}, err
	}
	return manifests{
		Namespace: namespaceYAML,
		Secret:    secretYAML,
		Pod:       podYAML,
		Service:   serviceYAML,
	}, nil
}

// renderProbeJob renders the one-shot SQL probe. It connects to the Service DNS
// name with a Secret-backed PGPASSWORD environment value and retries only the
// SQL attempt, never inventing a successful result.
func renderProbeJob(fixture *Postgres, jobName string) (string, error) {
	labels := map[string]string{
		LabelDependency: DependencyPostgres,
		LabelInstance:   fixture.name,
	}
	job := jobObject{
		APIVersion: "batch/v1",
		Kind:       "Job",
		Metadata: objectMeta{
			Name:      jobName,
			Namespace: fixture.namespace,
			Labels:    labels,
		},
		Spec: jobSpec{
			BackoffLimit:            0,
			ActiveDeadlineSeconds:   120,
			TTLSecondsAfterFinished: 180,
			Template: podTemplate{
				Metadata: objectMeta{Labels: labels},
				Spec: podSpec{
					AutomountServiceAccountToken: false,
					RestartPolicy:                "Never",
					SecurityContext: &podSecurityContext{
						RunAsNonRoot:   true,
						RunAsUser:      70,
						RunAsGroup:     70,
						SeccompProfile: &seccompProfile{Type: "RuntimeDefault"},
					},
					Containers: []containerSpec{{
						Name:            "probe",
						Image:           fixture.image,
						ImagePullPolicy: "IfNotPresent",
						Command:         []string{"sh", "-ec"},
						Args: []string{fmt.Sprintf(`attempt=0
while [ "$attempt" -lt %d ]; do
  attempt=$((attempt+1))
  if out=$(psql -v ON_ERROR_STOP=1 -tAc 'SELECT 1' 2>/dev/null); then
    printf '%%s' "$out"
    exit 0
  fi
  sleep 1
done
psql -v ON_ERROR_STOP=1 -tAc 'SELECT 1'
`, probeRetries)},
						Env: []envVar{
							{Name: "PGHOST", Value: fixture.ServiceHost()},
							{Name: "PGPORT", Value: strconv.Itoa(PostgresPort)},
							{Name: "PGUSER", Value: fixture.user},
							{Name: "PGDATABASE", Value: fixture.database},
							{Name: "PGSSLMODE", Value: "disable"},
							{
								Name: "PGPASSWORD",
								ValueFrom: &envValueFrom{SecretKeyRef: &keyReference{
									Name: fixture.SecretName(),
									Key:  "password",
								}},
							},
						},
						Resources: resourceRequirements{
							Requests: resourceList{"cpu": "50m", "memory": "64Mi"},
							Limits:   resourceList{"cpu": "500m", "memory": "256Mi"},
						},
						SecurityContext: &containerSecurityContext{
							AllowPrivilegeEscalation: new(false),
							Capabilities:             &capabilities{Drop: []string{"ALL"}},
						},
					}},
				},
			},
		},
	}
	return marshalManifest(job)
}

func marshalManifest(value any) (string, error) {
	document, err := yaml.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("kubedeps: render manifest: %w", err)
	}
	return string(document), nil
}

type objectMeta struct {
	Name      string            `yaml:"name"`
	Namespace string            `yaml:"namespace,omitempty"`
	Labels    map[string]string `yaml:"labels,omitempty"`
}

type namespaceObject struct {
	APIVersion string     `yaml:"apiVersion"`
	Kind       string     `yaml:"kind"`
	Metadata   objectMeta `yaml:"metadata"`
}

type secretObject struct {
	APIVersion string            `yaml:"apiVersion"`
	Kind       string            `yaml:"kind"`
	Metadata   objectMeta        `yaml:"metadata"`
	Type       string            `yaml:"type"`
	StringData map[string]string `yaml:"stringData"`
}

type podObject struct {
	APIVersion string     `yaml:"apiVersion"`
	Kind       string     `yaml:"kind"`
	Metadata   objectMeta `yaml:"metadata"`
	Spec       podSpec    `yaml:"spec"`
}

type podSpec struct {
	AutomountServiceAccountToken bool                `yaml:"automountServiceAccountToken"`
	RestartPolicy                string              `yaml:"restartPolicy"`
	SecurityContext              *podSecurityContext `yaml:"securityContext,omitempty"`
	Containers                   []containerSpec     `yaml:"containers"`
	Volumes                      []volumeSpec        `yaml:"volumes,omitempty"`
}

type podSecurityContext struct {
	RunAsNonRoot   bool            `yaml:"runAsNonRoot"`
	RunAsUser      int64           `yaml:"runAsUser,omitempty"`
	RunAsGroup     int64           `yaml:"runAsGroup,omitempty"`
	FSGroup        int64           `yaml:"fsGroup,omitempty"`
	SeccompProfile *seccompProfile `yaml:"seccompProfile,omitempty"`
}

type seccompProfile struct {
	Type string `yaml:"type"`
}

type containerSpec struct {
	Name            string                    `yaml:"name"`
	Image           string                    `yaml:"image"`
	ImagePullPolicy string                    `yaml:"imagePullPolicy,omitempty"`
	Command         []string                  `yaml:"command,omitempty"`
	Args            []string                  `yaml:"args,omitempty"`
	Ports           []containerPort           `yaml:"ports,omitempty"`
	Env             []envVar                  `yaml:"env,omitempty"`
	Resources       resourceRequirements      `yaml:"resources"`
	ReadinessProbe  *probeSpec                `yaml:"readinessProbe,omitempty"`
	SecurityContext *containerSecurityContext `yaml:"securityContext,omitempty"`
	VolumeMounts    []volumeMount             `yaml:"volumeMounts,omitempty"`
}

type containerSecurityContext struct {
	AllowPrivilegeEscalation *bool         `yaml:"allowPrivilegeEscalation,omitempty"`
	Capabilities             *capabilities `yaml:"capabilities,omitempty"`
}

type capabilities struct {
	Drop []string `yaml:"drop,omitempty"`
}

type containerPort struct {
	Name          string `yaml:"name"`
	ContainerPort int    `yaml:"containerPort"`
	Protocol      string `yaml:"protocol,omitempty"`
}

type envVar struct {
	Name      string        `yaml:"name"`
	Value     string        `yaml:"value,omitempty"`
	ValueFrom *envValueFrom `yaml:"valueFrom,omitempty"`
}

type envValueFrom struct {
	SecretKeyRef *keyReference `yaml:"secretKeyRef,omitempty"`
}

type keyReference struct {
	Name string `yaml:"name"`
	Key  string `yaml:"key"`
}

type resourceList map[string]string

type resourceRequirements struct {
	Requests resourceList `yaml:"requests,omitempty"`
	Limits   resourceList `yaml:"limits,omitempty"`
}

type probeSpec struct {
	TCPSocket           *tcpSocket `yaml:"tcpSocket,omitempty"`
	InitialDelaySeconds int        `yaml:"initialDelaySeconds,omitempty"`
	PeriodSeconds       int        `yaml:"periodSeconds,omitempty"`
	TimeoutSeconds      int        `yaml:"timeoutSeconds,omitempty"`
	FailureThreshold    int        `yaml:"failureThreshold,omitempty"`
}

type tcpSocket struct {
	Port string `yaml:"port"`
}

type volumeSpec struct {
	Name     string         `yaml:"name"`
	EmptyDir map[string]any `yaml:"emptyDir"`
}

type volumeMount struct {
	Name      string `yaml:"name"`
	MountPath string `yaml:"mountPath"`
}

type serviceObject struct {
	APIVersion string      `yaml:"apiVersion"`
	Kind       string      `yaml:"kind"`
	Metadata   objectMeta  `yaml:"metadata"`
	Spec       serviceSpec `yaml:"spec"`
}

type serviceSpec struct {
	Type     string            `yaml:"type"`
	Selector map[string]string `yaml:"selector"`
	Ports    []servicePort     `yaml:"ports"`
}

type servicePort struct {
	Name       string `yaml:"name"`
	Protocol   string `yaml:"protocol,omitempty"`
	Port       int    `yaml:"port"`
	TargetPort string `yaml:"targetPort"`
}

type jobObject struct {
	APIVersion string     `yaml:"apiVersion"`
	Kind       string     `yaml:"kind"`
	Metadata   objectMeta `yaml:"metadata"`
	Spec       jobSpec    `yaml:"spec"`
}

type jobSpec struct {
	BackoffLimit            int         `yaml:"backoffLimit"`
	ActiveDeadlineSeconds   int64       `yaml:"activeDeadlineSeconds,omitempty"`
	TTLSecondsAfterFinished int         `yaml:"ttlSecondsAfterFinished,omitempty"`
	Template                podTemplate `yaml:"template"`
}

type podTemplate struct {
	Metadata objectMeta `yaml:"metadata"`
	Spec     podSpec    `yaml:"spec"`
}
