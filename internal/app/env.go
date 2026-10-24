package app

import (
	"errors"
	"fmt"
	"strings"

	"github.com/optimiware/kodama/internal/execution"
	execmemory "github.com/optimiware/kodama/internal/execution/memory"
	"github.com/optimiware/kodama/internal/memdb"
	"github.com/optimiware/kodama/internal/runtime/podman"
	"github.com/optimiware/kodama/internal/runtime/process"
	"github.com/optimiware/kodama/internal/secrets/keyfile"
)

// Env is the environment that selects every outbound adapter of the process
// (ADR 0009, SD-12). Only this package reads it: domain and use-case packages
// depend on consumer-owned ports and never see an Env.
type Env string

// The three environments. There is no default.
const (
	// Development composes in-memory stores and the Process runtime.
	Development Env = "development"
	// Test composes the real dependency services and the Podman runtime.
	Test Env = "test"
	// Production composes the real services and the Kubernetes runtime and
	// refuses every development or test adapter.
	Production Env = "production"
)

// EnvVariable names the process setting that selects the environment.
const EnvVariable = "KODAMA_ENV"

var (
	// ErrEnvRequired reports that KODAMA_ENV is unset or empty. The environment
	// has no default: a process that cannot tell what it is must not guess.
	ErrEnvRequired = errors.New("KODAMA_ENV is required and has no default (development, test or production)")
	// ErrEnvUnknown reports a value that is not one of the three environments.
	ErrEnvUnknown = errors.New("KODAMA_ENV is not a known environment (development, test or production)")
	// ErrDevelopmentAdapter reports that production was asked to construct an
	// adapter that exists only for development or test.
	ErrDevelopmentAdapter = errors.New("production refuses development and test adapters")
)

// ParseEnv reads KODAMA_ENV once. An unset, empty or unknown value is an error
// that stops startup; the value is matched exactly (no case folding, no
// trimming), so a typo can never select a weaker environment than intended.
func ParseEnv(getenv func(string) string) (Env, error) {
	if getenv == nil {
		return "", ErrEnvRequired
	}
	v := getenv(EnvVariable)
	switch Env(v) {
	case Development, Test, Production:
		return Env(v), nil
	case "":
		return "", ErrEnvRequired
	}
	return "", fmt.Errorf("%w: %q", ErrEnvUnknown, v)
}

// Valid reports whether e is one of the three environments.
func (e Env) Valid() bool { return e == Development || e == Test || e == Production }

func (e Env) String() string { return string(e) }

// developmentOnly is the flag memory adapters carry for the composition root.
type developmentOnly interface{ DevelopmentOnly() bool }

// IsDevelopmentAdapter reports whether an adapter exists only for development
// or test. An adapter is one when it flags itself (`DevelopmentOnly() bool`,
// which every memory adapter of the `*test` packages does) or when it is one of
// the unflagged adapters this package knows by type: the memdb kernel, the
// execution memory store with its peer world and authority, the Process
// runtime and the development key file provider. A nil adapter is not one: a missing port is refused elsewhere.
func IsDevelopmentAdapter(adapter any) bool {
	if adapter == nil {
		return false
	}
	if d, ok := adapter.(developmentOnly); ok && d.DevelopmentOnly() {
		return true
	}
	switch adapter.(type) {
	case *memdb.Store, *execmemory.Store, *execmemory.World, *execmemory.Authority, *process.Runtime, *podman.Runtime, *keyfile.Provider:
		return true
	}
	// A runtime is identified by the backend it pins, so a decorator or any
	// other wrapper does not hide the Process or Podman runtime: production
	// runs workloads only on Kubernetes.
	if rt, ok := adapter.(execution.Runtime); ok {
		switch rt.Capabilities().Backend {
		case process.Backend, podman.Backend:
			return true
		}
	}
	return false
}

// AdapterError names the port whose adapter an environment refused.
type AdapterError struct {
	Env     Env
	Port    Port
	Adapter string
	Reason  string
}

func (e *AdapterError) Error() string {
	return fmt.Sprintf("%s refuses the %s adapter of the %s port: %s", e.Env, e.Adapter, e.Port, e.Reason)
}

// Is makes every refusal match ErrDevelopmentAdapter.
func (e *AdapterError) Is(target error) bool { return target == ErrDevelopmentAdapter }

// Admit applies the construction guard of SD-12 to one built adapter: an
// unknown environment admits nothing, and production refuses an adapter that
// is development or test only. The adapter is returned when it is admitted.
func Admit[T any](env Env, port Port, adapter T) (T, error) {
	var zero T
	if !env.Valid() {
		return zero, ErrEnvUnknown
	}
	if env == Production && IsDevelopmentAdapter(adapter) {
		return zero, &AdapterError{Env: env, Port: port, Adapter: adapterName(adapter), Reason: "it is flagged development or test only"}
	}
	return adapter, nil
}

func adapterName(adapter any) string {
	return strings.TrimPrefix(fmt.Sprintf("%T", adapter), "*")
}
