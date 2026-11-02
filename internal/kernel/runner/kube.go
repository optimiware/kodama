package runner

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
)

// KubeConfig is the Kubernetes execution configuration of one installation.
// Tenant namespaces are explicit: a tenant absent from the map is denied.
type KubeConfig struct {
	APIURL    string                `json:"-"`
	TokenFile string                `json:"-"`
	CAFile    string                `json:"-"`
	Tenants   map[string]KubeTenant `json:"tenants"`
	Storage   KubeStorage           `json:"storage"`
	Identity  KubeIdentity          `json:"identity"`
	Checkout  KubeCheckout          `json:"checkout"`
}

type KubeTenant struct {
	Namespace      string `json:"namespace"`
	ServiceAccount string `json:"serviceAccount"`
}

type KubeStorage struct {
	Class string `json:"class"`
	Size  string `json:"size"`
}

type KubeIdentity struct {
	RunAsUser  int64  `json:"runAsUser"`
	RunAsGroup int64  `json:"runAsGroup"`
	PullPolicy string `json:"pullPolicy,omitempty"`
}

type KubeCheckout struct {
	Image   string   `json:"image"`
	Command []string `json:"command,omitempty"`
}

// Kube runs each step as a restricted Job in the tenant's namespace. The run's
// workspace is a ReadWriteOnce claim shared by its sequential Jobs. The
// checkout is a first Job over the same claim, so a source-consuming run never
// starts on an empty workspace.
type Kube struct {
	cfg    KubeConfig
	client *kubernetes.Client
}

var _ Backend = (*Kube)(nil)
var _ Checkout = (*Kube)(nil)
var _ SourcePreparer = (*Kube)(nil)

// NewKube validates the installation and builds the in-cluster API client.
func NewKube(cfg KubeConfig) (*Kube, error) {
	if cfg.APIURL == "" || cfg.TokenFile == "" || cfg.CAFile == "" {
		return nil, errors.New("kernel: the Kubernetes executor needs the API URL, token file and CA file")
	}
	if len(cfg.Tenants) == 0 {
		return nil, errors.New("kernel: the Kubernetes executor needs at least one tenant namespace")
	}
	if cfg.Storage.Class == "" || cfg.Storage.Size == "" {
		return nil, errors.New("kernel: the Kubernetes executor needs a storage class and workspace size")
	}
	if cfg.Identity.RunAsUser <= 0 || cfg.Identity.RunAsGroup <= 0 {
		return nil, errors.New("kernel: the Kubernetes executor needs a non-root user and group")
	}
	if cfg.Checkout.Image == "" {
		return nil, errors.New("kernel: the Kubernetes executor needs a digest-pinned checkout image")
	}
	caPEM, err := os.ReadFile(cfg.CAFile)
	if err != nil {
		return nil, fmt.Errorf("kernel: Kubernetes CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("kernel: Kubernetes CA contains no certificate")
	}
	if _, err := os.ReadFile(cfg.TokenFile); err != nil {
		return nil, fmt.Errorf("kernel: Kubernetes token: %w", err)
	}
	base := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, ResponseHeaderTimeout: 30 * time.Second}
	hc := &http.Client{Transport: &kubeTokenTransport{base: base, tokenFile: cfg.TokenFile},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	client, err := kubernetes.NewClient(kubernetes.Config{Origin: cfg.APIURL, HTTP: hc})
	if err != nil {
		return nil, err
	}
	return &Kube{cfg: cfg, client: client}, nil
}

type kubeTokenTransport struct {
	base      http.RoundTripper
	tokenFile string
}

func (t *kubeTokenTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	raw, err := os.ReadFile(t.tokenFile)
	if err != nil {
		return nil, err
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return nil, errors.New("kubernetes token is empty")
	}
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+token)
	return t.base.RoundTrip(r)
}

func (k *Kube) Name() string { return "kubernetes" }

func (k *Kube) tenant(scope domain.Scope) (KubeTenant, error) {
	t, ok := k.cfg.Tenants[string(scope.TenantID)]
	if !ok || t.Namespace == "" {
		return KubeTenant{}, fmt.Errorf("kernel: tenant %s has no Kubernetes namespace", scope.TenantID)
	}
	if t.ServiceAccount == "" {
		t.ServiceAccount = "kodama-execution"
	}
	return t, nil
}

func runName(scope domain.Scope, runID string) string {
	return "kodama-run-" + sanitize(string(scope.TenantID)) + "-" + sanitize(runID)
}

func stepName(scope domain.Scope, runID, step string) string {
	name := "kodama-step-" + sanitize(runID) + "-" + sanitize(step)
	if len(name) > 63 {
		name = name[:63]
	}
	return strings.TrimRight(name, "-.")
}

// Prepare creates or adopts the run's workspace claim. A claim is never bound to
// a host directory: the Jobs of the run mount it one at a time.
func (k *Kube) Prepare(ctx context.Context, scope domain.Scope, runID string) (string, func(context.Context) error, error) {
	t, err := k.tenant(scope)
	if err != nil {
		return "", nil, err
	}
	ns, err := k.client.GetNamespace(ctx, t.Namespace)
	if err != nil {
		return "", nil, fmt.Errorf("kernel: tenant namespace %s: %w", t.Namespace, err)
	}
	if got := ns.Metadata.Annotations["kodama.optimi.com/tenant"]; got != string(scope.TenantID) {
		return "", nil, fmt.Errorf("kernel: namespace %s is not bound to tenant %s", t.Namespace, scope.TenantID)
	}
	name := runName(scope, runID)
	claim, err := k.ensureClaim(ctx, t.Namespace, name, sanitize(runID))
	if err != nil {
		return "", nil, err
	}
	_ = claim
	cleanup := func(cctx context.Context) error {
		return k.cleanupRun(cctx, t.Namespace, runID)
	}
	return name, cleanup, nil
}

func (k *Kube) ensureClaim(ctx context.Context, ns, name, runLabel string) (kubernetes.PersistentVolumeClaim, error) {
	p, err := k.client.GetPVC(ctx, ns, name)
	if err == nil {
		if p.Metadata.Labels["app.kubernetes.io/managed-by"] != "kodama" {
			return kubernetes.PersistentVolumeClaim{}, fmt.Errorf("kernel: claim %s is not managed by kodama", name)
		}
		return p, nil
	}
	if !errors.Is(err, kubernetes.ErrNotFound) {
		return kubernetes.PersistentVolumeClaim{}, fmt.Errorf("kernel: read workspace claim: %w", err)
	}
	class := k.cfg.Storage.Class
	created, err := k.client.CreatePVC(ctx, ns, kubernetes.PersistentVolumeClaim{
		Metadata: kubernetes.ObjectMeta{Name: name, Labels: map[string]string{"app.kubernetes.io/managed-by": "kodama", "kodama.optimi.com/run": runLabel}},
		Spec: kubernetes.PVCSpec{
			AccessModes: []string{"ReadWriteOnce"}, StorageClassName: &class, VolumeMode: "Filesystem",
			Resources: kubernetes.VolumeResources{Requests: map[string]string{"storage": k.cfg.Storage.Size}},
		},
	})
	if err == nil {
		return created, nil
	}
	if errors.Is(err, kubernetes.ErrAlreadyExists) || errors.Is(err, kubernetes.ErrUnknownOutcome) {
		p, rerr := k.client.GetPVC(ctx, ns, name)
		if rerr == nil {
			return p, nil
		}
	}
	return kubernetes.PersistentVolumeClaim{}, fmt.Errorf("kernel: create workspace claim: %w", err)
}

// Checkout is not a host operation for Kubernetes; PrepareSources runs the
// checkout in the run's volume. This method keeps the generic refusal sound.
func (k *Kube) Checkout(context.Context, Request) error {
	return fmt.Errorf("%w: the Kubernetes backend checks sources out in a Job", ErrCheckoutUnconfigured)
}

// PrepareSources writes the run's Git credentials into a Secret and runs one
// checkout Job over the run's claim. The credential directory is host scratch;
// the Secret is the only copy inside the cluster.
func (k *Kube) PrepareSources(ctx context.Context, req Request) error {
	t, err := k.tenant(req.Scope)
	if err != nil {
		return err
	}
	script, err := CheckoutScript(req.Sources)
	if err != nil {
		return err
	}
	secretName := strings.Replace(req.Workspace, "kodama-run-", "kodama-git-", 1)
	if err := k.createFileSecret(ctx, t.Namespace, secretName, req.Credentials.Dir, sanitize(req.RunID)); err != nil {
		return err
	}
	jobName := strings.Replace(req.Workspace, "kodama-run-", "kodama-checkout-", 1)
	job := k.checkoutJob(t.Namespace, jobName, req.Workspace, secretName, script, sanitize(req.RunID))
	_, err = k.runJob(ctx, t.Namespace, jobName, job, "checkout", nil)
	return err
}

func (k *Kube) createFileSecret(ctx context.Context, ns, name, dir, runLabel string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("kernel: read Git credentials: %w", err)
	}
	data := map[string][]byte{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return err
		}
		data[e.Name()] = b
	}
	s := kubernetes.Secret{Metadata: kubernetes.ObjectMeta{Name: name, Labels: map[string]string{"app.kubernetes.io/managed-by": "kodama", "kodama.optimi.com/run": runLabel}}, Type: "Opaque", Data: data}
	if _, err := k.client.CreateSecret(ctx, ns, s); err != nil {
		if errors.Is(err, kubernetes.ErrAlreadyExists) {
			cur, gerr := k.client.GetSecret(ctx, ns, name)
			if gerr != nil {
				return gerr
			}
			if cur.Metadata.Labels["app.kubernetes.io/managed-by"] != "kodama" {
				return errors.New("kernel: the run's credential Secret is not managed by kodama")
			}
			// A resume may rotate a run credential. Update the projected file in
			// place under the Secret's resourceVersion; the next Pod reads it.
			cur.Data = data
			if _, err := k.client.UpdateSecret(ctx, ns, cur); err != nil {
				return fmt.Errorf("kernel: update credential Secret: %w", err)
			}
			return nil
		}
		return fmt.Errorf("kernel: create Git credential Secret: %w", err)
	}
	return nil
}

func (k *Kube) checkoutJob(ns, name, claim, secret, script, runLabel string) kubernetes.Job {
	user, group := k.cfg.Identity.RunAsUser, k.cfg.Identity.RunAsGroup
	no := false
	return kubernetes.Job{
		Metadata: kubernetes.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{"app.kubernetes.io/managed-by": "kodama", "kodama.optimi.com/component": "checkout", "kodama.optimi.com/run": runLabel}},
		Spec: kubernetes.JobSpec{
			Parallelism: int32Ptr(1), Completions: int32Ptr(1), BackoffLimit: int32Ptr(0),
			Template: kubernetes.PodTemplate{
				Metadata: kubernetes.ObjectMeta{Labels: map[string]string{"app.kubernetes.io/managed-by": "kodama", "kodama.optimi.com/component": "checkout", "kodama.optimi.com/run": runLabel}},
				Spec: kubernetes.PodSpec{
					RestartPolicy: "Never", AutomountServiceAccountToken: &no, EnableServiceLinks: &no,
					SecurityContext: &kubernetes.PodSecurity{RunAsNonRoot: boolPtr(true), RunAsUser: &user, RunAsGroup: &group, FSGroup: &group,
						SeccompProfile: &kubernetes.SeccompProfile{Type: "RuntimeDefault"}},
					Containers: []kubernetes.Container{{
						Name: "checkout", Image: k.cfg.Checkout.Image, ImagePullPolicy: k.cfg.Identity.PullPolicy,
						Command: []string{"/bin/sh", "-ec", script}, WorkingDir: "/workspace",
						Env: []kubernetes.EnvVar{
							{Name: "KODAMA_GIT_DIR", Value: GitContainerDir},
							{Name: "GIT_CONFIG_GLOBAL", Value: GitContainerDir + "/" + GitConfigFile},
							{Name: "GIT_TERMINAL_PROMPT", Value: "0"},
							{Name: "HOME", Value: "/tmp"},
						},
						SecurityContext: &kubernetes.ContainerSecurity{Privileged: boolPtr(false), AllowPrivilegeEscalation: boolPtr(false),
							ReadOnlyRootFilesystem: boolPtr(true), RunAsNonRoot: boolPtr(true), RunAsUser: &user, RunAsGroup: &group,
							Capabilities: &kubernetes.Capabilities{Drop: []string{"ALL"}}, SeccompProfile: &kubernetes.SeccompProfile{Type: "RuntimeDefault"}},
						VolumeMounts: []kubernetes.VolumeMount{{Name: "workspace", MountPath: "/workspace"}, {Name: "git", MountPath: GitContainerDir, ReadOnly: true}, {Name: "tmp", MountPath: "/tmp"}},
					}},
					Volumes: []kubernetes.Volume{
						{Name: "workspace", PersistentVolumeClaim: &kubernetes.PVCVolume{ClaimName: claim}},
						{Name: "git", Secret: &kubernetes.SecretVolume{SecretName: secret, DefaultMode: int32Ptr(0o440)}},
						{Name: "tmp", EmptyDir: &kubernetes.EmptyDir{SizeLimit: "128Mi"}},
					},
				},
			},
		},
	}
}

// Execute runs one step as a Job and waits for it. The returned exit code is the
// step container's; a lost create is adopted by name, never repeated.
func (k *Kube) Execute(ctx context.Context, spec Spec, log io.Writer) (int, error) {
	scope := spec.Scope
	t, err := k.tenant(scope)
	if err != nil {
		return 0, err
	}
	secretName := ""
	if spec.SecretsDir != "" {
		if _, err := os.Stat(spec.SecretsDir); err == nil {
			secretName = strings.Replace(runName(scope, spec.RunID), "kodama-run-", "kodama-secrets-", 1) + "-" + sanitize(spec.StepKey)
			if err := k.createFileSecret(ctx, t.Namespace, secretName, spec.SecretsDir, sanitize(spec.RunID)); err != nil {
				return 0, err
			}
		}
	}
	gitSecret := ""
	if spec.GitDir != "" {
		gitSecret = strings.Replace(runName(scope, spec.RunID), "kodama-run-", "kodama-git-", 1)
	}
	name := stepName(scope, spec.RunID, spec.StepKey)
	job := k.stepJob(t, name, runName(scope, spec.RunID), secretName, gitSecret, spec)
	return k.runJob(ctx, t.Namespace, name, job, "step", log)
}

func (k *Kube) stepJob(t KubeTenant, name, claim, secretName, gitSecret string, spec Spec) kubernetes.Job {
	user, group := k.cfg.Identity.RunAsUser, k.cfg.Identity.RunAsGroup
	no := false
	env := []kubernetes.EnvVar{
		{Name: "KODAMA_SECRETS_DIR", Value: "/kodama/secrets"},
		{Name: "HOME", Value: "/tmp"},
	}
	for key, value := range spec.Env {
		env = append(env, kubernetes.EnvVar{Name: key, Value: value})
	}
	volumes := []kubernetes.Volume{
		{Name: "workspace", PersistentVolumeClaim: &kubernetes.PVCVolume{ClaimName: claim}},
		{Name: "tmp", EmptyDir: &kubernetes.EmptyDir{SizeLimit: "512Mi"}},
	}
	mounts := []kubernetes.VolumeMount{{Name: "workspace", MountPath: "/workspace"}, {Name: "tmp", MountPath: "/tmp"}}
	if secretName != "" {
		volumes = append(volumes, kubernetes.Volume{Name: "secrets", Secret: &kubernetes.SecretVolume{SecretName: secretName, DefaultMode: int32Ptr(0o440)}})
		mounts = append(mounts, kubernetes.VolumeMount{Name: "secrets", MountPath: "/kodama/secrets", ReadOnly: true})
	}
	if gitSecret != "" {
		env = append(env,
			kubernetes.EnvVar{Name: "KODAMA_GIT_DIR", Value: GitContainerDir},
			kubernetes.EnvVar{Name: "GIT_CONFIG_GLOBAL", Value: GitContainerDir + "/" + GitConfigFile},
			kubernetes.EnvVar{Name: "GIT_TERMINAL_PROMPT", Value: "0"},
		)
		volumes = append(volumes, kubernetes.Volume{Name: "git", Secret: &kubernetes.SecretVolume{SecretName: gitSecret, DefaultMode: int32Ptr(0o440)}})
		mounts = append(mounts, kubernetes.VolumeMount{Name: "git", MountPath: GitContainerDir, ReadOnly: true})
	}
	deadline := int64(spec.Timeout.Seconds())
	if deadline < 1 {
		deadline = 900
	}
	return kubernetes.Job{
		Metadata: kubernetes.ObjectMeta{Name: name, Namespace: t.Namespace, Labels: map[string]string{"app.kubernetes.io/managed-by": "kodama", "kodama.optimi.com/component": "step", "kodama.optimi.com/run": sanitize(spec.RunID)}},
		Spec: kubernetes.JobSpec{
			Parallelism: int32Ptr(1), Completions: int32Ptr(1), BackoffLimit: int32Ptr(0), ActiveDeadlineSeconds: &deadline,
			Template: kubernetes.PodTemplate{
				Metadata: kubernetes.ObjectMeta{Labels: map[string]string{"app.kubernetes.io/managed-by": "kodama", "kodama.optimi.com/component": "step", "kodama.optimi.com/run": sanitize(spec.RunID)}},
				Spec: kubernetes.PodSpec{
					ServiceAccountName: t.ServiceAccount, AutomountServiceAccountToken: &no, EnableServiceLinks: &no,
					RestartPolicy: "Never",
					SecurityContext: &kubernetes.PodSecurity{RunAsNonRoot: boolPtr(true), RunAsUser: &user, RunAsGroup: &group, FSGroup: &group,
						SeccompProfile: &kubernetes.SeccompProfile{Type: "RuntimeDefault"}},
					Containers: []kubernetes.Container{{
						Name: "step", Image: spec.Image, ImagePullPolicy: k.cfg.Identity.PullPolicy,
						Command: []string{"/bin/sh", "-ec", strings.Join(spec.Commands, "\n")}, WorkingDir: "/workspace",
						Env: env, SecurityContext: &kubernetes.ContainerSecurity{Privileged: boolPtr(false), AllowPrivilegeEscalation: boolPtr(false),
							ReadOnlyRootFilesystem: boolPtr(true), RunAsNonRoot: boolPtr(true), RunAsUser: &user, RunAsGroup: &group,
							Capabilities: &kubernetes.Capabilities{Drop: []string{"ALL"}}, SeccompProfile: &kubernetes.SeccompProfile{Type: "RuntimeDefault"}},
						VolumeMounts: mounts,
					}},
					Volumes: volumes,
				},
			},
		},
	}
}

// runJob creates or adopts a Job, waits for it and mirrors its Pod log. The
// create is deterministic: a retry, including after a controller restart, reads
// the same Job instead of starting a second one.
func (k *Kube) runJob(ctx context.Context, ns, name string, job kubernetes.Job, container string, log io.Writer) (int, error) {
	existing, err := k.client.GetJob(ctx, ns, name)
	uid := existing.Metadata.UID
	if err != nil {
		if !errors.Is(err, kubernetes.ErrNotFound) {
			return 0, fmt.Errorf("kernel: read Job %s: %w", name, err)
		}
		created, cerr := k.client.CreateJob(ctx, ns, job)
		switch {
		case cerr == nil:
			uid = created.Metadata.UID
		case errors.Is(cerr, kubernetes.ErrAlreadyExists), errors.Is(cerr, kubernetes.ErrUnknownOutcome):
			cur, rerr := k.client.GetJob(ctx, ns, name)
			if rerr != nil {
				return 0, fmt.Errorf("kernel: adopt Job %s: %w", name, rerr)
			}
			if !jobsOwned(cur, container) {
				return 0, fmt.Errorf("kernel: Job %s is not this run's %s workload", name, container)
			}
			uid = cur.Metadata.UID
		default:
			return 0, fmt.Errorf("kernel: create Job %s: %w", name, cerr)
		}
	} else if !jobsOwned(existing, container) {
		return 0, fmt.Errorf("kernel: Job %s is not this run's %s workload", name, container)
	}
	return k.waitJob(ctx, ns, name, uid, container, log)
}

func jobsOwned(j kubernetes.Job, container string) bool {
	return j.Metadata.Labels["app.kubernetes.io/managed-by"] == "kodama" &&
		j.Metadata.Labels["kodama.optimi.com/component"] == container
}

func (k *Kube) waitJob(ctx context.Context, ns, name, uid, container string, log io.Writer) (int, error) {
	var last int
	for {
		job, err := k.client.GetJob(ctx, ns, name)
		if err != nil {
			return 0, fmt.Errorf("kernel: read Job %s: %w", name, err)
		}
		if job.Metadata.UID != "" && uid != "" && job.Metadata.UID != uid {
			return 0, fmt.Errorf("kernel: Job %s was replaced by a foreign object", name)
		}
		if log != nil {
			last = k.mirrorLog(ctx, ns, name, container, log, last)
		}
		if _, ok := job.Status.Condition("Complete"); ok {
			if log != nil {
				_ = k.mirrorLog(ctx, ns, name, container, log, last)
			}
			return 0, nil
		}
		if _, ok := job.Status.Condition("Failed"); ok {
			if log != nil {
				_ = k.mirrorLog(ctx, ns, name, container, log, last)
			}
			return 1, nil
		}
		select {
		case <-ctx.Done():
			// A cancelled step is cleaned by the run's cleanup; a graceful
			// shutdown must leave the Job for the next process to adopt.
			return 0, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func (k *Kube) mirrorLog(ctx context.Context, ns, job, container string, dst io.Writer, last int) int {
	pods, err := k.client.ListPods(ctx, ns, "job-name="+job)
	if err != nil || len(pods) == 0 {
		return last
	}
	raw, err := k.client.PodLog(ctx, ns, pods[0].Metadata.Name, container)
	if err != nil || len(raw) <= last {
		return last
	}
	_, _ = dst.Write(raw[last:])
	return len(raw)
}

func (k *Kube) deleteJob(ctx context.Context, ns, name, uid string) error {
	if uid == "" {
		j, err := k.client.GetJob(ctx, ns, name)
		if err != nil {
			if errors.Is(err, kubernetes.ErrNotFound) {
				return nil
			}
			return err
		}
		uid = j.Metadata.UID
	}
	err := k.client.DeleteJob(ctx, ns, name, kubernetes.DeleteOptions{UID: uid, Propagation: "Foreground"})
	if err != nil && !errors.Is(err, kubernetes.ErrNotFound) {
		return err
	}
	return nil
}

func (k *Kube) cleanupRun(ctx context.Context, ns, runID string) error {
	jobs, err := k.client.ListJobs(ctx, ns, "app.kubernetes.io/managed-by=kodama,kodama.optimi.com/run="+sanitize(runID))
	if err != nil {
		return err
	}
	for _, j := range jobs {
		if err := k.deleteJob(ctx, ns, j.Metadata.Name, j.Metadata.UID); err != nil {
			return err
		}
	}
	secrets, err := k.client.ListSecrets(ctx, ns, "app.kubernetes.io/managed-by=kodama,kodama.optimi.com/run="+sanitize(runID))
	if err != nil {
		return err
	}
	for _, s := range secrets {
		if err := k.client.DeleteSecret(ctx, ns, s.Metadata.Name, kubernetes.DeleteOptions{UID: s.Metadata.UID}); err != nil && !errors.Is(err, kubernetes.ErrNotFound) {
			return err
		}
	}
	// The claim name is kodama-run-<tenant>-<run>; the caller passes the run ID.
	for name := range k.cfg.Tenants {
		claim := "kodama-run-" + sanitize(name) + "-" + sanitize(runID)
		p, err := k.client.GetPVC(ctx, ns, claim)
		if err != nil {
			if errors.Is(err, kubernetes.ErrNotFound) {
				continue
			}
			return err
		}
		if err := k.client.DeletePVC(ctx, ns, claim, kubernetes.DeleteOptions{UID: p.Metadata.UID}); err != nil && !errors.Is(err, kubernetes.ErrNotFound) {
			return err
		}
	}
	return nil
}

// JobExists reports the deterministic Job of a step (tests and recovery).
func (k *Kube) JobExists(ctx context.Context, scope domain.Scope, runID, step string) (bool, error) {
	t, err := k.tenant(scope)
	if err != nil {
		return false, err
	}
	_, err = k.client.GetJob(ctx, t.Namespace, stepName(scope, runID, step))
	if errors.Is(err, kubernetes.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

func boolPtr(v bool) *bool    { return &v }
func int32Ptr(v int32) *int32 { return &v }
