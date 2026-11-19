package workspace

// The toolbox contract shared by the container runtime adapters.
//
// A container runtime mounts the control plane's own kodema binary read-only
// at ToolboxRoot and starts the Kernel's script through Entrypoint. The image's
// shell stays the container's first process: the toolbox is a file the
// commands may call and, for an action that declares outputs, the one command
// the shell runs after the script has succeeded. Everything here is pure: it
// renders strings and never reads a file, starts a process or contacts a
// runtime.

import (
	"strconv"
	"strings"

	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/id"
	"github.com/optimiware/kodema/internal/kernel/script"
)

const (
	// ToolboxRoot is the read-only directory that holds the toolbox. The
	// launcher appends it to the image's PATH.
	ToolboxRoot = "/kodema/bin"
	// ToolboxName is the file name of the toolbox binary.
	ToolboxName = "kodema"
	// ToolboxPath is the toolbox binary inside a container action.
	ToolboxPath = ToolboxRoot + "/" + ToolboxName
	// EnvCAFile names the mounted installation CA bundle.
	EnvCAFile = "KODEMA_CA_FILE"
	// EnvGitURL is the base URL of the installation's Git service as a
	// workload reaches it.
	EnvGitURL = "KODEMA_GIT_URL"
	// OutputDirectoryBytes bounds the writable output directory. It holds at
	// most eight values of four kibibytes each.
	OutputDirectoryBytes = 1 << 20
)

// Git's environment configuration: GIT_CONFIG_COUNT pairs of
// GIT_CONFIG_KEY_<n> and GIT_CONFIG_VALUE_<n>.
const (
	envGitConfigCount       = "GIT_CONFIG_COUNT"
	envGitConfigKeyPrefix   = "GIT_CONFIG_KEY_"
	envGitConfigValuePrefix = "GIT_CONFIG_VALUE_"
)

// Launch selects what the launcher of one container action does.
type Launch struct {
	// Toolbox reports that the toolbox is mounted at ToolboxRoot.
	Toolbox bool
	// Outputs reports that the action declares outputs, so its output
	// directory is mounted writable.
	Outputs bool
}

// Entrypoint returns the entrypoint that runs the Kernel's script at
// scriptPath. Without a toolbox it is the image's shell on the script and
// nothing else. With one, the shell appends ToolboxRoot to the image's own
// PATH, never replacing it and never leading with an empty element, and runs
// the script; when the action also declares outputs and the script exited
// zero, the shell is replaced by `kodema output push`. The exit status is the
// script's when it failed and the push's otherwise.
func Entrypoint(scriptPath string, launch Launch) []string {
	if !launch.Toolbox {
		return []string{script.ShellPath, scriptPath}
	}
	command := `export PATH="${PATH:+$PATH:}` + ToolboxRoot + `"; `
	if launch.Outputs {
		command += script.ShellPath + " " + scriptPath + " && exec " + ToolboxPath + " output push"
	} else {
		command += "exec " + script.ShellPath + " " + scriptPath
	}
	return []string{script.ShellPath, "-c", command}
}

// OutputDirectory returns the writable directory a container action writes
// its own outputs to. It is the path later actions read them from.
func OutputDirectory(action id.ActionName) (string, error) {
	if _, err := id.ParseActionName(string(action)); err != nil {
		return "", fault.Wrap(fault.InvalidInput, "output action name is invalid", err)
	}
	return OutputRoot + "/" + string(action), nil
}

// GitOrigin validates the workload Git URL of an installation and returns its
// origin. The URL is an absolute https URL without user information, query or
// fragment: the credential helper refuses every other scheme, and Git matches
// its configuration by origin. The URL is rendered into a shell word of that
// configuration, so quotes, backslashes, white space and control characters
// are refused.
func GitOrigin(gitURL string) (string, error) {
	rest, ok := strings.CutPrefix(gitURL, "https://")
	if !ok {
		return "", fault.New(fault.InvalidInput, "workload Git URL must be an absolute https URL")
	}
	for index := 0; index < len(gitURL); index++ {
		switch character := gitURL[index]; {
		case character <= 0x20 || character == 0x7f, character == '\'', character == '"', character == '\\':
			return "", fault.New(fault.InvalidInput, "workload Git URL contains a quote, a backslash, white space or a control character")
		case character == '@', character == '?', character == '#':
			return "", fault.New(fault.InvalidInput, "workload Git URL must not carry user information, a query or a fragment")
		}
	}
	host, _, _ := strings.Cut(rest, "/")
	if host == "" || strings.HasPrefix(host, ":") || strings.HasSuffix(host, ":") {
		return "", fault.New(fault.InvalidInput, "workload Git URL must name a host")
	}
	for index := 0; index < len(host); index++ {
		character := host[index]
		switch {
		case character >= 'a' && character <= 'z', character >= 'A' && character <= 'Z', character >= '0' && character <= '9':
		case character == '.', character == '-', character == ':', character == '[', character == ']':
		default:
			return "", fault.New(fault.InvalidInput, "workload Git URL host is invalid")
		}
	}
	return "https://" + host, nil
}

// GitEnvironment returns the environment entries, as NAME=value, that give a
// container action the installation's Git service: EnvGitURL and, with a
// mounted toolbox, Git configuration scoped to that origin alone. The
// credential helper is the toolbox and nothing else, restricted to the origin,
// so the execution credential is offered to no other host and kept by no
// other helper; caFile, when set, is the CA bundle
// Git trusts for that origin only, so every other host keeps the image's
// roots. An empty gitURL yields no entry.
func GitEnvironment(gitURL, caFile string, toolbox bool) ([]string, error) {
	if gitURL == "" {
		return nil, nil
	}
	origin, err := GitOrigin(gitURL)
	if err != nil {
		return nil, err
	}
	env := []string{EnvGitURL + "=" + strings.TrimRight(gitURL, "/")}
	if !toolbox {
		return env, nil
	}
	helper := "credential." + origin + ".helper"
	settings := [][2]string{
		// An empty helper resets the list for this origin: a helper the image
		// configures, a store or a keychain, is never asked for the execution
		// credential and never keeps it after a successful exchange.
		{helper, ""},
		// Git runs a helper that starts with an absolute path through the
		// shell, with the operation appended.
		{helper, ToolboxPath + " --trusted-origin '" + origin + "' git credential"},
	}
	if caFile != "" {
		settings = append(settings, [2]string{"http." + origin + ".sslCAInfo", caFile})
	}
	env = append(env, envGitConfigCount+"="+strconv.Itoa(len(settings)))
	for index, setting := range settings {
		position := strconv.Itoa(index)
		env = append(env,
			envGitConfigKeyPrefix+position+"="+setting[0],
			envGitConfigValuePrefix+position+"="+setting[1],
		)
	}
	return env, nil
}

// IsGitConfigEnv reports whether name is one of Git's environment
// configuration variables. An adapter that adds GitEnvironment drops a
// pipeline's own entries: a second GIT_CONFIG_COUNT would otherwise decide
// which pairs Git reads.
func IsGitConfigEnv(name string) bool {
	return name == envGitConfigCount ||
		strings.HasPrefix(name, envGitConfigKeyPrefix) ||
		strings.HasPrefix(name, envGitConfigValuePrefix)
}
