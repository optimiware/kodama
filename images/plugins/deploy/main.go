package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/optimiware/kodama/images/plugins/deploy/driver"
	"github.com/optimiware/kodama/images/plugins/deploy/drivers/cloudflare"
	"github.com/optimiware/kodama/images/plugins/deploy/drivers/gcp"
	"github.com/optimiware/kodama/images/plugins/deploy/drivers/kubernetes"
	"github.com/optimiware/kodama/images/plugins/deploy/engine"
	pluginstate "github.com/optimiware/kodama/pkg/pluginruntime/state"
)

type common struct {
	stateURL          string
	attempt           string
	binding           string
	tokenFile         string
	actionKey         string
	operationID       string
	recipe            string
	recipeVersion     int64
	driver            string
	cluster           string
	namespace         string
	release           string
	accountID         string
	worker            string
	workerURL         string
	verificationURL   string
	buildMarker       string
	project           string
	region            string
	service           string
	kubeconfig        string
	kubeContext       string
	bundle            string
	manifest          string
	image             string
	auxiliaryImages   string
	allowMutableImage bool
	desired           string
	rollback          string
	planIn            string
	planOut           string
	importFile        string
	keep              int
	expired           bool
	expiresIn         time.Duration
	timeout           time.Duration
	resultFile        string
	outputsDir        string
	contextFile       string
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
	fs.StringVar(&c.cluster, "cluster", os.Getenv("KODAMA_PLUGIN_CLUSTER"), "stable target cluster identity")
	fs.StringVar(&c.namespace, "namespace", "", "target namespace")
	fs.StringVar(&c.release, "release", "", "release name")
	fs.StringVar(&c.accountID, "account-id", "", "Cloudflare account ID")
	fs.StringVar(&c.worker, "worker", "", "Cloudflare Worker name")
	fs.StringVar(&c.workerURL, "url", "", "public application URL for the receipt")
	fs.StringVar(&c.verificationURL, "verification-url", "", "served URL checked for readiness")
	fs.StringVar(&c.buildMarker, "build-marker", "", "string that must appear at the verification URL")
	fs.StringVar(&c.project, "project", "", "GCP project")
	fs.StringVar(&c.region, "region", "", "GCP region")
	fs.StringVar(&c.service, "service", "", "GCP Cloud Run service")
	fs.StringVar(&c.kubeconfig, "kubeconfig", os.Getenv("KUBECONFIG"), "target kubeconfig path")
	fs.StringVar(&c.kubeContext, "kube-context", "", "pinned kubeconfig context")
	fs.StringVar(&c.bundle, "bundle", "", "verified bundle path")
	fs.StringVar(&c.manifest, "manifest", "", "local manifest path (development only)")
	fs.StringVar(&c.image, "image", "", "expected primary image reference")
	fs.StringVar(&c.auxiliaryImages, "auxiliary-images", "", "comma-separated digest-pinned auxiliary images")
	fs.BoolVar(&c.allowMutableImage, "allow-mutable-image", false, "development-only mutable image opt-in")
	fs.StringVar(&c.desired, "desired-release", "", "desired release ID")
	fs.StringVar(&c.rollback, "rollback-release", "", "recorded release to roll back to")
	fs.StringVar(&c.planIn, "plan", "", "approved immutable plan to apply")
	fs.StringVar(&c.planOut, "plan-out", "", "write the immutable plan here")
	fs.StringVar(&c.importFile, "import", "", "legacy inventory document to import")
	fs.IntVar(&c.keep, "keep", 10, "release records retained by prune")
	fs.BoolVar(&c.expired, "expired", false, "clean up every expired resource in the binding")
	fs.DurationVar(&c.expiresIn, "expires-in", 0, "record a cleanup expiry this far in the future")
	fs.DurationVar(&c.timeout, "timeout", 5*time.Minute, "operation timeout")
	fs.StringVar(&c.resultFile, "result-file", os.Getenv("KODAMA_RESULTS_FILE"), "result manifest path")
	fs.StringVar(&c.outputsDir, "outputs-dir", envOr("KODAMA_OUTPUTS_DIR", "/kodama/outputs"), "declared outputs root")
	fs.StringVar(&c.contextFile, "context-file", os.Getenv("KODAMA_CONTEXT_FILE"), "private runtime context document")
	if err := fs.Parse(os.Args[2:]); err != nil {
		os.Exit(2)
	}
	if c.actionKey == "" {
		c.actionKey = "local:" + cmd
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
	ctxOutputs := readContext(c.contextFile)
	descriptors, err := artifacts(c.bundle, c.manifest, ctxOutputs)
	if err != nil {
		fail(err)
	}
	cfg := engine.Config{
		ActionKey: c.actionKey, OperationID: c.operationID, Resource: c.release,
		Recipe: c.recipe, RecipeVersion: c.recipeVersion,
		Configuration: mustJSON(map[string]any{
			"cluster": c.cluster, "namespace": c.namespace, "release": c.release, "kubeconfig": c.kubeconfig, "kubeContext": c.kubeContext,
			"bundlePath": c.bundle, "manifestPath": c.manifest, "image": c.image,
			"auxiliaryImages": splitList(c.auxiliaryImages), "allowMutableImage": c.allowMutableImage,
			"accountId": c.accountID, "worker": c.worker, "url": c.workerURL,
			"verificationUrl": c.verificationURL, "buildMarker": c.buildMarker,
			"project": c.project, "region": c.region, "service": c.service,
			"timeoutSeconds": int(c.timeout.Seconds()),
		}),
		DesiredRelease: c.desired, Timeout: c.timeout, ExpiresIn: c.expiresIn,
		Artifacts: descriptors,
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout+30*time.Second)
	defer cancel()
	switch cmd {
	case "plan":
		plan, err := eng.Plan(ctx, cfg)
		if err != nil {
			fail(err)
		}
		if c.planOut != "" {
			raw, err := json.MarshalIndent(plan, "", "  ")
			if err != nil {
				fail(err)
			}
			if err := os.WriteFile(c.planOut, append(raw, '\n'), 0o644); err != nil {
				fail(err)
			}
		}
		write(plan)
	case "apply":
		if c.planIn != "" {
			raw, err := os.ReadFile(c.planIn)
			if err != nil {
				fail(err)
			}
			var plan engine.PlanDoc
			if err := json.Unmarshal(raw, &plan); err != nil {
				fail(err)
			}
			cfg.DesiredRelease = plan.Release
			cfg.Artifacts = plan.Artifacts
			cfg.Configuration = plan.Configuration
			cfg.PlanDigest = plan.Fingerprint
		}
		out, err := eng.Apply(ctx, cfg)
		if err != nil {
			write(out)
			fail(err)
		}
		emitResult(c, out)
		write(out)
	case "prune":
		removed, err := eng.Prune(ctx, cfg, c.keep)
		write(map[string]any{"removed": removed})
		if err != nil {
			fail(err)
		}
	case "import-legacy":
		if c.importFile == "" {
			fail(fmt.Errorf("--import is required"))
		}
		raw, err := os.ReadFile(c.importFile)
		if err != nil {
			fail(err)
		}
		var doc engine.LegacyImport
		if err := json.Unmarshal(raw, &doc); err != nil {
			fail(err)
		}
		out, err := eng.ImportLegacy(ctx, cfg, doc)
		if err != nil {
			fail(err)
		}
		write(out)
	case "status":
		resource, obs, err := eng.Status(ctx, cfg)
		out := map[string]any{"resource": resource, "observation": obs}
		if err == nil {
			emitResult(c, out)
		}
		write(out)
		if err != nil {
			fail(err)
		}
	case "reconcile":
		out, err := eng.Reconcile(ctx, cfg)
		if err == nil {
			emitResult(c, out)
		}
		write(out)
		if err != nil {
			fail(err)
		}
	case "rollback":
		out, err := eng.Rollback(ctx, cfg, c.rollback)
		if err == nil {
			emitResult(c, out)
		}
		write(out)
		if err != nil {
			fail(err)
		}
	case "cleanup":
		if c.expired {
			out, err := eng.CleanupExpired(ctx, cfg)
			write(out)
			if err != nil {
				fail(err)
			}
			return
		}
		out, err := eng.Cleanup(ctx, cfg)
		if err == nil {
			emitResult(c, out)
		}
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

func splitList(value string) []string {
	var out []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// artifacts maps local input files to the verified descriptors pinned in the
// attempt context. The server compares the state reference digest with the
// artifact module's own digest, so a locally invented value can never be used.
func artifacts(bundle, manifest string, ctx contextOutputs) ([]driver.Artifact, error) {
	var out []driver.Artifact
	for _, in := range []struct {
		alias    string
		path     string
		fallback string
	}{{"bundle", bundle, "bundle"}, {"manifest", manifest, "manifest"}} {
		if in.path == "" {
			continue
		}
		artifact, err := artifactDescriptor(ctx, in.alias, in.path, in.fallback)
		if err != nil {
			return nil, err
		}
		out = append(out, artifact)
	}
	return out, nil
}

func artifactDescriptor(ctx contextOutputs, alias, path, fallback string) (driver.Artifact, error) {
	sum, err := sha256File(path)
	if err != nil {
		return driver.Artifact{}, fmt.Errorf("read %s artifact: %w", alias, err)
	}
	for _, pinned := range ctx.ArtifactFiles {
		if pinned.Name != alias {
			continue
		}
		if pinned.ArtifactID == "" || pinned.Digest == "" {
			return driver.Artifact{}, fmt.Errorf("pinned %s artifact descriptor is incomplete", alias)
		}
		if pinned.Digest != sum {
			return driver.Artifact{}, fmt.Errorf("pinned %s artifact digest does not match the local file", alias)
		}
		return driver.Artifact{ID: pinned.ArtifactID, Digest: pinned.Digest, Kind: driver.ArtifactFile, Path: path}, nil
	}
	// A development invocation without a runtime context still gets a real
	// content digest, never a value the server cannot verify.
	return driver.Artifact{ID: fallback, Digest: sum, Kind: driver.ArtifactFile, Path: path}, nil
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func readContext(path string) contextOutputs {
	if path == "" {
		return contextOutputs{}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return contextOutputs{}
	}
	var ctx contextOutputs
	_ = json.Unmarshal(raw, &ctx)
	return ctx
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
	fmt.Fprintln(os.Stderr, "usage: deploy plan|apply|status|reconcile|rollback|cleanup|prune|import-legacy|identify [flags]")
}

// contextOutputs is the small part of the pinned context the plugin needs to
// produce its declared result manifest. The entrypoint owns the full context;
// this command never infers outputs from the pipeline file.
type contextOutputs struct {
	ArtifactFiles []struct {
		Name       string `json:"name"`
		ArtifactID string `json:"artifactId"`
		Digest     string `json:"digest"`
	} `json:"artifactFiles"`
	Outputs []struct {
		Name string  `json:"name"`
		Type string  `json:"type"`
		Path *string `json:"path"`
	} `json:"outputs"`
}

func emitResult(c common, outcome any) {
	if c.resultFile == "" || c.contextFile == "" {
		return
	}
	raw, err := os.ReadFile(c.contextFile)
	if err != nil {
		return
	}
	var ctx contextOutputs
	if json.Unmarshal(raw, &ctx) != nil || len(ctx.Outputs) == 0 {
		return
	}
	resource := map[string]any{}
	url := ""
	if doc, ok := outcome.(map[string]any); ok {
		if r, ok := doc["resource"].(map[string]any); ok {
			resource = r
		}
	} else if doc, ok := outcome.(engine.Outcome); ok {
		resource = map[string]any{
			"desiredRelease": doc.Resource.DesiredRelease, "providerVersion": doc.Resource.ProviderVersion,
			"providerRelease": doc.Resource.ProviderRelease,
		}
		if doc.Operation.Result != nil {
			url = doc.Operation.Result.URL
		}
	}
	receipt := receiptFor(outcome, url)
	outputs := map[string]any{}
	for _, decl := range ctx.Outputs {
		switch decl.Type {
		case "artifact", "ociArtifact":
			if decl.Path == nil || decl.Type != "artifact" {
				continue
			}
			if err := os.MkdirAll(filepath.Dir(*decl.Path), 0o755); err != nil {
				fail(err)
			}
			var data []byte
			if decl.Name == "receipt" || decl.Name == "deployment" {
				data, _ = json.MarshalIndent(receipt, "", "  ")
			} else {
				data, _ = json.MarshalIndent(outcome, "", "  ")
			}
			if err := os.WriteFile(*decl.Path, append(data, '\n'), 0o644); err != nil {
				fail(err)
			}
		case "string":
			value := ""
			switch decl.Name {
			case "release_id":
				value = stringValue(resource["desiredRelease"])
			case "provider_version":
				value = stringValue(resource["providerVersion"])
			case "provider_release":
				value = stringValue(resource["providerRelease"])
			case "url":
				value = url
			}
			outputs[decl.Name] = value
		case "boolean":
			outputs[decl.Name] = false
		case "integer":
			outputs[decl.Name] = 0
		}
	}
	if err := os.MkdirAll(filepath.Dir(c.resultFile), 0o755); err != nil {
		fail(err)
	}
	data, err := json.Marshal(map[string]any{"outcome": "completed", "outputs": outputs})
	if err != nil {
		fail(err)
	}
	if err := os.WriteFile(c.resultFile, append(data, '\n'), 0o644); err != nil {
		fail(err)
	}
}

func stringValue(v any) string {
	s, _ := v.(string)
	return s
}

// receiptFor is a versioned, provider-neutral summary of one invocation. It
// carries identifiers and the observed provider version, never credentials or
// expiring download URLs.
func receiptFor(outcome any, url string) map[string]any {
	receipt := map[string]any{
		"schemaVersion": 1,
		"outcome":       "completed",
		"url":           url,
		"observedAt":    time.Now().UTC().Format(time.RFC3339Nano),
	}
	if doc, ok := outcome.(engine.Outcome); ok {
		receipt["operationId"] = doc.Operation.ID
		receipt["operationKind"] = doc.Operation.Kind
		receipt["operationStatus"] = doc.Operation.Status
		receipt["release"] = doc.Resource.DesiredRelease
		receipt["providerRelease"] = doc.Resource.ProviderRelease
		receipt["providerVersion"] = doc.Resource.ProviderVersion
		receipt["effect"] = doc.Operation.Effect
		if doc.Operation.Result != nil {
			receipt["result"] = doc.Operation.Result
		}
		if doc.Operation.Observation != nil {
			receipt["observation"] = doc.Operation.Observation
		}
	}
	return receipt
}
