package assembly_test

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func TestGitPreflightFailsWhenGitOrBackendIsMissing(t *testing.T) {
	ok := gitTools{
		lookPath: func(string) (string, error) { return "/usr/bin/git", nil },
		execPath: func(string) (string, error) { return "/exec", nil },
		stat:     func(string) (os.FileInfo, error) { return nil, nil },
	}
	if err := ok.preflight(); err != nil {
		t.Fatalf("complete toolset: %v", err)
	}
	missing := errors.New("missing")
	for name, tc := range map[string]struct {
		tools gitTools
		want  string
	}{
		"no git": {gitTools{
			lookPath: func(string) (string, error) { return "", missing }, execPath: ok.execPath, stat: ok.stat,
		}, "git is not installed"},
		"no exec path": {gitTools{
			lookPath: ok.lookPath, execPath: func(string) (string, error) { return "", missing }, stat: ok.stat,
		}, "--exec-path"},
		"no backend": {gitTools{
			lookPath: ok.lookPath, execPath: ok.execPath, stat: func(string) (os.FileInfo, error) { return nil, missing },
		}, "git-http-backend is not available at /exec/git-http-backend"},
	} {
		err := tc.tools.preflight()
		if err == nil || !strings.Contains(err.Error(), tc.want) || !errors.Is(err, missing) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := hostGitTools.preflight(); err != nil {
		t.Errorf("this host cannot run the assembly acceptance: %v", err)
	}
}
