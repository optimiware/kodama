package workspace_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/shell/workspace"
)

// TestEntrypointWithoutAToolboxIsThePlainScript pins that a runtime without a
// toolbox starts the Kernel's script exactly as before.
func TestEntrypointWithoutAToolboxIsThePlainScript(t *testing.T) {
	for _, launch := range []workspace.Launch{{}, {Outputs: true}} {
		got := workspace.Entrypoint("/kodema/script", launch)
		if want := []string{"/bin/sh", "/kodema/script"}; !slices.Equal(got, want) {
			t.Fatalf("Entrypoint(%+v) = %q; want %q", launch, got, want)
		}
	}
}

// TestGitEnvironment pins the Git configuration of a workload: scoped to the
// origin of the installation's Git service, with the toolbox as the only
// credential helper, restricted to that origin, and the CA bundle trusted for it alone.
func TestGitEnvironment(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		gitURL  string
		caFile  string
		toolbox bool
		want    []string
	}{
		{name: "no Git service"},
		{name: "a Git service, a toolbox and a CA bundle",
			gitURL: "https://host.containers.internal:8443/git/", caFile: "/kodema/ca.crt", toolbox: true,
			want: []string{
				"KODEMA_GIT_URL=https://host.containers.internal:8443/git",
				"GIT_CONFIG_COUNT=3",
				"GIT_CONFIG_KEY_0=credential.https://host.containers.internal:8443.helper",
				"GIT_CONFIG_VALUE_0=",
				"GIT_CONFIG_KEY_1=credential.https://host.containers.internal:8443.helper",
				"GIT_CONFIG_VALUE_1=/kodema/bin/kodema --trusted-origin 'https://host.containers.internal:8443' git credential",
				"GIT_CONFIG_KEY_2=http.https://host.containers.internal:8443.sslCAInfo",
				"GIT_CONFIG_VALUE_2=/kodema/ca.crt",
			}},
		{name: "a Git service and a toolbox, publicly trusted",
			gitURL: "https://git.example.com", toolbox: true,
			want: []string{
				"KODEMA_GIT_URL=https://git.example.com",
				"GIT_CONFIG_COUNT=2",
				"GIT_CONFIG_KEY_0=credential.https://git.example.com.helper",
				"GIT_CONFIG_VALUE_0=",
				"GIT_CONFIG_KEY_1=credential.https://git.example.com.helper",
				"GIT_CONFIG_VALUE_1=/kodema/bin/kodema --trusted-origin 'https://git.example.com' git credential",
			}},
		{name: "a Git service without a toolbox has no helper to name",
			gitURL: "https://git.example.com", caFile: "/kodema/ca.crt",
			want: []string{"KODEMA_GIT_URL=https://git.example.com"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := workspace.GitEnvironment(testCase.gitURL, testCase.caFile, testCase.toolbox)
			if err != nil {
				t.Fatalf("GitEnvironment: %v", err)
			}
			if !slices.Equal(got, testCase.want) {
				t.Fatalf("GitEnvironment =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(testCase.want, "\n"))
			}
		})
	}
}

// TestGitOriginRefusesWhatTheHelperWould pins the workload Git URL rules. The
// credential helper answers https origins only, and the URL is rendered into a
// shell word of Git's configuration.
func TestGitOriginRefusesWhatTheHelperWould(t *testing.T) {
	for _, gitURL := range []string{
		"http://git.example.com",
		"git.example.com/repo",
		"https://",
		"https://user@git.example.com",
		"https://git.example.com/?x=1",
		"https://git.example.com/#fragment",
		"https://git.example.com/it's",
		"https://git.example.com/a b",
		"ssh://git@git.example.com/repo",
	} {
		if _, err := workspace.GitOrigin(gitURL); err == nil {
			t.Errorf("GitOrigin(%q) was accepted", gitURL)
		} else if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
			t.Errorf("GitOrigin(%q) fault class = %v; want invalid_input", gitURL, class)
		}
		if _, err := workspace.GitEnvironment(gitURL, "", true); err == nil {
			t.Errorf("GitEnvironment(%q) was accepted", gitURL)
		}
	}
	if origin, err := workspace.GitOrigin("https://git.example.com:8443/git/"); err != nil || origin != "https://git.example.com:8443" {
		t.Fatalf("GitOrigin = %q, %v; want the origin without the path", origin, err)
	}
}

// TestIsGitConfigEnv pins the names an adapter drops from a pipeline's
// environment when it configures Git itself.
func TestIsGitConfigEnv(t *testing.T) {
	for name, want := range map[string]bool{
		"GIT_CONFIG_COUNT":   true,
		"GIT_CONFIG_KEY_0":   true,
		"GIT_CONFIG_VALUE_7": true,
		"GIT_CONFIG_GLOBAL":  false,
		"GIT_SSL_CAINFO":     false,
		"KODEMA_GIT_URL":     false,
	} {
		if got := workspace.IsGitConfigEnv(name); got != want {
			t.Errorf("IsGitConfigEnv(%q) = %t; want %t", name, got, want)
		}
	}
}

// TestOutputDirectory pins that an action writes its outputs where later
// actions read them.
func TestOutputDirectory(t *testing.T) {
	directory, err := workspace.OutputDirectory("build")
	if err != nil || directory != "/kodema/outputs/build" {
		t.Fatalf("OutputDirectory = %q, %v", directory, err)
	}
	read, err := workspace.OutputPath("build", "candidate")
	if err != nil || read != directory+"/candidate" {
		t.Fatalf("OutputPath = %q, %v; want a file of %q", read, err, directory)
	}
	if _, err := workspace.OutputDirectory("../escape"); err == nil {
		t.Fatal("an invalid action name was accepted")
	}
}
