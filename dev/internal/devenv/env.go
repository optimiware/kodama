package devenv

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Environment is one scoped development stack.
type Environment struct {
	Scope     string
	Provider  string
	Requested string
	Stack     string
	Dir       string
	StatePath string
	State     *State
	Env       []string
	Kind      string
	Kubectl   string
	// RequestedGit is the explicit Git hosting selection (DEV_GIT), "" when none.
	RequestedGit string
}

// Hooks tests replace instead of running a real engine or downloading tools.
var (
	lookPath     = defaultLookPath
	installTool  = InstallTool
	buildImageFn = func(e *Environment) error { return e.buildImage() }
	certGenFn    = func(dir, host string) error {
		_, err := output("go", "run", "./dev/certs", dir, host)
		return err
	}
)

// Load reads the environment of a scope. A scope that already recorded a
// stack keeps it.
func Load(scope, provider, requestedStack string) (*Environment, error) {
	if err := ValidateScope(scope); err != nil {
		return nil, err
	}
	if err := ValidateProvider(provider); err != nil {
		return nil, err
	}
	if requestedStack != "" {
		if err := ValidateStack(requestedStack); err != nil {
			return nil, err
		}
	}
	e := &Environment{
		Scope: scope, Provider: provider, Requested: requestedStack,
		Dir:       filepath.Join(StateDir(), "environments", scope),
		StatePath: filepath.Join(StateDir(), "environments", scope, "state.json"),
		Env: append(os.Environ(), "KIND_EXPERIMENTAL_PROVIDER="+provider,
			"PODMAN_COMPOSE_WARNING_LOGS=false"),
	}
	state, err := LoadState(scope, provider)
	if err != nil {
		return nil, err
	}
	e.State = state
	switch {
	case state != nil:
		e.Stack = state.Stack
	case requestedStack != "":
		e.Stack = requestedStack
	default:
		e.Stack = DefaultStack
	}
	return e, nil
}

// SelectStack refuses to build a different stack into an existing scope.
func (e *Environment) SelectStack() error {
	if e.Requested != "" && e.State != nil && e.Requested != e.Stack {
		return fmt.Errorf("scope %s was created with DEV_STACK=%s; run make dev-down first to select %s",
			e.Scope, e.Stack, e.Requested)
	}
	return nil
}

// GitMode is the Git hosting of the scope: the recorded one, else the requested
// one, else the compatibility fixture.
func (e *Environment) GitMode() string {
	switch {
	case e.State != nil && e.State.GitHosting != "":
		return e.State.GitHosting
	case e.State != nil:
		return DefaultGitHosting // created before the selection existed
	case e.RequestedGit != "":
		return e.RequestedGit
	default:
		return DefaultGitHosting
	}
}

// SelectGitHosting refuses to build a different Git hosting into an existing
// scope, and the native hosting anywhere but the services stack.
func (e *Environment) SelectGitHosting() error {
	if e.RequestedGit != "" && e.State != nil && e.RequestedGit != e.GitMode() {
		return fmt.Errorf("scope %s was created with DEV_GIT=%s; run make dev-down first to select %s",
			e.Scope, e.GitMode(), e.RequestedGit)
	}
	if e.GitMode() == GitNative && e.Stack != StackServices {
		return fmt.Errorf("DEV_GIT=%s hosts the kernel's Git workers on the Podman services network and needs DEV_STACK=%s; scope %s is %s",
			GitNative, StackServices, e.Scope, e.Stack)
	}
	return nil
}

// Provides reports whether the recorded stack owns a resource.
func (e *Environment) Provides(resource string) bool { return Provides(e.Stack, resource) }

// WorkloadHost is the name a workload (a Pod or a container) uses for a
// service.
func (e *Environment) WorkloadHost(service string) string {
	if e.Stack == StackCluster {
		return service + "." + DepsNamespace + ".svc.cluster.local"
	}
	return e.Container(service)
}

// Container is the Compose container name of a service.
func (e *Environment) Container(service string) string {
	return e.State.Cluster + "-" + service
}

// Database is the per-scope database name.
func (e *Environment) Database() string {
	return "kodama_" + strings.ReplaceAll(e.Scope, "-", "_")
}

// Network is the network every service joins: kind's own, or the scoped
// Podman network of the services stack.
func (e *Environment) Network() string {
	if e.Stack == StackCluster {
		return "kind"
	}
	return e.State.Network
}

// WorkloadEnv is the endpoint inventory for code running inside the stack.
func (e *Environment) WorkloadEnv() map[string]string {
	values := ProbeEnv(e.Scope, e.WorkloadHost)
	if e.Stack != StackServices {
		return values
	}
	// The server under test runs on the host, not as a Service in this stack.
	delete(values, "KODAMA_SERVER_URL")
	values["KODAMA_POSTGRES_URL"] = fmt.Sprintf("postgres://%s:5432/%s", e.WorkloadHost("postgres"), e.Database())
	values["KODAMA_PODMAN_NETWORK"] = e.State.Network
	values["KODAMA_SERVER_CALLBACK_HOST"] = CallbackHosts[e.Provider]
	if e.GitMode() == GitNative {
		// No external Git fixture runs: a workload reaches a tenant's own worker.
		delete(values, "KODAMA_GIT_URL")
		values["KODAMA_GIT_WORKER_URL"] = e.GitWorkerURLTemplate()
	}
	return values
}

// probeChecks selects the dependency probe checks the scope can answer: the
// native Git hosting has no fixture for the HTTPS Git check.
func (e *Environment) probeChecks() string {
	if e.GitMode() != GitNative {
		return ServicesProbeChecks
	}
	var checks []string
	for _, check := range strings.Split(ServicesProbeChecks, ",") {
		if check != "https_git" {
			checks = append(checks, check)
		}
	}
	return strings.Join(checks, ",")
}

// Save writes the ownership journal.
func (e *Environment) Save() error { return WriteJSON(e.StatePath, e.State) }

// Prepare requires the engine, its Compose provider and, for a cluster scope,
// the pinned Kubernetes tools.
func (e *Environment) Prepare() error {
	if _, err := lookPath(e.Provider); err != nil {
		return fmt.Errorf("%s is missing from PATH", e.Provider)
	}
	if _, err := runCmd(Cmd{Args: []string{e.Provider, "info"}, Capture: true}); err != nil {
		return err
	}
	if _, err := runCmd(Cmd{Args: []string{e.Provider, "compose", "version"}, Capture: true, Env: e.Env}); err != nil {
		return err
	}
	if e.Provides("kind-cluster") {
		var err error
		if e.Kind, err = installTool("kind"); err != nil {
			return err
		}
		if e.Kubectl, err = installTool("kubectl"); err != nil {
			return err
		}
	}
	return nil
}

// Compose runs Compose for this scope; the project name is the cluster name. The
// compatibility Git fixture is a profile, started only when the scope selected it.
func (e *Environment) Compose(args ...string) error {
	var profiles []string
	if e.GitMode() == GitFixture {
		profiles = append(profiles, profileGitFixture)
	}
	return e.composeProject(e.State.Cluster, profiles, nil, args...)
}

// composeProject runs Compose for one project of this scope with the given
// profiles and extra variables.
func (e *Environment) composeProject(project string, profiles, extra []string, args ...string) error {
	env := append(append([]string{}, e.Env...),
		"KODAMA_PROJECT="+e.State.Cluster,
		"KODAMA_OWNER="+e.State.Owner,
		"KODAMA_FIXTURES="+filepath.Join(e.Dir, "fixtures"),
		"KODAMA_IMAGE="+defaultString(e.State.LocalImage, "scaffold:dev"),
		"KODAMA_NETWORK="+e.Network(),
	)
	env = append(env, extra...)
	command := []string{e.Provider, "compose", "-p", project, "-f", ComposeFile()}
	for _, profile := range profiles {
		command = append(command, "--profile", profile)
	}
	_, err := runCmd(Cmd{Args: append(command, args...), Env: env})
	return err
}

// Kube runs kubectl against this scope's private kubeconfig, never the
// caller's current context.
func (e *Environment) Kube(args ...string) (string, error) {
	return e.KubeWith(nil, args...)
}

// Inspect reads one container.
func (e *Environment) Inspect(name string) (*Container, error) {
	result, err := runCmd(Cmd{Args: []string{e.Provider, "container", "inspect", name}, Capture: true})
	if err != nil {
		lower := strings.ToLower(result.Stderr)
		if strings.Contains(lower, "no such") || strings.Contains(lower, "not found") {
			return nil, nil
		}
		return nil, fmt.Errorf("container inspection failed: %w", err)
	}
	containers, err := decodeContainers(result.Stdout)
	if err != nil {
		return nil, err
	}
	if len(containers) != 1 {
		return nil, fmt.Errorf("container inspection returned %d objects", len(containers))
	}
	return &containers[0], nil
}

// OwnedContainers lists the containers carrying this scope's random label.
func (e *Environment) OwnedContainers() ([]string, error) {
	stdout, err := output(e.Provider, "ps", "-aq", "--filter", "label="+ScopeLabel+"="+e.State.Owner)
	if err != nil {
		return nil, err
	}
	return strings.Fields(stdout), nil
}

func defaultLookPath(name string) (string, error) {
	if path, err := exec.LookPath(name); err == nil && path != "" {
		return path, nil
	}
	return "", fmt.Errorf("%s not found in PATH", name)
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
