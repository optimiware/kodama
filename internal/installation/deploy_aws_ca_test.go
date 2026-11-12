package installation

// Static checks for the AWS database trust inputs and outputs.
//
// deploy/aws is never planned or applied against a real account from a unit
// test. These checks pin the parts of the root a reviewer can verify as text:
// the operator-supplied public RDS CA PEM bundle input, the explicit RDS CA
// identifier wired to aws_db_instance.ca_cert_identifier, the exact
// database_ca_certificate output name scripts/bootstrap-secrets reads, and the
// verify-full DSN rule. The executable proof that the HCL really exports the
// bundle runs in test/component/deploy/aws_ca_component_test.go with mocked
// providers; this file cannot substitute a hand-written fixture for it.

import (
	"regexp"
	"strings"
	"testing"
)

// awsDeployRead reads one file under deploy/aws.
func awsDeployRead(t *testing.T, name string) string {
	t.Helper()
	return readText(t, repoRoot(t)+"/deploy/aws/"+name)
}

// awsTopLevelBlock returns the first top-level HCL block `keyword "name" {
// ... }` whose closing brace starts at column zero, which is how tofu fmt
// writes top-level blocks.
func awsTopLevelBlock(t *testing.T, body, keyword, name string) string {
	t.Helper()
	pattern := `(?ms)^` + keyword + `\s+"` + regexp.QuoteMeta(name) + `"[^{]*\{.*?^\}`
	match := regexp.MustCompile(pattern).FindString(body)
	if match == "" {
		t.Fatalf("deploy/aws: no %s %q block", keyword, name)
	}
	return match
}

// awsRequire matches expression inside body.
func awsRequire(t *testing.T, body, expression, description string) {
	t.Helper()
	if !regexp.MustCompile(expression).MatchString(body) {
		t.Errorf("%s: no match for %s", description, expression)
	}
}

// awsForbid fails if expression matches body.
func awsForbid(t *testing.T, body, expression, description string) {
	t.Helper()
	if regexp.MustCompile(expression).MatchString(body) {
		t.Errorf("%s: unexpected match for %s", description, expression)
	}
}

// TestAWSDatabaseCAInputRejectsMissingBundle pins the operator-supplied public
// RDS CA bundle input. The bundle is required (no default), non-nullable, and
// its validation refuses an empty or whitespace-only placeholder and any value
// without PEM certificate markers. The RDS CA identifier is a separate input:
// an identifier is not a certificate and must never be used as the bundle.
func TestAWSDatabaseCAInputRejectsMissingBundle(t *testing.T) {
	vars := awsDeployRead(t, "variables.tf")

	bundle := awsTopLevelBlock(t, vars, "variable", "database_ca_bundle")
	awsRequire(t, bundle, `type\s*=\s*string`, "database_ca_bundle type")
	awsRequire(t, bundle, `nullable\s*=\s*false`, "database_ca_bundle is non-nullable")
	awsRequire(t, bundle, `sensitive\s*=\s*true`, "database_ca_bundle is marked sensitive")
	awsRequire(t, bundle, `validation\s*\{`, "database_ca_bundle has a validation block")
	awsForbid(t, bundle, `(?m)^[ \t]*default[ \t]*=`, "database_ca_bundle must not have a default; an empty placeholder would satisfy apply")
	awsRequire(t, bundle, `length\(trimspace\(var\.database_ca_bundle\)\)\s*>\s*0`, "database_ca_bundle validation refuses an empty bundle")
	awsRequire(t, bundle, `-----BEGIN CERTIFICATE-----`, "database_ca_bundle validation requires a PEM begin marker")
	awsRequire(t, bundle, `-----END CERTIFICATE-----`, "database_ca_bundle validation requires a PEM end marker")

	identifier := awsTopLevelBlock(t, vars, "variable", "postgres_ca_cert_identifier")
	awsRequire(t, identifier, `type\s*=\s*string`, "postgres_ca_cert_identifier type")
	awsRequire(t, identifier, `validation\s*\{`, "postgres_ca_cert_identifier has a validation block")
	awsRequire(t, identifier, `rds-ca-`, "postgres_ca_cert_identifier validates an RDS CA identifier shape")
	awsForbid(t, identifier, `(?i)-----BEGIN`, "postgres_ca_cert_identifier must not accept a PEM bundle")
}

// TestAWSDatabaseCAInputRejectsPrivateKeysAndDisorderedMarkers pins the two
// remaining content rules: the bundle is public certificate material only, so a
// PRIVATE KEY block is refused, and the BEGIN/END markers must be ordered as a
// complete certificate block. A mere containment check passes a certificate
// concatenated with a private key, reversed markers, or markers out of order;
// those values would land in the ca.crt Secret and in state.
func TestAWSDatabaseCAInputRejectsPrivateKeysAndDisorderedMarkers(t *testing.T) {
	vars := awsDeployRead(t, "variables.tf")
	bundle := awsTopLevelBlock(t, vars, "variable", "database_ca_bundle")

	awsRequire(t, bundle, `!strcontains\(var\.database_ca_bundle,\s*"PRIVATE KEY"\)`, "database_ca_bundle refuses any PRIVATE KEY block")
	awsRequire(t, bundle, `\(\?:-----BEGIN CERTIFICATE-----`, "database_ca_bundle requires one or more ordered BEGIN...END certificate blocks")
	awsRequire(t, bundle, `\[A-Za-z0-9\+/=\]\[A-Za-z0-9\+/=\\\\s\]\*-----END CERTIFICATE-----`, "each certificate block body excludes PEM markers, so a duplicate BEGIN cannot be swallowed")
	awsForbid(t, bundle, `replace\(`, "database_ca_bundle must not rely on lazy replace() to find leftover markers")

	// The required and marker-order conditions must be one validation rule: two
	// separate rules would let a later success hide an earlier failure only if
	// the author accidentally used `anytrue` instead of `alltrue`.
	awsRequire(t, bundle, `length\(trimspace\(var\.database_ca_bundle\)\)\s*>\s*0\s*&&`, "database_ca_bundle combines the non-empty and content checks")
}

// TestAWSDatabaseCAOutputFeedsBootstrapSecret pins the exact output name and
// value scripts/bootstrap-secrets reads. The output must be the operator's
// variable, never the RDS CA identifier, a resource attribute, or the internal
// cluster CA.
func TestAWSDatabaseCAOutputFeedsBootstrapSecret(t *testing.T) {
	outputs := awsDeployRead(t, "outputs.tf")

	block := awsTopLevelBlock(t, outputs, "output", "database_ca_certificate")
	awsRequire(t, block, `(?m)^[ \t]*value[ \t]*=[ \t]*var\.database_ca_bundle[ \t]*$`, "database_ca_certificate exports var.database_ca_bundle")
	awsRequire(t, block, `sensitive\s*=\s*true`, "database_ca_certificate is sensitive")
	awsForbid(t, block, `ca_cert_identifier`, "database_ca_certificate must not export the RDS CA identifier")
	awsForbid(t, block, `aws_[a-z_]+\.`, "database_ca_certificate must not export a resource attribute (another CA)")

	bootstrap := readText(t, repoRoot(t)+"/scripts/bootstrap-secrets")
	if !strings.Contains(bootstrap, "output -raw database_ca_certificate") {
		t.Fatal("scripts/bootstrap-secrets no longer reads `output -raw database_ca_certificate`; the AWS output name and the consumer have diverged")
	}
}

// TestAWSDatabaseTrustKeepsVerifyFull pins the trust chain: the descriptor
// feeds verify-full (not the weaker verify-ca), the RDS instance uses the
// explicit CA identifier, and no AWS file re-enables a weaker TLS mode.
func TestAWSDatabaseTrustKeepsVerifyFull(t *testing.T) {
	outputs := awsDeployRead(t, "outputs.tf")
	awsRequire(t, outputs, `ssl_mode\s*=\s*"verify-full"`, "AWS descriptor postgres.ssl_mode is verify-full")
	awsForbid(t, outputs, `verify-ca`, "AWS descriptor must not weaken PostgreSQL TLS to verify-ca")

	postgres := awsDeployRead(t, "postgres.tf")
	instance := awsTopLevelBlock(t, postgres, "resource", "aws_db_instance")
	awsRequire(t, instance, `ca_cert_identifier\s*=\s*var\.postgres_ca_cert_identifier`, "aws_db_instance uses the explicit RDS CA identifier")

	for _, name := range []string{"outputs.tf", "postgres.tf", "variables.tf"} {
		awsForbid(t, awsDeployRead(t, name), `verify-ca`, "deploy/aws/"+name+" must not weaken PostgreSQL TLS to verify-ca")
	}
}

// TestAWSPostgresParameterGroupAndAPICIDRs pins the AWS RDS parameter-group
// family derivation and the mandatory EKS/GKE Kubernetes API CIDR restrictions.
func TestAWSPostgresParameterGroupAndAPICIDRs(t *testing.T) {
	postgres := awsDeployRead(t, "postgres.tf")
	awsRequire(t, postgres, `postgres_major_version\s*=\s*split\("\.",\s*var\.postgres_engine_version\)\[0\]`, "postgres.tf derives postgres_major_version from var.postgres_engine_version")
	paramGroup := awsTopLevelBlock(t, postgres, "resource", "aws_db_parameter_group")
	awsRequire(t, paramGroup, `name\s*=\s*"\$\{module\.context\.name_prefix\}-pg\$\{local\.postgres_major_version\}"`, "aws_db_parameter_group name uses local.postgres_major_version")
	awsRequire(t, paramGroup, `family\s*=\s*"postgres\$\{local\.postgres_major_version\}"`, "aws_db_parameter_group family uses local.postgres_major_version")
	awsRequire(t, paramGroup, `create_before_destroy\s*=\s*true`, "aws_db_parameter_group uses create_before_destroy")
	awsForbid(t, paramGroup, `"postgres16"`, "aws_db_parameter_group must not hardcode postgres16")

	vars := awsDeployRead(t, "variables.tf")
	engine := awsTopLevelBlock(t, vars, "variable", "postgres_engine_version")
	awsRequire(t, engine, `validation\s*\{`, "postgres_engine_version validates its format")

	allowed := awsTopLevelBlock(t, vars, "variable", "k8s_api_allowed_cidrs")
	awsForbid(t, allowed, `(?m)^[ \t]*default[ \t]*=`, "AWS k8s_api_allowed_cidrs must not have a default")
	awsRequire(t, allowed, `length\(var\.k8s_api_allowed_cidrs\)\s*>\s*0`, "AWS k8s_api_allowed_cidrs requires a non-empty list")

	kubernetes := awsDeployRead(t, "kubernetes.tf")
	eks := awsTopLevelBlock(t, kubernetes, "resource", "aws_eks_cluster")
	awsRequire(t, eks, `public_access_cidrs\s*=\s*var\.k8s_api_allowed_cidrs`, "aws_eks_cluster wires public_access_cidrs")

	gcpVars := readText(t, repoRoot(t)+"/deploy/gcp/variables.tf")
	gcpAllowed := awsTopLevelBlock(t, gcpVars, "variable", "k8s_api_allowed_cidrs")
	awsForbid(t, gcpAllowed, `(?m)^[ \t]*default[ \t]*=`, "GCP k8s_api_allowed_cidrs must not have a default")
	awsRequire(t, gcpAllowed, `length\(var\.k8s_api_allowed_cidrs\)\s*>\s*0`, "GCP k8s_api_allowed_cidrs requires a non-empty list")

	gcpKubernetes := readText(t, repoRoot(t)+"/deploy/gcp/kubernetes.tf")
	gke := awsTopLevelBlock(t, gcpKubernetes, "resource", "google_container_cluster")
	awsRequire(t, gke, `master_authorized_networks_config\s*\{`, "google_container_cluster configures master_authorized_networks_config")
	awsRequire(t, gke, `for_each\s*=\s*var\.k8s_api_allowed_cidrs`, "google_container_cluster iterates var.k8s_api_allowed_cidrs")

	contextVars := readText(t, repoRoot(t)+"/deploy/modules/context/variables.tf")
	tenants := awsTopLevelBlock(t, contextVars, "variable", "tenants")
	awsRequire(t, tenants, regexp.QuoteMeta(`^[a-z0-9]([-a-z0-9]{0,47}[a-z0-9])?$`), "context tenants variable caps tenant IDs at 49 characters")
}
