package runprofile

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// Cluster is the qualifier's handle on the target: the kubectl client and
// the task-owned namespaces created in this run.
type Cluster struct {
	K          *Kubectl
	RunID      string
	Namespaces map[string]string // name -> metadata.uid
}

// NewRunID returns the random unique suffix of this run's namespaces.
func NewRunID() (string, error) {
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (c *Cluster) labels(extra map[string]any) map[string]any {
	m := map[string]any{LabelTask: TaskValue, LabelRun: c.RunID}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

// obj builds a namespaced object with ownership labels.
func (c *Cluster) obj(apiVersion, kind, ns, name string, labels map[string]any, spec map[string]any) map[string]any {
	meta := map[string]any{"name": name, "labels": c.labels(labels)}
	if ns != "" {
		meta["namespace"] = ns
	}
	o := map[string]any{"apiVersion": apiVersion, "kind": kind, "metadata": meta}
	if spec != nil {
		o["spec"] = spec
	}
	return o
}

// Apply guards, then creates or updates one object.
func (c *Cluster) Apply(ctx context.Context, obj map[string]any) (string, error) {
	data, err := json.Marshal(obj)
	if err != nil {
		return "", err
	}
	var norm map[string]any
	if err := json.Unmarshal(data, &norm); err != nil {
		return "", err
	}
	if err := Guard(norm, c.RunID); err != nil {
		return "", err
	}
	if norm["kind"] == "Namespace" {
		// Apply would adopt and relabel a pre-existing namespace of the same
		// name. Namespaces are only ever created, through CreateNamespace.
		return "", fmt.Errorf("guard: namespaces are create-only; use CreateNamespace")
	}
	return c.K.Run(ctx, data, "apply", "-f", "-")
}

// ApplyAll applies objects in order.
func (c *Cluster) ApplyAll(ctx context.Context, objs ...map[string]any) error {
	for _, o := range objs {
		if _, err := c.Apply(ctx, o); err != nil {
			return err
		}
	}
	return nil
}

// ApplyDryRun server-side validates an object including admission (PSA).
func (c *Cluster) ApplyDryRun(ctx context.Context, obj map[string]any) (string, error) {
	data, _ := json.Marshal(obj)
	var norm map[string]any
	_ = json.Unmarshal(data, &norm)
	if err := Guard(norm, c.RunID); err != nil {
		return "", err
	}
	return c.K.Run(ctx, data, "apply", "--dry-run=server", "-f", "-")
}

// CreateNamespace creates a probe namespace with a bounded ResourceQuota and
// records its UID for the ownership-checked cleanup.
func (c *Cluster) CreateNamespace(ctx context.Context, name string, podSecurity map[string]string) error {
	if err := c.CreateNamespaceOnly(ctx, name, podSecurity); err != nil {
		return err
	}
	quota := c.obj("v1", "ResourceQuota", name, "run-profile-bounds", nil, map[string]any{"hard": map[string]any{
		"pods": "12", "persistentvolumeclaims": "2", "requests.storage": "2Gi",
		"requests.cpu": "4", "limits.memory": "12Gi", "requests.memory": "6Gi",
		"secrets": "12", "services": "8",
	}})
	return c.ApplyAll(ctx, quota)
}

// CreateNamespaceOnly creates (never updates) the namespace and records the
// UID returned by the creation response.
func (c *Cluster) CreateNamespaceOnly(ctx context.Context, name string, podSecurity map[string]string) error {
	labels := map[string]any{}
	for k, v := range podSecurity {
		labels[k] = v
	}
	ns := c.obj("v1", "Namespace", "", name, labels, nil)
	data, err := json.Marshal(ns)
	if err != nil {
		return err
	}
	var norm map[string]any
	if err := json.Unmarshal(data, &norm); err != nil {
		return err
	}
	if err := Guard(norm, c.RunID); err != nil {
		return err
	}
	// Create-only: an existing namespace of the same name is never modified,
	// relabelled, adopted or recorded. The UID comes from the creation
	// response itself, not from a later lookup by name that a replacement
	// object could answer.
	out, err := c.K.Run(ctx, data, "create", "-f", "-", "-o", "jsonpath={.metadata.uid}")
	if err != nil {
		if strings.Contains(err.Error(), "AlreadyExists") {
			return fmt.Errorf("refusing namespace %q: it already exists and is not owned by this run", name)
		}
		return err
	}
	uid := strings.TrimSpace(out)
	if uid == "" || strings.ContainsAny(uid, " \n{}") {
		return fmt.Errorf("namespace %q: creation response carried no UID; not recording ownership", name)
	}
	if c.Namespaces == nil {
		c.Namespaces = map[string]string{}
	}
	c.Namespaces[name] = uid
	return nil
}

// Pod is a probe Pod description; PodSpec renders it.
type Pod struct {
	Name      string
	Namespace string
	Labels    map[string]any
	Image     string
	Command   []string
	Env       []any // pre-built env entries
	UID       int64
	GID       int64
	FSGroup   int64
	// StepRole renders exactly the O-30 `step` role (seam S3): UID/GID
	// StepUID, hostUsers false (always), all capabilities dropped then
	// SETUID/SETGID added, allowPrivilegeEscalation true, Localhost seccomp
	// and AppArmor profiles. Unmasked additionally sets procMount: Unmasked
	// (the one optional variant). Nothing else widens a probe Pod.
	StepRole   bool
	Unmasked   bool
	Volumes    []any
	Mounts     []any
	CPU        string
	MemLimit   string
	Readiness  map[string]any
	Restart    string
	ReadOnlyFS bool
	DeadlineS  int64
}

// PodObject renders the guarded Pod. There is no field for privileged mode,
// host namespaces or an unconfined profile.
func (c *Cluster) PodObject(p Pod) map[string]any {
	psc := map[string]any{
		"runAsUser": p.UID, "runAsGroup": p.GID, "runAsNonRoot": p.UID != 0,
		"seccompProfile": map[string]any{"type": "RuntimeDefault"},
	}
	if p.FSGroup != 0 {
		psc["fsGroup"] = p.FSGroup
	}
	if p.UID == 0 {
		delete(psc, "runAsNonRoot")
	}
	caps := map[string]any{"drop": []any{"ALL"}}
	csc := map[string]any{"allowPrivilegeEscalation": false, "capabilities": caps, "readOnlyRootFilesystem": p.ReadOnlyFS}
	if p.StepRole {
		caps["add"] = []any{"SETUID", "SETGID"}
		csc["allowPrivilegeEscalation"] = true
		psc["seccompProfile"] = map[string]any{"type": "Localhost", "localhostProfile": StepSeccompProfile}
		psc["appArmorProfile"] = map[string]any{"type": "Localhost", "localhostProfile": StepAppArmorProfile}
		if p.Unmasked {
			csc["procMount"] = "Unmasked"
		}
	}
	cpu := p.CPU
	if cpu == "" {
		cpu = "100m"
	}
	mem := p.MemLimit
	if mem == "" {
		mem = "512Mi"
	}
	command := []any{}
	for _, a := range p.Command {
		command = append(command, a)
	}
	ctr := map[string]any{
		"name": "main", "image": p.Image,
		"securityContext": csc,
		"resources":       map[string]any{"requests": map[string]any{"cpu": cpu, "memory": "64Mi"}, "limits": map[string]any{"memory": mem}},
	}
	if len(command) > 0 {
		ctr["command"] = command
	}
	if len(p.Env) > 0 {
		ctr["env"] = p.Env
	}
	if len(p.Mounts) > 0 {
		ctr["volumeMounts"] = p.Mounts
	}
	if p.Readiness != nil {
		ctr["readinessProbe"] = p.Readiness
	}
	restart := p.Restart
	if restart == "" {
		restart = "Never"
	}
	deadline := p.DeadlineS
	if deadline == 0 {
		deadline = 1500
	}
	spec := map[string]any{
		"hostUsers": false, "restartPolicy": restart, "automountServiceAccountToken": false,
		"enableServiceLinks": false, "terminationGracePeriodSeconds": 5, "activeDeadlineSeconds": deadline,
		"securityContext": psc, "containers": []any{ctr},
	}
	if len(p.Volumes) > 0 {
		spec["volumes"] = p.Volumes
	}
	return c.obj("v1", "Pod", p.Namespace, p.Name, p.Labels, spec)
}

// PodJSON is the subset of a Pod the qualifier reads.
type PodJSON struct {
	Metadata struct {
		Name string `json:"name"`
		UID  string `json:"uid"`
	} `json:"metadata"`
	Spec struct {
		NodeName string `json:"nodeName"`
	} `json:"spec"`
	Status struct {
		Phase      string `json:"phase"`
		Reason     string `json:"reason"`
		Message    string `json:"message"`
		Conditions []struct {
			Type    string `json:"type"`
			Status  string `json:"status"`
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"conditions"`
		ContainerStatuses []struct {
			Name    string `json:"name"`
			ImageID string `json:"imageID"`
			State   struct {
				Waiting *struct {
					Reason  string `json:"reason"`
					Message string `json:"message"`
				} `json:"waiting"`
				Terminated *struct {
					ExitCode int    `json:"exitCode"`
					Reason   string `json:"reason"`
				} `json:"terminated"`
			} `json:"state"`
		} `json:"containerStatuses"`
	} `json:"status"`
}

// GetPod reads a Pod.
func (c *Cluster) GetPod(ctx context.Context, ns, name string) (*PodJSON, error) {
	var p PodJSON
	if err := c.K.JSON(ctx, &p, "-n", ns, "get", "pod", name); err != nil {
		return nil, err
	}
	return &p, nil
}

// Wait polls until check reports done or the timeout expires.
func Wait(ctx context.Context, timeout time.Duration, every time.Duration, check func() (bool, error)) error {
	deadline := time.Now().Add(timeout)
	var last error
	for {
		done, err := check()
		if done {
			return nil
		}
		var term terminalError
		if errors.As(err, &term) {
			return err
		}
		last = err
		if time.Now().After(deadline) {
			if last == nil {
				last = errors.New("condition not met")
			}
			return fmt.Errorf("timed out after %s: %w", timeout, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(every):
		}
	}
}

// terminalError aborts a Wait immediately: the condition can no longer be met.
type terminalError struct{ error }

func (t terminalError) Unwrap() error { return t.error }

// profileWaiting reports a terminal error when a container cannot start
// because the node cannot apply a referenced Localhost seccomp/AppArmor
// profile: waiting longer cannot fix an uninstalled profile.
func profileWaiting(p *PodJSON, name string) error {
	for _, cs := range p.Status.ContainerStatuses {
		if w := cs.State.Waiting; w != nil && (w.Reason == "CreateContainerError" || w.Reason == "CreateContainerConfigError") && profileFailure(w.Message) {
			return terminalError{fmt.Errorf("pod %s container cannot start: %s: %s", name, w.Reason, tail(w.Message, 300))}
		}
	}
	return nil
}

// profileFailure recognises a node-side seccomp/AppArmor profile refusal.
func profileFailure(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "seccomp") || strings.Contains(m, "apparmor")
}

// WaitReady waits for the Pod's Ready condition.
func (c *Cluster) WaitReady(ctx context.Context, ns, name string, timeout time.Duration) error {
	return Wait(ctx, timeout, 3*time.Second, func() (bool, error) {
		p, err := c.GetPod(ctx, ns, name)
		if err != nil {
			return false, err
		}
		if p.Status.Phase == "Failed" || p.Status.Phase == "Succeeded" {
			return false, terminalError{fmt.Errorf("pod %s ended in phase %s: %s %s", name, p.Status.Phase, p.Status.Reason, tail(p.Status.Message, 300))}
		}
		if err := profileWaiting(p, name); err != nil {
			return false, err
		}
		for _, cd := range p.Status.Conditions {
			if cd.Type == "Ready" && cd.Status == "True" {
				return true, nil
			}
		}
		return false, fmt.Errorf("pod %s phase %s not ready", name, p.Status.Phase)
	})
}

// WaitDone waits for a terminal phase and returns it with the main
// container's exit code.
func (c *Cluster) WaitDone(ctx context.Context, ns, name string, timeout time.Duration) (string, int, error) {
	phase, code := "", -1
	err := Wait(ctx, timeout, 3*time.Second, func() (bool, error) {
		p, err := c.GetPod(ctx, ns, name)
		if err != nil {
			return false, err
		}
		phase = p.Status.Phase
		if err := profileWaiting(p, name); err != nil {
			return false, err
		}
		if phase == "Succeeded" || phase == "Failed" {
			for _, cs := range p.Status.ContainerStatuses {
				if cs.State.Terminated != nil {
					code = cs.State.Terminated.ExitCode
				}
			}
			return true, nil
		}
		return false, fmt.Errorf("phase %s", phase)
	})
	return phase, code, err
}

// Exec runs a shell script in a Pod. The script is a fixture, never a
// secret carrier: secrets reach containers only through Secret volumes and
// environment references.
func (c *Cluster) Exec(ctx context.Context, ns, pod string, stdin io.Reader, script string) (string, error) {
	return c.K.RunReader(ctx, stdin, nil, "-n", ns, "exec", "-i", pod, "--", "sh", "-c", script)
}

// DeletePod deletes a Pod this run created and waits until it is gone.
func (c *Cluster) DeletePod(ctx context.Context, ns, name string, timeout time.Duration) error {
	if !strings.HasPrefix(ns, NamespacePrefix) {
		return fmt.Errorf("refusing to delete in namespace %q", ns)
	}
	owned, err := c.K.Run(ctx, nil, "-n", ns, "get", "pod", "--ignore-not-found", "-o", "name", "-l", LabelTask+"="+TaskValue+","+LabelRun+"="+c.RunID, "--field-selector", "metadata.name="+name)
	if err != nil {
		return err
	}
	if strings.TrimSpace(owned) != "" {
		if _, err := c.K.Run(ctx, nil, "-n", ns, "delete", "pod", name, "--ignore-not-found", "--wait=false"); err != nil {
			return err
		}
	}
	return Wait(ctx, timeout, 2*time.Second, func() (bool, error) {
		out, err := c.K.Run(ctx, nil, "-n", ns, "get", "pod", name, "--ignore-not-found", "-o", "name")
		if err != nil {
			return false, err
		}
		if strings.TrimSpace(out) == "" {
			return true, nil
		}
		return false, fmt.Errorf("pod %s still present", name)
	})
}

// Forward opens kubectl port-forward to a Pod port and returns the local
// address and a stop function. The forward is bounded by ctx.
func (c *Cluster) Forward(ctx context.Context, ns, pod string, port int) (string, func(), error) {
	fctx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(fctx, c.K.Binary, "--kubeconfig", c.K.Kubeconfig, "-n", ns, "port-forward", "--address", "127.0.0.1", "pod/"+pod, fmt.Sprintf(":%d", port))
	cmd.Env = cleanEnv()
	var errb bytes.Buffer
	cmd.Stderr = &errb
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return "", nil, err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return "", nil, err
	}
	stop := func() { cancel(); _ = cmd.Wait() }
	addr := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			line := sc.Text()
			if i := strings.Index(line, "127.0.0.1:"); i >= 0 {
				rest := line[i:]
				if j := strings.Index(rest, " "); j > 0 {
					rest = rest[:j]
				}
				addr <- rest
				break
			}
		}
		_, _ = io.Copy(io.Discard, stdout)
	}()
	select {
	case a := <-addr:
		c.K.Rec.record([]string{"-n", ns, "port-forward", "pod/" + pod, fmt.Sprintf(":%d", port)}, nil, 0, 0, "forwarding "+a)
		return a, stop, nil
	case <-time.After(30 * time.Second):
		stop()
		return "", nil, fmt.Errorf("port-forward did not start: %s", tail(errb.String(), 300))
	case <-ctx.Done():
		stop()
		return "", nil, ctx.Err()
	}
}
