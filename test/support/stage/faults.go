package stage

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Hold is an admission policy that refuses one operation on Jobs of one namespace *when the control plane's own
// service account issues it*. It is how a test holds back exactly one real Kubernetes call (a Job create or a Job
// delete) without a proxy on the path: the server sees the cluster's own refusal. The test's operator identity is
// never affected, and Release removes the policy.
type Hold struct {
	i         *Installation
	name      string
	namespace string
	op        string
}

// serverUser is the service account the server authenticates as.
func (i *Installation) serverUser(ctx context.Context) (string, error) {
	sa, err := i.Kube.Run(ctx, "-n", i.S.Namespace, "get", "deployment", i.Deployment(), "-o", "jsonpath={.spec.template.spec.serviceAccountName}")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("system:serviceaccount:%s:%s", i.S.Namespace, strings.TrimSpace(sa)), nil
}

// HoldJobs refuses CREATE or DELETE of Jobs in the namespace for the server. It returns once the refusal is in
// effect. The hold is released when the test ends at the latest.
func (i *Installation) HoldJobs(ctx context.Context, namespace, op string) *Hold {
	i.T.Helper()
	if op != "CREATE" && op != "DELETE" {
		i.T.Fatalf("hold: unsupported operation %q", op)
	}
	user, err := i.serverUser(ctx)
	if err != nil {
		i.T.Fatalf("hold: %v", err)
	}
	h := &Hold{i: i, name: fmt.Sprintf("kq-%s-hold-%s-%s", i.S.Name, strings.ToLower(op), random(2)), namespace: namespace, op: op}
	policy := fmt.Sprintf(`apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicy
metadata:
  name: %[1]s
  labels: {kodama.optimi.com/qualification-session: %[5]q}
spec:
  failurePolicy: Fail
  matchConstraints:
    resourceRules:
      - apiGroups: ["batch"]
        apiVersions: ["v1"]
        operations: [%[2]q]
        resources: ["jobs"]
  validations:
    - expression: "request.userInfo.username != %[3]q"
      message: "kodama qualification hold: the control plane may not %[4]s Jobs now"
      reason: Forbidden
---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicyBinding
metadata:
  name: %[1]s
  labels: {kodama.optimi.com/qualification-session: %[5]q}
spec:
  policyName: %[1]s
  validationActions: [Deny]
  matchResources:
    namespaceSelector:
      matchLabels: {kubernetes.io/metadata.name: %[6]q}
`, h.name, op, user, strings.ToLower(op), i.S.Name, namespace)
	if _, err := i.Kube.RunInput(ctx, []byte(policy), "apply", "-f", "-"); err != nil {
		i.T.Fatalf("hold: %v", err)
	}
	i.T.Cleanup(h.Release)
	h.waitEffective(ctx, user)
	return h
}

// waitEffective polls until a request of the server's identity is refused: the API server takes a few seconds to
// start enforcing a new policy.
func (h *Hold) waitEffective(ctx context.Context, user string) {
	i := h.i
	i.T.Helper()
	probe := "kq-probe-" + random(3)
	manifest := fmt.Sprintf(`apiVersion: batch/v1
kind: Job
metadata: {name: %s, namespace: %s}
spec:
  backoffLimit: 0
  template:
    spec:
      restartPolicy: Never
      containers: [{name: c, image: %s, command: ["true"]}]
`, probe, h.namespace, i.WorkloadImage())
	if h.op == "DELETE" {
		// A DELETE is judged against an existing object: create a throwaway one as the operator first.
		if _, err := i.Kube.RunInput(ctx, []byte(manifest), "create", "--dry-run=none", "-f", "-"); err != nil {
			i.T.Fatalf("hold probe: %v", err)
		}
		defer func() {
			c, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			_, _ = i.Kube.Run(c, "-n", h.namespace, "delete", "job", probe, "--ignore-not-found", "--wait=false")
		}()
	}
	WaitForEvery(i.T, "the admission hold to take effect", 90*time.Second, 2*time.Second, func() bool {
		var err error
		if h.op == "CREATE" {
			_, err = i.Kube.RunInput(ctx, []byte(manifest), "create", "--dry-run=server", "--as", user, "-f", "-")
		} else {
			_, err = i.Kube.Run(ctx, "-n", h.namespace, "delete", "job", probe, "--dry-run=server", "--as", user)
		}
		return err != nil && strings.Contains(err.Error(), "qualification hold")
	})
}

// Release removes the hold (idempotent).
func (h *Hold) Release() {
	c, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, _ = h.i.Kube.Run(c, "delete", "validatingadmissionpolicybinding", h.name, "--ignore-not-found")
	_, _ = h.i.Kube.Run(c, "delete", "validatingadmissionpolicy", h.name, "--ignore-not-found")
}
