package delivery

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Plan is everything needed to render the application's objects. Rendering is a
// pure function of the plan, so a retry produces byte-identical manifests.
type Plan struct {
	Namespace    string
	Annotations  map[string]string // the receipt's annotations, put on every object
	ImageRepo    string            // registry repository, without tag or digest
	ImageDigest  string
	SourceCommit string
	DeploymentID string
	ConfigDigest string
	Config       map[string]string // explicit nonsecret environment configuration
	Secrets      map[string]string // delivered script credentials by binding name; never logged
}

// Required explicit settings and credentials.
var (
	RequiredConfig  = []string{"OIDC_ISSUER", "OIDC_AUDIENCE", "ERP_BASE_URL", "WORKER_TENANT", "WORKER_SERVICE_ID", "DATABASE_RUNTIME_ROLE"}
	RequiredSecrets = []string{"DATABASE_RUNTIME_URL", "DATABASE_MIGRATION_URL", "CURSOR_KEY", "ERP_TOKEN"}
)

// Validate refuses a plan with a missing explicit input before anything is applied.
func (p Plan) Validate() error {
	var miss []string
	for _, k := range RequiredConfig {
		if strings.TrimSpace(p.Config[k]) == "" {
			miss = append(miss, "config "+k)
		}
	}
	for _, k := range RequiredSecrets {
		if p.Secrets[k] == "" {
			miss = append(miss, "credential "+k)
		}
	}
	if len(miss) > 0 {
		sort.Strings(miss)
		return fmt.Errorf("the environment does not supply: %s", strings.Join(miss, ", "))
	}
	if !digestRE.MatchString(p.ImageDigest) || p.ImageRepo == "" || strings.ContainsAny(p.ImageRepo, "@ \t") {
		return fmt.Errorf("the image must be a repository and a sha256 digest")
	}
	if n := p.Replicas(); n < 1 || n > 5 {
		return fmt.Errorf("REPLICAS must be an integer within 1..5")
	}
	return nil
}

// Replicas is the configured replica count (default 1).
func (p Plan) Replicas() int {
	v := strings.TrimSpace(p.Config["REPLICAS"])
	if v == "" {
		return 1
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0
	}
	return n
}

// ImageRef is the pull reference: always by digest.
func (p Plan) ImageRef() string { return p.ImageRepo + "@" + p.ImageDigest }

// JobName is unique per image and configuration, because a Job's template is immutable.
func (p Plan) JobName() string {
	sum := sha256.Sum256([]byte(p.ImageDigest + "|" + p.ConfigDigest))
	return "inventory-migrate-" + hex.EncodeToString(sum[:])[:10]
}

// Refs lists the intended object identities in apply order.
func (p Plan) Refs() []ObjectRef {
	ns := p.Namespace
	return []ObjectRef{
		{"v1", "ConfigMap", ns, "inventory-config"},
		{"v1", "Secret", ns, "inventory-runtime"},
		{"v1", "Secret", ns, "inventory-migrate"},
		{"batch/v1", "Job", ns, p.JobName()},
		{"apps/v1", "Deployment", ns, "inventory-api"},
		{"apps/v1", "Deployment", ns, "inventory-worker"},
		{"v1", "Service", ns, "inventory-api"},
	}
}

type m = map[string]any

func (p Plan) meta(name, component string) m {
	ann := m{}
	for k, v := range p.Annotations {
		ann[k] = v
	}
	return m{"name": name, "namespace": p.Namespace, "annotations": ann,
		"labels": m{"app.kubernetes.io/name": "inventory", "app.kubernetes.io/component": component}}
}

func (p Plan) podEnv() []m {
	return []m{
		{"name": "INVENTORY_DEPLOYMENT_ID", "value": p.DeploymentID},
		{"name": "INVENTORY_CONFIGURATION_DIGEST", "value": p.ConfigDigest},
		{"name": "INVENTORY_IMAGE_DIGEST", "value": p.ImageDigest},
		{"name": "INVENTORY_SOURCE_COMMIT", "value": p.SourceCommit},
	}
}

func hardened(name string, args []string, mounts []m) m {
	return m{
		"name": name, "image": "", "args": args, "env": nil, "volumeMounts": mounts,
		"securityContext": m{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "runAsNonRoot": true,
			"privileged": false, "capabilities": m{"drop": []string{"ALL"}}, "seccompProfile": m{"type": "RuntimeDefault"}},
		"resources": m{"requests": m{"cpu": "50m", "memory": "64Mi"}, "limits": m{"cpu": "500m", "memory": "256Mi"}},
	}
}

// secretFiles names, per component, the only credential files its pod may
// mount from the shared Secret objects: a projection, not the whole Secret.
// The API never receives the ERP token; the worker never receives the cursor
// key; the migrator receives only the migration database URL.
var secretFiles = map[string][]string{
	"api":     {"database-url", "cursor-key"},
	"worker":  {"database-url", "erp-token"},
	"migrate": {"database-url"},
}

// SecretFiles returns the credential files a component's pod mounts.
func SecretFiles(component string) []string { return append([]string(nil), secretFiles[component]...) }

func (p Plan) pod(component string, args []string, secretName string, probes bool) m {
	c := hardened(component, args, []m{
		{"name": "config", "mountPath": "/etc/inventory", "readOnly": true},
		{"name": "secrets", "mountPath": "/run/secrets/inventory", "readOnly": true},
		{"name": "tmp", "mountPath": "/tmp"},
	})
	c["image"] = p.ImageRef()
	c["env"] = p.podEnv()
	if probes {
		c["ports"] = []m{{"name": "http", "containerPort": 8080}}
		c["readinessProbe"] = m{"httpGet": m{"path": "/readyz", "port": "http"}, "periodSeconds": 5, "failureThreshold": 6}
		c["livenessProbe"] = m{"httpGet": m{"path": "/healthz", "port": "http"}, "periodSeconds": 10, "failureThreshold": 3}
	}
	return m{
		"automountServiceAccountToken": false,
		// fsGroup makes the 0440 credential files group-readable by the
		// non-root process (uid/gid 65532) without making them world-readable.
		"securityContext": m{"runAsNonRoot": true, "runAsUser": 65532, "runAsGroup": 65532, "fsGroup": 65532, "seccompProfile": m{"type": "RuntimeDefault"}},
		"containers":      []m{c},
		"volumes": []m{
			{"name": "config", "configMap": m{"name": "inventory-config"}},
			{"name": "secrets", "secret": m{"secretName": secretName, "defaultMode": 288, "items": secretItems(component)}},
			{"name": "tmp", "emptyDir": m{"sizeLimit": "16Mi"}},
		},
	}
}

func secretItems(component string) []m {
	var items []m
	for _, f := range secretFiles[component] {
		items = append(items, m{"key": f, "path": f})
	}
	return items
}

func (p Plan) deployment(name, component string, args []string, replicas int, probes bool) m {
	labels := m{"app.kubernetes.io/name": "inventory", "app.kubernetes.io/component": component}
	ann := m{}
	for k, v := range p.Annotations {
		ann[k] = v
	}
	return m{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": p.meta(name, component),
		"spec": m{"replicas": replicas, "selector": m{"matchLabels": labels},
			"template": m{"metadata": m{"labels": labels, "annotations": ann}, "spec": p.pod(component, args, "inventory-runtime", probes)}}}
}

// Render returns the JSON List of every object.
func (p Plan) Render() ([]byte, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	scopes := []string{"inventory:adjust", "inventory:read"}
	api := m{"httpAddr": ":8080", "database": m{"urlFile": "/run/secrets/inventory/database-url", "maxConns": 10},
		"oidc":          m{"issuer": p.Config["OIDC_ISSUER"], "audience": p.Config["OIDC_AUDIENCE"], "allowInsecureIssuer": p.Config["OIDC_ALLOW_INSECURE"] == "true"},
		"cursorKeyFile": "/run/secrets/inventory/cursor-key", "shutdownTimeout": "10s"}
	worker := m{"database": m{"urlFile": "/run/secrets/inventory/database-url", "maxConns": 4}, "tenant": p.Config["WORKER_TENANT"], "serviceId": p.Config["WORKER_SERVICE_ID"],
		"erp": m{"baseUrl": p.Config["ERP_BASE_URL"], "timeoutSeconds": 10, "allowInsecure": p.Config["ERP_ALLOW_INSECURE"] == "true", "scopes": scopes,
			"credentialFiles": m{p.Config["WORKER_TENANT"]: "/run/secrets/inventory/erp-token"}}}
	migrate := m{"database": m{"urlFile": "/run/secrets/inventory/database-url", "maxConns": 2}, "runtimeRole": p.Config["DATABASE_RUNTIME_ROLE"]}
	enc := func(v any) string { b, _ := json.Marshal(v); return string(b) }

	job := m{"apiVersion": "batch/v1", "kind": "Job", "metadata": p.meta(p.JobName(), "migrate"),
		"spec": m{"backoffLimit": 2, "ttlSecondsAfterFinished": 86400, "template": m{"spec": func() m {
			pod := p.pod("migrate", []string{"migrate", "--config", "/etc/inventory/migrate.json"}, "inventory-migrate", false)
			pod["restartPolicy"] = "Never"
			return pod
		}()}}}
	items := []m{
		{"apiVersion": "v1", "kind": "ConfigMap", "metadata": p.meta("inventory-config", "config"),
			"data": m{"api.json": enc(api), "worker.json": enc(worker), "migrate.json": enc(migrate)}},
		{"apiVersion": "v1", "kind": "Secret", "metadata": p.meta("inventory-runtime", "runtime"), "type": "Opaque",
			"stringData": m{"database-url": p.Secrets["DATABASE_RUNTIME_URL"], "cursor-key": p.Secrets["CURSOR_KEY"], "erp-token": p.Secrets["ERP_TOKEN"]}},
		{"apiVersion": "v1", "kind": "Secret", "metadata": p.meta("inventory-migrate", "migrate"), "type": "Opaque",
			"stringData": m{"database-url": p.Secrets["DATABASE_MIGRATION_URL"]}},
		job,
		p.deployment("inventory-api", "api", []string{"serve", "--config", "/etc/inventory/api.json"}, p.Replicas(), true),
		p.deployment("inventory-worker", "worker", []string{"worker", "--config", "/etc/inventory/worker.json"}, 1, false),
		{"apiVersion": "v1", "kind": "Service", "metadata": p.meta("inventory-api", "api"),
			"spec": m{"selector": m{"app.kubernetes.io/name": "inventory", "app.kubernetes.io/component": "api"}, "ports": []m{{"name": "http", "port": 8080, "targetPort": "http"}}}},
	}
	// Defence in depth: every object stays in the one namespace the credential covers.
	for _, it := range items {
		if it["metadata"].(m)["namespace"] != p.Namespace {
			return nil, fmt.Errorf("rendered object outside the environment namespace")
		}
	}
	return json.MarshalIndent(m{"apiVersion": "v1", "kind": "List", "items": items}, "", "  ")
}
