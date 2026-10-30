package kube

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
)

// This file renders the Kubernetes objects of a run resource from its canonical
// spec, the installation's settings and the pinned execution policy body. It is
// pure and deterministic: no generated UID, timestamp or credential. The same
// rendering builds what is created and what adoption compares the live object
// with, so an object that differs in any controlled field is not ours.

const (
	prepCommandName    = "prepare"
	serviceRunCommand  = "service-run"
	reservedEnvPrefix  = "KODAMA_"
	mountShm           = "/dev/shm"
	volumeShm          = "shm"
	suffixIngress      = "-in"
	suffixClients      = "-cli"
	suffixServiceDNS   = "-svc"
	suffixPublic       = "-pub"
	suffixEgress       = "-eg"
	suffixEdgeOut      = "-eo"
	suffixEdgeIn       = "-ei"
	pathKodamaEntry    = pathTools + "/kodama"
	keyAdmissionFile   = pathBootstrap + "/" + KeyAdmission
	keyCAFile          = pathBootstrap + "/" + KeyCA
	keyCheckoutFile    = pathBootstrap + "/" + KeyCheckoutCredential
	keyGitCAFile       = pathBootstrap + "/" + KeyGitCA
	portProtocolTCP    = "TCP"
	probeSuccessThresh = 1
	probeFailureThresh = 1
)

// runRender is the rendering context of one resource.
type runRender struct {
	cfg  PolicyConfig
	tgt  TenantTarget
	spec execution.ResourceSpec
	fp   string
	body execution.ExecutionPolicy
	// publicExcept are the address blocks public egress never reaches.
	publicExcept []string
}

func (x runRender) runKey() string { return RunKey(x.spec.Scope, x.spec.RunID) }

// meta returns the object metadata of the resource's own objects: ownership
// labels and identity annotations. extra labels select Pods by role.
func (x runRender) meta(name string, extra map[string]string) kubernetes.ObjectMeta {
	labels := ownerLabels(x.spec.Scope, x.spec.RunID, x.spec.Kind)
	for k, v := range extra {
		labels[k] = v
	}
	return kubernetes.ObjectMeta{Name: name, Namespace: x.tgt.Namespace, Labels: labels, Annotations: identityAnnotations(x.spec, x.fp)}
}

func (x runRender) want(uid string) ownedWant {
	return ownedWant{scope: x.spec.Scope, run: x.spec.RunID, kind: x.spec.Kind, key: x.spec.Key, name: x.spec.Name, uid: uid, fingerprint: x.fp}
}

// ---- workspace ----

// claim renders the run's scratch claim: ReadWriteOncePod of the policy's
// storage class and the resolved size. An empty class would select the cluster
// default, which is never accepted.
func (x runRender) claim() (kubernetes.PersistentVolumeClaim, error) {
	w := x.body.Workspace
	if w.Access != "ReadWriteOncePod" || w.Reclaim != "Delete" || w.StorageClass == "" {
		return kubernetes.PersistentVolumeClaim{}, wrap(ErrPolicy, "the workspace policy is not ReadWriteOncePod with a named delete-reclaim class")
	}
	if x.spec.Workspace == nil || x.spec.Workspace.SizeMiB < 1 || x.spec.Workspace.SizeMiB > x.body.Limits.MaxRunWorkspaceMiB {
		return kubernetes.PersistentVolumeClaim{}, wrap(ErrPolicy, "the workspace size is outside the pinned policy")
	}
	class := w.StorageClass
	return kubernetes.PersistentVolumeClaim{
		Metadata: x.meta(x.spec.Name, nil),
		Spec: kubernetes.PVCSpec{
			AccessModes: []string{w.Access}, StorageClassName: &class, VolumeMode: "Filesystem",
			Resources: kubernetes.VolumeResources{Requests: map[string]string{"storage": fmt.Sprintf("%dMi", x.spec.Workspace.SizeMiB)}},
		},
	}, nil
}

// ---- Pods ----

func (x runRender) injector() kubernetes.Container {
	c := x.cfg
	return kubernetes.Container{
		Name: InjectContainer, Image: c.InjectorImage, ImagePullPolicy: c.ImagePullPolicy,
		Command:      append([]string(nil), c.InjectorCommand...),
		Resources:    resources(c.Injector),
		VolumeMounts: []kubernetes.VolumeMount{{Name: "tools", MountPath: pathTools}},
		SecurityContext: &kubernetes.ContainerSecurity{
			Privileged: ptr(false), AllowPrivilegeEscalation: ptr(false), ReadOnlyRootFilesystem: ptr(true),
			RunAsNonRoot: ptr(true), RunAsUser: ptr(c.RunAsUser), RunAsGroup: ptr(c.RunAsGroup),
			Capabilities:   &kubernetes.Capabilities{Drop: []string{"ALL"}},
			SeccompProfile: &kubernetes.SeccompProfile{Type: "RuntimeDefault"},
		},
	}
}

// credentialVolume mounts only the credential keys of the record Secret: never
// the stored spec.
func credentialVolume(secretName string, checkout bool) kubernetes.Volume {
	items := []kubernetes.KeyToPath{{Key: KeyAdmission, Path: KeyAdmission}, {Key: KeyCA, Path: KeyCA}}
	if checkout {
		items = append(items, kubernetes.KeyToPath{Key: KeyCheckoutCredential, Path: KeyCheckoutCredential}, kubernetes.KeyToPath{Key: KeyGitCA, Path: KeyGitCA})
	}
	return kubernetes.Volume{Name: "bootstrap", Secret: &kubernetes.SecretVolume{SecretName: secretName, DefaultMode: ptr(int32(0o440)), Optional: ptr(false), Items: items}}
}

func fieldEnv(name, path string) kubernetes.EnvVar {
	return kubernetes.EnvVar{Name: name, ValueFrom: &kubernetes.EnvVarSource{FieldRef: &kubernetes.FieldRef{APIVersion: "v1", FieldPath: path}}}
}

// resourceEnv is the environment of a resource workload (seam S4). jobOwned is
// true for a Pod controlled by a Job (preparation): the workload is then the Job
// and the Pod identity is added.
func (x runRender) resourceEnv(jobOwned bool) []kubernetes.EnvVar {
	env := []kubernetes.EnvVar{
		{Name: "KODAMA_SERVER_URL", Value: x.cfg.RuntimeURL},
		{Name: "KODAMA_CA_FILE", Value: keyCAFile},
		{Name: "KODAMA_RUN_ID", Value: string(x.spec.RunID)},
		{Name: "KODAMA_RESOURCE_KEY", Value: x.spec.Key},
		{Name: "KODAMA_ADMISSION_CREDENTIAL_FILE", Value: keyAdmissionFile},
		{Name: "KODAMA_WORKLOAD_NAME", Value: x.spec.Name},
	}
	if jobOwned {
		env = append(env,
			fieldEnv("KODAMA_WORKLOAD_UID", "metadata.labels['"+PodLabelJobUID+"']"),
			fieldEnv("KODAMA_POD_NAME", "metadata.name"),
			fieldEnv("KODAMA_POD_UID", "metadata.uid"),
		)
		return env
	}
	return append(env, fieldEnv("KODAMA_WORKLOAD_UID", "metadata.uid"))
}

// podLabels are the labels run policies select a resource Pod by.
func (x runRender) podLabels(role, workload string, callback bool) map[string]string {
	l := ownerLabels(x.spec.Scope, x.spec.RunID, x.spec.Kind)
	l[LabelRole], l[LabelWorkload], l[LabelRunShared] = role, WorkloadHash(x.spec.Scope, x.spec.RunID, workload), "true"
	if callback {
		// The chart's runtime-egress policy selects this label: it is how the
		// Kodama callback (and the repository host) stays a namespace-policy matter.
		l[LabelManagedBy] = ValueManagedBy
	}
	return l
}

func (x runRender) podSpec(sec roleSecurity, c kubernetes.Container, inject bool, volumes []kubernetes.Volume) kubernetes.PodSpec {
	grace := x.body.Limits.StopGraceSeconds
	ps := kubernetes.PodSpec{
		ServiceAccountName: x.tgt.ServiceAccount, AutomountServiceAccountToken: ptr(false), EnableServiceLinks: ptr(false),
		RestartPolicy: "Never", TerminationGracePeriodSeconds: &grace, HostUsers: sec.hostUsers, SecurityContext: sec.pod,
		Containers: []kubernetes.Container{c}, Volumes: volumes,
	}
	if inject {
		ps.InitContainers = []kubernetes.Container{x.injector()}
	}
	return ps
}

// ---- preparation ----

// prepJob renders the run's one preparation Job (seam S4): backoffLimit 0, a
// Pod that never restarts, the policy's digest-pinned preparation image running
// `kodama prepare` through the injected entrypoint, the workspace claim at
// /workspace and the read-only checkout credential next to the admission one.
func (x runRender) prepJob() (kubernetes.Job, error) {
	p := x.spec.Preparation
	role := x.body.Roles[execution.RolePreparation]
	if p == nil || p.Image != role.Image || p.TimeoutSecs < 1 {
		return kubernetes.Job{}, wrap(ErrPolicy, "the preparation is not the policy's pinned image")
	}
	sec, err := roleSecurityOf(role, x.cfg)
	if err != nil {
		return kubernetes.Job{}, err
	}
	c, lim := x.cfg, x.body.Limits
	bounds := c.Workload
	if lim.PreparationCPUMillis > 0 {
		bounds.CPURequest, bounds.CPULimit = fmt.Sprintf("%dm", lim.PreparationCPUMillis), fmt.Sprintf("%dm", lim.PreparationCPUMillis)
	}
	if lim.PreparationMemoryMiB > 0 {
		bounds.MemoryRequest, bounds.MemoryLimit = fmt.Sprintf("%dMi", lim.PreparationMemoryMiB), fmt.Sprintf("%dMi", lim.PreparationMemoryMiB)
	}
	env := append(x.resourceEnv(true),
		kubernetes.EnvVar{Name: "KODAMA_WORKSPACE_DIR", Value: pathWorkspace},
		kubernetes.EnvVar{Name: "KODAMA_CHECKOUT_CREDENTIAL_FILE", Value: keyCheckoutFile},
		kubernetes.EnvVar{Name: "KODAMA_GIT_CA_FILE", Value: keyGitCAFile},
		kubernetes.EnvVar{Name: "HOME", Value: pathTmp}, kubernetes.EnvVar{Name: "TMPDIR", Value: pathTmp},
	)
	primary := kubernetes.Container{
		Name: PrimaryContainer, Image: p.Image, ImagePullPolicy: c.ImagePullPolicy,
		Command: []string{pathKodamaEntry}, Args: []string{prepCommandName}, WorkingDir: pathTmp, Env: env,
		Resources: resources(bounds), SecurityContext: sec.container,
		VolumeMounts: []kubernetes.VolumeMount{
			{Name: "tools", MountPath: pathTools, ReadOnly: true},
			{Name: volumeWorkspace, MountPath: pathWorkspace},
			{Name: "tmp", MountPath: pathTmp},
			{Name: "identity", MountPath: pathIdentity},
			{Name: "bootstrap", MountPath: pathBootstrap, ReadOnly: true},
		},
	}
	wsName := execution.ResourceName(x.spec.Scope, x.spec.RunID, execution.KindWorkspace, execution.KeyWorkspace)
	volumes := []kubernetes.Volume{
		{Name: "tools", EmptyDir: &kubernetes.EmptyDir{SizeLimit: c.Volumes.Tools}},
		{Name: volumeWorkspace, PersistentVolumeClaim: &kubernetes.PVCVolume{ClaimName: wsName}},
		{Name: "tmp", EmptyDir: &kubernetes.EmptyDir{SizeLimit: c.Volumes.Scratch}},
		{Name: "identity", EmptyDir: &kubernetes.EmptyDir{Medium: "Memory", SizeLimit: c.Volumes.Identity}},
		credentialVolume(recName(x.spec.Name), true),
	}
	labels := x.podLabels(RoleLabelPreparation, execution.KeyPreparation, true)
	m := x.meta(x.spec.Name, nil)
	return kubernetes.Job{
		APIVersion: "batch/v1", Kind: "Job", Metadata: m,
		Spec: kubernetes.JobSpec{
			Parallelism: ptr(int32(1)), Completions: ptr(int32(1)), BackoffLimit: ptr(int32(0)),
			ActiveDeadlineSeconds: ptr(p.TimeoutSecs),
			Template: kubernetes.PodTemplate{
				Metadata: kubernetes.ObjectMeta{Labels: labels},
				Spec:     x.podSpec(sec, primary, true, volumes),
			},
		},
	}, nil
}

// ---- service ----

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// usesSupervisor reports that the service runs under `kodama service-run`: it
// has pinned secret bindings, so the process is started by the supervisor that
// resolves them.
func usesSupervisor(s *execution.ServiceSpec) bool { return len(s.Secrets) > 0 }

// servicePod renders the service's Pod (seam S4): restartPolicy Never, the
// declared image and argv (or `kodama service-run -- argv` with bindings),
// bounded storage, a memory-backed /dev/shm, the translated readiness probe and
// the pinned service role. It never mounts the workspace.
func (x runRender) servicePod() (kubernetes.Pod, error) {
	s := x.spec.Service
	if s == nil || (s.Role != "" && s.Role != "general") {
		return kubernetes.Pod{}, wrap(ErrPolicy, "only general services are supported")
	}
	if s.Host != execution.EndpointHost(x.spec.Scope, x.spec.RunID, s.Name) {
		return kubernetes.Pod{}, wrap(ErrPolicy, "the service endpoint host is not the run's")
	}
	sec, err := roleSecurityOf(x.body.Roles[execution.RoleService], x.cfg)
	if err != nil {
		return kubernetes.Pod{}, err
	}
	if len(s.Ports) == 0 || len(s.Ports) > 8 {
		return kubernetes.Pod{}, wrap(ErrPolicy, "a service declares one to eight ports")
	}
	var ports []kubernetes.ContainerPort
	for _, n := range sortedKeys(s.Ports) {
		num := s.Ports[n]
		if !validPortName(n) || num < 1 || num > 65535 {
			return kubernetes.Pod{}, wrap(ErrPolicy, "a service port is not a valid name and number")
		}
		ports = append(ports, kubernetes.ContainerPort{Name: n, ContainerPort: int32(num), Protocol: portProtocolTCP})
	}
	probe, err := x.probe(s)
	if err != nil {
		return kubernetes.Pod{}, err
	}
	var env []kubernetes.EnvVar
	for _, k := range sortedKeys(s.Env) {
		if strings.HasPrefix(k, reservedEnvPrefix) {
			return kubernetes.Pod{}, wrap(ErrPolicy, "a service environment name uses the reserved prefix")
		}
		env = append(env, kubernetes.EnvVar{Name: k, Value: s.Env[k]})
	}
	bounds, volumes, mounts := x.serviceStorage(s)
	c := x.cfg
	primary := kubernetes.Container{
		Name: PrimaryContainer, Image: s.Image, ImagePullPolicy: c.ImagePullPolicy, Env: env, Ports: ports, ReadinessProbe: probe,
		Resources: resources(bounds), SecurityContext: sec.container, VolumeMounts: mounts,
	}
	supervised := usesSupervisor(s)
	if supervised {
		if len(s.Command) == 0 {
			return kubernetes.Pod{}, wrap(ErrPolicy, "a service with secret bindings must declare its command")
		}
		primary.Command = []string{pathKodamaEntry}
		primary.Args = append([]string{serviceRunCommand, "--"}, append(append([]string(nil), s.Command...), s.Args...)...)
		primary.Env = append(append([]kubernetes.EnvVar(nil), x.resourceEnv(false)...), primary.Env...)
		primary.VolumeMounts = append(primary.VolumeMounts,
			kubernetes.VolumeMount{Name: "tools", MountPath: pathTools, ReadOnly: true},
			kubernetes.VolumeMount{Name: "secrets", MountPath: pathSecrets},
			kubernetes.VolumeMount{Name: "identity", MountPath: pathIdentity},
			kubernetes.VolumeMount{Name: "bootstrap", MountPath: pathBootstrap, ReadOnly: true},
		)
		volumes = append(volumes,
			kubernetes.Volume{Name: "tools", EmptyDir: &kubernetes.EmptyDir{SizeLimit: c.Volumes.Tools}},
			kubernetes.Volume{Name: "secrets", EmptyDir: &kubernetes.EmptyDir{Medium: "Memory", SizeLimit: c.Volumes.Secrets}},
			kubernetes.Volume{Name: "identity", EmptyDir: &kubernetes.EmptyDir{Medium: "Memory", SizeLimit: c.Volumes.Identity}},
			credentialVolume(recName(x.spec.Name), false),
		)
	} else {
		// Image defaults and argv follow the argv matrix: command replaces the image
		// ENTRYPOINT, args the image CMD; absent stays unset.
		if len(s.Command) > 0 {
			primary.Command = append([]string(nil), s.Command...)
		}
		if len(s.Args) > 0 {
			primary.Args = append([]string(nil), s.Args...)
		}
	}
	m := x.meta(x.spec.Name, x.podLabels(RoleLabelService, execution.ServiceKey(s.Name), supervised))
	return kubernetes.Pod{APIVersion: "v1", Kind: "Pod", Metadata: m, Spec: x.podSpec(sec, primary, supervised, volumes)}, nil
}

// serviceStorage renders the service's resource bounds, its declared storage and
// its bounded memory-backed /dev/shm. The Pod's ephemeral limit covers the
// writable layer, logs and /tmp plus every declared volume.
func (x runRender) serviceStorage(s *execution.ServiceSpec) (ResourceBounds, []kubernetes.Volume, []kubernetes.VolumeMount) {
	r := s.Resources
	eph := r.EphemeralMiB
	var volumes []kubernetes.Volume
	var mounts []kubernetes.VolumeMount
	for i, st := range s.Storage {
		name := fmt.Sprintf("data-%d", i)
		eph += st.SizeMiB
		volumes = append(volumes, kubernetes.Volume{Name: name, EmptyDir: &kubernetes.EmptyDir{SizeLimit: fmt.Sprintf("%dMi", st.SizeMiB)}})
		mounts = append(mounts, kubernetes.VolumeMount{Name: name, MountPath: st.Path})
	}
	if r.ShmMiB > 0 {
		volumes = append(volumes, kubernetes.Volume{Name: volumeShm, EmptyDir: &kubernetes.EmptyDir{Medium: "Memory", SizeLimit: fmt.Sprintf("%dMi", r.ShmMiB)}})
		mounts = append(mounts, kubernetes.VolumeMount{Name: volumeShm, MountPath: mountShm})
	}
	cpu, mem, e := fmt.Sprintf("%dm", r.CPUMillis), fmt.Sprintf("%dMi", r.MemoryMiB), fmt.Sprintf("%dMi", eph)
	return ResourceBounds{CPURequest: cpu, CPULimit: cpu, MemoryRequest: mem, MemoryLimit: mem, EphemeralRequest: e, EphemeralLimit: e}, volumes, mounts
}

// probe translates the readiness contract into a Pod readiness probe over plain
// HTTP or TCP, or an exec probe without a shell.
func (x runRender) probe(s *execution.ServiceSpec) (*kubernetes.Probe, error) {
	r := s.Readiness
	if r.PeriodSecs < 1 || r.ProbeTimeout < 1 {
		return nil, wrap(ErrPolicy, "the readiness probe has no period or timeout")
	}
	p := &kubernetes.Probe{
		TimeoutSeconds: int32(r.ProbeTimeout), PeriodSeconds: int32(r.PeriodSecs),
		SuccessThreshold: probeSuccessThresh, FailureThreshold: probeFailureThresh,
	}
	port := func() (int32, error) {
		n, ok := s.Ports[r.Port]
		if !ok {
			return 0, wrap(ErrPolicy, "the readiness probe names an undeclared port")
		}
		return int32(n), nil
	}
	switch r.Probe {
	case "tcp":
		n, err := port()
		if err != nil {
			return nil, err
		}
		p.TCPSocket = &kubernetes.TCPSocketAction{Port: n}
	case "http":
		n, err := port()
		if err != nil {
			return nil, err
		}
		if !strings.HasPrefix(r.Path, "/") {
			return nil, wrap(ErrPolicy, "the readiness path is not absolute")
		}
		p.HTTPGet = &kubernetes.HTTPGetAction{Path: r.Path, Port: n, Scheme: "HTTP"}
	case "exec":
		if len(r.Exec) == 0 {
			return nil, wrap(ErrPolicy, "the exec probe has no command")
		}
		p.Exec = &kubernetes.ExecAction{Command: append([]string(nil), r.Exec...)}
	default:
		return nil, wrap(ErrPolicy, "the readiness probe is neither tcp, http nor exec")
	}
	return p, nil
}

// serviceObject renders the ClusterIP Service named by the endpoint host. It
// selects exactly the service's Pod (run, role and workload labels).
func (x runRender) serviceObject() (kubernetes.Service, error) {
	s := x.spec.Service
	var ports []kubernetes.ServicePort
	for _, n := range sortedKeys(s.Ports) {
		ports = append(ports, kubernetes.ServicePort{Name: n, Protocol: portProtocolTCP, Port: int32(s.Ports[n]), TargetPort: int32(s.Ports[n])})
	}
	return kubernetes.Service{
		APIVersion: "v1", Kind: "Service", Metadata: x.meta(s.Host, nil),
		Spec: kubernetes.ServiceSpec{Type: "ClusterIP", Selector: x.workloadSelector(RoleLabelService, execution.ServiceKey(s.Name)), Ports: ports},
	}, nil
}

func (x runRender) workloadSelector(role, workload string) map[string]string {
	return map[string]string{
		LabelRunKey: x.runKey(), LabelRole: role, LabelWorkload: WorkloadHash(x.spec.Scope, x.spec.RunID, workload),
	}
}

// ingressPolicy renders the service's ingress rule: its declared ports, from the
// tenant's run_shared Pods under the tenant model and from the run's own steps
// under the run model. Other peers, services included, reach it only along a
// declared edge (see networkPolicies).
func (x runRender) ingressPolicy(model string) kubernetes.NetworkPolicy {
	s := x.spec.Service
	peer := map[string]string{LabelRunShared: "true"}
	if model == execution.IsolationRun {
		peer = map[string]string{LabelRunKey: x.runKey(), LabelRole: RoleLabelStep}
	}
	var ports []map[string]any
	for _, n := range sortedKeys(s.Ports) {
		ports = append(ports, map[string]any{"protocol": portProtocolTCP, "port": s.Ports[n]})
	}
	sort.Slice(ports, func(i, j int) bool { return ports[i]["port"].(int64) < ports[j]["port"].(int64) })
	rule := map[string]any{"from": []map[string]any{{"podSelector": map[string]any{"matchLabels": peer}}}, "ports": ports}
	var p kubernetes.NetworkPolicy
	p.APIVersion, p.Kind = "networking.k8s.io/v1", "NetworkPolicy"
	p.Metadata = x.meta(x.spec.Name+suffixIngress, nil)
	p.Spec = kubernetes.NetworkPolicySpec{
		PodSelector: kubernetes.LabelSelector{MatchLabels: x.workloadSelector(RoleLabelService, execution.ServiceKey(s.Name))},
		PolicyTypes: []string{"Ingress"}, Ingress: []map[string]any{rule},
	}
	return p
}

// precheck renders the resource once, before anything is created: a spec the
// adapter cannot render exactly (an unsupported role, a malformed port name, a
// reserved environment name, a profile it cannot express) is refused with no
// record, no registration and no object.
func (x runRender) precheck() error {
	switch x.spec.Kind {
	case execution.KindWorkspace:
		_, err := x.claim()
		return err
	case execution.KindPreparation:
		_, err := x.prepJob()
		return err
	case execution.KindService:
		if _, err := x.servicePod(); err != nil {
			return err
		}
		_, err := x.serviceObject()
		return err
	}
	// Hostname destinations are resolved later and pinned; here any answer will do.
	_, err := x.networkPolicies("precheck", func(string) ([]netip.Prefix, error) {
		return []netip.Prefix{netip.MustParsePrefix("192.0.2.1/32")}, nil
	})
	return err
}
