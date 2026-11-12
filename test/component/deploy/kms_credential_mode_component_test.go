//go:build component

// The cloud KMS credential mode per key adapter (deployment parity for AWS and
// GCP KMS credentials).
//
//   - scw-kms authenticates with a static IAM API key. The Scaleway root
//     exports kms_credentials, scripts/bootstrap-secrets creates
//     kodama-kms-credentials, scripts/deploy names it in the values, and the
//     chart requires it in production and reads it through a non-optional
//     envFrom.
//   - aws-kms authenticates with EKS IRSA and gcp-kms with GKE Workload
//     Identity. Neither root exports a static credential, bootstrap-secrets
//     creates no credential Secret, scripts/deploy renders the ServiceAccount
//     annotation instead, and the chart renders no reference to the Secret but
//     refuses a production render whose chart-created ServiceAccount lacks the
//     annotation.
//
// The chart must not demand the Scaleway-only Secret for every cloud. The rule
// lives in two layers, values.schema.json and templates/_extraenv.tpl; each
// layer is proven alone and both must give the same verdict. helm runs locally
// against a test-owned copy of the charts; nothing contacts a cluster, a cloud
// or the network.
package deploy

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

const (
	kmsCredentialsSecretName    = "kodama-kms-credentials"
	awsRoleARNAnnotation        = "eks.amazonaws.com/role-arn"
	gcpServiceAccountAnnotation = "iam.gke.io/gcp-service-account"
)

// kmsCredentialMode is one production key adapter's credential mode.
type kmsCredentialMode struct {
	cloud   string
	adapter string
	// staticSecret is true when the adapter reads a static credential from
	// kodama-kms-credentials; false when it uses the pod's workload identity.
	staticSecret bool
	// annotation is the ServiceAccount annotation a workload-identity adapter
	// needs, and identity an example of its value.
	annotation string
	identity   string
}

// kmsCredentialModes is the explicit per-cloud inventory. A cloud added to
// expectedDeploymentFacts without an entry here fails
// TestKMSCredentialModeMatchesRootsAndBootstrap.
var kmsCredentialModes = []kmsCredentialMode{
	{cloud: "scw", adapter: "scw-kms", staticSecret: true},
	{cloud: "aws", adapter: "aws-kms", annotation: awsRoleARNAnnotation, identity: "arn:aws:iam::123456789012:role/kodama-stage-kms-controller"},
	{cloud: "gcp", adapter: "gcp-kms", annotation: gcpServiceAccountAnnotation, identity: "kodama-stage-ctrl@demo-project.iam.gserviceaccount.com"},
}

// adapterArgs selects the key adapter. prefix is "" for the kodama chart and
// "kodama." for the stack chart.
func adapterArgs(prefix, adapter string) []string {
	return []string{"--set", prefix + "config.keyAdapter=" + adapter}
}

// serviceAccountAnnotationArgs sets serviceAccount.annotations as one JSON
// object, so a dotted annotation name and an empty value need no escaping.
func serviceAccountAnnotationArgs(t *testing.T, prefix string, annotations map[string]string) []string {
	t.Helper()
	encoded, err := json.Marshal(annotations)
	if err != nil {
		t.Fatalf("encode annotations: %v", err)
	}
	return []string{"--set-json", prefix + "serviceAccount.annotations=" + string(encoded)}
}

// withoutKMSCredentialsArgs removes the chart default that names the Secret.
func withoutKMSCredentialsArgs(prefix string) []string {
	return []string{"--set-json", prefix + "secrets.kmsCredentials=null"}
}

// joinArgs concatenates argument groups.
func joinArgs(groups ...[]string) []string {
	var args []string
	for _, group := range groups {
		args = append(args, group...)
	}
	return args
}

// stageKodamaChartSchemaOnly stages the kodama chart without the template
// guard, so only values.schema.json can refuse a render.
func stageKodamaChartSchemaOnly(t *testing.T) (string, string) {
	t.Helper()
	helm, charts := stagedCharts(t)
	chart := filepath.Join(charts, "kodama")
	deploymentPath := filepath.Join(chart, "templates", "deployment.yaml")
	body := readFileForTest(t, deploymentPath)
	withoutInclude := strings.Replace(body, "{{- include \"kodama.validateExtraEnv\" . }}\n", "", 1)
	if withoutInclude == body {
		t.Fatal("staged deployment.yaml does not include the kodama.validateExtraEnv guard")
	}
	rewriteStagedFile(t, deploymentPath, withoutInclude)
	if err := os.Remove(filepath.Join(chart, "templates", "_extraenv.tpl")); err != nil {
		t.Fatalf("remove staged guard template: %v", err)
	}
	return helm, chart
}

// stageKodamaChartTemplateOnly stages the kodama chart without the schema's
// conditional rules, so only templates/_extraenv.tpl can refuse a render.
func stageKodamaChartTemplateOnly(t *testing.T) (string, string) {
	t.Helper()
	helm, charts := stagedCharts(t)
	chart := filepath.Join(charts, "kodama")
	schemaPath := filepath.Join(chart, "values.schema.json")
	schema := map[string]any{}
	if err := json.Unmarshal([]byte(readFileForTest(t, schemaPath)), &schema); err != nil {
		t.Fatalf("decode staged schema: %v", err)
	}
	if _, ok := schema["allOf"]; !ok {
		t.Fatal("staged schema has no allOf conditions to remove")
	}
	delete(schema, "allOf")
	encoded, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		t.Fatalf("encode staged schema: %v", err)
	}
	rewriteStagedFile(t, schemaPath, string(encoded))
	return helm, chart
}

// podSpecOf returns the pod template spec of a rendered workload.
func podSpecOf(t *testing.T, object map[string]any) map[string]any {
	t.Helper()
	return objectAtKey(t, objectAtKey(t, objectAtKey(t, object, "spec"), "template"), "spec")
}

// assertNoCredentialSecretReference proves a rendered release carries no
// reference to the static KMS credential Secret in any object, that the
// control-plane container has no envFrom source at all, and that no container
// sets a cloud variable. The EKS pod identity webhook injects AWS_ROLE_ARN,
// AWS_WEB_IDENTITY_TOKEN_FILE and AWS_REGION itself and skips a name the
// container already sets, so the chart must not render one.
func assertNoCredentialSecretReference(t *testing.T, rendered string) {
	t.Helper()
	if strings.Contains(rendered, kmsCredentialsSecretName) {
		t.Errorf("rendered output references %s:\n%s", kmsCredentialsSecretName, rendered)
	}
	server := workloadContainer(t, renderedObjectNamed(t, rendered, "Deployment", "kodama"), "kodama")
	if envFrom, present := server["envFrom"]; present {
		t.Errorf("control-plane container has an envFrom source: %v", envFrom)
	}
	cloudVariable := regexp.MustCompile(`^(SCW|AWS|GOOGLE)_`)
	for _, object := range renderedObjects(t, rendered) {
		for _, container := range containersOf(t, object) {
			env, _ := container["env"].([]any)
			for _, entry := range env {
				environment, ok := entry.(map[string]any)
				if !ok {
					t.Fatalf("env entry is not an object")
				}
				if name := stringAtKey(t, environment, "name"); cloudVariable.MatchString(name) {
					t.Errorf("%v container %v sets the cloud variable %s", object["kind"], container["name"], name)
				}
			}
		}
	}
}

// assertWorkloadIdentityPod proves the control-plane pod runs under the
// annotated ServiceAccount and can read a projected identity token: the pod
// uses the ServiceAccount the chart renders, in the release namespace, does not
// disable the ServiceAccount token, and sets fsGroup to its non-root user (the
// projected IRSA token is otherwise readable by root only).
func assertWorkloadIdentityPod(t *testing.T, rendered string, mode kmsCredentialMode) {
	t.Helper()
	serviceAccount := renderedObjectNamed(t, rendered, "ServiceAccount", "kodama")
	metadata := objectAtKey(t, serviceAccount, "metadata")
	if got := stringAtKey(t, metadata, "namespace"); got != "kodama-system" {
		t.Errorf("ServiceAccount namespace = %q, want the release namespace kodama-system", got)
	}
	annotations := objectAtKey(t, metadata, "annotations")
	if got := annotations[mode.annotation]; got != mode.identity {
		t.Errorf("ServiceAccount annotation %s = %v, want %s", mode.annotation, got, mode.identity)
	}

	deployment := renderedObjectNamed(t, rendered, "Deployment", "kodama")
	pod := podSpecOf(t, deployment)
	if got := stringAtKey(t, pod, "serviceAccountName"); got != "kodama" {
		t.Errorf("control-plane serviceAccountName = %q, want the annotated ServiceAccount kodama", got)
	}
	if automount, present := pod["automountServiceAccountToken"]; present && automount == false {
		t.Error("control-plane pod disables its ServiceAccount token")
	}
	securityContext := objectAtKey(t, pod, "securityContext")
	runAsUser, ok := intAtKey(securityContext, "runAsUser")
	if !ok || runAsUser == 0 {
		t.Fatalf("control-plane pod runAsUser = %v, want a non-root user", securityContext["runAsUser"])
	}
	if fsGroup, ok := intAtKey(securityContext, "fsGroup"); !ok || fsGroup != runAsUser {
		t.Errorf("control-plane pod fsGroup = %v, want %d so the non-root user can read the projected identity token", securityContext["fsGroup"], runAsUser)
	}
	if got := stringAtKey(t, renderedEnvEntry(t, deployment, "KODAMA_KEY_ADAPTER"), "value"); got != mode.adapter {
		t.Errorf("KODAMA_KEY_ADAPTER = %q, want %s", got, mode.adapter)
	}
	// The KEK blob is still mounted: workload identity replaces the
	// credential, not the wrapped key.
	if got := stringAtKey(t, objectAtKey(t, renderedVolume(t, deployment, "keys"), "secret"), "secretName"); got != "kodama-keys" {
		t.Errorf("keys volume reads Secret %q, want kodama-keys", got)
	}
}

// TestKMSCredentialModeMatchesRootsAndBootstrap pins the static facts behind
// the per-adapter rule: only the root of a static-credential adapter exports
// kms_credentials, and scripts/bootstrap-secrets creates
// kodama-kms-credentials in the scw branch alone. If either changes, the
// chart rule and this inventory must change with it.
func TestKMSCredentialModeMatchesRootsAndBootstrap(t *testing.T) {
	facts := readDeploymentFacts(t)
	modes := make(map[string]kmsCredentialMode, len(kmsCredentialModes))
	for _, mode := range kmsCredentialModes {
		modes[mode.cloud] = mode
	}
	for _, expected := range expectedDeploymentFacts {
		mode, ok := modes[expected.cloud]
		if !ok {
			t.Errorf("kmsCredentialModes has no entry for %s; decide its credential mode", expected.cloud)
			continue
		}
		fact := facts[expected.cloud]
		if fact.keyAdapter != mode.adapter {
			t.Errorf("deploy/modules/context/main.tf key_adapter_by_cloud[%s] = %q, want %q", mode.cloud, fact.keyAdapter, mode.adapter)
		}
		if fact.kmsCredentials != mode.staticSecret {
			t.Errorf("deploy/%s/outputs.tf kms_credentials declared = %t, but the %s adapter's static-credential mode is %t", mode.cloud, fact.kmsCredentials, mode.adapter, mode.staticSecret)
		}
		if mode.staticSecret == (mode.annotation != "") {
			t.Errorf("%s must have exactly one credential mode: a static Secret or a workload-identity annotation", mode.adapter)
		}
	}

	bootstrap := readRepoFile(t, repoRoot(t), "scripts/bootstrap-secrets")
	if !scwOnlyBootstrapSecret(bootstrap, "create secret generic "+kmsCredentialsSecretName) {
		t.Errorf("scripts/bootstrap-secrets no longer creates %s only in the scw branch; update kmsCredentialModes and the chart rule", kmsCredentialsSecretName)
	}
}

// TestKodamaChartWorkloadIdentityAdaptersRenderNoCredentialSecret proves a
// production aws-kms or gcp-kms installation renders without the static
// credential Secret and references it nowhere, whether the value is removed or
// left at the chart default, for the kodama chart and through the stack chart.
// The chart renders no Job, so the control-plane Deployment is the only
// workload that loads the keys.
func TestKodamaChartWorkloadIdentityAdaptersRenderNoCredentialSecret(t *testing.T) {
	helm, charts := stagedCharts(t)
	chart := filepath.Join(charts, "kodama")
	stackHelm, stack := stageStackChart(t)

	for _, mode := range kmsCredentialModes {
		if mode.staticSecret {
			continue
		}
		annotations := map[string]string{mode.annotation: mode.identity}
		for _, testCase := range []struct {
			name    string
			helm    string
			release string
			chart   string
			args    []string
		}{
			{
				name:    "without secrets.kmsCredentials",
				helm:    helm,
				release: "kodama",
				chart:   chart,
				args: kodamaProductionArgs(joinArgs(
					adapterArgs("", mode.adapter),
					serviceAccountAnnotationArgs(t, "", annotations),
					withoutKMSCredentialsArgs(""),
				)...),
			},
			{
				name:    "default secrets.kmsCredentials is ignored",
				helm:    helm,
				release: "kodama",
				chart:   chart,
				args: kodamaProductionArgs(joinArgs(
					adapterArgs("", mode.adapter),
					serviceAccountAnnotationArgs(t, "", annotations),
				)...),
			},
			{
				name:    "stack chart without secrets.kmsCredentials",
				helm:    stackHelm,
				release: "kodama-stack",
				chart:   stack,
				args: stackProductionArgs(joinArgs(
					adapterArgs("kodama.", mode.adapter),
					serviceAccountAnnotationArgs(t, "kodama.", annotations),
					withoutKMSCredentialsArgs("kodama."),
				)...),
			},
			{
				name:    "stack chart default secrets.kmsCredentials is ignored",
				helm:    stackHelm,
				release: "kodama-stack",
				chart:   stack,
				args: stackProductionArgs(joinArgs(
					adapterArgs("kodama.", mode.adapter),
					serviceAccountAnnotationArgs(t, "kodama.", annotations),
				)...),
			},
		} {
			t.Run(mode.adapter+"/"+testCase.name, func(t *testing.T) {
				helmLint(t, testCase.helm, testCase.chart, testCase.args...)
				rendered, err := renderChartArgs(t, testCase.helm, testCase.release, testCase.chart, testCase.args...)
				if err != nil {
					t.Fatalf("helm template refused a production %s render: %v\n%s", mode.adapter, err, rendered)
				}
				assertNoCredentialSecretReference(t, rendered)
				assertNoRenderedJob(t, rendered)
				assertWorkloadIdentityPod(t, rendered, mode)
			})
		}
	}
}

// TestKodamaChartStaticCredentialAdapterKeepsRequiredSecret proves the
// Scaleway rule is unchanged: a production scw-kms render reads
// kodama-kms-credentials through exactly one non-optional envFrom and needs no
// ServiceAccount annotation.
func TestKodamaChartStaticCredentialAdapterKeepsRequiredSecret(t *testing.T) {
	helm, charts := stagedCharts(t)
	chart := filepath.Join(charts, "kodama")
	for _, mode := range kmsCredentialModes {
		if !mode.staticSecret {
			continue
		}
		t.Run(mode.adapter, func(t *testing.T) {
			rendered, err := renderChartArgs(t, helm, "kodama", chart, kodamaProductionArgs(adapterArgs("", mode.adapter)...)...)
			if err != nil {
				t.Fatalf("helm template refused a production %s render: %v\n%s", mode.adapter, err, rendered)
			}
			assertKMSCredentialsEnvFrom(t, renderedDocument(t, rendered, "Deployment", "kodama"))
			server := workloadContainer(t, renderedObjectNamed(t, rendered, "Deployment", "kodama"), "kodama")
			envFrom, ok := server["envFrom"].([]any)
			if !ok || len(envFrom) != 1 {
				t.Fatalf("rendered envFrom = %v, want exactly the KMS credentials Secret", server["envFrom"])
			}
			secretRef := objectAtKey(t, envFrom[0].(map[string]any), "secretRef")
			if got := stringAtKey(t, secretRef, "name"); got != kmsCredentialsSecretName {
				t.Errorf("envFrom reads %q, want %s", got, kmsCredentialsSecretName)
			}
			if _, present := secretRef["optional"]; present {
				t.Errorf("production %s envFrom carries an optional flag: %v", mode.adapter, secretRef)
			}
			serviceAccount := renderedObjectNamed(t, rendered, "ServiceAccount", "kodama")
			if annotations, present := objectAtKey(t, serviceAccount, "metadata")["annotations"]; present {
				t.Errorf("%s ServiceAccount renders annotations without any configured: %v", mode.adapter, annotations)
			}
		})
	}
}

// TestKodamaChartKMSCredentialRuleSchemaAndTemplateAgree runs every credential
// case through three charts: the real one, one with the template guard removed
// (the schema decides alone) and one with the schema conditions removed (the
// template guard decides alone). All three must give the expected verdict, so
// neither layer is weaker than the other and a renderer that skips schema
// validation is still refused.
func TestKodamaChartKMSCredentialRuleSchemaAndTemplateAgree(t *testing.T) {
	helm, charts := stagedCharts(t)
	full := filepath.Join(charts, "kodama")
	_, schemaOnly := stageKodamaChartSchemaOnly(t)
	_, templateOnly := stageKodamaChartTemplateOnly(t)

	const (
		secretGuard     = "production requires secrets.kmsCredentials.existingSecret"
		annotationGuard = "production requires serviceAccount.annotations["
	)
	awsIdentity := map[string]string{awsRoleARNAnnotation: "arn:aws:iam::123456789012:role/kodama-stage-kms-controller"}
	gcpIdentity := map[string]string{gcpServiceAccountAnnotation: "kodama-stage-ctrl@demo-project.iam.gserviceaccount.com"}
	externalServiceAccount := []string{"--set", "serviceAccount.create=false"}

	for _, testCase := range []struct {
		name string
		args []string
		// For a refused render, wantSchema is the property the schema names
		// and wantGuard the template guard message.
		refused    bool
		wantSchema string
		wantGuard  string
	}{
		{
			name: "scw-kms with the credential Secret",
			args: adapterArgs("", "scw-kms"),
		},
		{
			name:       "scw-kms without the credential Secret",
			args:       joinArgs(adapterArgs("", "scw-kms"), withoutKMSCredentialsArgs("")),
			refused:    true,
			wantSchema: "kmsCredentials",
			wantGuard:  secretGuard,
		},
		{
			name:       "scw-kms without the Secret is not excused by an identity annotation",
			args:       joinArgs(adapterArgs("", "scw-kms"), withoutKMSCredentialsArgs(""), serviceAccountAnnotationArgs(t, "", awsIdentity)),
			refused:    true,
			wantSchema: "kmsCredentials",
			wantGuard:  secretGuard,
		},
		{
			name:       "an unknown adapter keeps the Secret requirement",
			args:       joinArgs(adapterArgs("", "aws_kms"), withoutKMSCredentialsArgs(""), serviceAccountAnnotationArgs(t, "", awsIdentity)),
			refused:    true,
			wantSchema: "kmsCredentials",
			wantGuard:  secretGuard,
		},
		{
			name: "aws-kms with the IRSA annotation and no Secret",
			args: joinArgs(adapterArgs("", "aws-kms"), withoutKMSCredentialsArgs(""), serviceAccountAnnotationArgs(t, "", awsIdentity)),
		},
		{
			name: "aws-kms with the IRSA annotation and the default Secret value",
			args: joinArgs(adapterArgs("", "aws-kms"), serviceAccountAnnotationArgs(t, "", awsIdentity)),
		},
		{
			name:       "aws-kms without an annotation",
			args:       joinArgs(adapterArgs("", "aws-kms"), withoutKMSCredentialsArgs("")),
			refused:    true,
			wantSchema: awsRoleARNAnnotation,
			wantGuard:  annotationGuard + `"` + awsRoleARNAnnotation + `"]`,
		},
		{
			name:       "aws-kms with the credential Secret but no annotation",
			args:       adapterArgs("", "aws-kms"),
			refused:    true,
			wantSchema: awsRoleARNAnnotation,
			wantGuard:  annotationGuard + `"` + awsRoleARNAnnotation + `"]`,
		},
		{
			name:    "aws-kms with an empty annotation",
			args:    joinArgs(adapterArgs("", "aws-kms"), serviceAccountAnnotationArgs(t, "", map[string]string{awsRoleARNAnnotation: ""})),
			refused: true,
			// The schema reports the annotation as an escaped JSON pointer
			// (the "/" in its name), so the stable part is the keyword.
			wantSchema: "minLength",
			wantGuard:  annotationGuard + `"` + awsRoleARNAnnotation + `"]`,
		},
		{
			name:       "aws-kms with only the GKE annotation",
			args:       joinArgs(adapterArgs("", "aws-kms"), serviceAccountAnnotationArgs(t, "", gcpIdentity)),
			refused:    true,
			wantSchema: awsRoleARNAnnotation,
			wantGuard:  annotationGuard + `"` + awsRoleARNAnnotation + `"]`,
		},
		{
			name: "aws-kms with an externally managed ServiceAccount",
			args: joinArgs(adapterArgs("", "aws-kms"), withoutKMSCredentialsArgs(""), externalServiceAccount),
		},
		{
			name: "gcp-kms with the Workload Identity annotation and no Secret",
			args: joinArgs(adapterArgs("", "gcp-kms"), withoutKMSCredentialsArgs(""), serviceAccountAnnotationArgs(t, "", gcpIdentity)),
		},
		{
			name: "gcp-kms with the Workload Identity annotation and the default Secret value",
			args: joinArgs(adapterArgs("", "gcp-kms"), serviceAccountAnnotationArgs(t, "", gcpIdentity)),
		},
		{
			name:       "gcp-kms without an annotation",
			args:       joinArgs(adapterArgs("", "gcp-kms"), withoutKMSCredentialsArgs("")),
			refused:    true,
			wantSchema: gcpServiceAccountAnnotation,
			wantGuard:  annotationGuard + `"` + gcpServiceAccountAnnotation + `"]`,
		},
		{
			name:    "gcp-kms with an empty annotation",
			args:    joinArgs(adapterArgs("", "gcp-kms"), serviceAccountAnnotationArgs(t, "", map[string]string{gcpServiceAccountAnnotation: ""})),
			refused: true,
			// The schema reports the annotation as an escaped JSON pointer
			// (the "/" in its name), so the stable part is the keyword.
			wantSchema: "minLength",
			wantGuard:  annotationGuard + `"` + gcpServiceAccountAnnotation + `"]`,
		},
		{
			name:       "gcp-kms with only the EKS annotation",
			args:       joinArgs(adapterArgs("", "gcp-kms"), serviceAccountAnnotationArgs(t, "", awsIdentity)),
			refused:    true,
			wantSchema: gcpServiceAccountAnnotation,
			wantGuard:  annotationGuard + `"` + gcpServiceAccountAnnotation + `"]`,
		},
		{
			name: "gcp-kms with an externally managed ServiceAccount",
			args: joinArgs(adapterArgs("", "gcp-kms"), withoutKMSCredentialsArgs(""), externalServiceAccount),
		},
		{
			name: "a development profile needs neither the Secret nor an annotation",
			args: joinArgs([]string{"--set", "environment=development"}, adapterArgs("", "aws-kms"), withoutKMSCredentialsArgs("")),
		},
		{
			name: "a test profile needs neither the Secret nor an annotation",
			args: joinArgs([]string{"--set", "environment=test"}, adapterArgs("", "gcp-kms"), withoutKMSCredentialsArgs("")),
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			for _, layer := range []struct {
				name  string
				chart string
				want  string
			}{
				{name: "schema and template", chart: full, want: testCase.wantSchema},
				{name: "schema alone", chart: schemaOnly, want: testCase.wantSchema},
				{name: "template guard alone", chart: templateOnly, want: testCase.wantGuard},
			} {
				rendered, err := renderChartArgs(t, helm, "kodama", layer.chart, kodamaProductionArgs(testCase.args...)...)
				if !testCase.refused {
					if err != nil {
						t.Errorf("%s refused the render: %v\n%s", layer.name, err, rendered)
					}
					continue
				}
				if err == nil {
					t.Errorf("%s accepted the render", layer.name)
					continue
				}
				if !strings.Contains(rendered, layer.want) {
					t.Errorf("%s refusal does not name %q: %v\n%s", layer.name, layer.want, err, rendered)
				}
				if layer.chart != templateOnly && !strings.Contains(rendered, "schema") {
					t.Errorf("%s refusal does not come from values.schema.json: %v\n%s", layer.name, err, rendered)
				}
			}
		})
	}
}

// TestKodamaChartExternalServiceAccountSkipsAnnotationGuard proves the
// annotation guard covers only a ServiceAccount the chart creates: with
// serviceAccount.create=false the chart renders no ServiceAccount, cannot see
// the operator's annotations and must not block the render.
func TestKodamaChartExternalServiceAccountSkipsAnnotationGuard(t *testing.T) {
	helm, charts := stagedCharts(t)
	chart := filepath.Join(charts, "kodama")
	for _, mode := range kmsCredentialModes {
		if mode.staticSecret {
			continue
		}
		t.Run(mode.adapter, func(t *testing.T) {
			rendered, err := renderChartArgs(t, helm, "kodama", chart, kodamaProductionArgs(joinArgs(
				adapterArgs("", mode.adapter),
				[]string{"--set", "serviceAccount.create=false", "--set", "serviceAccount.name=kodama-external"},
			)...)...)
			if err != nil {
				t.Fatalf("helm template refused an externally managed ServiceAccount: %v\n%s", err, rendered)
			}
			if got := renderedKindCount(rendered, "ServiceAccount"); got != 0 {
				t.Errorf("serviceAccount.create=false rendered %d ServiceAccounts", got)
			}
			pod := podSpecOf(t, renderedObjectNamed(t, rendered, "Deployment", "kodama"))
			if got := stringAtKey(t, pod, "serviceAccountName"); got != "kodama-external" {
				t.Errorf("control-plane serviceAccountName = %q, want the external ServiceAccount", got)
			}
			assertNoCredentialSecretReference(t, rendered)
		})
	}
}

// TestKodamaChartNonProductionKMSReferenceStaysOptional pins the unchanged
// development and test behaviour: a named Secret is referenced with
// optional: true for every adapter, and a profile that names none renders no
// reference instead of failing.
func TestKodamaChartNonProductionKMSReferenceStaysOptional(t *testing.T) {
	helm, charts := stagedCharts(t)
	chart := filepath.Join(charts, "kodama")
	for _, profile := range []string{"development", "test"} {
		for _, mode := range kmsCredentialModes {
			t.Run(profile+"/"+mode.adapter, func(t *testing.T) {
				base := joinArgs([]string{"--set", "environment=" + profile}, adapterArgs("", mode.adapter))
				rendered, err := renderChartArgs(t, helm, "kodama", chart, base...)
				if err != nil {
					t.Fatalf("helm template refused a %s render: %v\n%s", profile, err, rendered)
				}
				server := workloadContainer(t, renderedObjectNamed(t, rendered, "Deployment", "kodama"), "kodama")
				envFrom, ok := server["envFrom"].([]any)
				if !ok || len(envFrom) != 1 {
					t.Fatalf("%s envFrom = %v, want the optional KMS credentials Secret", profile, server["envFrom"])
				}
				secretRef := objectAtKey(t, envFrom[0].(map[string]any), "secretRef")
				if got := stringAtKey(t, secretRef, "name"); got != kmsCredentialsSecretName {
					t.Errorf("%s envFrom reads %q, want %s", profile, got, kmsCredentialsSecretName)
				}
				if optional, ok := secretRef["optional"].(bool); !ok || !optional {
					t.Errorf("%s envFrom does not mark the KMS credentials optional: %v", profile, secretRef)
				}

				rendered, err = renderChartArgs(t, helm, "kodama", chart, joinArgs(base, withoutKMSCredentialsArgs(""))...)
				if err != nil {
					t.Fatalf("helm template refused a %s render without secrets.kmsCredentials: %v\n%s", profile, err, rendered)
				}
				assertNoCredentialSecretReference(t, rendered)
			})
		}
	}
}

// TestDescriptorCredentialModePerCloud runs the real
// `scripts/deploy <cloud> render-values` for each cloud descriptor and proves
// the generated values carry the cloud's credential mode: the Scaleway values
// still name kodama-kms-credentials and no annotation, the AWS and GCP values
// name no credential Secret and carry the workload-identity annotation. The
// stack chart rendered from those values then references the Secret for
// Scaleway only.
func TestDescriptorCredentialModePerCloud(t *testing.T) {
	helm, chart := stageStackChart(t)
	modes := make(map[string]kmsCredentialMode, len(kmsCredentialModes))
	for _, mode := range kmsCredentialModes {
		modes[mode.cloud] = mode
	}
	for _, testCase := range cloudSecretsCases {
		t.Run(testCase.cloud, func(t *testing.T) {
			mode, ok := modes[testCase.cloud]
			if !ok {
				t.Fatalf("kmsCredentialModes has no entry for %s", testCase.cloud)
			}
			valuesPath, values := renderDescriptorValues(t, testCase.cloud, testCase.descriptorJSON(t))
			kodama, ok := values["kodama"].(map[string]any)
			if !ok {
				t.Fatalf("rendered values have no kodama section: %v", values)
			}

			secrets := objectAtKey(t, kodama, "secrets")
			wantSecrets := []string{"broker", "database", "keys", "natsAuth", "tls"}
			if mode.staticSecret {
				wantSecrets = []string{"broker", "database", "keys", "kmsCredentials", "natsAuth", "tls"}
			}
			if got := strings.Join(sortedKeys(secrets), ","); got != strings.Join(wantSecrets, ",") {
				t.Errorf("%s rendered secrets = %s, want %s", testCase.cloud, got, strings.Join(wantSecrets, ","))
			}
			if mode.staticSecret {
				if got := stringAtKey(t, objectAtKey(t, secrets, "kmsCredentials"), "existingSecret"); got != kmsCredentialsSecretName {
					t.Errorf("%s kmsCredentials.existingSecret = %q, want %s", testCase.cloud, got, kmsCredentialsSecretName)
				}
			}

			serviceAccount := objectAtKey(t, kodama, "serviceAccount")
			if got := stringAtKey(t, serviceAccount, "name"); got != "kodama" {
				t.Errorf("%s serviceAccount.name = %q; the cloud identity binding trusts exactly kodama", testCase.cloud, got)
			}
			if create, ok := serviceAccount["create"].(bool); !ok || !create {
				t.Errorf("%s serviceAccount.create = %v, want true", testCase.cloud, serviceAccount["create"])
			}
			annotations := objectAtKey(t, serviceAccount, "annotations")
			if mode.staticSecret {
				if len(annotations) != 0 {
					t.Errorf("%s rendered ServiceAccount annotations %v, want none", testCase.cloud, annotations)
				}
			} else if len(annotations) != 1 || annotations[mode.annotation] != testCase.workloadIdentity {
				t.Errorf("%s rendered ServiceAccount annotations %v, want only %s=%s", testCase.cloud, annotations, mode.annotation, testCase.workloadIdentity)
			}

			rendered, err := renderDescriptorStack(helm, chart, valuesPath)
			if err != nil {
				t.Fatalf("helm template refused the descriptor-derived values for %s: %v\n%s", testCase.cloud, err, rendered)
			}
			if mode.staticSecret {
				assertKMSCredentialsEnvFrom(t, renderedDocument(t, rendered, "Deployment", "kodama"))
				return
			}
			assertNoCredentialSecretReference(t, rendered)
			descriptorMode := mode
			descriptorMode.identity = testCase.workloadIdentity
			assertWorkloadIdentityPod(t, rendered, descriptorMode)
		})
	}
}

// TestDescriptorWithoutWorkloadIdentityIsRefused proves an AWS or GCP
// descriptor that lost kubernetes.workload_identity_ref cannot be installed:
// scripts/deploy renders no annotation and the chart refuses the release
// instead of starting a pod that can never decrypt.
func TestDescriptorWithoutWorkloadIdentityIsRefused(t *testing.T) {
	helm, chart := stageStackChart(t)
	for _, testCase := range cloudSecretsCases {
		mode := kmsCredentialMode{}
		for _, candidate := range kmsCredentialModes {
			if candidate.cloud == testCase.cloud {
				mode = candidate
			}
		}
		if mode.staticSecret || mode.annotation == "" {
			continue
		}
		t.Run(testCase.cloud, func(t *testing.T) {
			descriptor := testCase.descriptor()
			descriptor["kubernetes"].(map[string]any)["workload_identity_ref"] = ""
			body, err := json.Marshal(descriptor)
			if err != nil {
				t.Fatalf("encode descriptor: %v", err)
			}
			valuesPath, _ := renderDescriptorValues(t, testCase.cloud, string(body))
			rendered, err := renderDescriptorStack(helm, chart, valuesPath)
			if err == nil {
				t.Fatalf("helm template accepted a %s release without a workload identity:\n%s", testCase.cloud, rendered)
			}
			if !strings.Contains(rendered, mode.annotation) {
				t.Errorf("the refusal does not name %s: %v\n%s", mode.annotation, err, rendered)
			}
		})
	}
}

// renderDescriptorValues runs `scripts/deploy <cloud> render-values` against a
// descriptor in a test-owned state directory and returns the generated values
// file and its decoded content.
func renderDescriptorValues(t *testing.T, cloud, descriptor string) (string, map[string]any) {
	t.Helper()
	stateDir := t.TempDir()
	workDir := filepath.Join(stateDir, cloud, "stage")
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", workDir, err)
	}
	writeFile(t, filepath.Join(workDir, "descriptor.json"), descriptor)

	command := exec.Command("bash", filepath.Join(repoRoot(t), "scripts", "deploy"), cloud, "render-values", "--cluster", "stage")
	command.Env = append(os.Environ(),
		"KODAMA_DEPLOY_STATE_DIR="+stateDir,
		"KODAMA_IMAGE_REPOSITORY=registry.example.com/kodama",
		"KODAMA_IMAGE_TAG=0.1.0",
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("scripts/deploy %s render-values failed: %v\n%s", cloud, err, output)
	}
	valuesPath := filepath.Join(workDir, "values.generated.yaml")
	body, err := os.ReadFile(valuesPath)
	if err != nil {
		t.Fatalf("read rendered values: %v", err)
	}
	var values map[string]any
	if err := yaml.Unmarshal(body, &values); err != nil {
		t.Fatalf("parse rendered values: %v", err)
	}
	return valuesPath, values
}

// renderDescriptorStack renders the stack chart from descriptor-derived values
// plus the production OIDC inputs, as `scripts/deploy <cloud> helm` does.
func renderDescriptorStack(helm, chart, valuesPath string) (string, error) {
	args := []string{"template", "kodama-stage", chart, "--namespace", "kodama-system", "--values", valuesPath}
	args = append(args, stackProductionArgs()...)
	command := exec.Command(helm, args...)
	command.Dir = chart
	rendered, err := command.CombinedOutput()
	return string(rendered), err
}
