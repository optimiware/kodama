package devenv

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// State is the durable ownership journal of one scope. It is written before
// any external resource exists and never reconstructed by guessing.
type State struct {
	Root       string            `json:"root"`
	Scope      string            `json:"scope"`
	Provider   string            `json:"provider"`
	Owner      string            `json:"owner"`
	Stack      string            `json:"stack"`
	Cluster    string            `json:"cluster"`
	Nodes      map[string]string `json:"nodes"`
	Network    string            `json:"network,omitempty"`
	Port       int               `json:"port,omitempty"`
	LocalImage string            `json:"local_image,omitempty"`
	Image      string            `json:"image,omitempty"`
	// GitHosting is the Git hosting the scope was created with (fixture when
	// empty, as before the native hosting existed); GitTenants are the tenants
	// whose native worker containers this scope runs.
	GitHosting string   `json:"git_hosting,omitempty"`
	GitTenants []string `json:"git_tenants,omitempty"`
}

var ownerPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// ValidateState refuses a journal that does not belong to this checkout,
// provider or scope.
func ValidateState(state *State, scope, provider string) error {
	if state.Root != Root() || state.Provider != provider || state.Scope != scope || !ownerPattern.MatchString(state.Owner) {
		return fmt.Errorf("state owner/worktree/provider mismatch; refusing resource mutation")
	}
	if state.Stack == "" {
		state.Stack = DefaultStack
	}
	if err := ValidateStack(state.Stack); err != nil {
		return err
	}
	if state.Cluster != "kodama-"+scope+"-"+state.Owner[:8] {
		return fmt.Errorf("state resource names do not match ownership token")
	}
	if state.GitHosting != "" {
		if err := ValidateGitHosting(state.GitHosting); err != nil {
			return err
		}
	}
	for _, tenant := range state.GitTenants {
		if err := ValidateGitTenant(tenant); err != nil {
			return err
		}
	}
	if state.Stack == StackServices && state.Network != state.Cluster+"-net" {
		return fmt.Errorf("state network name does not match ownership token")
	}
	return nil
}

// LoadState reads the journal of a scope; a missing journal is (nil, nil).
func LoadState(scope, provider string) (*State, error) {
	data, err := os.ReadFile(filepath.Join(StateDir(), "environments", scope, "state.json"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(StateDir(), "environments", scope, "state.json"), err)
	}
	if state.Nodes == nil {
		state.Nodes = map[string]string{}
	}
	if err := ValidateState(&state, scope, provider); err != nil {
		return nil, err
	}
	return &state, nil
}

// WriteJSON writes a private file atomically.
func WriteJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}
