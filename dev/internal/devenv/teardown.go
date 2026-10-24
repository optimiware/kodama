package devenv

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// TeardownCheck is the real dev_teardown acceptance: the scope's own resources
// are removed while independent ones survive.
func TeardownCheck(scope, provider string) error {
	environment, err := Load(scope, provider, "")
	if err != nil {
		return err
	}
	if environment.State == nil {
		return fmt.Errorf("dev_teardown needs an existing scope from dev-up")
	}
	if err := environment.Prepare(); err != nil {
		return err
	}
	if environment.Stack == StackServices {
		return checkServicesStack(environment)
	}
	return checkClusterStack(environment)
}

func checkServicesStack(e *Environment) error {
	suffix, err := tokenHex(6)
	if err != nil {
		return err
	}
	foreign := "kodama-sentinel-" + suffix
	if err := execCmd(e.Provider, "network", "create", foreign); err != nil {
		return err
	}
	defer func() {
		_, _ = runCmd(Cmd{Args: []string{e.Provider, "rm", "-f", foreign}})
		_, _ = runCmd(Cmd{Args: []string{e.Provider, "network", "rm", foreign}})
	}()
	if err := execCmd(e.Provider, "run", "-d", "--name", foreign, "--network", foreign,
		"--entrypoint", "sleep", e.State.LocalImage, "600"); err != nil {
		return err
	}
	item, err := e.Inspect(foreign)
	if err != nil {
		return err
	}
	if item == nil {
		return fmt.Errorf("sentinel container %s was not created", foreign)
	}
	identity := item.ID
	owned, owner := e.State.Network, e.State.Owner
	// A network without our ownership label is never removed, and nothing else
	// is deleted first.
	e.State.Network = foreign
	refused := e.Down()
	e.State.Network = owned
	if refused == nil || !strings.Contains(refused.Error(), "ownership mismatch") {
		return fmt.Errorf("foreign network was accepted: %v", refused)
	}
	containers, err := e.OwnedContainers()
	if err != nil {
		return err
	}
	if len(containers) == 0 {
		return fmt.Errorf("refused teardown removed containers")
	}
	// A missing resource models interrupted or partial cleanup.
	if err := execCmd(e.Provider, "rm", "-f", containers[0]); err != nil {
		return err
	}
	if err := e.Down(); err != nil {
		return err
	}
	if err := e.Down(); err != nil {
		return err
	}
	remaining, err := runCmd(Cmd{Args: []string{e.Provider, "ps", "-aq", "--filter", "label=" + ScopeLabel + "=" + owner}, Capture: true})
	if err != nil {
		return err
	}
	if exists(e.Dir) || strings.TrimSpace(remaining.Stdout) != "" || networkExists(e, owned) {
		return fmt.Errorf("scoped resources remain")
	}
	after, err := e.Inspect(foreign)
	if err != nil {
		return err
	}
	if after == nil || after.ID != identity || !networkExists(e, foreign) {
		return fmt.Errorf("foreign resources changed")
	}
	fmt.Println("PASS dev_teardown (services): ownership guard, partial cleanup, repeated cleanup, foreign network/container")
	return nil
}

func checkClusterStack(e *Environment) error {
	suffix, err := tokenHex(6)
	if err != nil {
		return err
	}
	foreign := "kodama-sentinel-" + suffix
	sentinelDir := filepath.Join(StateDir(), "sentinels", foreign)
	if err := os.MkdirAll(sentinelDir, 0o700); err != nil {
		return err
	}
	config := filepath.Join(sentinelDir, "kubeconfig")
	userConfig := filepath.Join(homeDir(), ".kube", "config")
	original, originalExists := readFile(userConfig)
	lock, err := Versions()
	if err != nil {
		return err
	}
	defer func() {
		_, _ = runCmd(Cmd{Args: []string{e.Kind, "delete", "cluster", "--name", foreign, "--kubeconfig", config}, Env: e.Env})
		_ = os.RemoveAll(sentinelDir)
	}()
	if err := execCmdWithEnv(e.Env, e.Kind, "create", "cluster", "--name", foreign, "--image", lock.Images.Node,
		"--kubeconfig", config, "--wait", "180s"); err != nil {
		return err
	}
	kube := func(args ...string) (string, error) {
		return outputCmd(nil, e.Kubectl, append([]string{"--kubeconfig", config, "--context", "kind-" + foreign}, args...)...)
	}
	if _, err := kube("create", "configmap", "sentinel", "--from-literal=value=preserved"); err != nil {
		return err
	}
	nodesOutput, err := outputCmd(e.Env, e.Kind, "get", "nodes", "--name", foreign)
	if err != nil {
		return err
	}
	identities := map[string]string{}
	for _, name := range strings.Fields(nodesOutput) {
		item, err := e.Inspect(name)
		if err != nil {
			return err
		}
		if item == nil {
			return fmt.Errorf("sentinel node %s is missing", name)
		}
		identities[name] = item.ID
	}
	// First prove cleanup refusal while the original scope remains healthy.
	saved := map[string]string{}
	for name, id := range e.State.Nodes {
		saved[name] = id
	}
	for name := range e.State.Nodes {
		e.State.Nodes[name] = "wrong-id"
	}
	refused := e.Down()
	e.State.Nodes = saved
	if refused == nil || !strings.Contains(refused.Error(), "ownership mismatch") {
		return fmt.Errorf("foreign node identity was accepted: %v", refused)
	}
	// A missing resource models interrupted or partial cleanup.
	containers, err := e.OwnedContainers()
	if err != nil {
		return err
	}
	if len(containers) == 0 {
		return fmt.Errorf("no owned container to remove for partial cleanup")
	}
	if err := execCmd(e.Provider, "rm", "-f", containers[0]); err != nil {
		return err
	}
	ownedNodes := make([]string, 0, len(e.State.Nodes))
	for name := range e.State.Nodes {
		ownedNodes = append(ownedNodes, name)
	}
	if err := e.Down(); err != nil {
		return err
	}
	if err := e.Down(); err != nil {
		return err
	}
	if exists(e.Dir) {
		return fmt.Errorf("scope directory remains")
	}
	for _, name := range ownedNodes {
		item, err := e.Inspect(name)
		if err != nil {
			return err
		}
		if item != nil {
			return fmt.Errorf("scoped node %s remains", name)
		}
	}
	for name, identity := range identities {
		item, err := e.Inspect(name)
		if err != nil || item == nil || item.ID != identity {
			return fmt.Errorf("foreign node %s changed", name)
		}
	}
	data, err := kube("get", "configmap", "sentinel", "-o", "json")
	if err != nil {
		return err
	}
	var configMap struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal([]byte(data), &configMap); err != nil {
		return err
	}
	if configMap.Data["value"] != "preserved" {
		return fmt.Errorf("foreign resource changed")
	}
	after, afterExists := readFile(userConfig)
	if afterExists != originalExists || string(after) != string(original) {
		return fmt.Errorf("user kubeconfig changed")
	}
	fmt.Println("PASS dev_teardown: identity guard, partial cleanup, repeated cleanup, foreign cluster/resource, user kubeconfig")
	return nil
}

func networkExists(e *Environment, name string) bool {
	_, err := runCmd(Cmd{Args: []string{e.Provider, "network", "exists", name}, Capture: true})
	return err == nil
}

func execCmdWithEnv(env []string, name string, args ...string) error {
	_, err := runCmd(Cmd{Args: append([]string{name}, args...), Env: env})
	return err
}

func homeDir() string {
	home, _ := os.UserHomeDir()
	return home
}

func readFile(path string) ([]byte, bool) {
	data, err := os.ReadFile(path)
	return data, err == nil
}
