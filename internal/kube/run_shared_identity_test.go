package kube_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/kube"
	"github.com/optimiware/kodama/internal/kube/kubetest"
)

// sharedIdentityPolicy is the test policy with the run_shared workload identity
// of the builder image's `build` user (UID/GID 1000) beside the legacy 65532.
func sharedIdentityPolicy(t *testing.T) kube.ExecutionPolicy {
	t.Helper()
	cfg := kubetest.PolicyConfig(t, "a")
	cfg.RunShared = &kube.RunSharedIdentity{RunAsUser: 1000, RunAsGroup: 1000, FSGroup: 1000}
	p, err := kube.NewExecutionPolicy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// Test_unit_kube_run_shared_identity: run_shared workloads take their own
// non-root identity (container user, group and the Pod's volume group) while the
// injector and every attempt_private Job keep the legacy one; an unset identity
// keeps the legacy one; a root or incomplete identity is refused; and the policy
// digest of an installation that does not set it is unchanged.
func Test_unit_kube_run_shared_identity(t *testing.T) {
	ctx := context.Background()
	s := newSharedEnv(t, builderRole, func(re *runEnv) {
		re.policy = sharedIdentityPolicy(t)
		re.rr = re.build()
	})
	if _, err := s.rt.Start(ctx, s.intent()); err != nil {
		t.Fatalf("start: %v", err)
	}
	pod := s.job().Spec.Template.Spec
	if sc := pod.SecurityContext; sc == nil || *sc.RunAsUser != 1000 || *sc.RunAsGroup != 1000 || *sc.FSGroup != 1000 || !*sc.RunAsNonRoot {
		t.Fatalf("pod identity %+v", sc)
	}
	c := pod.Containers[0].SecurityContext
	if *c.RunAsUser != 1000 || *c.RunAsGroup != 1000 || !*c.RunAsNonRoot {
		t.Fatalf("step container identity %+v", c)
	}
	if inj := pod.InitContainers[0].SecurityContext; *inj.RunAsUser != 65532 {
		t.Fatalf("the injector keeps the legacy identity: %+v", inj)
	}

	// Unset: the legacy identity (the existing behavior).
	legacy := newSharedEnv(t, builderRole)
	if _, err := legacy.rt.Start(ctx, legacy.intent()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if sc := legacy.job().Spec.Template.Spec.SecurityContext; *sc.RunAsUser != 65532 || *sc.FSGroup != 65532 {
		t.Fatalf("an unset run_shared identity changed the legacy identity: %+v", sc)
	}

	for name, id := range map[string]kube.RunSharedIdentity{
		"root user": {RunAsUser: 0, RunAsGroup: 1000, FSGroup: 1000}, "root group": {RunAsUser: 1000, RunAsGroup: 0, FSGroup: 1000},
		"root volume group": {RunAsUser: 1000, RunAsGroup: 1000, FSGroup: 0}, "negative": {RunAsUser: -1, RunAsGroup: 1, FSGroup: 1},
	} {
		cfg := kubetest.PolicyConfig(t, "a")
		cfg.RunShared = &id
		if _, err := kube.NewExecutionPolicy(cfg); err == nil || !strings.Contains(err.Error(), "run_shared") {
			t.Fatalf("%s: want a refusal, got %v", name, err)
		}
	}

	// An installation that does not set the identity keeps the policy digest it
	// always had: the member is omitted from the digest's document.
	cfg := kubetest.PolicyConfig(t, "a")
	raw, err := json.Marshal(cfg)
	if err != nil || strings.Contains(string(raw), "RunShared") {
		t.Fatalf("an unset identity changed the policy document: %v", err)
	}
	a, err1 := kube.NewExecutionPolicy(cfg)
	cfg.RunShared = &kube.RunSharedIdentity{RunAsUser: 1000, RunAsGroup: 1000, FSGroup: 1000}
	b, err2 := kube.NewExecutionPolicy(cfg)
	if err1 != nil || err2 != nil || a.Digest() == b.Digest() {
		t.Fatalf("the identity must be part of the digest: %v %v", err1, err2)
	}
}
