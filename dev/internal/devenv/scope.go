package devenv

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ScopeLabel marks every external resource a scope owns. Touching an object
// without this label is never allowed, so two worktrees cannot delete each
// other's stacks.
const ScopeLabel = "dev.kodama.owner"

// Namespaces of the cluster stack.
const (
	DepsNamespace  = "kodama-deps"
	TestsNamespace = "kodama-test"
)

// Stacks this repository can build.
const (
	StackServices = "services"
	StackCluster  = "cluster"
)

// DefaultStack is used when neither the record nor the request names one.
const DefaultStack = StackCluster

// Git hosting of a scope (DEV_GIT). The external git-http-backend fixture is the
// compatibility profile and stays the default: it exercises HTTPS and tokens
// against a plain Git server and needs no kernel. native hosts the kernel's own
// Git service: one `kodama git serve` container per tenant on the scoped network
// (O-36, docs/implementation/internal-git-service.md). It is selected explicitly,
// and only on the services stack, which is the `test` environment.
const (
	GitFixture = "fixture"
	GitNative  = "native"
)

// DefaultGitHosting is used when neither the record nor the request names one.
const DefaultGitHosting = GitFixture

// Compose profiles of the two hostings.
const (
	profileGitFixture = "git-fixture"
	profileGitNative  = "git-native"
)

// GitWorkerPort is the port a tenant's worker listens on inside its container.
const GitWorkerPort = 8443

// Resources a stack owns. "cluster" is "services" plus the Kubernetes
// resources; teardown removes exactly the resources the recorded stack lists.
var (
	ServicesResources = []string{"registry", "postgres", "nats", "localstack", "git", "oauthsonas", "workload-network"}
	ClusterResources  = append(append([]string{}, ServicesResources...), "kind-cluster", "kubeconfig", "server-deployment")
)

// ServicePorts are the container ports the stack publishes on loopback.
var ServicePorts = map[string]int{
	"postgres": 5432, "nats": 4222, "localstack": 4566, "git": 8443, "oauthsonas": 8181, "registry": 5000,
}

// ServicesProbeChecks selects the dependency probe checks that make sense
// without a server Deployment.
const ServicesProbeChecks = "postgres,jetstream,s3,https_git,oidc_pkce,registry"

// CallbackHosts maps a container provider to the name a workload uses to
// reach the host.
var CallbackHosts = map[string]string{
	"podman": "host.containers.internal",
	"docker": "host.docker.internal",
}

// ProbeSecrets are the fixture files a probe workload receives.
var ProbeSecrets = []string{
	"database-runtime-url", "database-migration-url", "nats-token", "git-read-token", "git-write-token",
	"s3-access-key", "s3-secret-key", "ca.crt",
}

var scopePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,29}[a-z0-9]$`)

// ValidateScope rejects a scope name that could escape its state directory or
// collide with a shared resource.
func ValidateScope(scope string) error {
	if !scopePattern.MatchString(scope) {
		return fmt.Errorf("DEV_SCOPE must be 2-31 lowercase letters/digits/hyphens, starting with a letter, not %q", scope)
	}
	return nil
}

// ValidateProvider accepts the two supported container engines.
func ValidateProvider(provider string) error {
	if provider != "podman" && provider != "docker" {
		return fmt.Errorf("DEV_PROVIDER must be podman or docker, not %q", provider)
	}
	return nil
}

// ValidateStack accepts only the documented stacks.
func ValidateStack(stack string) error {
	if stack != StackServices && stack != StackCluster {
		return fmt.Errorf("DEV_STACK must be one of %s or %s, not %q", StackServices, StackCluster, stack)
	}
	return nil
}

// ValidateGitHosting accepts only the documented hostings.
func ValidateGitHosting(hosting string) error {
	if hosting != GitFixture && hosting != GitNative {
		return fmt.Errorf("DEV_GIT must be %s or %s, not %q", GitFixture, GitNative, hosting)
	}
	return nil
}

// RequestedGitHosting returns the explicit selection, else $DEV_GIT, or "" when
// neither is set. An explicit empty value is invalid.
func RequestedGitHosting(argument string, environ []string) (string, error) {
	value, set := argument, argument != ""
	if !set {
		for _, entry := range environ {
			if strings.HasPrefix(entry, "DEV_GIT=") {
				value, set = strings.TrimPrefix(entry, "DEV_GIT="), true
			}
		}
	}
	if !set {
		return "", nil
	}
	if err := ValidateGitHosting(value); err != nil {
		return "", err
	}
	return value, nil
}

var gitTenantPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,39}$`)

// ValidateGitTenant accepts the tenant identifiers a native worker can be named
// for: lowercase, because the container, its Compose project and the server's
// {tenantId} URL template must all spell the identifier the same way.
func ValidateGitTenant(tenant string) error {
	if !gitTenantPattern.MatchString(tenant) {
		return fmt.Errorf("the tenant ID must be 1-40 lowercase letters, digits, underscores and hyphens, starting with a letter, not %q", tenant)
	}
	return nil
}

// RequestedStack returns the explicit selection, else $DEV_STACK, or "" when
// neither is set. An explicit empty value is invalid.
func RequestedStack(argument string, environ []string) (string, error) {
	value, set := argument, argument != ""
	if !set {
		for _, entry := range environ {
			if strings.HasPrefix(entry, "DEV_STACK=") {
				value, set = strings.TrimPrefix(entry, "DEV_STACK="), true
			}
		}
	}
	if !set {
		return "", nil
	}
	if err := ValidateStack(value); err != nil {
		return "", err
	}
	return value, nil
}

// Resources returns the resources a stack provides.
func Resources(stack string) ([]string, error) {
	switch stack {
	case StackServices:
		return ServicesResources, nil
	case StackCluster:
		return ClusterResources, nil
	default:
		return nil, ValidateStack(stack)
	}
}

// Provides reports whether a stack owns a resource.
func Provides(stack, resource string) bool {
	resources, err := Resources(stack)
	if err != nil {
		return false
	}
	for _, item := range resources {
		if item == resource {
			return true
		}
	}
	return false
}

// ErrForeignResource reports that a resource is not bound to this scope.
var ErrForeignResource = errors.New("resource ownership mismatch")
