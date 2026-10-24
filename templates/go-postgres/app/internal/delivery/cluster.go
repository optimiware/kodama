package delivery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// ObjectRef identifies one namespaced object.
type ObjectRef struct {
	APIVersion string
	Kind       string
	Namespace  string
	Name       string
}

func (r ObjectRef) object() Object {
	return Object{APIVersion: r.APIVersion, Kind: r.Kind, Namespace: r.Namespace, Name: r.Name}
}

// Observed is what the cluster reports about one object.
type Observed struct {
	Ref         ObjectRef
	UID         string
	Annotations map[string]string
	// Ready is structured readiness: Deployments are fully rolled out, Jobs have
	// succeeded, other kinds are ready when they exist.
	Ready bool
	// Failed is a definitive failure (for example a Job past its backoff limit).
	Failed bool
}

// Cluster is the delivery helper's consumer-owned port to Kubernetes. Every
// call is namespace-scoped by the credential behind it.
type Cluster interface {
	// Apply sends the manifests (a JSON List). An error means the effect is
	// unknown: some objects may have been created.
	Apply(ctx context.Context, namespace string, list []byte) error
	// Get returns the object, or ok=false when it does not exist.
	Get(ctx context.Context, ref ObjectRef) (obs Observed, ok bool, err error)
}

// Kubectl is the Cluster adapter over the kubectl binary and a namespace-scoped
// kubeconfig file delivered by the step entrypoint.
type Kubectl struct {
	Binary     string
	Kubeconfig string
	Timeout    time.Duration
}

var _ Cluster = (*Kubectl)(nil)

func (k *Kubectl) run(ctx context.Context, stdin []byte, args ...string) ([]byte, []byte, error) {
	bin := k.Binary
	if bin == "" {
		bin = "kubectl"
	}
	timeout := k.Timeout
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	full := append([]string{"--kubeconfig", k.Kubeconfig, "--request-timeout", "30s"}, args...)
	cmd := exec.CommandContext(ctx, bin, full...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	return out.Bytes(), errb.Bytes(), err
}

// Apply implements Cluster.
func (k *Kubectl) Apply(ctx context.Context, namespace string, list []byte) error {
	_, stderr, err := k.run(ctx, list, "--namespace", namespace, "apply", "-f", "-")
	if err != nil {
		return fmt.Errorf("kubectl apply failed: %s", firstLine(stderr, err))
	}
	return nil
}

func resource(ref ObjectRef) string {
	group := ""
	if i := strings.Index(ref.APIVersion, "/"); i >= 0 {
		group = "." + ref.APIVersion[:i]
	}
	return ref.Kind + group + "/" + ref.Name
}

// Get implements Cluster.
func (k *Kubectl) Get(ctx context.Context, ref ObjectRef) (Observed, bool, error) {
	stdout, stderr, err := k.run(ctx, nil, "--namespace", ref.Namespace, "get", resource(ref), "-o", "json")
	if err != nil {
		if bytes.Contains(stderr, []byte("NotFound")) || bytes.Contains(stderr, []byte("not found")) {
			return Observed{}, false, nil
		}
		return Observed{}, false, fmt.Errorf("kubectl get failed: %s", firstLine(stderr, err))
	}
	return parseObserved(ref, stdout)
}

func firstLine(b []byte, err error) string {
	s := strings.TrimSpace(string(b))
	if s == "" {
		return err.Error()
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

type kubeObject struct {
	Metadata struct {
		Name        string            `json:"name"`
		Namespace   string            `json:"namespace"`
		UID         string            `json:"uid"`
		Generation  int64             `json:"generation"`
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
	Spec struct {
		Replicas     *int64 `json:"replicas"`
		BackoffLimit *int64 `json:"backoffLimit"`
	} `json:"spec"`
	Status struct {
		ObservedGeneration int64 `json:"observedGeneration"`
		UpdatedReplicas    int64 `json:"updatedReplicas"`
		AvailableReplicas  int64 `json:"availableReplicas"`
		ReadyReplicas      int64 `json:"readyReplicas"`
		Replicas           int64 `json:"replicas"`
		Succeeded          int64 `json:"succeeded"`
		Failed             int64 `json:"failed"`
		Conditions         []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
		} `json:"conditions"`
	} `json:"status"`
}

func parseObserved(ref ObjectRef, raw []byte) (Observed, bool, error) {
	var o kubeObject
	if err := json.Unmarshal(raw, &o); err != nil {
		return Observed{}, false, errors.New("kubectl returned malformed JSON")
	}
	if o.Metadata.Name != ref.Name || (o.Metadata.Namespace != "" && o.Metadata.Namespace != ref.Namespace) {
		return Observed{}, false, errors.New("kubectl returned a different object than requested")
	}
	obs := Observed{Ref: ref, UID: o.Metadata.UID, Annotations: o.Metadata.Annotations}
	switch ref.Kind {
	case "Deployment":
		want := int64(1)
		if o.Spec.Replicas != nil {
			want = *o.Spec.Replicas
		}
		obs.Ready = want > 0 && o.Status.ObservedGeneration >= o.Metadata.Generation && o.Status.UpdatedReplicas >= want &&
			o.Status.AvailableReplicas >= want && o.Status.Replicas <= want
	case "Job":
		limit := int64(6)
		if o.Spec.BackoffLimit != nil {
			limit = *o.Spec.BackoffLimit
		}
		// Terminal conditions are authoritative: a Job the controller marked
		// Failed (backoff limit, deadline) never becomes ready, and one marked
		// Complete is ready. Counters alone cover a controller that has not yet
		// written a condition.
		complete, failed := false, false
		for _, c := range o.Status.Conditions {
			switch {
			case c.Type == "Complete" && c.Status == "True":
				complete = true
			case c.Type == "Failed" && c.Status == "True":
				failed = true
			}
		}
		obs.Failed = failed || o.Status.Failed > limit
		obs.Ready = !obs.Failed && (complete || o.Status.Succeeded >= 1)
	default:
		obs.Ready = true
	}
	return obs, true, nil
}
