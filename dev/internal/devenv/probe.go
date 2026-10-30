package devenv

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

var pulledImage = regexp.MustCompile(`(?m)^PULL_IMAGE ([a-z0-9/-]+@sha256:[0-9a-f]{64})$`)

// Integration runs the dependency probe: a restricted Job in the cluster
// stack, a labelled Podman workload in the services stack.
func (e *Environment) Integration() error {
	if err := e.SelectStack(); err != nil {
		return err
	}
	if e.State == nil || e.State.Image == "" {
		return fmt.Errorf("run make dev-up for this scope first")
	}
	if err := e.Prepare(); err != nil {
		return err
	}
	if e.Stack == StackServices {
		return e.servicesProbe()
	}
	suffix, err := tokenHex(4)
	if err != nil {
		return err
	}
	logs, err := e.Job("scaffold-"+suffix, e.State.Image, []string{"/usr/local/bin/probe"}, true)
	if err != nil {
		return err
	}
	match := pulledImage.FindStringSubmatch(logs)
	if match == nil || !strings.HasPrefix(match[1], e.Scope+"/") {
		return fmt.Errorf("probe did not report a scoped image digest")
	}
	image := fmt.Sprintf("localhost:%d/%s", e.State.Port, match[1])
	if _, err := e.Job("registry-pull-"+suffix, image, []string{"/usr/local/bin/kodama", "--help"}, false); err != nil {
		return err
	}
	fmt.Println("PASS cluster_smoke (including Pod push and kubelet digest pull)")
	return nil
}

// servicesProbe runs the probe as a Podman workload on the scoped network.
func (e *Environment) servicesProbe() error {
	fixtures := e.Dir + "/fixtures"
	environment := e.WorkloadEnv()
	environment["KODAMA_PROBE_CHECKS"] = e.probeChecks()
	suffix, err := tokenHex(4)
	if err != nil {
		return err
	}
	args := []string{e.Provider, "run", "--rm", "--network", e.State.Network,
		"--user", "65532:65532", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true",
		"--label", ScopeLabel + "=" + e.State.Owner,
		"--name", e.State.Cluster + "-probe-" + suffix}
	for key, value := range environment {
		args = append(args, "-e", key+"="+value)
	}
	for _, name := range ProbeSecrets {
		args = append(args, "-v", fixtures+"/"+name+":/secrets/"+name+":ro")
	}
	args = append(args, "--entrypoint", "/usr/local/bin/probe", e.State.LocalImage)
	result, err := runCmd(Cmd{Args: args, Capture: true, Timeout: 7 * time.Minute})
	fmt.Print(result.Stdout)
	if err != nil {
		return fmt.Errorf("services probe: %w: %s", err, strings.TrimSpace(result.Stderr))
	}
	fmt.Println("PASS services_smoke (Podman workload reached PostgreSQL, NATS, S3, Git, OIDC and the registry by workload URL)")
	return nil
}

// RuntimeAcceptance proves the endpoint files configure the real Podman
// runtime adapter. Run it on a services scope.
func (e *Environment) RuntimeAcceptance() error {
	if err := e.SelectStack(); err != nil {
		return err
	}
	if e.Stack != StackServices {
		return fmt.Errorf("runtime acceptance needs DEV_STACK=services; scope %s is %s", e.Scope, e.Stack)
	}
	if e.State == nil || e.State.Image == "" {
		return fmt.Errorf("run make dev-up DEV_STACK=services for this scope first")
	}
	if e.Provider != "podman" {
		return fmt.Errorf("the Podman runtime acceptance needs DEV_PROVIDER=podman")
	}
	env := append(os.Environ(), "KODAMA_DEV_SCOPE_DIR="+e.Dir, "KODAMA_REQUIRE_PODMAN=1")
	if _, err := runCmd(Cmd{Args: []string{"go", "test", "-count=1", "-v", "./dev/runtimeacceptance"},
		Env: env, Timeout: 10 * time.Minute}); err != nil {
		return err
	}
	fmt.Println("PASS runtime_acceptance (Podman runtime workloads reached PostgreSQL by workload URL and the host callback)")
	return nil
}

// Smoke builds a fresh scope and runs the dependency probe.
func (e *Environment) Smoke() error {
	if e.State != nil {
		return fmt.Errorf("dev smoke requires a fresh DEV_SCOPE")
	}
	if err := e.Up(); err != nil {
		return err
	}
	return e.Integration()
}
