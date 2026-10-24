package devenv

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeHost records commands and answers the few inspections the environment
// performs. It stands in for a container engine, kind and kubectl.
type fakeHost struct {
	t          *testing.T
	calls      [][]string
	nodes      []string
	owned      []string
	networks   map[string]map[string]string
	kubeconfig bool
	jobLogs    string
}

func (f *fakeHost) run(c Cmd) (Result, error) {
	args := append([]string{}, c.Args...)
	if len(args) > 0 {
		args[0] = filepath.Base(args[0])
	}
	f.calls = append(f.calls, args)
	result := Result{}
	fail := func(stderr string) (Result, error) {
		result.Stderr = stderr
		result.ExitCode = 1
		return result, fmt.Errorf("%s failed with exit 1", strings.Join(args[:min(len(args), 4)], " "))
	}
	switch args[0] {
	case "go":
		if len(args) > 1 && args[1] == "env" {
			lock, _ := Versions()
			result.Stdout = "go" + lock.Go + "\n"
		}
		return result, nil
	case "gofmt":
		return result, nil
	case "kind":
		if len(args) < 2 {
			return result, nil
		}
		switch args[1] {
		case "get":
			result.Stdout = strings.Join(f.nodes, "\n")
		case "create":
			f.nodes = []string{"kind-node"}
			for index, arg := range args {
				if arg == "--kubeconfig" && index+1 < len(args) {
					_ = os.WriteFile(args[index+1], []byte("kubeconfig"), 0o600)
					f.kubeconfig = true
				}
			}
		case "export":
			for index, arg := range args {
				if arg == "--kubeconfig" && index+1 < len(args) {
					_ = os.WriteFile(args[index+1], []byte("kubeconfig"), 0o600)
					f.kubeconfig = true
				}
			}
		case "delete":
			f.nodes = nil
		}
		return result, nil
	case "kubectl":
		if len(args) > 2 && args[len(args)-2] == "-o" && args[len(args)-1] == "json" {
			result.Stdout = `{"status":{"succeeded":1}}`
		}
		if len(args) > 1 && args[1] == "logs" {
			result.Stdout = f.jobLogs
		}
		return result, nil
	}
	if args[0] != "podman" && args[0] != "docker" {
		return result, nil
	}
	if len(args) < 2 {
		return result, nil
	}
	switch {
	case args[1] == "info" || (args[1] == "compose" && len(args) > 2 && args[2] == "version"):
		return result, nil
	case args[1] == "compose":
		if f.networks == nil {
			f.networks = map[string]map[string]string{}
		}
		return result, nil
	case args[1] == "network":
		if f.networks == nil {
			f.networks = map[string]map[string]string{}
		}
		switch args[2] {
		case "inspect":
			name := args[3]
			labels, ok := f.networks[name]
			if !ok {
				return fail("Error: no such network")
			}
			items := []map[string]any{{"Name": name, "Labels": labels}}
			data, _ := json.Marshal(items)
			result.Stdout = string(data)
			return result, nil
		case "create":
			name := args[len(args)-1]
			f.networks[name] = map[string]string{ScopeLabel: ownerFromArgs(args)}
			return result, nil
		case "rm":
			delete(f.networks, args[3])
			return result, nil
		case "exists":
			if _, ok := f.networks[args[3]]; !ok {
				return fail("not found")
			}
			return result, nil
		}
	case args[1] == "container" && len(args) > 2 && args[2] == "inspect":
		container := f.container(args[3])
		if container == nil {
			return fail("Error: no such container")
		}
		data, _ := json.Marshal([]Container{*container})
		result.Stdout = string(data)
		return result, nil
	case args[1] == "ps":
		result.Stdout = strings.Join(f.owned, "\n")
		return result, nil
	case args[1] == "rm":
		name := args[len(args)-1]
		kept := []string{}
		for _, item := range f.owned {
			if item != name {
				kept = append(kept, item)
			}
		}
		f.owned = kept
		return result, nil
	}
	return result, nil
}

func ownerFromArgs(args []string) string {
	for index, arg := range args {
		if arg == "--label" && index+1 < len(args) {
			if _, owner, ok := strings.Cut(args[index+1], "="); ok {
				return owner
			}
		}
	}
	return ""
}

func (f *fakeHost) container(name string) *Container {
	ports := ServicePorts
	container := &Container{ID: "id-" + name, Name: name}
	container.State.Running = true
	container.State.Status = "running"
	container.Config.Labels = map[string]string{}
	container.NetworkSettings.Networks = map[string]Network{"kind": {IPAddress: "10.0.0.5"}}
	container.NetworkSettings.Ports = map[string][]PortBinding{}
	for service, port := range ports {
		if strings.HasSuffix(name, "-"+service) {
			container.NetworkSettings.Ports[fmt.Sprintf("%d/tcp", port)] = []PortBinding{{HostPort: fmt.Sprintf("%d", port+10000)}}
		}
	}
	for _, node := range f.nodes {
		if name == node {
			container.ID = "node-id"
			container.Config.Labels["io.x-k8s.kind.cluster"] = f.clusterFromCalls()
		}
	}
	if strings.HasPrefix(name, "kind-node") {
		container.ID = "node-id"
	}
	return container
}

func (f *fakeHost) clusterFromCalls() string {
	for _, call := range f.calls {
		for index, arg := range call {
			if arg == "--name" && index+1 < len(call) {
				return call[index+1]
			}
		}
	}
	return ""
}

func fakeEnvironment(t *testing.T) (*fakeHost, *Environment) {
	t.Helper()
	host := &fakeHost{t: t, networks: map[string]map[string]string{}}
	previousState := stateDirOverride
	previousRun := runCmd
	previousPath := lookPath
	previousInstall := installTool
	previousBuild := buildImageFn
	previousCert := certGenFn
	previousSleep := sleep
	previousURL := reachableURLFn
	previousHostPort := reachableHostPortFn
	stateDirOverride = t.TempDir()
	runCmd = host.run
	lookPath = func(name string) (string, error) { return "/simulated/bin/" + name, nil }
	installTool = func(name string) (string, error) { return "/tools/" + name, nil }
	buildImageFn = func(*Environment) error { return nil }
	certGenFn = func(string, string) error { return nil }
	sleep = func(d time.Duration) {}
	reachableURLFn = func(string, time.Duration) bool { return true }
	reachableHostPortFn = func(string, string, time.Duration) bool { return true }
	t.Cleanup(func() {
		stateDirOverride = previousState
		runCmd = previousRun
		lookPath = previousPath
		installTool = previousInstall
		buildImageFn = previousBuild
		certGenFn = previousCert
		sleep = previousSleep
		reachableURLFn = previousURL
		reachableHostPortFn = previousHostPort
	})
	return host, nil
}

func newTestEnvironment(t *testing.T, stack string) (*fakeHost, *Environment) {
	t.Helper()
	host, _ := fakeEnvironment(t)
	environment, err := Load("t-test-scope", "podman", stack)
	if err != nil {
		t.Fatal(err)
	}
	return host, environment
}

func (f *fakeHost) verbs(words ...string) [][]string {
	found := [][]string{}
	for _, call := range f.calls {
		for start := 0; start+len(words) <= len(call); start++ {
			match := true
			for index, word := range words {
				if call[start+index] != word {
					match = false
					break
				}
			}
			if match {
				found = append(found, call)
				break
			}
		}
	}
	return found
}

func TestUnitStackSelection(t *testing.T) {
	t.Run("stacks are exactly services and cluster", func(t *testing.T) {
		if err := ValidateStack(StackServices); err != nil {
			t.Fatal(err)
		}
		if err := ValidateStack(StackCluster); err != nil {
			t.Fatal(err)
		}
		if DefaultStack != StackCluster {
			t.Fatalf("default stack = %q", DefaultStack)
		}
	})
	t.Run("unknown or empty values are rejected", func(t *testing.T) {
		for _, value := range []string{"", "kind", "Services", "cluster ", "none", "services,cluster"} {
			if err := ValidateStack(value); err == nil {
				t.Fatalf("%q was accepted", value)
			}
		}
	})
	t.Run("selection comes from the argument, then the environment, then the default", func(t *testing.T) {
		value, err := RequestedStack("services", []string{"DEV_STACK=cluster"})
		if err != nil || value != "services" {
			t.Fatalf("argument selection = %q %v", value, err)
		}
		value, err = RequestedStack("", []string{"DEV_STACK=services"})
		if err != nil || value != "services" {
			t.Fatalf("environment selection = %q %v", value, err)
		}
		value, err = RequestedStack("", nil)
		if err != nil || value != "" {
			t.Fatalf("unset selection = %q %v", value, err)
		}
	})
	t.Run("documented resource sets", func(t *testing.T) {
		if len(ServicesResources) != 7 {
			t.Fatalf("services resources = %v", ServicesResources)
		}
		for _, resource := range []string{"kind-cluster", "kubeconfig", "server-deployment"} {
			if Provides(StackServices, resource) {
				t.Fatalf("services provides %s", resource)
			}
			if !Provides(StackCluster, resource) {
				t.Fatalf("cluster does not provide %s", resource)
			}
		}
	})
	t.Run("services never creates a cluster or a kubeconfig", func(t *testing.T) {
		host, environment := newTestEnvironment(t, StackServices)
		if err := environment.Up(); err != nil {
			t.Fatal(err)
		}
		if len(host.verbs("kind")) != 0 {
			t.Fatalf("kind was invoked: %v", host.verbs("kind"))
		}
		if len(host.verbs("kubectl")) != 0 {
			t.Fatalf("kubectl was invoked: %v", host.verbs("kubectl"))
		}
		if exists(filepath.Join(environment.Dir, "kubeconfig")) {
			t.Fatal("services wrote a kubeconfig")
		}
		if environment.State.Stack != StackServices || environment.State.Network != environment.State.Cluster+"-net" {
			t.Fatalf("state = %+v", environment.State)
		}
		if len(host.verbs("network", "create")) != 1 {
			t.Fatalf("network create calls = %v", host.verbs("network", "create"))
		}
	})
	t.Run("services joins compose to the scoped network and cluster to kind", func(t *testing.T) {
		host, services := newTestEnvironment(t, StackServices)
		if err := services.Up(); err != nil {
			t.Fatal(err)
		}
		if services.Network() != services.State.Cluster+"-net" {
			t.Fatalf("network = %q", services.Network())
		}
		if len(host.verbs("compose", "-p")) != 2 {
			t.Fatalf("compose calls = %v", host.verbs("compose", "-p"))
		}
		clusterHost, cluster := newTestEnvironment(t, StackCluster)
		if err := cluster.Up(); err != nil {
			t.Fatal(err)
		}
		if cluster.Network() != "kind" {
			t.Fatalf("cluster network = %q", cluster.Network())
		}
		if len(clusterHost.verbs("kind", "create")) != 1 {
			t.Fatalf("kind create calls = %v", clusterHost.verbs("kind", "create"))
		}
	})
	t.Run("an existing scope refuses a different stack", func(t *testing.T) {
		_, services := newTestEnvironment(t, StackServices)
		if err := services.Up(); err != nil {
			t.Fatal(err)
		}
		requested, err := Load(services.Scope, services.Provider, StackCluster)
		if err != nil {
			t.Fatal(err)
		}
		host := &fakeHost{t: t, networks: map[string]map[string]string{}}
		previous := runCmd
		runCmd = host.run
		t.Cleanup(func() { runCmd = previous })
		if err := requested.Up(); err == nil || !strings.Contains(err.Error(), "created with DEV_STACK=services") {
			t.Fatalf("up error = %v", err)
		}
		if err := requested.Preflight(); err == nil || !strings.Contains(err.Error(), "created with DEV_STACK=services") {
			t.Fatalf("preflight error = %v", err)
		}
		if len(host.calls) != 0 {
			t.Fatalf("commands ran: %v", host.calls)
		}
	})
	t.Run("a scope without a recorded stack is a cluster scope", func(t *testing.T) {
		_, environment := newTestEnvironment(t, StackServices)
		if err := environment.Up(); err != nil {
			t.Fatal(err)
		}
		environment.State.Stack = ""
		if err := environment.Save(); err != nil {
			t.Fatal(err)
		}
		legacy, err := Load(environment.Scope, environment.Provider, "")
		if err != nil {
			t.Fatal(err)
		}
		if legacy.Stack != StackCluster || !legacy.Provides("kind-cluster") {
			t.Fatalf("legacy stack = %q", legacy.Stack)
		}
	})
}

func TestUnitServicesEndpoints(t *testing.T) {
	_, environment := newTestEnvironment(t, StackServices)
	if err := environment.Up(); err != nil {
		t.Fatal(err)
	}
	hostEnv, err := readStringMap(filepath.Join(environment.Dir, "host-env.json"))
	if err != nil {
		t.Fatal(err)
	}
	workload, err := readStringMap(filepath.Join(environment.Dir, "endpoints.json"))
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(environment.Dir, "host-env.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("host-env mode = %o", info.Mode().Perm())
	}
	if hostEnv["DEV_STACK"] != StackServices {
		t.Fatalf("DEV_STACK = %q", hostEnv["DEV_STACK"])
	}
	for _, key := range []string{"KODAMA_TEST_KUBECONFIG", "KUBECTL"} {
		if _, ok := hostEnv[key]; ok {
			t.Fatalf("services host env names %s", key)
		}
	}
	if strings.Contains(fmt.Sprint(hostEnv, workload), "svc.cluster.local") {
		t.Fatal("services endpoints name a cluster URL")
	}
	if _, ok := workload["KODAMA_SERVER_URL"]; ok {
		t.Fatal("services workload env names a server URL")
	}
	if workload["KODAMA_PODMAN_NETWORK"] != environment.State.Network {
		t.Fatalf("podman network = %q", workload["KODAMA_PODMAN_NETWORK"])
	}
	if workload["KODAMA_SERVER_CALLBACK_HOST"] != "host.containers.internal" {
		t.Fatalf("callback host = %q", workload["KODAMA_SERVER_CALLBACK_HOST"])
	}
	for _, item := range []struct{ service, key, scheme string }{
		{"postgres", "KODAMA_POSTGRES_URL", "postgres://"},
		{"nats", "KODAMA_NATS_URL", "nats://"},
		{"localstack", "KODAMA_S3_ENDPOINT", "http://"},
		{"git", "KODAMA_GIT_URL", "https://"},
		{"oauthsonas", "KODAMA_OIDC_ISSUER", "http://"},
		{"registry", "KODAMA_REGISTRY_URL", "http://"},
	} {
		prefix := item.scheme + environment.State.Cluster + "-" + item.service + ":"
		if !strings.HasPrefix(workload[item.key], prefix) {
			t.Fatalf("%s = %q, want prefix %q", item.key, workload[item.key], prefix)
		}
	}
	if strings.Contains(workload["KODAMA_POSTGRES_URL"], "@") {
		t.Fatal("workload PostgreSQL URL carries credentials")
	}
	if strings.Contains(fmt.Sprint(workload), "127.0.0.1") || strings.Contains(fmt.Sprint(workload), "localhost") {
		t.Fatal("workload endpoints name a host address")
	}
}

func TestUnitEnvironmentOwnership(t *testing.T) {
	for _, scope := range []string{"", "../outside", "default;exit", strings.Repeat("a", 40), "UPPER", "-prefix", "suffix-"} {
		if err := ValidateScope(scope); err == nil {
			t.Fatalf("scope %q was accepted", scope)
		}
	}
	_, environment := newTestEnvironment(t, StackServices)
	if err := environment.Up(); err != nil {
		t.Fatal(err)
	}
	environment.State.Root = "/another/worktree"
	if err := environment.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := Load("t-test-scope", "podman", ""); err == nil {
		t.Fatal("a foreign worktree was accepted")
	}
}

func TestUnitFixtures(t *testing.T) {
	_, environment := newTestEnvironment(t, StackServices)
	if err := environment.Up(); err != nil {
		t.Fatal(err)
	}
	directory, err := environment.Fixtures()
	if err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(filepath.Join(directory, "git-write-token"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := environment.Fixtures(); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(filepath.Join(directory, "git-write-token"))
	if string(first) != string(second) {
		t.Fatal("fixture credentials changed on a second setup")
	}
	info, err := os.Stat(directory)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("fixtures mode = %o", info.Mode().Perm())
	}
	sql, _ := os.ReadFile(filepath.Join(directory, "init.sql"))
	if !strings.Contains(string(sql), environment.Database()) {
		t.Fatalf("init.sql does not name %s", environment.Database())
	}
	read, _ := os.ReadFile(filepath.Join(directory, "git-read-token"))
	if string(read) == string(first) {
		t.Fatal("read and write tokens are equal")
	}
}

func TestUnitPreflight(t *testing.T) {
	t.Run("a missing scope names the directory and the recovery command", func(t *testing.T) {
		host, environment := newTestEnvironment(t, StackServices)
		err := environment.Preflight()
		if err == nil || !strings.Contains(err.Error(), "missing dev scope") ||
			!strings.Contains(err.Error(), "make dev-up DEV_SCOPE=t-test-scope") {
			t.Fatalf("preflight error = %v", err)
		}
		if len(host.calls) != 0 {
			t.Fatalf("preflight provisioned: %v", host.calls)
		}
	})
	t.Run("an unreachable endpoint reports its key, never the credential", func(t *testing.T) {
		_, environment := newTestEnvironment(t, StackServices)
		if err := environment.Up(); err != nil {
			t.Fatal(err)
		}
		reachableHostPortFn = func(string, string, time.Duration) bool { return false }
		err := environment.Preflight()
		if err == nil || !strings.Contains(err.Error(), "fixture unavailable: KODAMA_TEST_POSTGRES_ADMIN_URL") {
			t.Fatalf("preflight error = %v", err)
		}
		for _, secret := range []string{"postgres-password", "private diagnostic"} {
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("preflight leaked %q", secret)
			}
		}
	})
	t.Run("a foreign network fails preflight", func(t *testing.T) {
		host, environment := newTestEnvironment(t, StackServices)
		if err := environment.Up(); err != nil {
			t.Fatal(err)
		}
		host.networks[environment.State.Network] = map[string]string{ScopeLabel: strings.Repeat("d", 32)}
		err := environment.Preflight()
		if err == nil || !strings.Contains(err.Error(), "fixture unavailable: network") {
			t.Fatalf("preflight error = %v", err)
		}
	})
}

func TestUnitTeardown(t *testing.T) {
	t.Run("follows the recorded stack, not the request", func(t *testing.T) {
		host, environment := newTestEnvironment(t, StackServices)
		if err := environment.Up(); err != nil {
			t.Fatal(err)
		}
		host.owned = []string{"owned-1", "owned-2"}
		requested, err := Load(environment.Scope, environment.Provider, StackCluster)
		if err != nil {
			t.Fatal(err)
		}
		if err := requested.Down(); err != nil {
			t.Fatal(err)
		}
		if len(host.verbs("network", "rm")) != 1 {
			t.Fatalf("network rm calls = %v", host.verbs("network", "rm"))
		}
		if len(host.verbs("kind", "delete")) != 0 {
			t.Fatalf("kind delete calls = %v", host.verbs("kind", "delete"))
		}
		if exists(environment.Dir) {
			t.Fatal("scope directory remains")
		}
	})
	t.Run("removes exactly the labelled containers and the network, in order", func(t *testing.T) {
		host, environment := newTestEnvironment(t, StackServices)
		if err := environment.Up(); err != nil {
			t.Fatal(err)
		}
		host.owned = []string{"owned-1", "owned-2"}
		network, owner := environment.State.Network, environment.State.Owner
		if err := environment.Down(); err != nil {
			t.Fatal(err)
		}
		selection := host.verbs("ps")
		if len(selection) != 1 || !strings.Contains(strings.Join(selection[0], " "), ScopeLabel+"="+owner) {
			t.Fatalf("container selection = %v", selection)
		}
		removed := host.verbs("podman", "rm", "-f")
		if len(removed) != 2 || removed[0][3] != "owned-1" || removed[1][3] != "owned-2" {
			t.Fatalf("container removals = %v", removed)
		}
		networks := host.verbs("podman", "network", "rm")
		if len(networks) != 1 || networks[0][3] != network {
			t.Fatalf("network removals = %v", networks)
		}
		if indexOfCall(host.calls, networks[0]) < indexOfCall(host.calls, removed[len(removed)-1]) {
			t.Fatal("network removal ran before container removal")
		}
	})
	t.Run("refuses a foreign network before deleting anything", func(t *testing.T) {
		host, environment := newTestEnvironment(t, StackServices)
		if err := environment.Up(); err != nil {
			t.Fatal(err)
		}
		host.networks[environment.State.Network] = map[string]string{ScopeLabel: strings.Repeat("e", 32)}
		err := environment.Down()
		if err == nil || !strings.Contains(err.Error(), "ownership mismatch") {
			t.Fatalf("down error = %v", err)
		}
		if len(host.verbs("podman", "compose", "down")) != 0 || len(host.verbs("podman", "rm")) != 0 || len(host.verbs("podman", "network", "rm")) != 0 {
			t.Fatalf("commands ran: %v", host.calls)
		}
	})
	t.Run("handles an interrupted setup and repeats", func(t *testing.T) {
		host, environment := newTestEnvironment(t, StackServices)
		if err := environment.Up(); err != nil {
			t.Fatal(err)
		}
		delete(host.networks, environment.State.Network)
		if err := environment.Down(); err != nil {
			t.Fatal(err)
		}
		if len(host.verbs("network", "rm")) != 0 {
			t.Fatal("an absent network was removed")
		}
		if err := environment.Down(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("rejects a state whose network is not bound to the owner", func(t *testing.T) {
		_, environment := newTestEnvironment(t, StackServices)
		if err := environment.Up(); err != nil {
			t.Fatal(err)
		}
		environment.State.Network = "podman"
		if err := environment.Save(); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(environment.Scope, environment.Provider, ""); err == nil {
			t.Fatal("a foreign network name was accepted")
		}
	})
	t.Run("an absent scope needs no engine", func(t *testing.T) {
		host, _ := fakeEnvironment(t)
		environment, err := Load("t-absent", "podman", StackServices)
		if err != nil {
			t.Fatal(err)
		}
		if err := environment.Down(); err != nil {
			t.Fatal(err)
		}
		if len(host.calls) != 0 {
			t.Fatalf("commands ran: %v", host.calls)
		}
	})
}

func TestUnitManifests(t *testing.T) {
	_, environment := newTestEnvironment(t, StackCluster)
	if err := environment.Up(); err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	for _, name := range []string{"ca.crt", "tls.key", "postgres-password", "master-key", "git-read-token"} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte("fixture"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	environment.State.Image = "localhost:15000/t-test-scope/scaffold@sha256:" + strings.Repeat("b", 64)
	addresses := map[string]string{}
	for service := range ServicePorts {
		addresses[service] = "10.89.0.5"
	}
	items, err := environment.Manifests(directory, addresses)
	if err != nil {
		t.Fatal(err)
	}
	var secret map[string]string
	slices := 0
	for _, item := range items {
		switch item["kind"] {
		case "Secret":
			secret = item["data"].(map[string]string)
		case "EndpointSlice":
			slices++
		}
		metadata := item["metadata"].(map[string]any)
		labels := metadata["labels"].(map[string]string)
		if labels[ScopeLabel] != environment.State.Owner {
			t.Fatalf("%v carries no ownership label", metadata["name"])
		}
	}
	if secret == nil {
		t.Fatal("no fixture secret")
	}
	if _, ok := secret["tls.key"]; ok {
		t.Fatal("the probe secret carries the TLS private key")
	}
	if _, ok := secret["master-key"]; ok {
		t.Fatal("the probe secret carries the master key")
	}
	if _, ok := secret["postgres-password"]; ok {
		t.Fatal("the probe secret carries the PostgreSQL administrator password")
	}
	if _, ok := secret["ca.crt"]; !ok {
		t.Fatal("the probe secret lacks the CA")
	}
	if slices != len(ServicePorts) {
		t.Fatalf("endpoint slices = %d", slices)
	}
	if strings.Contains(fmt.Sprint(items), "latest") {
		t.Fatal("a manifest names a mutable tag")
	}
}

func indexOfCall(calls [][]string, wanted []string) int {
	for index, call := range calls {
		if len(call) != len(wanted) {
			continue
		}
		match := true
		for position := range call {
			if call[position] != wanted[position] {
				match = false
				break
			}
		}
		if match {
			return index
		}
	}
	return -1
}
