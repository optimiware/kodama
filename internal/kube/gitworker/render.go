package gitworker

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
)

// Objects are the Kubernetes objects of one tenant's Git hosting, rendered from
// the tenant and the desired worker. Rendering is pure and deterministic: no
// generated UID, timestamp or credential. The same rendering builds what is
// created and what verification compares the live objects with.
type Objects struct {
	Namespace      kubernetes.Namespace
	ServiceAccount kubernetes.ServiceAccount
	Claim          kubernetes.PersistentVolumeClaim
	// Secret is the identity Secret without data: the data is the issued material.
	Secret     kubernetes.Secret
	Service    kubernetes.Service
	Policies   []kubernetes.NetworkPolicy
	Deployment kubernetes.Deployment
}

// Pod Security labels of the service namespace: the restricted profile at the
// pinned version, enforced, audited and warned.
func podSecurityLabels(version string) map[string]string {
	l := map[string]string{}
	for _, mode := range []string{"enforce", "audit", "warn"} {
		l[labelPSAPrefix+mode] = "restricted"
		l[labelPSAPrefix+mode+"-version"] = version
	}
	return l
}

func ownerLabels(key, role string) map[string]string {
	return map[string]string{LabelOwner: ValueOwner, LabelTenantKey: key, LabelObject: role}
}

func selectorLabels(key string) map[string]string {
	return map[string]string{LabelOwner: ValueOwner, LabelTenantKey: key, LabelApp: ValueApp}
}

func merge(ms ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range ms {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// fingerprintOf is the digest of a rendered object (before its fingerprint and
// namespace UID annotations exist).
func fingerprintOf(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic("gitworker: unencodable object: " + err.Error()) // typed objects always encode
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func ptr[T any](v T) *T { return &v }

// meta builds the metadata of an owned object.
func meta(name, ns, key string, tenant domain.TenantID, role string, extra map[string]string) kubernetes.ObjectMeta {
	return kubernetes.ObjectMeta{
		Name: name, Namespace: ns,
		Labels:      merge(ownerLabels(key, role), extra),
		Annotations: map[string]string{AnnTenant: string(tenant)},
	}
}

// seal computes the fingerprint of the object as rendered and records it.
func seal[T any](v *T, m *kubernetes.ObjectMeta) {
	m.Annotations[AnnFingerprint] = ""
	m.Annotations[AnnFingerprint] = fingerprintOf(v)
}

// Render builds every object of the tenant's hosting. The spec is validated and
// defaulted first; an invalid spec renders nothing.
func Render(tenant domain.TenantID, spec WorkerSpec) (Objects, error) {
	if err := tenant.Validate(); err != nil {
		return Objects{}, invalid("the tenant ID is not valid")
	}
	spec = spec.withDefaults()
	if err := spec.validate(); err != nil {
		return Objects{}, err
	}
	key, ns := TenantKey(tenant), NamespaceOf(tenant)
	var o Objects

	o.Namespace = kubernetes.Namespace{Metadata: meta(ns, "", key, tenant, RoleNamespace, podSecurityLabels(spec.PodSecurityVersion))}
	seal(&o.Namespace, &o.Namespace.Metadata)

	o.ServiceAccount = kubernetes.ServiceAccount{Metadata: meta(ServiceAccountName, ns, key, tenant, RoleServiceAccount, nil), AutomountServiceAccountToken: ptr(false)}
	seal(&o.ServiceAccount, &o.ServiceAccount.Metadata)

	class := spec.Storage.Class
	o.Claim = kubernetes.PersistentVolumeClaim{
		Metadata: meta(ClaimName, ns, key, tenant, RoleClaim, nil),
		Spec: kubernetes.PVCSpec{
			AccessModes: []string{"ReadWriteOncePod"}, StorageClassName: &class, VolumeMode: "Filesystem",
			Resources: kubernetes.VolumeResources{Requests: map[string]string{"storage": spec.Storage.Size}},
		},
	}
	// The claim's fingerprint covers its class and access mode, not its size:
	// resizing a volume is an operator action, never a reconciliation.
	view := o.Claim
	view.Metadata.Annotations = merge(o.Claim.Metadata.Annotations)
	view.Spec.Resources = kubernetes.VolumeResources{}
	seal(&view, &view.Metadata)
	o.Claim.Metadata.Annotations[AnnFingerprint] = view.Metadata.Annotations[AnnFingerprint]

	o.Secret = kubernetes.Secret{Metadata: meta(SecretName, ns, key, tenant, RoleSecret, nil), Type: "Opaque"}
	seal(&o.Secret, &o.Secret.Metadata)

	o.Service = kubernetes.Service{
		Metadata: meta(ServiceName, ns, key, tenant, RoleService, nil),
		Spec: kubernetes.ServiceSpec{
			Type: "ClusterIP", Selector: selectorLabels(key),
			Ports: []kubernetes.ServicePort{{Name: PortName, Protocol: "TCP", Port: spec.ListenPort, TargetPort: spec.ListenPort}},
		},
	}
	seal(&o.Service, &o.Service.Metadata)

	o.Policies = renderPolicies(tenant, spec, key, ns)
	o.Deployment = renderDeployment(tenant, spec, key, ns)
	return o, nil
}

// ---- network ----

func renderPolicies(tenant domain.TenantID, spec WorkerSpec, key, ns string) []kubernetes.NetworkPolicy {
	cp := spec.ControlPlane
	tcp := "TCP"
	controlPeer := kubernetes.NetworkPeer{
		NamespaceSelector: &kubernetes.LabelSelector{MatchLabels: map[string]string{labelNSName: cp.Namespace}},
		PodSelector:       &kubernetes.LabelSelector{MatchLabels: merge(cp.PodLabels)},
	}
	dnsPeer := kubernetes.NetworkPeer{
		NamespaceSelector: &kubernetes.LabelSelector{MatchLabels: map[string]string{labelNSName: "kube-system"}},
		PodSelector:       &kubernetes.LabelSelector{MatchLabels: map[string]string{"k8s-app": "kube-dns"}},
	}
	worker := kubernetes.LabelSelector{MatchLabels: selectorLabels(key)}
	pols := []kubernetes.NetworkPolicy{
		{
			// Default deny: every Pod of the namespace admits and makes no connection
			// until another policy says so.
			Metadata: meta(PolicyDenyAll, ns, key, tenant, RolePolicy, nil),
			Spec:     kubernetes.NetworkPolicySpec{PodSelector: kubernetes.LabelSelector{}, PolicyTypes: []string{"Ingress", "Egress"}},
		},
		{
			// Ingress: only the control plane's Pods, from the control plane's
			// namespace, on the worker's port.
			Metadata: meta(PolicyIngress, ns, key, tenant, RolePolicy, nil),
			Spec: kubernetes.NetworkPolicySpec{
				PodSelector: worker, PolicyTypes: []string{"Ingress"},
				Ingress: []map[string]any{{
					"from":  []kubernetes.NetworkPeer{controlPeer},
					"ports": []kubernetes.NetworkPort{{Protocol: tcp, Port: spec.ListenPort}},
				}},
			},
		},
		{
			// Egress: the control plane's Pods on their port and cluster name
			// resolution. Nothing else: no database, broker, API server or Internet.
			Metadata: meta(PolicyEgress, ns, key, tenant, RolePolicy, nil),
			Spec: kubernetes.NetworkPolicySpec{
				PodSelector: worker, PolicyTypes: []string{"Egress"},
				Egress: []kubernetes.NetworkEgress{
					{To: []kubernetes.NetworkPeer{controlPeer}, Ports: []kubernetes.NetworkPort{{Protocol: tcp, Port: cp.PodPort}}},
					{To: []kubernetes.NetworkPeer{dnsPeer}, Ports: []kubernetes.NetworkPort{{Protocol: "UDP", Port: int32(53)}, {Protocol: tcp, Port: int32(53)}}},
				},
			},
		},
	}
	for i := range pols {
		seal(&pols[i], &pols[i].Metadata)
	}
	return pols
}

// ---- worker ----

func resourceBounds(r Resources) *kubernetes.Resources {
	return &kubernetes.Resources{
		Requests: map[string]string{"cpu": r.CPURequest, "memory": r.MemoryRequest, "ephemeral-storage": r.EphemeralRequest},
		Limits:   map[string]string{"cpu": r.CPULimit, "memory": r.MemoryLimit, "ephemeral-storage": r.EphemeralLimit},
	}
}

func probe(spec WorkerSpec, period, initial, failures int32) *kubernetes.Probe {
	scheme := "HTTP"
	if spec.Scheme == "https" {
		scheme = "HTTPS"
	}
	return &kubernetes.Probe{
		HTTPGet:        &kubernetes.HTTPGetAction{Path: "/healthz", Port: spec.ListenPort, Scheme: scheme},
		TimeoutSeconds: 2, PeriodSeconds: period, SuccessThreshold: 1, FailureThreshold: failures, InitialDelaySeconds: initial,
	}
}

// workerEnv is the whole environment of the worker. It names where the data and
// the identity files are and who the control plane is; it holds no value that
// grants anything, and it never references a Secret.
func workerEnv(tenant domain.TenantID, spec WorkerSpec) []kubernetes.EnvVar {
	return []kubernetes.EnvVar{
		{Name: EnvTenantID, Value: string(tenant)},
		{Name: EnvDataRoot, Value: DataRoot},
		{Name: EnvListen, Value: ":" + strconv.Itoa(int(spec.ListenPort))},
		{Name: EnvControlURL, Value: spec.ControlPlane.URL},
		{Name: EnvTokenFile, Value: IdentityDir + "/" + KeyToken},
		{Name: EnvPublicKeys, Value: IdentityDir + "/" + KeyPublicKeys},
		{Name: EnvCAFile, Value: IdentityDir + "/" + KeyCA},
		{Name: EnvWorkerInstance, ValueFrom: &kubernetes.EnvVarSource{FieldRef: &kubernetes.FieldRef{APIVersion: "v1", FieldPath: "metadata.uid"}}},
		{Name: "HOME", Value: TmpDir},
		{Name: "TMPDIR", Value: TmpDir},
	}
}

func renderDeployment(tenant domain.TenantID, spec WorkerSpec, key, ns string) kubernetes.Deployment {
	podLabels := selectorLabels(key)
	container := kubernetes.WorkerContainer{
		Container: kubernetes.Container{
			Name: ContainerName, Image: spec.Image, ImagePullPolicy: spec.ImagePullPolicy,
			Command: append([]string(nil), spec.Command...), Args: append([]string(nil), spec.Args...),
			Env:       workerEnv(tenant, spec),
			Ports:     []kubernetes.ContainerPort{{Name: PortName, ContainerPort: spec.ListenPort, Protocol: "TCP"}},
			Resources: resourceBounds(spec.Resources),
			VolumeMounts: []kubernetes.VolumeMount{
				{Name: "data", MountPath: DataRoot},
				{Name: "tmp", MountPath: TmpDir},
				{Name: "identity", MountPath: IdentityDir, ReadOnly: true},
			},
			ReadinessProbe: probe(spec, 5, 0, 3),
			SecurityContext: &kubernetes.ContainerSecurity{
				Privileged: ptr(false), AllowPrivilegeEscalation: ptr(false), ReadOnlyRootFilesystem: ptr(true),
				RunAsNonRoot: ptr(true), RunAsUser: ptr(spec.RunAsUser), RunAsGroup: ptr(spec.RunAsGroup),
				Capabilities:   &kubernetes.Capabilities{Drop: []string{"ALL"}},
				SeccompProfile: &kubernetes.SeccompProfile{Type: "RuntimeDefault"},
			},
			TerminationMessagePolicy: "File",
		},
		LivenessProbe: probe(spec, 20, 10, 3),
	}
	podSpec := kubernetes.WorkerPodSpec{
		PodSpec: kubernetes.PodSpec{
			ServiceAccountName: ServiceAccountName, AutomountServiceAccountToken: ptr(false), EnableServiceLinks: ptr(false),
			RestartPolicy: "Always", DNSPolicy: "ClusterFirst", SchedulerName: "default-scheduler",
			TerminationGracePeriodSeconds: ptr(spec.TerminationGraceSeconds),
			SecurityContext: &kubernetes.PodSecurity{
				RunAsNonRoot: ptr(true), RunAsUser: ptr(spec.RunAsUser), RunAsGroup: ptr(spec.RunAsGroup), FSGroup: ptr(spec.RunAsGroup),
				SeccompProfile: &kubernetes.SeccompProfile{Type: "RuntimeDefault"},
			},
			Volumes: []kubernetes.Volume{
				{Name: "data", PersistentVolumeClaim: &kubernetes.PVCVolume{ClaimName: ClaimName}},
				{Name: "tmp", EmptyDir: &kubernetes.EmptyDir{SizeLimit: spec.Resources.TmpSize}},
				{Name: "identity", Secret: &kubernetes.SecretVolume{
					SecretName: SecretName, DefaultMode: ptr(int32(0o440)), Optional: ptr(false),
					Items: []kubernetes.KeyToPath{{Key: KeyToken, Path: KeyToken}, {Key: KeyPublicKeys, Path: KeyPublicKeys}, {Key: KeyCA, Path: KeyCA}},
				}},
			},
		},
		Containers: []kubernetes.WorkerContainer{container},
	}
	d := kubernetes.Deployment{
		Metadata: meta(DeploymentName, ns, key, tenant, RoleDeployment, map[string]string{LabelApp: ValueApp}),
		Spec: kubernetes.DeploymentSpec{
			// One replica and Recreate: the old Pod is gone before the new one starts,
			// and ReadWriteOncePod would refuse a second writer anyway.
			Replicas: ptr(int32(1)), Selector: kubernetes.LabelSelector{MatchLabels: selectorLabels(key)},
			Strategy:             kubernetes.DeploymentStrategy{Type: "Recreate"},
			RevisionHistoryLimit: ptr(int32(2)), ProgressDeadlineSeconds: ptr(int32(600)),
			Template: kubernetes.DeploymentTemplate{
				Metadata: kubernetes.ObjectMeta{Labels: podLabels},
				Spec:     podSpec,
			},
		},
	}
	seal(&d, &d.Metadata)
	return d
}

// SpecFingerprint is the digest of everything rendered for the tenant: the value
// Status reports as the desired state.
func (o Objects) SpecFingerprint() string {
	parts := []string{
		o.Namespace.Metadata.Annotations[AnnFingerprint], o.ServiceAccount.Metadata.Annotations[AnnFingerprint],
		o.Claim.Metadata.Annotations[AnnFingerprint], o.Secret.Metadata.Annotations[AnnFingerprint],
		o.Service.Metadata.Annotations[AnnFingerprint], o.Deployment.Metadata.Annotations[AnnFingerprint],
	}
	for _, p := range o.Policies {
		parts = append(parts, p.Metadata.Annotations[AnnFingerprint])
	}
	return fingerprintOf(parts)
}

// EndpointURL is the base URL of the worker's Service (for the worker route
// family), for a rendered tenant.
func EndpointURL(tenant domain.TenantID, spec WorkerSpec) string {
	spec = spec.withDefaults()
	return fmt.Sprintf("%s://%s.%s.svc:%d", spec.Scheme, ServiceName, NamespaceOf(tenant), spec.ListenPort)
}
