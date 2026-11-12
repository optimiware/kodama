//go:build component

// Static facts about what the real deployment roots output.
//
// The secrets qualifiers run scripts/bootstrap-secrets against fake cloud CLIs.
// A fake can only be trusted if it serves exactly what the real deploy/<cloud>
// roots produce, so this file reads the three outputs.tf files and the shared
// deploy/modules/context/main.tf as text (never `tofu plan` or `apply`) and
// pins, per cloud:
//
//   - the KMS adapter and key-reference prefix in deploy/modules/context/main.tf,
//   - that the deployment_descriptor local carries kms.{key_adapter,key_id,key_ref}
//     and wires the adapter/prefix to that shared context module,
//   - whether each root declares the database_ca_certificate output, and
//   - whether each root declares the kms_credentials output.
//
// The expected table is duplicated on purpose: a changed root fails here until
// the expectation and the secrets fixtures are updated together.
//
// Three related facts are pinned positively elsewhere:
//
// deploy/aws exports database_ca_certificate from an operator-supplied public
// RDS CA bundle, so the AWS secrets subtest runs the success path and the
// missing-output refusal is pinned by
// test/component/deploy/aws_ca_component_test.go.
//
// The chart's kodama-kms-credentials Secret requirement follows the key
// adapter (a static Secret for scw-kms, a workload-identity ServiceAccount
// annotation and no Secret for aws-kms and gcp-kms); the positive pins are in
// kms_credential_mode_component_test.go, which also keeps the kms_credentials
// output facts below and scwOnlyBootstrapSecret in use.
//
// The production entry selects the adapter from KODAMA_KEY_ADAPTER;
// TestProductionEntrySelectsEveryDeployedKeyAdapter pins that every adapter a
// root deploys is one the entry and the shell both build.
package deploy

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// databaseCACertificateUnavailable is the exact refusal scripts/bootstrap-secrets
// prints when the applied root has no database_ca_certificate output. The AWS
// missing-output subtest in aws_ca_component_test.go pins it.
const databaseCACertificateUnavailable = "OpenTofu output database_ca_certificate is unavailable; run this after a successful apply"

// deploymentOutputFacts is one cloud's statically parsed deployment facts.
type deploymentOutputFacts struct {
	cloud                 string
	keyAdapter            string
	keyRefPrefix          string
	databaseCACertificate bool
	kmsCredentials        bool
}

// expectedDeploymentFacts is the explicit inventory of what the real roots must
// output. All three roots export database_ca_certificate; AWS supplies the
// operator's public RDS CA bundle.
var expectedDeploymentFacts = []deploymentOutputFacts{
	{cloud: "scw", keyAdapter: "scw-kms", keyRefPrefix: "scw-kms:", databaseCACertificate: true, kmsCredentials: true},
	{cloud: "aws", keyAdapter: "aws-kms", keyRefPrefix: "aws-kms:", databaseCACertificate: true, kmsCredentials: false},
	{cloud: "gcp", keyAdapter: "gcp-kms", keyRefPrefix: "gcp-kms:", databaseCACertificate: true, kmsCredentials: false},
}

// readRepoFile reads one repository file or fails the test.
func readRepoFile(t *testing.T, root, relative string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
	if err != nil {
		t.Fatalf("read %s: %v", relative, err)
	}
	return string(body)
}

// hclAssignmentBlock returns the body of an HCL assignment `name = { ... }`.
// It scans balanced braces while skipping quoted strings and # comments, so a
// nested block (kms inside deployment_descriptor) is returned whole and a brace
// inside an interpolation string cannot end it early.
func hclAssignmentBlock(body, name string) (string, bool) {
	opener := regexp.MustCompile(`(?m)^[ \t]*` + regexp.QuoteMeta(name) + `[ \t]*=[ \t]*\{`)
	loc := opener.FindStringIndex(body)
	if loc == nil {
		return "", false
	}
	start := loc[1]
	depth := 1
	inString := false
	escaped := false
	for index := start; index < len(body); index++ {
		character := body[index]
		if inString {
			switch {
			case escaped:
				escaped = false
			case character == '\\':
				escaped = true
			case character == '"':
				inString = false
			}
			continue
		}
		switch character {
		case '"':
			inString = true
		case '#':
			for index < len(body) && body[index] != '\n' {
				index++
			}
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return body[start:index], true
			}
		}
	}
	return "", false
}

// hclStringAssignments parses the `key = "value"` lines of an object block.
func hclStringAssignments(block string) map[string]string {
	assignment := regexp.MustCompile(`(?m)^[ \t]*([A-Za-z0-9_-]+)[ \t]*=[ \t]*"([^"]*)"[ \t]*$`)
	values := make(map[string]string)
	for _, match := range assignment.FindAllStringSubmatch(block, -1) {
		values[match[1]] = match[2]
	}
	return values
}

// hclOutputDeclared reports whether a root declares `output "name" {`.
func hclOutputDeclared(body, name string) bool {
	declaration := regexp.MustCompile(`(?m)^[ \t]*output[ \t]+"` + regexp.QuoteMeta(name) + `"[ \t]*\{`)
	return declaration.MatchString(body)
}

// hclHasAssignment reports whether block assigns name with any expression.
func hclHasAssignment(block, name string) bool {
	assignment := regexp.MustCompile(`(?m)^[ \t]*` + regexp.QuoteMeta(name) + `[ \t]*=`)
	return assignment.MatchString(block)
}

// readDeploymentFacts parses the shared context mapping and the three roots.
// It fails if the expected structure is not found, because a silently empty
// fact would make the secrets qualifiers vacuous.
func readDeploymentFacts(t *testing.T) map[string]deploymentOutputFacts {
	t.Helper()
	root := repoRoot(t)
	contextBody := readRepoFile(t, root, "deploy/modules/context/main.tf")

	adapterBlock, ok := hclAssignmentBlock(contextBody, "key_adapter_by_cloud")
	if !ok {
		t.Fatal("deploy/modules/context/main.tf has no key_adapter_by_cloud mapping")
	}
	prefixBlock, ok := hclAssignmentBlock(contextBody, "key_ref_prefix_by_cloud")
	if !ok {
		t.Fatal("deploy/modules/context/main.tf has no key_ref_prefix_by_cloud mapping")
	}
	adapters := hclStringAssignments(adapterBlock)
	prefixes := hclStringAssignments(prefixBlock)

	facts := make(map[string]deploymentOutputFacts, len(expectedDeploymentFacts))
	for _, cloud := range []string{"scw", "aws", "gcp"} {
		adapter, ok := adapters[cloud]
		if !ok {
			t.Fatalf("key_adapter_by_cloud has no %s entry:\n%s", cloud, adapterBlock)
		}
		prefix, ok := prefixes[cloud]
		if !ok {
			t.Fatalf("key_ref_prefix_by_cloud has no %s entry:\n%s", cloud, prefixBlock)
		}
		outputs := readRepoFile(t, root, "deploy/"+cloud+"/outputs.tf")
		facts[cloud] = deploymentOutputFacts{
			cloud:                 cloud,
			keyAdapter:            adapter,
			keyRefPrefix:          prefix,
			databaseCACertificate: hclOutputDeclared(outputs, "database_ca_certificate"),
			kmsCredentials:        hclOutputDeclared(outputs, "kms_credentials"),
		}
	}
	return facts
}

// TestDeploymentDescriptorOutputsMatchCloudFacts pins the real KMS adapter,
// key-reference prefix and output inventory per cloud. It runs no tofu command:
// these are text facts, so the secrets fixtures below can agree with the roots
// without a cloud or a state file.
func TestDeploymentDescriptorOutputsMatchCloudFacts(t *testing.T) {
	root := repoRoot(t)
	facts := readDeploymentFacts(t)

	for _, want := range expectedDeploymentFacts {
		t.Run(want.cloud, func(t *testing.T) {
			got, ok := facts[want.cloud]
			if !ok {
				t.Fatalf("deploy/%s facts were not parsed", want.cloud)
			}
			if got.keyAdapter != want.keyAdapter {
				t.Errorf("deploy/modules/context/main.tf key_adapter_by_cloud[%s] = %q, want %q", want.cloud, got.keyAdapter, want.keyAdapter)
			}
			if got.keyRefPrefix != want.keyRefPrefix {
				t.Errorf("deploy/modules/context/main.tf key_ref_prefix_by_cloud[%s] = %q, want %q", want.cloud, got.keyRefPrefix, want.keyRefPrefix)
			}
			if got.databaseCACertificate != want.databaseCACertificate {
				t.Errorf("deploy/%s/outputs.tf database_ca_certificate declared = %t, want %t (if this changed, update expectedDeploymentFacts)", want.cloud, got.databaseCACertificate, want.databaseCACertificate)
			}
			if got.kmsCredentials != want.kmsCredentials {
				t.Errorf("deploy/%s/outputs.tf kms_credentials declared = %t, want %t (if this changed, update expectedDeploymentFacts and kmsCredentialModes)", want.cloud, got.kmsCredentials, want.kmsCredentials)
			}

			outputs := readRepoFile(t, root, "deploy/"+want.cloud+"/outputs.tf")
			if !hclOutputDeclared(outputs, "deployment_descriptor") {
				t.Fatalf("deploy/%s/outputs.tf does not declare the deployment_descriptor output", want.cloud)
			}
			descriptor, ok := hclAssignmentBlock(outputs, "deployment_descriptor")
			if !ok {
				t.Fatalf("deploy/%s/outputs.tf has no deployment_descriptor local", want.cloud)
			}
			kms, ok := hclAssignmentBlock(descriptor, "kms")
			if !ok {
				t.Fatalf("deploy/%s/outputs.tf deployment_descriptor has no kms object", want.cloud)
			}
			for _, field := range []string{"key_adapter", "key_id", "key_ref"} {
				if !hclHasAssignment(kms, field) {
					t.Errorf("deploy/%s/outputs.tf deployment_descriptor.kms does not emit %s", want.cloud, field)
				}
			}
			// The adapter and reference prefix must come from the one shared
			// mapping these tests pin, not a second per-root copy.
			if !regexp.MustCompile(`(?m)^[ \t]*key_adapter[ \t]*=[ \t]*module\.context\.key_adapter[ \t]*$`).MatchString(kms) {
				t.Errorf("deploy/%s/outputs.tf deployment_descriptor.kms.key_adapter is not module.context.key_adapter", want.cloud)
			}
			if !strings.Contains(outputs, "module.context.key_ref_prefix") {
				t.Errorf("deploy/%s/outputs.tf kms_key_ref does not use module.context.key_ref_prefix", want.cloud)
			}
		})
	}
}

// TestProductionEntrySelectsEveryDeployedKeyAdapter pins that no root deploys a
// key adapter the server cannot build: the production entry accepts the
// adapter and the shell has a provider for it. The behaviour itself is covered
// by cmd/kodama (TestBuildProductionSelectsTheKeyAdapter).
func TestProductionEntrySelectsEveryDeployedKeyAdapter(t *testing.T) {
	root := repoRoot(t)
	entry := readRepoFile(t, root, "cmd/kodama/production.go")
	shell := readRepoFile(t, root, "internal/shell/app/production.go")
	constants := map[string]string{"scw-kms": "KeyScalewayKMS", "aws-kms": "KeyAWSKMS", "gcp-kms": "KeyGCPKMS"}

	accepted := regexp.MustCompile(`(?m)^[ \t]*case ([^\n]*):\n[ \t]*return adapter, nil`).FindStringSubmatch(entry)
	if accepted == nil {
		t.Fatal("cmd/kodama/production.go has no key adapter selection that returns the accepted adapter")
	}
	for _, want := range expectedDeploymentFacts {
		constant, ok := constants[want.keyAdapter]
		if !ok {
			t.Fatalf("deploy/%s deploys key adapter %q, which this test does not know", want.cloud, want.keyAdapter)
		}
		if !strings.Contains(accepted[1], "installation."+constant) {
			t.Errorf("cmd/kodama/production.go does not accept %s, which deploy/%s deploys", want.keyAdapter, want.cloud)
		}
		if !strings.Contains(entry, "case installation."+constant+":") {
			t.Errorf("cmd/kodama/production.go builds no KMS client for %s", want.keyAdapter)
		}
		if !strings.Contains(shell, "case installation."+constant+":") {
			t.Errorf("internal/shell/app/production.go has no key provider for %s", want.keyAdapter)
		}
	}
	if regexp.MustCompile(`(?m)Keys:[ \t]+installation\.Key[A-Za-z]+KMS`).MatchString(entry) {
		t.Error("cmd/kodama/production.go hard-codes one key adapter instead of the selected one")
	}
}

// scwOnlyBootstrapSecret reports whether the named Secret creation still sits
// inside the `if [[ "$cloud" == "scw" ]]` branch: the nearest preceding scw
// branch must not have been closed by `fi` before the creation.
func scwOnlyBootstrapSecret(body, creation string) bool {
	before, _, ok := strings.Cut(body, creation)
	if !ok {
		return false
	}
	prefix := before
	branch := strings.LastIndex(prefix, `if [[ "$cloud" == "scw" ]]; then`)
	if branch < 0 {
		return false
	}
	return !strings.Contains(prefix[branch:], "\nfi\n")
}
