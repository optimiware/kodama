package kernelrt

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/kernel/runtime"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
)

const (
	pathWorkspace = "/workspace"
	pathTmp       = "/tmp"
)

func ptr[T any](v T) *T { return &v }

var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// validateEnv refuses names the adapter reserves or Kubernetes cannot carry.
func validateEnv(env map[string]string) error {
	for k := range env {
		if !envNameRE.MatchString(k) || strings.HasPrefix(k, "KODAMA_") {
			return fmt.Errorf("%w: environment name %q is invalid or reserved", runtime.ErrCapability, k)
		}
	}
	return nil
}

func sortedEnv(env map[string]string) []kubernetes.EnvVar {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]kubernetes.EnvVar, 0, len(keys))
	for _, k := range keys {
		out = append(out, kubernetes.EnvVar{Name: k, Value: env[k]})
	}
	return out
}

// containerSecurity is the one restricted container shape of this adapter.
func (c *core) containerSecurity() *kubernetes.ContainerSecurity {
	return &kubernetes.ContainerSecurity{
		Privileged: ptr(false), AllowPrivilegeEscalation: ptr(false), ReadOnlyRootFilesystem: ptr(false),
		RunAsNonRoot: ptr(true), RunAsUser: ptr(c.cfg.RunAsUser), RunAsGroup: ptr(c.cfg.RunAsGroup),
		Capabilities:   &kubernetes.Capabilities{Drop: []string{"ALL"}},
		SeccompProfile: &kubernetes.SeccompProfile{Type: "RuntimeDefault"},
	}
}

// podSpec is the restricted Pod shape shared by step Jobs and services.
func (c *core) podSpec(t Target, container kubernetes.Container, volumes []kubernetes.Volume) kubernetes.PodSpec {
	grace := int64(c.cfg.TerminationGrace.Seconds())
	return kubernetes.PodSpec{
		ServiceAccountName: t.ServiceAccount, AutomountServiceAccountToken: ptr(false), EnableServiceLinks: ptr(false),
		RestartPolicy: "Never", TerminationGracePeriodSeconds: &grace,
		SecurityContext: &kubernetes.PodSecurity{
			RunAsNonRoot: ptr(true), RunAsUser: ptr(c.cfg.RunAsUser), RunAsGroup: ptr(c.cfg.RunAsGroup), FSGroup: ptr(c.cfg.RunAsGroup),
			SeccompProfile: &kubernetes.SeccompProfile{Type: "RuntimeDefault"},
		},
		Containers: []kubernetes.Container{container},
		Volumes:    volumes,
	}
}

func (c *core) tmpVolume() kubernetes.Volume {
	return kubernetes.Volume{Name: "tmp", EmptyDir: &kubernetes.EmptyDir{SizeLimit: c.cfg.ScratchSize}}
}

// validateSecretDir refuses a materialization directory that is not private
// to the step: it must be absolute, clean and outside /workspace and /tmp.
func validateSecretDir(dir string) error {
	if !path.IsAbs(dir) || path.Clean(dir) != dir || dir == "/" {
		return fmt.Errorf("%w: secret directory %q must be an absolute clean path", runtime.ErrCapability, dir)
	}
	for _, reserved := range []string{pathWorkspace, pathTmp} {
		if dir == reserved || strings.HasPrefix(dir, reserved+"/") {
			return fmt.Errorf("%w: secret directory %q must be outside %s", runtime.ErrCapability, dir, reserved)
		}
	}
	return nil
}

// stepJob renders the Job of one step attempt. workspaceClaim is the PVC name;
// materialName is the Secret of the step's declared secrets, empty for none.
func (c *core) stepJob(id ident, req runtime.StartRequest, t Target, workspaceClaim, materialName string, serviceEnv []kubernetes.EnvVar) kubernetes.Job {
	spec := req.Spec
	env := []kubernetes.EnvVar{
		{Name: "HOME", Value: pathTmp}, {Name: "TMPDIR", Value: pathTmp},
		{Name: "KODAMA_RUN_ID", Value: string(id.run)}, {Name: "KODAMA_STEP_KEY", Value: string(id.step)},
		{Name: "KODAMA_ATTEMPT", Value: fmt.Sprint(id.attempt)},
	}
	env = append(env, serviceEnv...)
	env = append(env, sortedEnv(spec.Env)...)
	mounts := []kubernetes.VolumeMount{
		{Name: "workspace", MountPath: pathWorkspace},
		{Name: "tmp", MountPath: pathTmp},
	}
	volumes := []kubernetes.Volume{
		{Name: "workspace", PersistentVolumeClaim: &kubernetes.PVCVolume{ClaimName: workspaceClaim}},
		c.tmpVolume(),
	}
	if materialName != "" {
		mounts = append(mounts, kubernetes.VolumeMount{Name: "secrets", MountPath: req.Secrets.Directory, ReadOnly: true})
		volumes = append(volumes, kubernetes.Volume{Name: "secrets", Secret: &kubernetes.SecretVolume{
			SecretName: materialName, DefaultMode: ptr(int32(0o440)), Optional: ptr(false),
		}})
	}
	container := kubernetes.Container{
		Name: containerStep, Image: spec.Image, ImagePullPolicy: c.cfg.ImagePullPolicy,
		Command:    append([]string(nil), c.cfg.Shell...),
		Args:       []string{strings.Join(spec.Commands, "\n")},
		WorkingDir: pathWorkspace, Env: env, Resources: c.cfg.Workload.resources(),
		VolumeMounts: mounts, SecurityContext: c.containerSecurity(),
	}
	labels := id.labels(roleObject)
	ann := id.annotations()
	ann[annFence] = fmt.Sprint(uint64(req.Binding.Fence))
	job := kubernetes.Job{
		APIVersion: "batch/v1", Kind: "Job",
		Metadata: kubernetes.ObjectMeta{Name: id.base(), Namespace: t.Namespace, Labels: labels, Annotations: ann},
		Spec: kubernetes.JobSpec{
			Parallelism: ptr(int32(1)), Completions: ptr(int32(1)), BackoffLimit: ptr(int32(0)),
			Template: kubernetes.PodTemplate{
				Metadata: kubernetes.ObjectMeta{Labels: id.labels(roleObject), Annotations: id.annotations()},
				Spec:     c.podSpec(t, container, volumes),
			},
		},
	}
	if spec.Timeout > 0 {
		secs := int64((spec.Timeout + time.Second - 1) / time.Second)
		job.Spec.ActiveDeadlineSeconds = &secs
	}
	return job
}

// servicePod renders a run service container. A declared argv replaces the
// image command; the first declared port carries the readiness probe.
func (c *core) servicePod(id ident, s runtime.ServiceSpec, t Target, fence runtime.Fence) kubernetes.Pod {
	container := kubernetes.Container{
		Name: containerSvc, Image: s.Image, ImagePullPolicy: c.cfg.ImagePullPolicy,
		Command:   append([]string(nil), s.Commands...),
		Env:       append([]kubernetes.EnvVar{{Name: "HOME", Value: pathTmp}, {Name: "TMPDIR", Value: pathTmp}}, sortedEnv(s.Env)...),
		Resources: c.cfg.Service.resources(), SecurityContext: c.containerSecurity(),
		VolumeMounts: []kubernetes.VolumeMount{{Name: "tmp", MountPath: pathTmp}},
	}
	for _, p := range s.Ports {
		container.Ports = append(container.Ports, kubernetes.ContainerPort{ContainerPort: int32(p), Protocol: "TCP"})
	}
	if len(s.Ports) > 0 {
		container.ReadinessProbe = &kubernetes.Probe{
			TCPSocket: &kubernetes.TCPSocketAction{Port: int32(s.Ports[0])}, PeriodSeconds: 2, TimeoutSeconds: 2, SuccessThreshold: 1, FailureThreshold: 3,
		}
	}
	ann := id.annotations()
	ann[annFence] = fmt.Sprint(uint64(fence))
	return kubernetes.Pod{
		APIVersion: "v1", Kind: "Pod",
		Metadata: kubernetes.ObjectMeta{Name: id.base(), Namespace: t.Namespace, Labels: id.labels(roleObject), Annotations: ann},
		Spec:     c.podSpec(t, container, []kubernetes.Volume{c.tmpVolume()}),
	}
}

func (c *core) serviceObject(id ident, s runtime.ServiceSpec, t Target) kubernetes.Service {
	svc := kubernetes.Service{
		APIVersion: "v1", Kind: "Service",
		Metadata: kubernetes.ObjectMeta{Name: id.base(), Namespace: t.Namespace, Labels: id.labels(roleObject), Annotations: id.annotations()},
		Spec:     kubernetes.ServiceSpec{Type: "ClusterIP", Selector: map[string]string{labelObject: id.base(), labelManaged: valueManaged}},
	}
	for _, p := range s.Ports {
		svc.Spec.Ports = append(svc.Spec.Ports, kubernetes.ServicePort{Name: fmt.Sprintf("p%d", p), Protocol: "TCP", Port: int32(p), TargetPort: int32(p)})
	}
	return svc
}

func (c *core) workspaceClaim(id ident, t Target) kubernetes.PersistentVolumeClaim {
	class := c.cfg.StorageClass
	return kubernetes.PersistentVolumeClaim{
		APIVersion: "v1", Kind: "PersistentVolumeClaim",
		Metadata: kubernetes.ObjectMeta{Name: id.base(), Namespace: t.Namespace, Labels: id.labels(roleObject), Annotations: id.annotations()},
		Spec: kubernetes.PVCSpec{
			AccessModes: []string{"ReadWriteOncePod"}, StorageClassName: &class, VolumeMode: "Filesystem",
			Resources: kubernetes.VolumeResources{Requests: map[string]string{"storage": c.cfg.WorkspaceSize}},
		},
	}
}

// serviceHostEnv names the cluster DNS name of each run service, so a step
// reaches it by its declared name.
func serviceHostEnv(ns string, svcs []serviceRef) []kubernetes.EnvVar {
	out := make([]kubernetes.EnvVar, 0, len(svcs))
	for _, s := range svcs {
		out = append(out, kubernetes.EnvVar{Name: "KODAMA_SERVICE_" + envKey(s.name) + "_HOST", Value: s.base + "." + ns + ".svc"})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

type serviceRef struct{ name, base string }

func envKey(name string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r - 'a' + 'A'
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		}
		return '_'
	}, name)
}
