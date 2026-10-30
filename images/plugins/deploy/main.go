package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	statev1 "github.com/optimiware/kodama/contracts/state/v1"
	"github.com/optimiware/kodama/images/plugins/deploy/driver"
	"github.com/optimiware/kodama/images/plugins/deploy/drivers/cloudflare"
	"github.com/optimiware/kodama/images/plugins/deploy/drivers/gcp"
	"github.com/optimiware/kodama/images/plugins/deploy/drivers/kubernetes"
	"github.com/optimiware/kodama/images/plugins/deploy/engine"
	pluginstate "github.com/optimiware/kodama/pkg/pluginruntime/state"
)

type common struct {
	stateURL      string
	attempt       string
	binding       string
	tokenFile     string
	actionKey     string
	operationID   string
	recipe        string
	recipeVersion int64
	driver        string
	namespace     string
	release       string
	accountID     string
	worker        string
	project       string
	region        string
	service       string
	kubeconfig    string
	bundle        string
	manifest      string
	image         string
	desired       string
	rollback      string
	timeout       time.Duration
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	c := common{}
	fs.StringVar(&c.stateURL, "state-url", envFirst("KODAMA_STATE_URL", "KODAMA_SERVER_URL"), "state API base URL")
	fs.StringVar(&c.attempt, "state-attempt", os.Getenv("KODAMA_ATTEMPT_ID"), "runtime attempt ID")
	fs.StringVar(&c.binding, "state-binding", "", "declared state binding alias")
	fs.StringVar(&c.tokenFile, "state-token-file", os.Getenv("KODAMA_TOKEN_FILE"), "runtime token file")
	fs.StringVar(&c.actionKey, "action-key", os.Getenv("KODAMA_ACTION_KEY"), "stable logical action key")
	fs.StringVar(&c.operationID, "operation-id", "", "stable operation ID")
	fs.StringVar(&c.recipe, "recipe", "kubernetes-manifests", "recipe name")
	fs.Int64Var(&c.recipeVersion, "recipe-version", 1, "recipe version")
	fs.StringVar(&c.driver, "driver", "kubernetes", "driver: kubernetes, cloudflare, gcp")
	fs.StringVar(&c.namespace, "namespace", "", "target namespace")
	fs.StringVar(&c.release, "release", "", "release name")
	fs.StringVar(&c.accountID, "account-id", "", "Cloudflare account ID")
	fs.StringVar(&c.worker, "worker", "", "Cloudflare Worker name")
	fs.StringVar(&c.project, "project", "", "GCP project")
	fs.StringVar(&c.region, "region", "", "GCP region")
	fs.StringVar(&c.service, "service", "", "GCP Cloud Run service")
	fs.StringVar(&c.kubeconfig, "kubeconfig", os.Getenv("KUBECONFIG"), "target kubeconfig path")
	fs.StringVar(&c.bundle, "bundle", "", "verified bundle path")
	fs.StringVar(&c.manifest, "manifest", "", "local manifest path (development only)")
	fs.StringVar(&c.image, "image", "", "expected primary image reference")
	fs.StringVar(&c.desired, "desired-release", "", "desired release ID")
	fs.StringVar(&c.rollback, "rollback-release", "", "recorded release to roll back to")
	fs.DurationVar(&c.timeout, "timeout", 5*time.Minute, "operation timeout")
	if err := fs.Parse(os.Args[2:]); err != nil {
		os.Exit(2)
	}
	if c.actionKey == "" {
		c.actionKey = "local:" + cmd
	}
	if c.desired == "" {
		c.desired = c.release
	}
	if c.binding == "" {
		fail(fmt.Errorf("state-binding is required"))
	}
	client, err := pluginstate.New(c.stateURL, c.attempt, c.binding, c.tokenFile)
	if err != nil {
		fail(err)
	}
	drv, err := driverFor(c.driver)
	if err != nil {
		fail(err)
	}
	eng := &engine.Engine{State: client, Driver: drv}
	cfg := engine.Config{
		ActionKey: c.actionKey, OperationID: c.operationID, Resource: c.release,
		Recipe: c.recipe, RecipeVersion: c.recipeVersion,
		Configuration: mustJSON(map[string]any{
			"namespace": c.namespace, "release": c.release, "kubeconfig": c.kubeconfig,
			"bundlePath": c.bundle, "manifestPath": c.manifest, "image": c.image,
			"accountId": c.accountID, "worker": c.worker,
			"project": c.project, "region": c.region, "service": c.service,
			"timeoutSeconds": int(c.timeout.Seconds()),
		}),
		DesiredRelease: c.desired, Timeout: c.timeout,
		Artifacts: artifacts(c.bundle, c.manifest, c.image),
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout+30*time.Second)
	defer cancel()
	switch cmd {
	case "apply":
		out, err := eng.Apply(ctx, cfg)
		if err != nil {
			write(out)
			fail(err)
		}
		write(out)
	case "status":
		resource, obs, err := eng.Status(ctx, cfg)
		write(map[string]any{"resource": resource, "observation": obs})
		if err != nil {
			fail(err)
		}
	case "reconcile":
		out, err := eng.Reconcile(ctx, cfg)
		write(out)
		if err != nil {
			fail(err)
		}
	case "rollback":
		out, err := eng.Rollback(ctx, cfg, c.rollback)
		write(out)
		if err != nil {
			fail(err)
		}
	case "cleanup":
		out, err := eng.Cleanup(ctx, cfg)
		write(out)
		if err != nil {
			fail(err)
		}
	case "identify":
		id, err := drv.Identify(ctx, driver.IdentityRequest{Recipe: c.recipe, RecipeVersion: c.recipeVersion, Configuration: cfg.Configuration})
		write(id)
		if err != nil {
			fail(err)
		}
	default:
		usage()
		os.Exit(2)
	}
}

func driverFor(name string) (driver.Driver, error) {
	switch name {
	case "kubernetes":
		return &kubernetes.Driver{Kubectl: envOr("KUBECTL", "kubectl")}, nil
	case "cloudflare":
		return &cloudflare.Driver{Wrangler: envOr("WRANGLER", "wrangler")}, nil
	case "gcp":
		return &gcp.Driver{Gcloud: envOr("GCLOUD", "gcloud")}, nil
	default:
		return nil, fmt.Errorf("unknown driver %q", name)
	}
}

func artifacts(bundle, manifest, image string) []driver.Artifact {
	if bundle != "" {
		return []driver.Artifact{{ID: "bundle", Digest: digestFile(bundle), Kind: driver.ArtifactFile, Path: bundle}}
	}
	if manifest != "" {
		return []driver.Artifact{{ID: "manifest", Digest: digestFile(manifest), Kind: driver.ArtifactFile, Path: manifest}}
	}
	if image != "" {
		return []driver.Artifact{{ID: "image", Digest: digestValue(image), Kind: driver.ArtifactOCIImage, Registry: image}}
	}
	return nil
}

func digestFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return digestValue(string(data))
}

func digestValue(value string) string {
	// Kept local to the plugin binary; the server independently verifies the
	// artifact digest before delivering it to a runtime.
	return statev1.DigestValue(json.RawMessage(fmt.Sprintf("%q", value)), nil)
}

func mustJSON(v any) json.RawMessage {
	raw, _ := json.Marshal(v)
	return raw
}

func write(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "deploy:", err)
	os.Exit(1)
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); strings.TrimSpace(v) != "" {
		return v
	}
	return fallback
}

func envFirst(names ...string) string {
	for _, name := range names {
		if v := os.Getenv(name); strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: deploy apply|status|reconcile|rollback|cleanup|identify [flags]")
}
