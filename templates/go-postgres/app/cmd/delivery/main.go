// Command delivery is the helper behind the ordinary ci/*.sh scripts.
//
//	delivery build-verify --ref REPO@sha256:DIGEST
//	delivery deploy --kind preview|stage
//	delivery e2e
//
// Settings are explicit: flags override the environment. The runtime origin and
// token file are installation inputs (KODAMA_RUNTIME_URL and
// KODAMA_RUNTIME_TOKEN_FILE); the token itself is read from the file at call
// time, never from an environment variable. Outputs use the step entrypoint's
// variables KODAMA_OUTPUTS_DIR, KODAMA_RESULTS_FILE and KODAMA_SECRETS_DIR.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"example.invalid/inventory/internal/delivery"
)

func main() { os.Exit(run(os.Args[1:])) }

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func fatal(format string, a ...any) int {
	fmt.Fprintf(os.Stderr, "delivery: "+format+"\n", a...)
	return 1
}

func run(args []string) int {
	if len(args) == 0 {
		return usage()
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	switch args[0] {
	case "build-verify":
		return buildVerify(ctx, args[1:])
	case "deploy":
		return deploy(ctx, args[1:])
	case "e2e":
		return e2e(ctx, args[1:])
	case "snapshot-digest":
		return snapshotDigest()
	case "snapshot-verify":
		return snapshotVerify(args[1:])
	}
	return usage()
}

func usage() int {
	fmt.Fprintln(os.Stderr, "usage: delivery build-verify|deploy|e2e|snapshot-digest|snapshot-verify [flags]")
	return 2
}

// snapshotDigest reads an environment snapshot (JSON) on stdin and prints the
// configuration digest the factory pins for it, so an operator can check the
// run input before creating the run.
func snapshotDigest() int {
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil {
		return fatal("%v", err)
	}
	snap, err := delivery.ParseSnapshot(string(raw))
	if err != nil {
		return fatal("%v", err)
	}
	d, err := snap.Digest()
	if err != nil {
		return fatal("%v", err)
	}
	fmt.Println(d)
	return 0
}

// snapshotVerify reads {"context": <attempt context>, "snapshot": <environment
// snapshot>} on stdin and applies the same verification the deploy script runs
// before any effect. It exits 0 only when the snapshot matches the pinned
// context exactly. It lets an operator, and the contract tests, check a run
// input against a retained context without a factory or a cluster.
func snapshotVerify(args []string) int {
	fs := flag.NewFlagSet("snapshot-verify", flag.ContinueOnError)
	kind := fs.String("kind", "", "preview or stage")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil {
		return fatal("%v", err)
	}
	var in struct {
		Context  delivery.Context `json:"context"`
		Snapshot json.RawMessage  `json:"snapshot"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return fatal("malformed input: %v", err)
	}
	snap, err := delivery.ParseSnapshot(string(in.Snapshot))
	if err != nil {
		return fatal("%v", err)
	}
	if err := snap.Verify(in.Context, *kind); err != nil {
		return fatal("%v", err)
	}
	return 0
}

func registry() *delivery.Registry {
	return &delivery.Registry{Insecure: env("REGISTRY_INSECURE", "false") == "true", HTTP: &http.Client{Timeout: 20 * time.Second}}
}

func fail(results, reason string) {
	if err := delivery.WriteResults(results, "failed", reason, nil); err != nil {
		fmt.Fprintln(os.Stderr, "delivery: could not write the results file:", err)
	}
}

func buildVerify(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("build-verify", flag.ContinueOnError)
	ref := fs.String("ref", "", "pushed image reference repository@sha256:digest")
	commit := fs.String("source-commit", env("SOURCE_COMMIT", ""), "built source commit")
	tool := fs.String("tool-image", env("TOOLS_IMAGE", ""), "tools image used for the build")
	goVer := fs.String("go-version", env("GO_VERSION", ""), "Go toolchain version")
	platformList := fs.String("platforms", env("BUILD_PLATFORMS", ""), "comma-separated build platform set (linux/amd64,linux/arm64)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	outputs, results := env("KODAMA_OUTPUTS_DIR", ""), env("KODAMA_RESULTS_FILE", "")
	platforms, err := delivery.ParsePlatforms(*platformList)
	if err != nil {
		fail(results, "build_unverified")
		return fatal("%v", err)
	}
	in := delivery.BuildInput{Ref: *ref, SourceCommit: *commit, ToolImage: *tool, GoVersion: *goVer, Platforms: platforms, OutputsDir: outputs, ResultsFile: results, Images: registry()}
	// When the runtime is configured, the built commit must be the pinned one.
	if url := env("KODAMA_RUNTIME_URL", ""); url != "" {
		rt, err := delivery.NewRuntime(url, env("KODAMA_RUNTIME_TOKEN_FILE", ""), env("KODAMA_ATTEMPT_ID", ""))
		if err != nil {
			return fatal("%v", err)
		}
		cx, err := rt.Context(ctx)
		if err != nil {
			return fatal("reading the pinned context: %v", err)
		}
		// With a runtime context the pinned source and command image are
		// required and the build must agree with both.
		if cx.Workspace.Source == nil || cx.Command.Image == "" {
			fail(results, "build_unverified")
			return fatal("the pinned context has no source or command image to verify the build against")
		}
		in.ContextCommit, in.ContextToolImage = cx.Workspace.Source.Commit, cx.Command.Image
		if in.ToolImage == "" {
			in.ToolImage = cx.Command.Image
		}
	}
	if _, err := delivery.VerifyBuild(ctx, in); err != nil {
		fail(results, "build_unverified")
		return fatal("%v", err)
	}
	return 0
}

func deploy(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("deploy", flag.ContinueOnError)
	kind := fs.String("kind", "", "preview or stage")
	kubectl := fs.String("kubectl", env("KUBECTL", "kubectl"), "kubectl binary")
	timeout := fs.Duration("readiness-timeout", 3*time.Minute, "bound for object readiness")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	results, outputs := env("KODAMA_RESULTS_FILE", ""), env("KODAMA_OUTPUTS_DIR", "")
	rt, err := delivery.NewRuntime(env("KODAMA_RUNTIME_URL", ""), env("KODAMA_RUNTIME_TOKEN_FILE", ""), env("KODAMA_ATTEMPT_ID", ""))
	if err != nil {
		return fatal("%v", err)
	}
	d := &delivery.Deployer{Factory: rt, In: delivery.Input{
		Kind: *kind, SnapshotJSON: env("KODAMA_INPUT_ENVIRONMENT_SNAPSHOT", ""), SecretsDir: env("KODAMA_SECRETS_DIR", ""), Getenv: os.Getenv, OutputsDir: outputs,
		ClusterFor: func(kubeconfig string) delivery.Cluster {
			return &delivery.Kubectl{Binary: *kubectl, Kubeconfig: kubeconfig}
		},
		Images: registry(), ReadinessTimeout: *timeout,
	}}
	res, err := d.Run(ctx)
	if err != nil {
		fail(results, "deploy_internal_error")
		return fatal("%v", err)
	}
	if !res.Completed {
		fail(results, res.Reason)
		return fatal("deployment %s did not complete: %s", res.DeploymentID, res.Reason)
	}
	if err := delivery.WriteArtifact(outputs, "deployment.json", res.Evidence); err != nil {
		fail(results, "evidence_unwritable")
		return fatal("%v", err)
	}
	if err := delivery.WriteResults(results, "completed", "", map[string]any{"deployment_id": res.DeploymentID, "app_url": res.AppURL}); err != nil {
		return fatal("%v", err)
	}
	return 0
}

func e2e(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("e2e", flag.ContinueOnError)
	settle := fs.Duration("settle", 30*time.Second, "how long to wait for the ERP outcome")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	results, outputs, secrets := env("KODAMA_RESULTS_FILE", ""), env("KODAMA_OUTPUTS_DIR", ""), env("KODAMA_SECRETS_DIR", "")
	token := func(name string) string {
		raw, err := os.ReadFile(filepath.Join(secrets, name))
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(raw))
	}
	in := delivery.E2EInput{
		AppURL: env("APP_URL", ""), DeploymentID: env("DEPLOYMENT_ID", ""), ConfigurationDigest: env("CONFIGURATION_DIGEST", ""),
		ImageDigest: env("IMAGE_DIGEST", ""), SourceCommit: env("SOURCE_COMMIT", ""), Tenant: env("E2E_TENANT", ""), OtherTenant: env("E2E_OTHER_TENANT", ""),
		ReviewerToken: token("E2E_TOKEN_REVIEWER"), OutsiderToken: token("E2E_TOKEN_OUTSIDER"), Settle: *settle,
	}
	for name, v := range map[string]string{"APP_URL": in.AppURL, "DEPLOYMENT_ID": in.DeploymentID, "CONFIGURATION_DIGEST": in.ConfigurationDigest, "IMAGE_DIGEST": in.ImageDigest,
		"SOURCE_COMMIT": in.SourceCommit, "E2E_TENANT": in.Tenant, "E2E_OTHER_TENANT": in.OtherTenant, "E2E_TOKEN_REVIEWER": in.ReviewerToken, "E2E_TOKEN_OUTSIDER": in.OutsiderToken} {
		if v == "" {
			fail(results, "e2e_input_missing")
			return fatal("%s is required", name)
		}
	}
	rep, err := delivery.RunE2E(ctx, in)
	if werr := delivery.WriteArtifact(outputs, "e2e.json", rep); werr != nil {
		return fatal("%v", werr)
	}
	switch {
	case err != nil:
		fail(results, "e2e_target_unreachable")
		return fatal("%v", err)
	case !rep.Passed && len(rep.Checks) > 0 && rep.Checks[0].ID == "live-target-matches-deployment" && !rep.Checks[0].Passed:
		fail(results, delivery.ReasonDeploymentMismatch)
		return fatal("the live target is not deployment %s", in.DeploymentID)
	case !rep.Passed:
		fail(results, "e2e_failed")
		return fatal("end-to-end checks failed")
	}
	return writeOK(results)
}

func writeOK(results string) int {
	if err := delivery.WriteResults(results, "completed", "", nil); err != nil {
		return fatal("%v", err)
	}
	return 0
}
