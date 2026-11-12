//go:build qualification

package production

import (
	"strings"
	"testing"
)

// TestHarnessConfinementGuardRefusals proves the kubernetes safety guard admits
// only the assigned context and namespaces, only namespaced workload mutations,
// and never a node, cluster-scoped, tofu or cloud command.
func TestHarnessConfinementGuardRefusals(t *testing.T) {
	guard, err := NewGuard("kodama-stage-k8s", "kodama-tenant-tenant-a", "kodama-tenant-tenant-b")
	if err != nil {
		t.Fatalf("build guard: %v", err)
	}

	admitted := []Access{
		{Context: "kodama-stage-k8s", Namespace: ReleaseNamespace, Verb: "delete", Resource: "pods"},
		{Context: "kodama-stage-k8s", Namespace: "kodama-tenant-tenant-a", Verb: "patch", Resource: "deployments"},
		{Context: "kodama-stage-k8s", Namespace: "kodama-tenant-tenant-b", Verb: "scale", Resource: "statefulsets"},
		{Context: "kodama-stage-k8s", Namespace: ReleaseNamespace, Verb: "port-forward", Resource: "services"},
		{Context: "kodama-stage-k8s", Namespace: ReleaseNamespace, Verb: "get", Resource: "secrets"},
		{Context: "kodama-stage-k8s", Namespace: "kodama-tenant-tenant-a", Verb: "get", Resource: "networkpolicies"},
		{Context: "kodama-stage-k8s", Namespace: "kodama-tenant-tenant-b", Verb: "get", Resource: "limitranges"},
		{Context: "kodama-stage-k8s", Namespace: "kodama-tenant-tenant-a", Verb: "get", Resource: "namespaces/kodama-tenant-tenant-a"},
		{Context: "kodama-stage-k8s", Namespace: ReleaseNamespace, Verb: "describe", Resource: "namespaces/" + ReleaseNamespace},
		{Context: "kodama-stage-k8s", Namespace: "kodama-tenant-tenant-a", Verb: "delete", Resource: "persistentvolumeclaims"},
	}
	for _, access := range admitted {
		if err := guard.Admit(access); err != nil {
			t.Errorf("admitted access %+v was refused: %v", access, err)
		}
	}

	refused := []struct {
		name   string
		access Access
	}{
		{
			name:   "another context",
			access: Access{Context: "kodama-probe", Namespace: ReleaseNamespace, Verb: "delete", Resource: "pods"},
		},
		{
			name:   "empty context",
			access: Access{Context: "", Namespace: ReleaseNamespace, Verb: "delete", Resource: "pods"},
		},
		{
			name:   "another namespace",
			access: Access{Context: "kodama-stage-k8s", Namespace: "default", Verb: "delete", Resource: "pods"},
		},
		{
			name:   "namespace traversal",
			access: Access{Context: "kodama-stage-k8s", Namespace: ReleaseNamespace + "/../default", Verb: "delete", Resource: "pods"},
		},
		{
			name:   "nodes",
			access: Access{Context: "kodama-stage-k8s", Namespace: ReleaseNamespace, Verb: "delete", Resource: "nodes"},
		},
		{
			name:   "cluster role binding",
			access: Access{Context: "kodama-stage-k8s", Namespace: ReleaseNamespace, Verb: "create", Resource: "clusterrolebindings"},
		},
		{
			name:   "unknown mutation resource",
			access: Access{Context: "kodama-stage-k8s", Namespace: ReleaseNamespace, Verb: "delete", Resource: "persistentvolumes"},
		},
		{
			name:   "unknown verb",
			access: Access{Context: "kodama-stage-k8s", Namespace: ReleaseNamespace, Verb: "apply", Resource: "pods"},
		},
		{
			name:   "empty resource",
			access: Access{Context: "kodama-stage-k8s", Namespace: ReleaseNamespace, Verb: "delete", Resource: ""},
		},
		{
			name:   "exec verb",
			access: Access{Context: "kodama-stage-k8s", Namespace: ReleaseNamespace, Verb: "exec", Resource: "pods"},
		},
		{
			name:   "copy verb",
			access: Access{Context: "kodama-stage-k8s", Namespace: ReleaseNamespace, Verb: "cp", Resource: "pods"},
		},
		{
			name:   "attach verb",
			access: Access{Context: "kodama-stage-k8s", Namespace: ReleaseNamespace, Verb: "attach", Resource: "pods"},
		},
		{
			name:   "edit verb",
			access: Access{Context: "kodama-stage-k8s", Namespace: ReleaseNamespace, Verb: "edit", Resource: "deployments"},
		},
		{
			name:   "proxy verb",
			access: Access{Context: "kodama-stage-k8s", Namespace: ReleaseNamespace, Verb: "proxy", Resource: "services"},
		},
		{
			name:   "auth verb",
			access: Access{Context: "kodama-stage-k8s", Namespace: ReleaseNamespace, Verb: "auth", Resource: "pods"},
		},
		{
			name:   "delete secret",
			access: Access{Context: "kodama-stage-k8s", Namespace: ReleaseNamespace, Verb: "delete", Resource: "secrets"},
		},
		{
			name:   "patch configmap",
			access: Access{Context: "kodama-stage-k8s", Namespace: ReleaseNamespace, Verb: "patch", Resource: "configmaps"},
		},
		{
			name:   "delete service",
			access: Access{Context: "kodama-stage-k8s", Namespace: ReleaseNamespace, Verb: "delete", Resource: "services"},
		},
		{
			name:   "delete namespace",
			access: Access{Context: "kodama-stage-k8s", Namespace: ReleaseNamespace, Verb: "delete", Resource: "namespaces"},
		},
		{
			name:   "list namespaces",
			access: Access{Context: "kodama-stage-k8s", Namespace: ReleaseNamespace, Verb: "get", Resource: "namespaces"},
		},
		{
			name:   "read a foreign namespace",
			access: Access{Context: "kodama-stage-k8s", Namespace: ReleaseNamespace, Verb: "get", Resource: "namespaces/default"},
		},
		{
			name:   "mutate an assigned namespace",
			access: Access{Context: "kodama-stage-k8s", Namespace: "kodama-tenant-tenant-a", Verb: "patch", Resource: "namespaces/kodama-tenant-tenant-a"},
		},
		{
			name:   "mutate a network policy",
			access: Access{Context: "kodama-stage-k8s", Namespace: "kodama-tenant-tenant-a", Verb: "delete", Resource: "networkpolicies"},
		},
		{
			name:   "create a claim in a tenant namespace",
			access: Access{Context: "kodama-stage-k8s", Namespace: "kodama-tenant-tenant-a", Verb: "create", Resource: "persistentvolumeclaims"},
		},
		{
			name:   "patch a claim in a tenant namespace",
			access: Access{Context: "kodama-stage-k8s", Namespace: "kodama-tenant-tenant-a", Verb: "patch", Resource: "persistentvolumeclaims"},
		},
		{
			name:   "delete a claim in the release namespace",
			access: Access{Context: "kodama-stage-k8s", Namespace: ReleaseNamespace, Verb: "delete", Resource: "persistentvolumeclaims"},
		},
	}
	for _, testCase := range refused {
		t.Run(testCase.name, func(t *testing.T) {
			if err := guard.Admit(testCase.access); err == nil {
				t.Fatalf("access %+v was admitted", testCase.access)
			}
		})
	}

	for _, executable := range []string{"tofu", "terraform", "scw", "aws", "gcloud", "az", "/usr/bin/tofu"} {
		if err := guard.CheckExecutable(executable); err == nil {
			t.Errorf("cloud or infrastructure executable %q was accepted", executable)
		}
	}
	for _, executable := range []string{"kubectl", "/usr/local/bin/kubectl", "kubectl.exe"} {
		if err := guard.CheckExecutable(executable); err != nil {
			t.Errorf("kubectl executable %q was refused: %v", executable, err)
		}
	}
}

// TestHarnessGuardClaimMutationScope pins the one claim mutation the probes may
// perform: delete in an assigned tenant namespace. kodama-system holds the Git
// keep claim and the NATS claim, so a claim create, patch or delete there - and
// any non-delete claim verb in any namespace - must stay refused.
func TestHarnessGuardClaimMutationScope(t *testing.T) {
	guard, err := NewGuard("kodama-stage-k8s", "kodama-tenant-tenant-a", "kodama-tenant-tenant-b")
	if err != nil {
		t.Fatalf("build guard: %v", err)
	}
	namespaces := []string{ReleaseNamespace, "kodama-tenant-tenant-a", "kodama-tenant-tenant-b"}
	for _, namespace := range namespaces {
		for verb := range mutationVerbs {
			access := Access{Context: "kodama-stage-k8s", Namespace: namespace, Verb: verb, Resource: "persistentvolumeclaims"}
			wantAdmit := verb == "delete" && namespace != ReleaseNamespace
			err := guard.Admit(access)
			switch {
			case wantAdmit && err != nil:
				t.Errorf("delete claim in tenant namespace %q was refused: %v", namespace, err)
			case !wantAdmit && err == nil:
				t.Errorf("claim mutation %+v was admitted; only delete in an assigned tenant namespace is allowed", access)
			}
		}
	}
	// The restriction is claim-specific: the other allowlisted workload
	// mutations keep working in the release and tenant namespaces.
	for _, access := range []Access{
		{Context: "kodama-stage-k8s", Namespace: ReleaseNamespace, Verb: "create", Resource: "jobs"},
		{Context: "kodama-stage-k8s", Namespace: ReleaseNamespace, Verb: "patch", Resource: "deployments"},
		{Context: "kodama-stage-k8s", Namespace: "kodama-tenant-tenant-a", Verb: "patch", Resource: "jobs"},
		{Context: "kodama-stage-k8s", Namespace: "kodama-tenant-tenant-b", Verb: "delete", Resource: "pods"},
	} {
		if err := guard.Admit(access); err != nil {
			t.Errorf("workload mutation %+v was refused: %v", access, err)
		}
	}
}

// TestHarnessGuardConstruction proves malformed assigned scopes are refused
// before any kubectl command can be built.
func TestHarnessGuardConstruction(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		context    string
		namespaces []string
	}{
		{name: "empty context"},
		{name: "flag context", context: "--namespace=other"},
		{name: "empty namespace", context: "ctx", namespaces: []string{""}},
		{name: "bad namespace", context: "ctx", namespaces: []string{"Bad_Namespace"}},
		{name: "duplicate namespace", context: "ctx", namespaces: []string{"tenant-a", "tenant-a"}},
		{name: "kube system namespace", context: "ctx", namespaces: []string{"kube-system"}},
		{name: "kube public namespace", context: "ctx", namespaces: []string{"kube-public"}},
		{name: "kube node lease namespace", context: "ctx", namespaces: []string{"kube-node-lease"}},
		{name: "default namespace", context: "ctx", namespaces: []string{"default"}},
		{name: "release namespace duplicated", context: "ctx", namespaces: []string{ReleaseNamespace}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := NewGuard(testCase.context, testCase.namespaces...)
			if err == nil {
				t.Fatalf("guard %q %v was constructed", testCase.context, testCase.namespaces)
			}
			if testCase.context != "" && !strings.Contains(err.Error(), testCase.context) && len(testCase.namespaces) == 0 {
				t.Fatalf("error %q does not name the context", err)
			}
		})
	}
}
