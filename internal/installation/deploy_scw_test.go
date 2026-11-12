package installation

// Static checks for the Scaleway (scw) OpenTofu root in deploy/scw.
//
// There is no Terraform test framework in this repository and the deploy roots
// are never planned against a real cloud from a unit test. These checks pin the
// parts of the scw root that a reviewer can verify as text and as JSON with the
// standard library: the application-database ownership split (Scaleway creates
// every database owned by its managed superadmin, so Terraform does not own
// it), the deployment-descriptor keys the scripts consume, credential and
// destroy safety, the optional Kubernetes API ACL, provider lock completeness
// and the example/real tfvars shape.
//
// A few regexes intentionally depend on the current formatting of the HCL.
// tofu fmt keeps that formatting canonical and every assertion names the exact
// block it protects, so a formatting change fails loudly instead of silently
// disabling a check.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// scwDir returns deploy/scw inside the repository root.
func scwDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "deploy", "scw")
}

// scwRead reads one file under deploy/scw.
func scwRead(t *testing.T, parts ...string) string {
	t.Helper()
	all := append([]string{scwDir(t)}, parts...)
	return readText(t, filepath.Join(all...))
}

// scwBlock returns the first top-level HCL block `keyword "name" { ... }`.
// The closing brace must start at column zero, which is how tofu fmt writes a
// top-level block; nested braces (maps, dynamic blocks) are indented.
func scwBlock(t *testing.T, body, keyword, name string) string {
	t.Helper()
	pattern := `(?ms)^` + keyword + `\s+"` + regexp.QuoteMeta(name) + `"[^{]*\{.*?^\}`
	match := regexp.MustCompile(pattern).FindString(body)
	if match == "" {
		t.Fatalf("deploy/scw: no %s %q block", keyword, name)
	}
	return match
}

// scwLocals returns the first top-level locals block.
func scwLocals(t *testing.T, body string) string {
	t.Helper()
	match := regexp.MustCompile(`(?ms)^locals\s*\{.*?^\}`).FindString(body)
	if match == "" {
		t.Fatal("deploy/scw: no locals block")
	}
	return match
}

// scwRequire matches expression inside body.
func scwRequire(t *testing.T, body, expression, description string) {
	t.Helper()
	if !regexp.MustCompile(expression).MatchString(body) {
		t.Errorf("%s: no match for %s", description, expression)
	}
}

// scwForbid fails if expression matches body.
func scwForbid(t *testing.T, body, expression, description string) {
	t.Helper()
	if regexp.MustCompile(expression).MatchString(body) {
		t.Errorf("%s: unexpected match for %s", description, expression)
	}
}

// scwOpenRange matches a CIDR that permits every address: the IPv4 spelling and
// the IPv6 spellings an operator who pastes a rule could use.
const scwOpenRange = `0\.0\.0\.0/0|::/0|0::/0|0:0:0:0:0:0:0:0/0`

// scwTerraformFiles returns every .tf file in deploy/scw keyed by slash path.
func scwTerraformFiles(t *testing.T) map[string]string {
	t.Helper()
	root := scwDir(t)
	files := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || filepath.Ext(path) != ".tf" {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(relative)] = readText(t, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk deploy/scw: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("deploy/scw has no .tf files")
	}
	return files
}

// TestScwApplicationDatabaseNotOwnedByTerraform pins the Scaleway database
// ownership decision: a Scaleway Managed Database creates every database as
// _rdb_superadmin, and the bootstrap administrator is CREATEROLE/CREATEDB but
// not superuser, so Terraform must not try to own the application database or
// a privilege on it. `kodama migrate` creates the database as the
// administrator through var.postgres_admin_database.
func TestScwApplicationDatabaseNotOwnedByTerraform(t *testing.T) {
	files := scwTerraformFiles(t)
	if !strings.Contains(files["postgres.tf"], `resource "scaleway_rdb_instance" "main"`) {
		t.Fatal("deploy/scw/postgres.tf no longer declares the RDB instance")
	}

	for name, body := range files {
		scwForbid(t, body, `resource\s+"scaleway_rdb_database"`, name+" still declares scaleway_rdb_database")
		scwForbid(t, body, `resource\s+"scaleway_rdb_privilege"`, name+" still declares scaleway_rdb_privilege")
		scwForbid(t, body, `scaleway_rdb_database\.`, name+" still references a scaleway_rdb_database resource")
	}
}

// TestScwDescriptorPostgresContract pins the descriptor keys scripts read and
// the database split: database_name is the application database the migration
// step creates, admin_database is the maintenance database the administrator
// connects to, and ssl_mode is verify-full.
func TestScwDescriptorPostgresContract(t *testing.T) {
	outputs := scwRead(t, "outputs.tf")
	descriptor := scwLocals(t, outputs)
	if !strings.Contains(descriptor, "deployment_descriptor") {
		t.Fatal("deploy/scw/outputs.tf locals no longer build deployment_descriptor")
	}

	scwRequire(t, descriptor, `database_name\s*=\s*var\.postgres_database_name`, "descriptor postgres.database_name from var.postgres_database_name")
	scwRequire(t, descriptor, `admin_database\s*=\s*var\.postgres_admin_database`, "descriptor postgres.admin_database from var.postgres_admin_database")
	scwRequire(t, descriptor, `admin_user\s*=\s*var\.postgres_admin_user`, "descriptor postgres.admin_user from var.postgres_admin_user")
	scwRequire(t, descriptor, `host\s*=\s*local\.pg_host`, "descriptor postgres.host from local.pg_host")
	scwRequire(t, descriptor, `port\s*=\s*local\.pg_port`, "descriptor postgres.port from local.pg_port")
	scwRequire(t, descriptor, `ssl_mode\s*=\s*"verify-full"`, "descriptor postgres.ssl_mode is verify-full")
	scwRequire(t, descriptor, `key_adapter\s*=\s*module\.context\.key_adapter`, "descriptor kms.key_adapter from module.context.key_adapter")
	scwRequire(t, descriptor, `pg_private_endpoint\s*=\s*scaleway_rdb_instance\.main\.private_network\[0\]`, "pg_private_endpoint indexes the private network directly")
	scwRequire(t, descriptor, `pg_host\s*=\s*coalesce\(try\(local\.pg_private_endpoint\.ip,\s*null\),\s*local\.pg_private_endpoint\.hostname\)`, "pg_host coalesces private endpoint IP and hostname without a loopback fallback")
	scwForbid(t, outputs, regexp.QuoteMeta("127.0.0.1"), "deploy/scw/outputs.tf must not fall back to 127.0.0.1")

	vars := scwRead(t, "variables.tf")
	database := scwBlock(t, vars, "variable", "postgres_database_name")
	scwRequire(t, database, `type\s*=\s*string`, "postgres_database_name type")
	scwRequire(t, database, `default\s*=\s*"kodama"`, "postgres_database_name default")

	admin := scwBlock(t, vars, "variable", "postgres_admin_database")
	scwRequire(t, admin, `type\s*=\s*string`, "postgres_admin_database type")
	scwRequire(t, admin, `default\s*=\s*"rdb"`, "postgres_admin_database default")
}

// TestScwSensitiveOutputs pins credential custody in the root outputs: the
// kubeconfig, the database CA certificate and the KMS API credentials are
// sensitive, and the public descriptor carries none of their values.
func TestScwSensitiveOutputs(t *testing.T) {
	outputs := scwRead(t, "outputs.tf")

	for _, name := range []string{"kubeconfig_raw", "database_ca_certificate", "kms_credentials"} {
		block := scwBlock(t, outputs, "output", name)
		scwRequire(t, block, `sensitive\s*=\s*true`, "output "+name+" is sensitive")
	}

	descriptor := scwLocals(t, outputs)
	// Nothing that authenticates or decrypts may appear in the public
	// descriptor: credentials, certificates, kubeconfig material and KMS
	// credentials stay in the sensitive outputs. The legitimately public keys
	// (kms.key_adapter, postgres.database_name) must keep passing.
	scwForbid(t, descriptor, `(?i)access_key|secret_key|password|certificate|kubeconfig|config_file|kms_credentials|token`, "deployment_descriptor must not carry credentials, certificates, kubeconfig or KMS credentials")

	block := scwBlock(t, outputs, "output", "deployment_descriptor")
	scwForbid(t, block, `sensitive\s*=\s*true`, "deployment_descriptor is a public contract, not a secret")
}

// TestScwNoLiteralCredentials scans every scw .tf file for inline credential
// values. Passwords come from TF_VAR_* and are written with password_wo; API
// keys are generated resources.
func TestScwNoLiteralCredentials(t *testing.T) {
	literal := regexp.MustCompile(`(?i)^\s*(password|password_wo|secret_key|access_key|token|api_key|secret|private_key)\s*=\s*"[^"]+"`)
	for name, body := range scwTerraformFiles(t) {
		for line := range strings.SplitSeq(body, "\n") {
			if literal.MatchString(line) {
				t.Errorf("%s: literal credential assignment %q", name, strings.TrimSpace(line))
			}
		}
	}

	postgres := scwRead(t, "postgres.tf")
	scwRequire(t, postgres, `password_wo\s*=\s*var\.postgres_admin_password`, "RDB password uses the write-only variable")
	scwRequire(t, postgres, `password_wo_version\s*=\s*var\.postgres_password_wo_version`, "RDB password_wo_version uses its variable")
}

// TestScwKMSChurnAndCustody pins two KMS invariants: the
// provider reports the API key expiry the API assigns, so ignoring it keeps
// plans from replacing the decrypt key on every run, and the key's
// unprotected flag stays tied to var.kms_unprotected (false on stage).
func TestScwKMSChurnAndCustody(t *testing.T) {
	kms := scwRead(t, "kms.tf")

	key := scwBlock(t, kms, "resource", "scaleway_key_manager_key")
	scwRequire(t, key, `unprotected\s*=\s*var\.kms_unprotected`, "KEK unprotected from var.kms_unprotected")

	apiKey := scwBlock(t, kms, "resource", "scaleway_iam_api_key")
	scwRequire(t, apiKey, `(?s)ignore_changes\s*=\s*\[\s*expires_at\s*\]`, "API key ignores the provider-assigned expires_at")
	scwForbid(t, apiKey, `expires_at\s*=\s*`, "API key must not set expires_at explicitly")

	vars := scwRead(t, "variables.tf")
	unprotected := scwBlock(t, vars, "variable", "kms_unprotected")
	scwRequire(t, unprotected, `default\s*=\s*false`, "kms_unprotected defaults to false")

	for _, file := range []string{"stage.tfvars.json", "preview.tfvars.json", "stage.tfvars.json.example", "preview.tfvars.json.example"} {
		document := scwTfvars(t, file)
		want := file == "preview.tfvars.json" || file == "preview.tfvars.json.example"
		if got, ok := document["kms_unprotected"].(bool); !ok || got != want {
			t.Errorf("%s: kms_unprotected = %v, want %v", file, document["kms_unprotected"], want)
		}
	}
}

// TestScwIAMTagsUseKeyValue pins the IAM tag format: Scaleway IAM rejects ":"
// in tags (it accepts "key=value"), unlike the resources tagged through
// module.context.common_tag_list.
func TestScwIAMTagsUseKeyValue(t *testing.T) {
	kms := scwRead(t, "kms.tf")
	locals := scwLocals(t, kms)
	scwRequire(t, locals, `iam_tags\s*=\s*\[for\s+k,\s*v\s+in\s+module\.context\.common_labels\s*:\s*"\$\{k\}=\$\{v\}"\]`, "IAM tags convert labels with key=value")
	scwForbid(t, locals, `"\$\{k\}:\$\{v\}"`, "IAM tags must not use a colon separator")

	for resource, body := range map[string]string{
		"IAM application": scwBlock(t, kms, "resource", "scaleway_iam_application"),
		"IAM policy":      scwBlock(t, kms, "resource", "scaleway_iam_policy"),
	} {
		scwRequire(t, body, `tags\s*=\s*local\.iam_tags`, resource+" uses key=value IAM tags")
		scwForbid(t, body, `tags\s*=\s*module\.context\.common_tag_list`, resource+" must not use colon labels")
	}
}

// TestScwKMSIAMLeastPrivilege pins the KMS policy rule: exactly one rule scoped
// to the cluster's project with only KeyManagerReadOnly and
// KeyManagerKeyDecrypt. A wider permission set (IAMManager,
// AllProductsFullAccess) or an organization-wide rule would let the cluster
// identity administer the account, not just decrypt the KEK.
func TestScwKMSIAMLeastPrivilege(t *testing.T) {
	policy := scwBlock(t, scwRead(t, "kms.tf"), "resource", "scaleway_iam_policy")
	scwRequire(t, policy, `project_ids\s*=\s*\[var\.project_id\]`, "KMS policy is scoped to var.project_id")
	scwRequire(t, policy, `(?s)permission_set_names\s*=\s*\[\s*"KeyManagerReadOnly"\s*,\s*"KeyManagerKeyDecrypt"\s*,?\s*\]`, "KMS policy grants exactly KeyManagerReadOnly and KeyManagerKeyDecrypt")
	if rules := regexp.MustCompile(`\brule\s*\{`).FindAllString(policy, -1); len(rules) != 1 {
		t.Errorf("KMS policy has %d rule blocks, want exactly 1", len(rules))
	}
	scwForbid(t, policy, `(?i)IAMManager|AllProductsFullAccess|organization_id|OrganizationManager`, "KMS policy must not grant account-wide administration")
}

// TestScwKubernetesAPIACLNeverOpenWhenConfigured pins the mandatory API ACL: it
// is created unconditionally, requires a non-empty list of narrow IPv4 CIDRs,
// always allows Scaleway's own ranges, rejects malformed entries and every open
// /0 range, and ensures every committed environment file sets a non-empty list
// free of 0.0.0.0/0 (or its IPv6 spellings).
func TestScwKubernetesAPIACLNeverOpenWhenConfigured(t *testing.T) {
	kubernetes := scwRead(t, "kubernetes.tf")
	acl := scwBlock(t, kubernetes, "resource", "scaleway_k8s_acl")
	scwForbid(t, acl, `(?m)^  (count|for_each)[ \t]*=`, "scaleway_k8s_acl.api must be created unconditionally")
	scwRequire(t, acl, `dynamic\s+"acl_rules"`, "ACL renders one rule per operator CIDR")
	scwRequire(t, acl, `scaleway_ranges\s*=\s*true`, "ACL always allows Scaleway node/service ranges")

	// The whole ACL block must stay free of an open range, including a literal
	// added next to the dynamic rules or the Scaleway-range rule.
	scwForbid(t, acl, scwOpenRange, "scaleway_k8s_acl must not hardcode an open CIDR range")

	// No root .tf file may reintroduce an open range, whether in the ACL, a
	// variable default or a future resource. This walks bootstrap/ too because
	// scwTerraformFiles is recursive; neither root has a legitimate open range.
	// Full-line comments and validation blocks are exempt: a comment cannot
	// configure a rule, and the k8s_api_allowed_cidrs guard must name the exact
	// ranges it refuses (the guard itself is pinned below).
	comments := regexp.MustCompile(`(?m)^[ \t]*(#|//).*$`)
	validationBlock := regexp.MustCompile(`(?ms)^[ \t]*validation[ \t]*\{.*?^[ \t]*\}`)
	for name, body := range scwTerraformFiles(t) {
		body = validationBlock.ReplaceAllString(comments.ReplaceAllString(body, ""), "")
		scwForbid(t, body, scwOpenRange, name+" contains an open CIDR range")
	}

	vars := scwRead(t, "variables.tf")
	allowed := scwBlock(t, vars, "variable", "k8s_api_allowed_cidrs")
	scwForbid(t, allowed, `(?m)^[ \t]*default[ \t]*=`, "k8s_api_allowed_cidrs must not have a default; empty [] would leave the API open")
	scwRequire(t, allowed, `nullable\s*=\s*false`, "k8s_api_allowed_cidrs is non-nullable")
	scwRequire(t, allowed, `validation\s*\{`, "k8s_api_allowed_cidrs has a validation block")
	scwRequire(t, allowed, `length\(var\.k8s_api_allowed_cidrs\)\s*>\s*0`, "k8s_api_allowed_cidrs requires at least one CIDR")
	scwRequire(t, allowed, `can\(cidrnetmask\(c\)\)`, "k8s_api_allowed_cidrs validates IPv4 CIDR entries")
	// cidrnetmask accepts every /0 spelling (0.0.0.0/0, 0.0.0.1/0,
	// 10.0.0.0/0, 0.0.0.0/00), so an exact-string deny list is not enough. A
	// prefix-length rule refuses every address-range that covers the whole
	// space and accepts only a narrow /1../32.
	scwRequire(t, allowed, regexp.QuoteMeta(`can(regex("/([1-9]|[12][0-9]|3[0-2])$"`), "k8s_api_allowed_cidrs requires a non-zero prefix length")
	scwForbid(t, allowed, regexp.QuoteMeta(`!contains(["0.0.0.0/0", "::/0"`), "k8s_api_allowed_cidrs must not rely on an exact-string open-range deny list")

	for _, file := range []string{"stage.tfvars.json", "preview.tfvars.json", "stage.tfvars.json.example", "preview.tfvars.json.example"} {
		document := scwTfvars(t, file)
		value, present := document["k8s_api_allowed_cidrs"]
		if !present {
			t.Fatalf("%s: k8s_api_allowed_cidrs must be set explicitly", file)
		}
		list, ok := value.([]any)
		if !ok {
			t.Fatalf("%s: k8s_api_allowed_cidrs is not a list: %v", file, value)
		}
		if len(list) == 0 {
			t.Errorf("%s: k8s_api_allowed_cidrs is set to an empty list, which is refused", file)
		}
		for _, entry := range list {
			text, ok := entry.(string)
			if !ok {
				t.Fatalf("%s: k8s_api_allowed_cidrs entry %v is not a string", file, entry)
			}
			if regexp.MustCompile(scwOpenRange).MatchString(text) {
				t.Errorf("%s: k8s_api_allowed_cidrs contains the open range %q", file, text)
			}
		}
	}
}

// TestScwDestroySafety pins the teardown and durability choices: Kapsule does
// not delete additional resources, the database keeps encrypted backups on its
// private network for at least 30 days, the state bucket is private and
// versioned, force_destroy is wired to the false-defaulting variable (a
// hardcoded true in main.tf would destroy the state bucket on teardown), and
// the protected stage key cannot be deleted by accident (kms_unprotected=false).
func TestScwDestroySafety(t *testing.T) {
	kubernetes := scwRead(t, "kubernetes.tf")
	cluster := scwBlock(t, kubernetes, "resource", "scaleway_k8s_cluster")
	scwRequire(t, cluster, `delete_additional_resources\s*=\s*false`, "Kapsule cluster does not delete additional resources")

	postgres := scwRead(t, "postgres.tf")
	instance := scwBlock(t, postgres, "resource", "scaleway_rdb_instance")
	scwRequire(t, instance, `disable_backup\s*=\s*false`, "RDB instance keeps backups")
	scwRequire(t, instance, `backup_same_region\s*=\s*true`, "RDB backups stay in region")
	scwRequire(t, instance, `backup_schedule_retention\s*=\s*var\.postgres_backup_retention_days`, "RDB backup retention uses its variable")
	scwRequire(t, instance, `encryption_at_rest\s*=\s*true`, "RDB storage is encrypted at rest")
	scwRequire(t, instance, `(?s)private_network\s*\{[^}]*pn_id\s*=\s*scaleway_vpc_private_network\.main\.id`, "RDB instance attaches only the private network")
	scwForbid(t, instance, `(?m)^[ \t]*(load_balancer|public_endpoint|endpoint_ip|is_public)[ \t]*[={]`, "RDB instance must not expose a public endpoint or load balancer")

	vars := scwRead(t, "variables.tf")
	retention := scwBlock(t, vars, "variable", "postgres_backup_retention_days")
	if match := regexp.MustCompile(`default\s*=\s*([0-9]+)`).FindStringSubmatch(retention); match == nil {
		t.Error("postgres_backup_retention_days has no numeric default")
	} else if days, err := strconv.Atoi(match[1]); err != nil || days < 30 {
		t.Errorf("postgres_backup_retention_days default = %s, want at least 30 days", match[1])
	}

	files := scwTerraformFiles(t)
	bootstrap, ok := files["bootstrap/main.tf"]
	if !ok {
		t.Fatal("deploy/scw/bootstrap/main.tf is missing")
	}
	bucket := scwBlock(t, bootstrap, "resource", "scaleway_object_bucket")
	scwRequire(t, bucket, `force_destroy\s*=\s*var\.force_destroy`, "state bucket force_destroy comes from var.force_destroy")
	scwRequire(t, bucket, `(?s)versioning\s*\{[^}]*enabled\s*=\s*true`, "state bucket versioning is enabled")
	scwRequire(t, scwBlock(t, bootstrap, "resource", "scaleway_object_bucket_acl"), `acl\s*=\s*"private"`, "state bucket ACL is private")
	for name, body := range files {
		if !strings.HasPrefix(name, "bootstrap/") {
			continue
		}
		scwForbid(t, body, `force_destroy\s*=\s*true`, name+" hardcodes force_destroy = true")
	}

	bootstrapVars := scwRead(t, "bootstrap", "variables.tf")
	forceDestroy := scwBlock(t, bootstrapVars, "variable", "force_destroy")
	scwRequire(t, forceDestroy, `default\s*=\s*false`, "state bucket force_destroy defaults to false")
}

// TestScwProviderLocksComplete pins both provider lock files: the scw root and
// its bootstrap root declare only the Scaleway provider, each lock selects a
// version inside the declared constraint, and each lock's constraints line
// equals the provider constraint.
//
// The h1:/zh: hashes cannot be verified statically: only `tofu init`
// recomputes them against the provider registry, so this test pins their
// presence while tofu init verifies their values.
func TestScwProviderLocksComplete(t *testing.T) {
	roots := map[string]string{
		"deploy/scw/versions.tf":           "deploy/scw/.terraform.lock.hcl",
		"deploy/scw/bootstrap/versions.tf": "deploy/scw/bootstrap/.terraform.lock.hcl",
	}
	for versionsFile, lockFile := range roots {
		versions := readText(t, filepath.Join(repoRoot(t), versionsFile))
		scwRequire(t, versions, `scaleway\s*=\s*\{`, versionsFile+" declares the scaleway provider")
		scwRequire(t, versions, `source\s*=\s*"scaleway/scaleway"`, versionsFile+" provider source")
		constraintMatch := regexp.MustCompile(`(?s)scaleway\s*=\s*\{[^}]*version\s*=\s*"([^"]+)"`).FindStringSubmatch(versions)
		if constraintMatch == nil {
			t.Fatalf("%s: scaleway provider has no version constraint", versionsFile)
		}
		constraint := constraintMatch[1]

		lock := readText(t, filepath.Join(repoRoot(t), lockFile))
		scwRequire(t, lock, `provider\s+"registry\.opentofu\.org/scaleway/scaleway"`, lockFile+" pins the Scaleway provider")
		scwRequire(t, lock, `(?m)^\s*"h1:`, lockFile+" has a platform hash")
		scwRequire(t, lock, `(?m)^\s*"zh:`, lockFile+" has registry hashes")
		scwRequire(t, lock, `(?m)^\s*constraints\s*=\s*"`+regexp.QuoteMeta(constraint)+`"\s*$`, lockFile+" constraints line equals the provider constraint")

		versionMatch := regexp.MustCompile(`(?m)^\s*version\s*=\s*"([0-9]+\.[0-9]+\.[0-9]+)"`).FindStringSubmatch(lock)
		if versionMatch == nil {
			t.Fatalf("%s: no pinned provider version", lockFile)
		}
		if !scwVersionSatisfies(t, versionMatch[1], constraint) {
			t.Errorf("%s: pinned Scaleway provider %s is outside the %s constraint", lockFile, versionMatch[1], constraint)
		}
	}
}

// scwVersionSatisfies reports whether version is inside a pessimistic (~>)
// constraint. The lock files currently pin ~> 2.68, so only the two- and
// three-part pessimistic forms are supported; another constraint fails the
// test instead of being silently ignored.
func scwVersionSatisfies(t *testing.T, version, constraint string) bool {
	t.Helper()
	rest, ok := strings.CutPrefix(constraint, "~>")
	if !ok {
		t.Fatalf("unsupported provider constraint %q (want ~> X.Y[.Z])", constraint)
	}
	parse := func(name, value string) []int {
		t.Helper()
		parts := strings.Split(strings.TrimSpace(value), ".")
		out := make([]int, len(parts))
		for i, part := range parts {
			n, err := strconv.Atoi(part)
			if err != nil {
				t.Fatalf("%s component %q is not a number", name, part)
			}
			out[i] = n
		}
		return out
	}
	got := parse("version", version)
	want := parse("constraint", rest)
	if len(got) != 3 || (len(want) != 2 && len(want) != 3) {
		t.Fatalf("cannot compare version %q with constraint %q", version, constraint)
	}
	if len(want) == 2 {
		return got[0] == want[0] && got[1] >= want[1]
	}
	return got[0] == want[0] && got[1] == want[1] && got[2] >= want[2]
}

// TestScwTfvarsShape pins the environment files: the real files and their
// .example twins carry the same top-level keys, and the versions match the
// module defaults so a plan does not silently use a different Kubernetes or
// PostgreSQL major version.
func TestScwTfvarsShape(t *testing.T) {
	for _, cluster := range []string{"stage", "preview"} {
		real := scwTfvars(t, cluster+".tfvars.json")
		example := scwTfvars(t, cluster+".tfvars.json.example")
		if got, want := keysOf(real), keysOf(example); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s tfvars keys = %v, example keys = %v", cluster, got, want)
		}
	}

	vars := scwRead(t, "variables.tf")
	kubernetes := scwBlock(t, vars, "variable", "kubernetes_version")
	scwRequire(t, kubernetes, `default\s*=\s*"1\.37"`, "kubernetes_version defaults to 1.37")
	postgres := scwBlock(t, vars, "variable", "postgres_engine")
	scwRequire(t, postgres, `default\s*=\s*"PostgreSQL-17"`, "postgres_engine defaults to PostgreSQL-17")

	for _, file := range []string{"stage.tfvars.json", "preview.tfvars.json", "stage.tfvars.json.example", "preview.tfvars.json.example"} {
		document := scwTfvars(t, file)
		if document["kubernetes_version"] != "1.37" {
			t.Errorf("%s: kubernetes_version = %v, want 1.37", file, document["kubernetes_version"])
		}
		if document["postgres_engine"] != "PostgreSQL-17" {
			t.Errorf("%s: postgres_engine = %v, want PostgreSQL-17", file, document["postgres_engine"])
		}
	}
}

// scwTfvars decodes one file under deploy/scw/clusters.
func scwTfvars(t *testing.T, name string) map[string]any {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(scwDir(t), "clusters", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	var document map[string]any
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	return document
}

// keysOf returns the sorted top-level keys.
func keysOf(document map[string]any) []string {
	keys := make([]string, 0, len(document))
	for key := range document {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
