//go:build component

// Deploy-hardening rendered and evaluated checks.
//
// The AWS CA bundle and the Scaleway API CIDR rules are OpenTofu variable
// validations. A textual assertion cannot prove that a reversed marker or an
// effectively open range is actually refused, so these tests extract the real
// variable block from the deployment root and plan a minimal module containing
// only that block. OpenTofu evaluates the validation without initialising a
// provider or contacting a cloud. The chart checks render the real charts with
// helm and prove that empty OIDC values abort the render by name.
package deploy

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// deployVariableBlock returns one top-level `variable "name" { ... }` block
// from a root's variables.tf, exactly as OpenTofu parses it.
func deployVariableBlock(t *testing.T, root, cloud, name string) string {
	t.Helper()
	body := readRepoFile(t, root, filepath.Join("deploy", cloud, "variables.tf"))
	pattern := `(?ms)^variable\s+"` + regexp.QuoteMeta(name) + `"\s*\{.*?^\}`
	block := regexp.MustCompile(pattern).FindString(body)
	if block == "" {
		t.Fatalf("deploy/%s/variables.tf has no variable %q", cloud, name)
	}
	return block
}

// planVariable writes a module containing only the real variable block and
// plans it with the given value. It returns the combined output and exit code.
func planVariable(t *testing.T, block, name, value string) (string, int) {
	t.Helper()
	tofuBinary(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "main.tf"), block+"\n")
	env := append(tofuProcessEnv(tofuPluginCacheDir(t)), "TF_VAR_"+name+"="+value)
	return runTofu(t, dir, env, 2*time.Minute, "plan", "-input=false", "-no-color")
}

// TestAWSDatabaseCABundleValidationSemantics evaluates the AWS variable's
// validation. A public multi-certificate bundle is accepted; a bundle carrying
// a private key, reversed markers or leftover/misordered markers is refused by
// the variable itself, not merely by a substring check.
func TestAWSDatabaseCABundleValidationSemantics(t *testing.T) {
	root := repoRoot(t)
	block := deployVariableBlock(t, root, "aws", "database_ca_bundle")
	fixture := string(awsDatabaseCAFixture(t))

	valid := map[string]string{
		"single certificate":    fixture,
		"multiple certificates": fixture + fixture,
	}
	for label, value := range valid {
		output, code := planVariable(t, block, "database_ca_bundle", value)
		if code != 0 {
			t.Errorf("%s: a public certificate bundle was refused:\n%s", label, output)
		}
	}

	invalid := map[string]string{
		"private key concatenated": fixture + "-----BEGIN PRIVATE KEY-----\nMIIB\n-----END PRIVATE KEY-----\n",
		"reversed markers":         "-----END CERTIFICATE-----\nMIID\n-----BEGIN CERTIFICATE-----\n",
		"misordered markers":       fixture + "-----END CERTIFICATE-----\n",
		"duplicate begin marker":   "-----BEGIN CERTIFICATE-----\n-----BEGIN CERTIFICATE-----\nMIID\n-----END CERTIFICATE-----\n",
		"empty block body":         "-----BEGIN CERTIFICATE-----\n-----END CERTIFICATE-----\n",
	}
	for label, value := range invalid {
		output, code := planVariable(t, block, "database_ca_bundle", value)
		if code == 0 {
			t.Errorf("%s: database_ca_bundle accepted the bundle:\n%s", label, output)
			continue
		}
		if !strings.Contains(output, "database_ca_bundle") && !strings.Contains(output, "Invalid value for variable") {
			t.Errorf("%s: refusal does not name the variable:\n%s", label, output)
		}
	}
}

// TestScwKubernetesAPICIDRValidationSemantics evaluates the real
// k8s_api_allowed_cidrs validation and unconditional ACL resource. Narrow
// ranges pass; an empty list, every /0 spelling and the IPv6 open range are
// refused because they permit the whole address space.
func TestScwKubernetesAPICIDRValidationSemantics(t *testing.T) {
	root := repoRoot(t)
	kubernetes := readRepoFile(t, root, filepath.Join("deploy", "scw", "kubernetes.tf"))
	acl := regexp.MustCompile(`(?ms)^resource\s+"scaleway_k8s_acl"\s+"api"\s*\{.*?^\}`).FindString(kubernetes)
	if acl == "" {
		t.Fatal("deploy/scw/kubernetes.tf has no scaleway_k8s_acl.api resource")
	}
	// Only a resource-level count/for_each (two-space indent) makes the ACL
	// conditional; the dynamic acl_rules block legitimately iterates the CIDRs.
	if regexp.MustCompile(`(?m)^  (count|for_each)[ \t]*=`).MatchString(acl) {
		t.Errorf("scaleway_k8s_acl.api is still conditional:\n%s", acl)
	}

	block := deployVariableBlock(t, root, "scw", "k8s_api_allowed_cidrs")

	valid := []string{
		`["203.0.113.7/32"]`,
		`["10.0.0.0/16"]`,
		`["10.0.0.0/8", "203.0.113.7/32"]`,
		`["198.51.100.0/24"]`,
	}
	for _, value := range valid {
		output, code := planVariable(t, block, "k8s_api_allowed_cidrs", value)
		if code != 0 {
			t.Errorf("a legitimate narrow range %s was refused:\n%s", value, output)
		}
	}

	invalid := []string{
		`[]`,
		`["0.0.0.0/0"]`,
		`["0.0.0.1/0"]`,
		`["10.0.0.0/0"]`,
		`["0.0.0.0/00"]`,
		`["::/0"]`,
	}
	for _, value := range invalid {
		output, code := planVariable(t, block, "k8s_api_allowed_cidrs", value)
		if code == 0 {
			t.Errorf("an empty or open range %s was accepted:\n%s", value, output)
			continue
		}
		if !strings.Contains(output, "k8s_api_allowed_cidrs") {
			t.Errorf("the refusal for %s does not name the variable:\n%s", value, output)
		}
	}
}

// TestAWSAndGCPKubernetesAPICIDRValidationSemantics evaluates the real
// k8s_api_allowed_cidrs validations for AWS EKS and GCP GKE.
func TestAWSAndGCPKubernetesAPICIDRValidationSemantics(t *testing.T) {
	root := repoRoot(t)
	for _, cloud := range []string{"aws", "gcp"} {
		t.Run(cloud, func(t *testing.T) {
			block := deployVariableBlock(t, root, cloud, "k8s_api_allowed_cidrs")
			for _, value := range []string{
				`["203.0.113.7/32"]`,
				`["198.51.100.0/24"]`,
				`["2001:db8::/32"]`,
			} {
				output, code := planVariable(t, block, "k8s_api_allowed_cidrs", value)
				if code != 0 {
					t.Errorf("%s refused narrow range %s:\n%s", cloud, value, output)
				}
			}
			for _, value := range []string{
				`[]`,
				`["0.0.0.0/0"]`,
				`["0.0.0.1/0"]`,
				`["10.0.0.0/0"]`,
				`["0.0.0.0/00"]`,
				`["::/0"]`,
				`["0::/0"]`,
				`["not-a-cidr"]`,
			} {
				output, code := planVariable(t, block, "k8s_api_allowed_cidrs", value)
				if code == 0 {
					t.Errorf("%s accepted %s:\n%s", cloud, value, output)
					continue
				}
				if !strings.Contains(output, "k8s_api_allowed_cidrs") {
					t.Errorf("%s refusal for %s does not name k8s_api_allowed_cidrs:\n%s", cloud, value, output)
				}
			}
		})
	}
}

// TestAWSPostgresEngineVersionAndContextTenantsValidation evaluates the AWS
// postgres_engine_version format guard and the shared context 49-char tenant cap.
func TestAWSPostgresEngineVersionAndContextTenantsValidation(t *testing.T) {
	root := repoRoot(t)
	pgBlock := deployVariableBlock(t, root, "aws", "postgres_engine_version")
	for _, valid := range []string{"16", "17", "16.4", "17.2"} {
		if output, code := planVariable(t, pgBlock, "postgres_engine_version", valid); code != 0 {
			t.Errorf("postgres_engine_version refused %q:\n%s", valid, output)
		}
	}
	for _, invalid := range []string{"", "postgres16", "POSTGRES_16", "016", "16.", "9"} {
		if output, code := planVariable(t, pgBlock, "postgres_engine_version", invalid); code == 0 {
			t.Errorf("postgres_engine_version accepted %q:\n%s", invalid, output)
		}
	}

	contextVars := readRepoFile(t, root, filepath.Join("deploy", "modules", "context", "variables.tf"))
	tenantsBlock := regexp.MustCompile(`(?ms)^variable\s+"tenants"\s*\{.*?^\}`).FindString(contextVars)
	if tenantsBlock == "" {
		t.Fatal("deploy/modules/context/variables.tf has no tenants variable")
	}
	for _, valid := range []string{
		`["a"]`,
		`["tenant-a"]`,
		`["` + strings.Repeat("a", 49) + `"]`,
	} {
		if output, code := planVariable(t, tenantsBlock, "tenants", valid); code != 0 {
			t.Errorf("context tenants refused valid value %s:\n%s", valid, output)
		}
	}
	for _, invalid := range []string{
		`[]`,
		`["` + strings.Repeat("a", 50) + `"]`,
		`["-tenant"]`,
		`["tenant-"]`,
		`["TenantA"]`,
	} {
		if output, code := planVariable(t, tenantsBlock, "tenants", invalid); code == 0 {
			t.Errorf("context tenants accepted invalid value %s:\n%s", invalid, output)
		}
	}
}

// TestKodamaChartRefusesEmptyOIDCValues renders the real chart with an empty
// value in an otherwise complete production document. An empty OIDC issuer,
// audience, JWKS URL, tenant claim or discovery client id must fail the render
// and name the setting; a silent empty environment value is not accepted.
func TestKodamaChartRefusesEmptyOIDCValues(t *testing.T) {
	helm, charts := stagedCharts(t)
	chart := filepath.Join(charts, "kodama")

	names := []string{
		"KODAMA_OIDC_ISSUER",
		"KODAMA_OIDC_AUDIENCE",
		"KODAMA_OIDC_JWKS_URL",
		"KODAMA_OIDC_TENANT_CLAIM",
		"KODAMA_DISCOVERY_CLIENT_ID",
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			rendered, err := renderChartArgs(t, helm, "kodama", chart,
				kodamaProductionArgs("--set", "config.extraEnv."+name+"=")...)
			if err == nil {
				t.Fatalf("helm template accepted an empty config.extraEnv.%s:\n%s", name, rendered)
			}
			if !strings.Contains(rendered, name) {
				t.Errorf("the empty-value refusal does not name %s:\n%s", name, rendered)
			}
		})
	}

	if _, err := renderChartArgs(t, helm, "kodama", chart, kodamaProductionArgs()...); err != nil {
		t.Fatalf("helm template refused the complete non-empty production document: %v", err)
	}
}

// TestOAuthSonasChartRefusesEmptyOIDCValues renders the stack chart with an
// OAuthSonas config whose issuer is present but whose client id or client
// secret is blank. The provider would start without usable client credentials,
// so the render must fail and name the empty member.
func TestOAuthSonasChartRefusesEmptyOIDCValues(t *testing.T) {
	helm, chart := stageStackChart(t)
	const issuer = "https://oauthsonas.kodama-system.svc.cluster.local:8443"
	cases := []struct {
		name   string
		config string
		want   string
	}{
		{
			name:   "empty client id",
			config: "issuer: " + issuer + "\nclients:\n  - id: \"\"\n    name: kodactl\n    public: true\n    redirect_uris:\n      - http://127.0.0.1:52790/callback\npersonas:\n  - sub: tenant-a-admin\n    claims:\n      org_id: tenant-a\n",
			want:   "client id",
		},
		{
			name:   "null client id",
			config: "issuer: " + issuer + "\nclients:\n  - id:\n    name: kodactl\n    public: true\n    redirect_uris:\n      - http://127.0.0.1:52790/callback\npersonas:\n  - sub: tenant-a-admin\n    claims:\n      org_id: tenant-a\n",
			want:   "client id",
		},
		{
			name:   "whitespace client id",
			config: "issuer: " + issuer + "\nclients:\n  - id: \" \"\n    name: kodactl\n    public: true\n    redirect_uris:\n      - http://127.0.0.1:52790/callback\npersonas:\n  - sub: tenant-a-admin\n    claims:\n      org_id: tenant-a\n",
			want:   "client id",
		},
		{
			name:   "empty client secret",
			config: "issuer: " + issuer + "\nclients:\n  - id: kodactl\n    secret: \"\"\n    redirect_uris:\n      - http://127.0.0.1:52790/callback\npersonas:\n  - sub: tenant-a-admin\n    claims:\n      org_id: tenant-a\n",
			want:   "client secret",
		},
		{
			name:   "null client secret",
			config: "issuer: " + issuer + "\nclients:\n  - id: kodactl\n    secret:\n    redirect_uris:\n      - http://127.0.0.1:52790/callback\npersonas:\n  - sub: tenant-a-admin\n    claims:\n      org_id: tenant-a\n",
			want:   "client secret",
		},
		{
			name:   "whitespace client secret",
			config: "issuer: " + issuer + "\nclients:\n  - id: kodactl\n    secret: \" \"\n    redirect_uris:\n      - http://127.0.0.1:52790/callback\npersonas:\n  - sub: tenant-a-admin\n    claims:\n      org_id: tenant-a\n",
			want:   "client secret",
		},
		{
			name:   "missing clients",
			config: "issuer: " + issuer + "\npersonas:\n  - sub: tenant-a-admin\n    claims:\n      org_id: tenant-a\n",
			want:   "client",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			values := oauthSonasValuesFile(t, "oauthsonas:\n  enabled: true\n  config: |\n    "+strings.ReplaceAll(testCase.config, "\n", "\n    "))
			rendered, err := oauthSonasRender(t, helm, chart, "kodama-system", values)
			if err == nil {
				t.Fatalf("render accepted %s:\n%s", testCase.name, rendered)
			}
			if !strings.Contains(strings.ToLower(rendered), strings.ToLower(testCase.want)) {
				t.Errorf("render refused %s for an unexpected reason (want %q): %v\n%s", testCase.name, testCase.want, err, rendered)
			}
		})
	}

	// A public client without a secret stays valid: the guard only refuses an
	// explicitly empty secret, not the absence of one.
	values := oauthSonasValuesFile(t, oauthSonasBaseValues)
	if _, err := oauthSonasRender(t, helm, chart, "kodama-system", values); err != nil {
		t.Fatalf("render refused a valid public OAuthSonas client: %v", err)
	}
}
