//go:build qualification

package production

// Resilience probe support for Q02 slice 3. Everything here runs behind the
// qualification build tag and is exercised by unit tests against fakes and
// local httptest servers; the live probes only run when the coordinator
// supplies the named stage inputs.
//
// The safety-critical invariant is replica restoration: every probe that
// scales a statefulset saves its original replica count first, and the
// t.Cleanup path restores that exact count with a read-back check even when the
// probe fails after the scale effect.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/optimiware/kodama/api"
)

const (
	// resilienceRestoreAttempts bounds one replica restoration before it is
	// reported as unrestored.
	resilienceRestoreAttempts = 3
	// resilienceRestoreTimeout bounds a cleanup restoration on every path.
	resilienceRestoreTimeout = 3 * time.Minute
	// resilienceDefaultInterval is the poll interval when a caller leaves it
	// unset.
	resilienceDefaultInterval = 250 * time.Millisecond
	// resilienceMaxReplicaCount is the largest replica count a probe may set.
	resilienceMaxReplicaCount = 10
	// resilienceMaxEventPages bounds one full committed-event read.
	resilienceMaxEventPages = 200
	// resilienceMaxLogPages bounds one retained-log read.
	resilienceMaxLogPages = 50
	// resilienceKodamaSelector is the controller Deployment's pod selector. It
	// excludes a one-off kodama-migrate pod: only the control-plane component is
	// a controller replica this probe may measure or delete.
	resilienceKodamaSelector = "app.kubernetes.io/name=kodama,app.kubernetes.io/component=control-plane"
	// resilienceNATSSelector is the stack NATS pod selector.
	resilienceNATSSelector = "app.kubernetes.io/name=nats"
	// resilienceNATSStatefulSet is the stage stack's NATS statefulset name.
	resilienceNATSStatefulSet = "nats"
	// resilienceNATSPod is the stage stack's one NATS pod.
	resilienceNATSPod = "nats-0"
)

// resilienceFakeOptions configures the fake kubectl used by the harness unit
// tests. Every successful scale and read is recorded in a private state
// directory; markers let a test fail one specific effect.
type resilienceFakeOptions struct {
	Replicas          int
	FailScaleToZero   bool
	FailRestoreOnce   bool
	FailRestoreAlways bool
	FailReplicasRead  bool
	LieOnScale        bool
}

// resilienceScaler is the minimal statefulset replica port the restoration
// guard needs.
type resilienceScaler interface {
	Replicas(ctx context.Context, namespace, resource, name string) (int, error)
	Scale(ctx context.Context, namespace, resource, name string, replicas int) error
}

// resilienceReplicaTarget is one saved statefulset/deployment replica count.
type resilienceReplicaTarget struct {
	Namespace string
	Resource  string
	Name      string
	Replicas  int
}

// resilienceReplicaGuard saves original replica counts and restores them with
// a bounded retry and a read-back verification. It is never satisfied by a
// scale request alone: the observed count must equal the saved count.
type resilienceReplicaGuard struct {
	scaler   resilienceScaler
	targets  []resilienceReplicaTarget
	restored []bool
}

// resilienceNewReplicaGuard builds a guard over one scaler.
func resilienceNewReplicaGuard(scaler resilienceScaler) *resilienceReplicaGuard {
	return &resilienceReplicaGuard{scaler: scaler}
}

// Save reads and records one original replica count. A target is saved at most
// once so a repeated save cannot overwrite the value a restore must reach.
func (guard *resilienceReplicaGuard) Save(ctx context.Context, namespace, resource, name string) (int, error) {
	if guard == nil || guard.scaler == nil {
		return 0, fmt.Errorf("resilience replica guard has no scaler")
	}
	if err := resilienceValidateReplicaTarget(namespace, resource, name); err != nil {
		return 0, err
	}
	for _, target := range guard.targets {
		if target.Namespace == namespace && target.Resource == resource && target.Name == name {
			return 0, fmt.Errorf("resilience replica target %s/%s/%s is already saved", namespace, resource, name)
		}
	}
	replicas, err := guard.scaler.Replicas(ctx, namespace, resource, name)
	if err != nil {
		return 0, fmt.Errorf("read the original replica count of %s/%s/%s: %w", namespace, resource, name, err)
	}
	guard.targets = append(guard.targets, resilienceReplicaTarget{
		Namespace: namespace,
		Resource:  resource,
		Name:      name,
		Replicas:  replicas,
	})
	guard.restored = append(guard.restored, false)
	return replicas, nil
}

// Scale changes one saved target. Refusing an unsaved target keeps every probe
// safe: a target that cannot be restored is never mutated.
func (guard *resilienceReplicaGuard) Scale(ctx context.Context, namespace, resource, name string, replicas int) error {
	if guard == nil || guard.scaler == nil {
		return fmt.Errorf("resilience replica guard has no scaler")
	}
	if err := resilienceValidateReplicaTarget(namespace, resource, name); err != nil {
		return err
	}
	if err := resilienceValidateReplicaCount(replicas); err != nil {
		return err
	}
	for index, target := range guard.targets {
		if target.Namespace != namespace || target.Resource != resource || target.Name != name {
			continue
		}
		// Any scale invalidates the restoration proof before the effect. A
		// scale that returns success is still not a restored, verified count;
		// only RestoreAll's read-back marks the target restored.
		guard.restored[index] = false
		return guard.scaler.Scale(ctx, namespace, resource, name, replicas)
	}
	return fmt.Errorf("resilience replica target %s/%s/%s was never saved", namespace, resource, name)
}

// RestoreAll restores every saved target, retrying each bounded number of
// times and verifying the observed count. Other targets are still attempted
// when one fails. The returned error names every target that stayed
// unrestored.
func (guard *resilienceReplicaGuard) RestoreAll(ctx context.Context) error {
	if guard == nil {
		return nil
	}
	if guard.scaler == nil {
		return fmt.Errorf("resilience replica guard has no scaler")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var problems []error
	for index, target := range guard.targets {
		if guard.restored[index] {
			continue
		}
		lastError := ""
		for attempt := 1; attempt <= resilienceRestoreAttempts; attempt++ {
			if err := ctx.Err(); err != nil {
				lastError = err.Error()
				break
			}
			if err := guard.scaler.Scale(ctx, target.Namespace, target.Resource, target.Name, target.Replicas); err != nil {
				lastError = err.Error()
				continue
			}
			observed, err := guard.scaler.Replicas(ctx, target.Namespace, target.Resource, target.Name)
			if err != nil {
				lastError = err.Error()
				continue
			}
			if observed == target.Replicas {
				guard.restored[index] = true
				lastError = ""
				break
			}
			lastError = fmt.Sprintf("observed %d replicas", observed)
		}
		if !guard.restored[index] {
			problems = append(problems, fmt.Errorf(
				"could not restore %s/%s/%s to %d replicas after %d attempts: %s",
				target.Namespace, target.Resource, target.Name, target.Replicas, resilienceRestoreAttempts, lastError,
			))
		}
	}
	return errors.Join(problems...)
}

// resilienceRestoreSafely is the one cleanup restoration path: restore with
// the bounded retry, require the read-back proof, and return the evidence
// restoration value. A failed or unverified restore is never reported as
// restored.
func resilienceRestoreSafely(ctx context.Context, guard *resilienceReplicaGuard) (string, error) {
	if err := guard.RestoreAll(ctx); err != nil {
		return "failed", err
	}
	if !guard.AllRestored() {
		return "unrestored", fmt.Errorf("resilience replica targets remain unrestored")
	}
	return guard.Restoration(), nil
}

// Broker outage bounds. The outage window is the total from scale-to-zero to
// the verified in-body restore; the restore budget is what must remain before
// the go test deadline so a killed test can still restore.
const (
	resilienceBrokerOutageWindow  = 4 * time.Minute
	resilienceBrokerRestoreBudget = 3 * time.Minute
)

// resilienceManualBrokerRestore is the exact operator command printed when the
// stage is found at zero replicas or a restore cannot be verified.
func resilienceManualBrokerRestore(kubeContext string) string {
	return fmt.Sprintf(
		"kubectl --context %s -n %s scale statefulset %s --replicas=<n>",
		kubeContext, ReleaseNamespace, resilienceNATSStatefulSet,
	)
}

// resilienceCheckBrokerEntry refuses to start an outage probe whose statefulset
// is already at zero replicas. This is a previous run that died without its
// cleanup, not a stage to scale from.
func resilienceCheckBrokerEntry(savedReplicas int, kubeContext string) error {
	if savedReplicas != 0 {
		return nil
	}
	return fmt.Errorf(
		"refusing to start the broker outage: statefulset/%s is already at 0 replicas; a previous run died before restoring it. Restore it manually with: %s",
		resilienceNATSStatefulSet, resilienceManualBrokerRestore(kubeContext),
	)
}

// resiliencePlanBrokerOutage returns the outage window or refuses before any
// scale when the remaining go test budget cannot hold both the window and the
// restore budget.
func resiliencePlanBrokerOutage(now, deadline time.Time, hasDeadline bool) (time.Duration, error) {
	if !hasDeadline || deadline.IsZero() {
		return 0, fmt.Errorf(
			"refusing to scale statefulset/%s: go test reported no deadline, so no verified in-body restore can be guaranteed; %s",
			resilienceNATSStatefulSet, resilienceManualBrokerRestore("<context>"),
		)
	}
	remaining := deadline.Sub(now)
	required := resilienceBrokerOutageWindow + resilienceBrokerRestoreBudget
	if remaining < required {
		return 0, fmt.Errorf(
			"refusing to scale statefulset/%s to 0: %s remain before the go test deadline, but the outage window (%s) plus the restore budget (%s) need %s. Increase -timeout and re-run. If a previous run left it at zero: %s",
			resilienceNATSStatefulSet, remaining, resilienceBrokerOutageWindow, resilienceBrokerRestoreBudget, required,
			resilienceManualBrokerRestore("<context>"),
		)
	}
	return resilienceBrokerOutageWindow, nil
}

// resilienceCloseOutage restores the saved replica count with a fresh bounded
// context. The outage context may already be cancelled or expired (window,
// SIGINT or SIGTERM); restoration must not inherit that cancellation.
func resilienceCloseOutage(outageCtx context.Context, guard *resilienceReplicaGuard, budget time.Duration) (string, error) {
	_ = outageCtx
	if budget <= 0 {
		budget = resilienceRestoreTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	return resilienceRestoreSafely(ctx, guard)
}

// AllRestored reports whether every saved target reached its saved count.
func (guard *resilienceReplicaGuard) AllRestored() bool {
	if guard == nil {
		return true
	}
	for _, restored := range guard.restored {
		if !restored {
			return false
		}
	}
	return true
}

// Restoration renders the evidence restoration value for the guard state.
func (guard *resilienceReplicaGuard) Restoration() string {
	if guard == nil || len(guard.targets) == 0 {
		return "not_applicable"
	}
	if guard.AllRestored() {
		return "restored"
	}
	return "unrestored"
}

// Targets returns a copy of the saved targets.
func (guard *resilienceReplicaGuard) Targets() []resilienceReplicaTarget {
	if guard == nil {
		return nil
	}
	return append([]resilienceReplicaTarget(nil), guard.targets...)
}

// resilienceValidateReplicaTarget admits only the namespaced workload kinds a
// fault-injection probe may scale.
func resilienceValidateReplicaTarget(namespace, resource, name string) error {
	if namespace == "" || len(namespace) > 63 || !dnsLabelPattern.MatchString(namespace) {
		return fmt.Errorf("replica target namespace %q is not a kubernetes namespace", namespace)
	}
	if resource != "statefulsets" && resource != "deployments" {
		return fmt.Errorf("replica target resource %q is not a scalable workload kind", resource)
	}
	if !dnsLabelPattern.MatchString(name) {
		return fmt.Errorf("replica target name %q is not a kubernetes workload name", name)
	}
	return nil
}

// resilienceValidateReplicaCount bounds a scale value before any process runs.
func resilienceValidateReplicaCount(replicas int) error {
	if replicas < 0 || replicas > resilienceMaxReplicaCount {
		return fmt.Errorf("replica count %d is outside 0..%d", replicas, resilienceMaxReplicaCount)
	}
	return nil
}

// resilienceCluster runs the guarded kubectl commands the live probes need.
// Every method validates its own scope first; the hardened guard remains the
// final authority before a process starts.
type resilienceCluster struct {
	client *Kubectl
}

// resilienceNewCluster wraps one hardened kubectl helper.
func resilienceNewCluster(client *Kubectl) resilienceCluster {
	return resilienceCluster{client: client}
}

func (cluster resilienceCluster) kubectl() (*Kubectl, error) {
	if cluster.client == nil || cluster.client.Guard == nil {
		return nil, fmt.Errorf("resilience cluster has no guarded kubectl helper")
	}
	return cluster.client, nil
}

// Replicas reads one workload's desired replica count.
func (cluster resilienceCluster) Replicas(ctx context.Context, namespace, resource, name string) (int, error) {
	if err := resilienceValidateReplicaTarget(namespace, resource, name); err != nil {
		return 0, err
	}
	kubectl, err := cluster.kubectl()
	if err != nil {
		return 0, err
	}
	data, err := kubectl.Run(ctx, namespace, "get", resource, name, "-o", "jsonpath={.spec.replicas}")
	if err != nil {
		return 0, err
	}
	return resilienceParseReplicas(data)
}

// Scale sets one workload's desired replica count.
func (cluster resilienceCluster) Scale(ctx context.Context, namespace, resource, name string, replicas int) error {
	if err := resilienceValidateReplicaTarget(namespace, resource, name); err != nil {
		return err
	}
	if err := resilienceValidateReplicaCount(replicas); err != nil {
		return err
	}
	kubectl, err := cluster.kubectl()
	if err != nil {
		return err
	}
	_, err = kubectl.Run(ctx, namespace, "scale", resource, name, "--replicas="+strconv.Itoa(replicas))
	return err
}

// DeletePods deletes every pod matching one selector.
func (cluster resilienceCluster) DeletePods(ctx context.Context, namespace, selector string) error {
	if err := resilienceValidateSelector(selector); err != nil {
		return err
	}
	kubectl, err := cluster.kubectl()
	if err != nil {
		return err
	}
	_, err = kubectl.Run(ctx, namespace, "delete", "pods", "-l", selector)
	return err
}

// DeletePod deletes one named pod.
func (cluster resilienceCluster) DeletePod(ctx context.Context, namespace, name string) error {
	if !dnsLabelPattern.MatchString(name) {
		return fmt.Errorf("pod name %q is not a kubernetes pod name", name)
	}
	kubectl, err := cluster.kubectl()
	if err != nil {
		return err
	}
	_, err = kubectl.Run(ctx, namespace, "delete", "pods", name)
	return err
}

// DeletePodForce deletes one named pod without waiting for a graceful stop.
// It is the qualification emulation of a node loss: the process gets no TERM
// and the runtime must report the lost workload instead of replaying its
// command on a fresh pod.
func (cluster resilienceCluster) DeletePodForce(ctx context.Context, namespace, name string) error {
	if !dnsLabelPattern.MatchString(name) {
		return fmt.Errorf("pod name %q is not a kubernetes pod name", name)
	}
	kubectl, err := cluster.kubectl()
	if err != nil {
		return err
	}
	_, err = kubectl.Run(ctx, namespace, "delete", "pods", name, "--grace-period=0", "--force=true")
	return err
}

// PodClaimName reads the one persistent volume claim a workload pod mounts.
// The workspace probe needs the durable claim identity while the execution is
// still running, before the public cleanup record exists.
func (cluster resilienceCluster) PodClaimName(ctx context.Context, namespace, pod string) (string, error) {
	if !dnsLabelPattern.MatchString(pod) {
		return "", fmt.Errorf("pod name %q is not a kubernetes pod name", pod)
	}
	kubectl, err := cluster.kubectl()
	if err != nil {
		return "", err
	}
	data, err := kubectl.Run(ctx, namespace, "get", "pods", pod, "-o",
		"jsonpath={.spec.volumes[*].persistentVolumeClaim.claimName}")
	if err != nil {
		return "", err
	}
	fields := strings.Fields(string(data))
	if len(fields) != 1 || !positionalExtraPattern.MatchString(fields[0]) {
		return "", fmt.Errorf("workload pod %q mounts %d persistent volume claims; want one bounded claim name", pod, len(fields))
	}
	return fields[0], nil
}

// RequestClaimDeletion asks Kubernetes to delete one owned workspace claim.
// pvc-protection can keep a claim terminating while its pod lives, so a
// non-zero kubectl status is not trusted: the caller verifies the deletion
// timestamp or absence before treating the loss as injected.
func (cluster resilienceCluster) RequestClaimDeletion(ctx context.Context, namespace, name string) error {
	if !positionalExtraPattern.MatchString(name) {
		return fmt.Errorf("claim name %q is not a kubernetes claim name", name)
	}
	kubectl, err := cluster.kubectl()
	if err != nil {
		return err
	}
	_, err = kubectl.Run(ctx, namespace, "delete", "persistentvolumeclaims", name, "--timeout=5s")
	return err
}

// ClaimGoneOrTerminating reports whether a claim is absent or carries a
// deletion timestamp. Either state is a lost workspace; only an unchanged
// claim means the injected loss did not take, and any kubectl error is
// propagated rather than mistaken for absence.
func (cluster resilienceCluster) ClaimGoneOrTerminating(ctx context.Context, namespace, name string) (bool, error) {
	if !positionalExtraPattern.MatchString(name) {
		return false, fmt.Errorf("claim name %q is not a kubernetes claim name", name)
	}
	kubectl, err := cluster.kubectl()
	if err != nil {
		return false, err
	}
	data, err := kubectl.Run(ctx, namespace, "get", "persistentvolumeclaims", "--field-selector=metadata.name="+name, "-o", "json")
	if err != nil {
		return false, err
	}
	var wire struct {
		Items []struct {
			Metadata struct {
				Name              string `json:"name"`
				DeletionTimestamp any    `json:"deletionTimestamp"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return false, fmt.Errorf("workspace claim list JSON is unreadable")
	}
	if len(wire.Items) == 0 {
		return true, nil
	}
	for _, item := range wire.Items {
		if item.Metadata.Name != name {
			return false, fmt.Errorf("workspace claim list returned unexpected claim %q for %q", item.Metadata.Name, name)
		}
		if item.Metadata.DeletionTimestamp == nil {
			return false, nil
		}
	}
	return true, nil
}

// WaitPodReady waits for one pod's Ready condition within the bound.
func (cluster resilienceCluster) WaitPodReady(ctx context.Context, namespace, name string, timeout time.Duration) error {
	if !dnsLabelPattern.MatchString(name) {
		return fmt.Errorf("pod name %q is not a kubernetes pod name", name)
	}
	if timeout < time.Second {
		return fmt.Errorf("pod readiness timeout %s is below one second", timeout)
	}
	kubectl, err := cluster.kubectl()
	if err != nil {
		return err
	}
	seconds := int(timeout / time.Second)
	_, err = kubectl.Run(ctx, namespace, "wait", "pods", name,
		"--for=condition=Ready", fmt.Sprintf("--timeout=%ds", seconds))
	return err
}

// PodNames lists the pods matching one selector.
func (cluster resilienceCluster) PodNames(ctx context.Context, namespace, selector string) ([]string, error) {
	if err := resilienceValidateSelector(selector); err != nil {
		return nil, err
	}
	kubectl, err := cluster.kubectl()
	if err != nil {
		return nil, err
	}
	data, err := kubectl.Run(ctx, namespace, "get", "pods", "-l", selector, "-o", "jsonpath={.items[*].metadata.name}")
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(string(data))
	names := make([]string, 0, len(fields))
	for _, field := range fields {
		if !dnsLabelPattern.MatchString(field) {
			return nil, fmt.Errorf("kubectl returned an unsafe pod name")
		}
		names = append(names, field)
	}
	sort.Strings(names)
	return names, nil
}

// PodUID reads one pod's object UID, which distinguishes a recreated pod that
// reuses the same name.
func (cluster resilienceCluster) PodUID(ctx context.Context, namespace, name string) (string, error) {
	if !dnsLabelPattern.MatchString(name) {
		return "", fmt.Errorf("pod name %q is not a kubernetes pod name", name)
	}
	kubectl, err := cluster.kubectl()
	if err != nil {
		return "", err
	}
	data, err := kubectl.Run(ctx, namespace, "get", "pods", name, "-o", "jsonpath={.metadata.uid}")
	if err != nil {
		return "", err
	}
	uid := strings.TrimSpace(string(data))
	if !positionalExtraPattern.MatchString(uid) {
		return "", fmt.Errorf("kubectl returned an unsafe pod UID")
	}
	return uid, nil
}

// ResourceNames lists names of one allowlisted owned resource matching a
// selector. It exists for cleanup attribution and never reads Secrets or a
// cluster-scoped resource.
func (cluster resilienceCluster) ResourceNames(ctx context.Context, namespace, resource, selector string) ([]string, error) {
	switch resource {
	case "jobs", "pods", "persistentvolumeclaims":
	default:
		return nil, fmt.Errorf("resource %q is not an attributable workload resource", resource)
	}
	if err := resilienceValidateSelector(selector); err != nil {
		return nil, err
	}
	kubectl, err := cluster.kubectl()
	if err != nil {
		return nil, err
	}
	data, err := kubectl.Run(ctx, namespace, "get", resource, "-l", selector, "-o", "name")
	if err != nil {
		return nil, err
	}
	return resilienceParseNameList(data)
}

// ClaimRetained reads the durable retention marker of one workspace claim.
// Cleanup completes only after Release clears this marker and deletes the
// claim, so a retained claim after complete cleanup is a documented retention
// exception rather than an orphan.
func (cluster resilienceCluster) ClaimRetained(ctx context.Context, namespace, name string) (bool, error) {
	if !positionalExtraPattern.MatchString(name) {
		return false, fmt.Errorf("claim name %q is not a kubernetes claim name", name)
	}
	kubectl, err := cluster.kubectl()
	if err != nil {
		return false, err
	}
	data, err := kubectl.Run(ctx, namespace, "get", "persistentvolumeclaims", name, "-o", "json")
	if err != nil {
		return false, err
	}
	return resilienceClaimRetained(data)
}

// resilienceClaimRetained parses the retention label from one bounded claim
// JSON document.
func resilienceClaimRetained(data []byte) (bool, error) {
	var wire struct {
		Metadata struct {
			Labels      map[string]string `json:"labels"`
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return false, fmt.Errorf("workspace claim JSON is unreadable")
	}
	const marker = "kodama.dev/retained"
	return wire.Metadata.Labels[marker] == "true" || wire.Metadata.Annotations[marker] == "true", nil
}

// resilienceContainerState is one fixed container-state observation.
type resilienceContainerState struct {
	State      string
	Reason     string
	ExitCode   int
	LastState  string
	LastReason string
	LastExit   int
}

// resiliencePodFacts is the fixed, log-free fact set of one workload pod.
type resiliencePodFacts struct {
	Name              string
	UID               string
	Phase             string
	DeletionTimestamp string
	Containers        []resilienceContainerState
}

// PodFacts reads the fixed pod facts for one selector. It never reads logs or
// object bodies into evidence, only identity, phase, deletion timestamp and
// container terminated state.
func (cluster resilienceCluster) PodFacts(ctx context.Context, namespace, selector string) ([]resiliencePodFacts, error) {
	if err := resilienceValidateSelector(selector); err != nil {
		return nil, err
	}
	kubectl, err := cluster.kubectl()
	if err != nil {
		return nil, err
	}
	data, err := kubectl.Run(ctx, namespace, "get", "pods", "-l", selector, "-o", "json")
	if err != nil {
		return nil, err
	}
	return resilienceParsePodFacts(data)
}

// resilienceParsePodFacts parses the fixed pod/container facts from one pod
// list. An unreadable or malformed document is an error, never a guessed fact.
func resilienceParsePodFacts(data []byte) ([]resiliencePodFacts, error) {
	type stateWire struct {
		Waiting    *struct{ Reason string } `json:"waiting"`
		Running    *struct{}                `json:"running"`
		Terminated *struct {
			Reason   string
			ExitCode int
		} `json:"terminated"`
	}
	var wire struct {
		Items []struct {
			Metadata struct {
				Name              string `json:"name"`
				UID               string `json:"uid"`
				DeletionTimestamp string `json:"deletionTimestamp"`
			} `json:"metadata"`
			Status struct {
				Phase             string `json:"phase"`
				ContainerStatuses []struct {
					Name      string    `json:"name"`
					State     stateWire `json:"state"`
					LastState stateWire `json:"lastState"`
				} `json:"containerStatuses"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, fmt.Errorf("pod list JSON is unreadable")
	}
	stateOf := func(state stateWire) (string, string, int) {
		switch {
		case state.Terminated != nil:
			return "terminated", state.Terminated.Reason, state.Terminated.ExitCode
		case state.Waiting != nil:
			return "waiting", state.Waiting.Reason, 0
		case state.Running != nil:
			return "running", "", 0
		default:
			return "none", "", 0
		}
	}
	facts := make([]resiliencePodFacts, 0, len(wire.Items))
	for _, item := range wire.Items {
		if !dnsLabelPattern.MatchString(item.Metadata.Name) || !positionalExtraPattern.MatchString(item.Metadata.UID) {
			return nil, fmt.Errorf("pod list JSON carries an unsafe identity")
		}
		pod := resiliencePodFacts{
			Name:              item.Metadata.Name,
			UID:               item.Metadata.UID,
			Phase:             item.Status.Phase,
			DeletionTimestamp: item.Metadata.DeletionTimestamp,
		}
		for _, container := range item.Status.ContainerStatuses {
			if !positionalExtraPattern.MatchString(container.Name) {
				return nil, fmt.Errorf("pod list JSON carries an unsafe container name")
			}
			state, reason, code := stateOf(container.State)
			lastState, lastReason, lastCode := stateOf(container.LastState)
			pod.Containers = append(pod.Containers, resilienceContainerState{
				State: state, Reason: reason, ExitCode: code,
				LastState: lastState, LastReason: lastReason, LastExit: lastCode,
			})
		}
		facts = append(facts, pod)
	}
	return facts, nil
}

// resilienceFormatPodFacts renders fixed pod facts for a failure message.
func resilienceFormatPodFacts(label string, facts []resiliencePodFacts) string {
	if len(facts) == 0 {
		return label + "=no-pods"
	}
	parts := make([]string, 0, len(facts))
	for _, pod := range facts {
		containers := make([]string, 0, len(pod.Containers))
		for index, container := range pod.Containers {
			containers = append(containers, fmt.Sprintf("c%d state=%s reason=%s exit=%d last=%s/%s/%d",
				index, container.State, container.Reason, container.ExitCode,
				container.LastState, container.LastReason, container.LastExit))
		}
		parts = append(parts, fmt.Sprintf("name=%s uid=%s phase=%s deletion=%s [%s]",
			pod.Name, pod.UID, pod.Phase, pod.DeletionTimestamp, strings.Join(containers, ", ")))
	}
	return label + "=" + strings.Join(parts, " | ")
}

// resilienceRecordPodFacts records pod identity/state in bounded evidence names
// with ids only in the allowlisted fields.
func resilienceRecordPodFacts(probe *resilienceProbe, executionID, label string, facts []resiliencePodFacts, observedAt time.Time) {
	if probe == nil {
		return
	}
	if len(facts) == 0 {
		probe.Record(resilienceEvidenceName("resilience-controller-workload", label, "none"), observedAt, observedAt, map[string]any{
			"execution_id": executionID,
		})
		return
	}
	for _, pod := range facts {
		deletion := "present"
		if pod.DeletionTimestamp != "" {
			deletion = "deleting"
		}
		probe.Record(resilienceEvidenceName("resilience-controller-workload", label, strings.ToLower(pod.Phase), deletion), observedAt, observedAt, map[string]any{
			"execution_id": executionID,
			"resource_id":  pod.UID,
		})
		for index, container := range pod.Containers {
			probe.Record(resilienceEvidenceName("resilience-controller-workload", label, "container", strconv.Itoa(index), container.State, container.Reason, strconv.Itoa(container.ExitCode)), observedAt, observedAt, map[string]any{
				"execution_id": executionID,
				"resource_id":  pod.UID,
			})
		}
	}
}

// resilienceValidateSelector refuses an empty or malformed selector before a
// kubectl process starts.
func resilienceValidateSelector(selector string) error {
	if selector == "" {
		return fmt.Errorf("kubernetes selector is empty")
	}
	if !selectorPattern.MatchString(selector) {
		return fmt.Errorf("kubernetes selector is not a bounded selector expression")
	}
	return nil
}

// resilienceParseReplicas parses one desired replica count.
func resilienceParseReplicas(data []byte) (int, error) {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || strings.ContainsAny(trimmed, " \t\r\n") {
		return 0, fmt.Errorf("kubectl returned no bounded replica count")
	}
	value, err := strconv.Atoi(trimmed)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("kubectl returned an invalid replica count")
	}
	return value, nil
}

// resilienceParseNameList parses kubectl -o name output, with or without a
// resource-kind prefix, into sorted unique-ish names.
func resilienceParseNameList(data []byte) ([]string, error) {
	fields := strings.Fields(string(data))
	names := make([]string, 0, len(fields))
	for _, field := range fields {
		parts := strings.Split(field, "/")
		var name string
		switch len(parts) {
		case 1:
			name = parts[0]
		case 2:
			name = parts[1]
		default:
			return nil, fmt.Errorf("kubectl returned an unsafe resource name")
		}
		if !positionalExtraPattern.MatchString(name) {
			return nil, fmt.Errorf("kubectl returned an unsafe resource name")
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// resilienceCountMarker counts one exact workload side-effect marker in the
// retained output. It refuses an empty marker so a probe can never pass by
// counting nothing.
func resilienceCountMarker(data []byte, marker string) (int, error) {
	if marker == "" {
		return 0, fmt.Errorf("resilience marker is empty")
	}
	return strings.Count(string(data), marker), nil
}

// resilienceElapsed validates and returns one measured interval.
func resilienceElapsed(started, finished time.Time) (time.Duration, error) {
	if finished.Before(started) {
		return 0, fmt.Errorf("finished_at %s is before started_at %s", finished, started)
	}
	return finished.Sub(started), nil
}

// resilienceWaiter is a deterministic polling helper. Tests inject Now and
// Sleep; live probes use the defaults.
type resilienceWaiter struct {
	Interval time.Duration
	Timeout  time.Duration
	Now      func() time.Time
	Sleep    func(time.Duration)
}

// Wait polls one observation until it succeeds, fails itself or the bounded
// timeout elapses, and returns the measured elapsed time.
func (waiter resilienceWaiter) Wait(ctx context.Context, poll func(context.Context) (bool, error)) (time.Duration, error) {
	if poll == nil {
		return 0, fmt.Errorf("resilience wait has no observation")
	}
	if waiter.Timeout <= 0 {
		return 0, fmt.Errorf("resilience wait timeout must be positive")
	}
	interval := waiter.Interval
	if interval <= 0 {
		interval = resilienceDefaultInterval
	}
	now := waiter.Now
	if now == nil {
		now = time.Now
	}
	sleep := waiter.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	if ctx == nil {
		ctx = context.Background()
	}
	started := now()
	deadline := started.Add(waiter.Timeout)
	for {
		if err := ctx.Err(); err != nil {
			return now().Sub(started), err
		}
		ok, err := poll(ctx)
		if err != nil {
			return now().Sub(started), err
		}
		if ok {
			return now().Sub(started), nil
		}
		if !now().Before(deadline) {
			return now().Sub(started), fmt.Errorf("resilience condition was not reached within %s", waiter.Timeout)
		}
		sleep(interval)
	}
}

// resilienceCheckStopBarrier is the handoff invariant as a pure predicate: NEW
// WORK (an activated/running replacement action) may not start while the
// predecessor's stop is unconfirmed. A replacement execution may be "running"
// during preparation; only an action with activated_at set or state running is
// new work.
func resilienceCheckStopBarrier(stopState string, actions []api.ExecutionAction) error {
	if stopState != "unconfirmed" {
		return nil
	}
	for _, action := range actions {
		if action.ActivatedAt != nil || action.State == "running" {
			return fmt.Errorf("replacement action %q is state %q with activated_at set while the predecessor stop is unconfirmed", action.Name, action.State)
		}
	}
	return nil
}

// resilienceFormatActions renders fixed action facts for a failure message.
func resilienceFormatActions(actions []api.ExecutionAction) string {
	if len(actions) == 0 {
		return "action=<none>"
	}
	parts := make([]string, 0, len(actions))
	for _, action := range actions {
		activated := "none"
		if action.ActivatedAt != nil {
			activated = action.ActivatedAt.UTC().Format(time.RFC3339Nano)
		}
		deadline := "none"
		if action.Deadline != nil {
			deadline = action.Deadline.UTC().Format(time.RFC3339Nano)
		}
		parts = append(parts, fmt.Sprintf("action=%s state=%s activated_at=%s deadline=%s", action.Name, action.State, activated, deadline))
	}
	return strings.Join(parts, "; ")
}

// resilienceEvidenceName builds one bounded lower-case evidence file name from
// fixed observation words.
func resilienceEvidenceName(parts ...string) string {
	name := strings.ToLower(strings.Join(parts, "-"))
	var builder strings.Builder
	for _, character := range name {
		switch {
		case character >= 'a' && character <= 'z':
		case character >= '0' && character <= '9':
		case character == '-', character == '.', character == '_':
		default:
			character = '-'
		}
		if builder.Len() == 0 && (character == '-' || character == '.' || character == '_') {
			continue
		}
		builder.WriteRune(character)
		if builder.Len() == 80 {
			break
		}
	}
	return builder.String()
}

// resilienceRecordBarrierObservation records fixed barrier observations in the
// evidence artifact names: replacement execution state, every action state with
// activated_at as its start time, and the predecessor stop state.
func resilienceRecordBarrierObservation(probe *resilienceProbe, replacement, predecessor api.ExecutionInspectResult, stopState string, observedAt time.Time) {
	if probe == nil {
		return
	}
	probe.Record(resilienceEvidenceName("resilience-handoff-barrier-replacement", replacement.State), observedAt, observedAt, map[string]any{
		"execution_id": replacement.ExecutionID,
	})
	for index, action := range replacement.Actions {
		started := observedAt
		if action.ActivatedAt != nil && !action.ActivatedAt.After(observedAt) {
			started = action.ActivatedAt.UTC()
		}
		probe.Record(resilienceEvidenceName("resilience-handoff-barrier-action", strconv.Itoa(index), action.State), started, observedAt, map[string]any{
			"execution_id": replacement.ExecutionID,
			"resource_id":  action.ActionID,
		})
	}
	probe.Record(resilienceEvidenceName("resilience-handoff-barrier-predecessor-stop", stopState), observedAt, observedAt, map[string]any{
		"execution_id": predecessor.ExecutionID,
	})
}

// resilienceWaitTaskSettled polls task.read until the task reaches the wanted
// state with the wanted current execution. The lifecycle suite polls for the
// same reason: the task projection is committed with the execution settlement,
// not necessarily before the first read.
func resilienceWaitTaskSettled(ctx context.Context, stage *resilienceStage, taskID, currentExecution, state string, timeout time.Duration) (api.TaskReadResult, error) {
	var settled api.TaskReadResult
	_, err := (resilienceWaiter{Timeout: timeout}).Wait(ctx, func(ctx context.Context) (bool, error) {
		task, err := resilienceReadTask(ctx, stage, taskID)
		if err != nil {
			return false, err
		}
		settled = task
		return task.State == state && task.CurrentExecution == currentExecution, nil
	})
	return settled, err
}

// resilienceEventDelta is the committed-event comparison across a broker
// failure. Lost ids were committed before and absent after; duplicated ids
// appear more than once after; added ids are new after the failure.
type resilienceCommittedEventDelta struct {
	Before     int
	After      int
	Lost       []string
	Duplicated []string
	Added      []string
}

// resilienceEventDelta compares two full committed-event reads by identity.
func resilienceEventDelta(before, after []string) resilienceCommittedEventDelta {
	delta := resilienceCommittedEventDelta{Before: len(before), After: len(after)}
	beforeCount := make(map[string]int, len(before))
	for _, id := range before {
		beforeCount[id]++
	}
	afterCount := make(map[string]int, len(after))
	for _, id := range after {
		afterCount[id]++
	}
	for id := range beforeCount {
		if afterCount[id] == 0 {
			delta.Lost = append(delta.Lost, id)
		}
	}
	for id, count := range afterCount {
		if count > 1 {
			delta.Duplicated = append(delta.Duplicated, id)
			continue
		}
		if beforeCount[id] == 0 {
			delta.Added = append(delta.Added, id)
		}
	}
	sort.Strings(delta.Lost)
	sort.Strings(delta.Duplicated)
	sort.Strings(delta.Added)
	return delta
}

// resiliencePaginateEvents follows the opaque committed-event cursor to the
// last page, refusing a cursor that does not advance and a page bound overrun.
func resiliencePaginateEvents(read func(cursor string) (api.EventReadResult, error), maximumPages int) ([]api.Event, error) {
	if read == nil {
		return nil, fmt.Errorf("event read has no page source")
	}
	if maximumPages <= 0 {
		return nil, fmt.Errorf("event read page bound must be positive")
	}
	cursor := ""
	var events []api.Event
	for range maximumPages {
		result, err := read(cursor)
		if err != nil {
			return nil, err
		}
		events = append(events, result.Events...)
		if !result.HasMore {
			return events, nil
		}
		if result.Next == "" || result.Next == cursor {
			return nil, fmt.Errorf("event read cursor did not advance")
		}
		cursor = result.Next
	}
	return nil, fmt.Errorf("event read exceeded %d pages", maximumPages)
}

// resiliencePaginateLogs follows retained-log cursors to the last page.
func resiliencePaginateLogs(read func(cursor string) (api.ExecutionLogsReadResult, error), maximumPages int) ([]api.LogChunk, error) {
	if read == nil {
		return nil, fmt.Errorf("log read has no page source")
	}
	if maximumPages <= 0 {
		return nil, fmt.Errorf("log read page bound must be positive")
	}
	cursor := ""
	var chunks []api.LogChunk
	for range maximumPages {
		result, err := read(cursor)
		if err != nil {
			return nil, err
		}
		chunks = append(chunks, result.Chunks...)
		if result.NextCursor == "" {
			return chunks, nil
		}
		if result.NextCursor == cursor {
			return nil, fmt.Errorf("log read cursor did not advance")
		}
		cursor = result.NextCursor
	}
	return nil, fmt.Errorf("log read exceeded %d pages", maximumPages)
}

// resilienceEvidenceInput is one evidence summary the probe builder turns into
// an allowlisted record.
type resilienceEvidenceInput struct {
	Probe       string
	Outcome     string
	Restoration string
	Started     time.Time
	Finished    time.Time
	Fields      map[string]any
}

// resilienceEvidenceFields builds one allowlisted evidence record. It accepts
// only the writer's fields, refuses reserved key overrides and never carries
// logs, bodies or credential material.
func resilienceEvidenceFields(input resilienceEvidenceInput) (map[string]any, error) {
	if !evidenceProbePattern.MatchString(input.Probe) {
		return nil, fmt.Errorf("evidence probe %q is not a bounded probe name", input.Probe)
	}
	if !evidenceOutcomes[input.Outcome] {
		return nil, fmt.Errorf("evidence outcome %q is not a known outcome", input.Outcome)
	}
	if !evidenceRestorations[input.Restoration] {
		return nil, fmt.Errorf("evidence restoration %q is not a known restoration result", input.Restoration)
	}
	if input.Finished.Before(input.Started) {
		return nil, fmt.Errorf("evidence finished_at is before started_at")
	}
	started := input.Started.UTC()
	finished := input.Finished.UTC()
	fields := map[string]any{
		"probe":       input.Probe,
		"outcome":     input.Outcome,
		"restoration": input.Restoration,
		"started_at":  started,
		"finished_at": finished,
		"duration_ms": finished.Sub(started).Milliseconds(),
	}
	for key, value := range input.Fields {
		if !evidenceFields[key] {
			return nil, fmt.Errorf("evidence field %q is not allowlisted", key)
		}
		switch key {
		case "probe", "outcome", "restoration", "started_at", "finished_at", "duration_ms", "recorded_at":
			return nil, fmt.Errorf("evidence field %q is reserved", key)
		}
		fields[key] = value
	}
	return fields, nil
}

// resilienceCredentialMinimumTTL is the frozen minimum execution-credential
// lifetime. Source: internal/shell/token/types.go, DefaultBounds.MinTTL
// (S05). A pipeline action timeout below it fails preparation with
// "credential lifetime is shorter than the minimum".
const resilienceCredentialMinimumTTL = time.Minute

// resilienceActionTimeout is every probe pipeline's action timeout. It must
// stay above the credential minimum with margin; the timeout test pins every
// generated document against the frozen minimum.
const resilienceActionTimeout = "2m"

// resilienceProbePipelineDocuments returns every probe's generated pipeline
// document for the timeout and admissibility tests.
func resilienceProbePipelineDocuments(image string) map[string][]byte {
	return map[string][]byte{
		"controller":    resilienceControllerPipeline(image, "once-marker", "workspace-marker"),
		"nats":          resilienceNATSPipeline(image, "nats-marker"),
		"broker":        resilienceBrokerPipeline(image, "broker-marker"),
		"handoff":       resilienceHandoffPipeline(image, "answer", "first", "replacement", "own-timer"),
		"cleanup":       resilienceCleanupPipeline(image, "cleanup-marker"),
		"cleanup-crash": resilienceCleanupCrashPipeline(image, "cleanup-crash-marker"),
		"workspace":     resilienceWorkspaceLossPipeline(image, "workspace-once", "workspace-after"),
		"pod-loss":      resilienceWorkloadPodLossPipeline(image, "pod-before", "pod-after"),
		"git":           resilienceGitOutagePipeline(image, "git-marker"),
		"recovery":      resilienceRecoveryPipeline(image, "recovery-marker"),
	}
}

// resiliencePipelineTimeouts extracts every action timeout from one generated
// pipeline document.
func resiliencePipelineTimeouts(document []byte) ([]string, error) {
	var wire struct {
		Actions []struct {
			Timeout string `yaml:"timeout"`
		} `yaml:"actions"`
	}
	if err := yaml.Unmarshal(document, &wire); err != nil {
		return nil, fmt.Errorf("pipeline document is not YAML: %w", err)
	}
	timeouts := make([]string, 0, len(wire.Actions))
	for _, action := range wire.Actions {
		timeouts = append(timeouts, action.Timeout)
	}
	return timeouts, nil
}

// resilienceControllerPipeline writes a marker, proves /workspace is writable
// with a fixed diagnostic if it is not, sleeps, then proves the same workspace
// is still mounted. Required image: any image with a POSIX sh, printf, grep and
// sleep, running as root or with a writable /workspace.
func resilienceControllerPipeline(image, onceMarker, workspaceMarker string) []byte {
	return resiliencePipelineYAML(image, "hold", resilienceActionTimeout, []string{
		"echo " + onceMarker + "\n" +
			"if [ ! -w /workspace ]; then\n" +
			"  echo 'q02: /workspace is not writable by the workload user'\n" +
			"  exit 1\n" +
			"fi\n" +
			"if ! printf preserved > /workspace/resilience-workspace 2>/dev/null; then\n" +
			"  echo 'q02: /workspace is not writable by the workload user'\n" +
			"  exit 1\n" +
			"fi\n" +
			"sleep 60\n" +
			"grep -q preserved /workspace/resilience-workspace && echo " + workspaceMarker,
	})
}

// resilienceNATSPipeline keeps one action running across the NATS restart.
// Required image: any image with a POSIX sh, echo and sleep.
func resilienceNATSPipeline(image, marker string) []byte {
	return resiliencePipelineYAML(image, "broker-work", resilienceActionTimeout, []string{
		"echo " + marker + "\nsleep 45",
	})
}

// resilienceBrokerPipeline starts an execution while the broker is down.
// Required image: any image with a POSIX sh, echo and sleep.
func resilienceBrokerPipeline(image, marker string) []byte {
	return resiliencePipelineYAML(image, "offline-work", resilienceActionTimeout, []string{
		"echo " + marker + "\nsleep 5",
	})
}

// resilienceHandoffPipeline ignores TERM until its own timer, then restores
// from the task answer on the replacement. Required image: any image with a
// POSIX sh, echo, grep and sleep.
func resilienceHandoffPipeline(image, answer, firstMarker, replacementMarker, timerMarker string) []byte {
	command := "if grep -q '" + answer + "' /kodama/task.json 2>/dev/null; then\n" +
		"  echo " + replacementMarker + "\n" +
		"  exit 0\n" +
		"fi\n" +
		"trap '' TERM\n" +
		"echo " + firstMarker + "\n" +
		"sleep 25\n" +
		"echo " + timerMarker
	return resiliencePipelineYAML(image, "resist", resilienceActionTimeout, []string{command})
}

// resilienceCleanupPipeline is the short terminal-cleanup execution. Required
// image: any image with a POSIX sh, echo and sleep.
func resilienceCleanupPipeline(image, marker string) []byte {
	return resiliencePipelineYAML(image, "short-work", resilienceActionTimeout, []string{
		"echo " + marker + "\nsleep 1",
	})
}

// resilienceCleanupCrashPipeline ignores TERM long enough for a cancelled
// execution's pending cleanup obligation to outlive a forced controller crash
// and replacement startup, so the replacement controller deterministically
// resumes and completes the interrupted cleanup.
func resilienceCleanupCrashPipeline(image, marker string) []byte {
	return resiliencePipelineYAML(image, "short-work", resilienceActionTimeout, []string{
		"trap '' TERM\necho " + marker + "\nsleep 25",
	})
}

// resilienceWorkspaceLossPipeline writes one marker and a workspace file, then
// a successor action requires the same workspace. Required image: any image
// with a POSIX sh, printf, grep and sleep.
func resilienceWorkspaceLossPipeline(image, onceMarker, afterMarker string) []byte {
	return []byte("actions:\n" +
		"  - name: \"hold\"\n" +
		"    image: " + image + "\n" +
		"    timeout: " + resilienceActionTimeout + "\n" +
		"    commands:\n" +
		"      - |\n" +
		"        echo " + onceMarker + "\n" +
		"        printf preserved > /workspace/resilience-loss-marker\n" +
		"        sleep 30\n" +
		"  - name: \"after\"\n" +
		"    image: " + image + "\n" +
		"    timeout: " + resilienceActionTimeout + "\n" +
		"    commands:\n" +
		"      - |\n" +
		"        if grep -q preserved /workspace/resilience-loss-marker 2>/dev/null; then\n" +
		"          echo " + afterMarker + "\n" +
		"        else\n" +
		"          echo q02-workspace-loss-marker-missing\n" +
		"          exit 1\n" +
		"        fi\n")
}

// resilienceWorkloadPodLossPipeline keeps one action running long enough to
// lose its pod abruptly. The successor must never run: a lost workload is a
// visible failure, not a replay. Required image: any image with a POSIX sh,
// echo and sleep.
func resilienceWorkloadPodLossPipeline(image, beforeMarker, afterMarker string) []byte {
	return []byte("actions:\n" +
		"  - name: \"hold\"\n" +
		"    image: " + image + "\n" +
		"    timeout: " + resilienceActionTimeout + "\n" +
		"    commands:\n" +
		"      - |\n" +
		"        echo " + beforeMarker + "\n" +
		"        sleep 30\n" +
		"        echo " + afterMarker + "\n" +
		"  - name: \"never\"\n" +
		"    image: " + image + "\n" +
		"    timeout: " + resilienceActionTimeout + "\n" +
		"    commands:\n" +
		"      - |\n" +
		"        echo q02-action-must-not-run\n")
}

// resilienceGitOutagePipeline is ordinary task-free work that must keep
// running while the optional Git dependency is down. Required image: any image
// with a POSIX sh, echo and sleep.
func resilienceGitOutagePipeline(image, marker string) []byte {
	return resiliencePipelineYAML(image, "plain-work", resilienceActionTimeout, []string{
		"echo " + marker + "\nsleep 1",
	})
}

// resilienceRecoveryPipeline is a fresh authorized attempt after a visibly
// failed execution. Required image: any image with a POSIX sh and echo.
func resilienceRecoveryPipeline(image, marker string) []byte {
	return resiliencePipelineYAML(image, "fresh-work", resilienceActionTimeout, []string{
		"echo " + marker,
	})
}

// resiliencePipelineYAML renders one exact pipeline document for a probe
// action. The image is digest-pinned by the caller and every command is one
// literal shell body.
func resiliencePipelineYAML(image, action, timeout string, commands []string) []byte {
	var document strings.Builder
	document.WriteString("actions:\n")
	fmt.Fprintf(&document, "  - name: %q\n", action)
	fmt.Fprintf(&document, "    image: %s\n", image)
	fmt.Fprintf(&document, "    timeout: %s\n", timeout)
	document.WriteString("    commands:\n")
	for _, command := range commands {
		document.WriteString("      - |\n")
		for line := range strings.SplitSeq(command, "\n") {
			if line == "" {
				document.WriteString("\n")
				continue
			}
			document.WriteString("        ")
			document.WriteString(line)
			document.WriteString("\n")
		}
	}
	return []byte(document.String())
}

// resilienceInvoke sends exactly one public request with an optional expected
// version and an optional alternate frozen route. It mirrors the base API
// helper's TLS, redirect, bound and no-retry rules; the qualification package
// owns it because the handoff probe must carry expected_version and the inline
// start uses pipeline.start's additive executions binding.
func resilienceInvoke(ctx context.Context, client *API, method api.Method, token Secret, operation string, input any, expectedVersion *uint64, idempotencyKey string, alternate bool) (api.Result, error) {
	if client == nil || client.client == nil || client.base == nil {
		return api.Result{}, fmt.Errorf("the public API helper is not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return api.Result{}, err
	}
	var route api.Route
	if alternate {
		for _, candidate := range api.AlternateRoutes() {
			if candidate.Operation == operation {
				route = candidate
				break
			}
		}
		if route.Path == "" {
			return api.Result{}, fmt.Errorf("operation %q has no alternate public route", operation)
		}
	} else {
		found, ok := api.LookupRoute(operation)
		if !ok {
			return api.Result{}, fmt.Errorf("unknown public operation %q", operation)
		}
		route = found
	}
	if route.Method != method {
		return api.Result{}, fmt.Errorf("operation %q does not use %s", operation, method)
	}
	if operationRequiresIdempotencyKey(operation) && idempotencyKey == "" {
		return api.Result{}, fmt.Errorf("operation %q requires an idempotency key", operation)
	}
	if expectedVersion != nil && *expectedVersion == 0 {
		return api.Result{}, fmt.Errorf("expected version must be positive")
	}
	raw, err := encodeInput(input)
	if err != nil {
		return api.Result{}, err
	}
	requestBody := api.Request{Input: raw, ExpectedVersion: expectedVersion, IdempotencyKey: idempotencyKey}
	if err := requestBody.Validate(); err != nil {
		return api.Result{}, err
	}
	path, err := expandRoutePath(route, raw)
	if err != nil {
		return api.Result{}, err
	}
	envelope, err := json.Marshal(requestBody)
	if err != nil {
		return api.Result{}, fmt.Errorf("encode the public request envelope")
	}
	if bound := api.MaxEnvelopeBytes(operation); len(envelope) > bound {
		return api.Result{}, fmt.Errorf("the request envelope exceeds %d bytes", bound)
	}
	target := *client.base
	target.Path = strings.TrimSuffix(client.base.Path, "/") + path
	target.RawPath = ""
	request, err := http.NewRequestWithContext(ctx, string(method), target.String(), bytes.NewReader(envelope))
	if err != nil {
		return api.Result{}, fmt.Errorf("build the public request")
	}
	request.Header.Set("Content-Type", api.MediaType)
	request.Header.Set("Accept", api.MediaType)
	if token.IsSet() {
		request.Header.Set("Authorization", "Bearer "+token.Reveal())
	}
	request.ContentLength = int64(len(envelope))

	requestCtx := ctx
	timeout := client.requestTimeout
	if timeout <= 0 {
		timeout = defaultAPITimeout
	}
	var cancel context.CancelFunc
	requestCtx, cancel = context.WithTimeout(ctx, timeout)
	defer cancel()
	response, err := client.client.Do(request.WithContext(requestCtx))
	if err != nil {
		if requestCtx.Err() != nil {
			return api.Result{}, requestCtx.Err()
		}
		return api.Result{}, fmt.Errorf("the public endpoint is unavailable")
	}
	defer response.Body.Close()

	data, err := readBounded(response.Body, client.maxResponse)
	if err != nil {
		return api.Result{}, err
	}
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return api.Result{}, fmt.Errorf("the public endpoint redirected; redirects are never followed")
	}
	if response.StatusCode == api.HTTPStatusSuccess {
		result, err := api.DecodeResult(data)
		if err != nil {
			return api.Result{}, fmt.Errorf("the public response is not a valid result")
		}
		if result.Operation != operation {
			return api.Result{}, fmt.Errorf("the public response names operation %q", result.Operation)
		}
		return result, nil
	}
	public, err := decodePublicError(data)
	if err != nil {
		return api.Result{}, fmt.Errorf("the public response is not a valid error")
	}
	status, ok := api.HTTPStatus(public.Code)
	if !ok || status != response.StatusCode {
		return api.Result{}, fmt.Errorf("the public response status does not match its code")
	}
	return api.Result{}, &APIError{Status: response.StatusCode, Code: public.Code, RequestID: public.RequestID}
}

// resilienceOperationMethod returns the frozen REST method of one canonical
// operation. Every probe mutation dispatches through this so a PUT route can
// never be sent as POST.
func resilienceOperationMethod(operation string) (api.Method, bool) {
	route, ok := api.LookupRoute(operation)
	if !ok {
		return "", false
	}
	return route.Method, true
}

// resilienceMutateVersioned sends one mutation guarded by the caller's expected
// version, using the operation's frozen REST method.
func resilienceMutateVersioned(ctx context.Context, client *API, token Secret, operation string, input any, expectedVersion uint64, idempotencyKey string) (api.Result, error) {
	if expectedVersion == 0 {
		return api.Result{}, fmt.Errorf("expected version must be positive")
	}
	method, ok := resilienceOperationMethod(operation)
	if !ok {
		return api.Result{}, fmt.Errorf("unknown public operation %q", operation)
	}
	return resilienceInvoke(ctx, client, method, token, operation, input, &expectedVersion, idempotencyKey, false)
}

// resilienceInlineStart admits one task-free inline pipeline through the frozen
// additive executions route and returns the execution identity.
func resilienceInlineStart(ctx context.Context, client *API, token Secret, projectID string, document []byte, idempotencyKey string) (api.PipelineStartResult, error) {
	if idempotencyKey == "" {
		return api.PipelineStartResult{}, fmt.Errorf("pipeline.start requires an idempotency key")
	}
	route, ok := api.LookupRoute("pipeline.start")
	if !ok || route.Method != api.MethodPost {
		return api.PipelineStartResult{}, fmt.Errorf("pipeline.start no longer accepts POST")
	}
	input := api.PipelineStartRequest{ProjectID: projectID, YAML: document}
	result, err := resilienceInvoke(ctx, client, route.Method, token, "pipeline.start", input, nil, idempotencyKey, true)
	if err != nil {
		return api.PipelineStartResult{}, err
	}
	return resilienceDecodeResult[api.PipelineStartResult](result, "pipeline.start")
}

// resilienceDecodeResult decodes and validates one public result body.
func resilienceDecodeResult[T any](result api.Result, operation string) (T, error) {
	var value T
	if result.Operation != operation {
		return value, fmt.Errorf("public result names operation %q, want %q", result.Operation, operation)
	}
	if err := json.Unmarshal(result.Body, &value); err != nil {
		return value, fmt.Errorf("public result body is not the frozen DTO: %w", err)
	}
	if validator, ok := any(&value).(interface{ Validate() error }); ok {
		if err := validator.Validate(); err != nil {
			return value, fmt.Errorf("public result body is invalid: %w", err)
		}
	}
	return value, nil
}
