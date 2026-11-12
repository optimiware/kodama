package native

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func fakeBinary(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kodama")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("WriteFile binary: %v", err)
	}
	return path
}

func TestInstallHooksWritesOnlyBatchPhases(t *testing.T) {
	repository := t.TempDir()
	binary := fakeBinary(t)
	report, err := InstallHooks(HooksConfig{RepositoryPath: repository, BinaryPath: binary})
	if err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}
	if len(report.Installed) != 2 || report.Installed[0] != "pre-receive" || report.Installed[1] != "post-receive" {
		t.Fatalf("installed = %v; want pre-receive and post-receive", report.Installed)
	}
	hooksDirectory := filepath.Join(repository, "hooks")
	for _, phase := range []string{"pre-receive", "post-receive"} {
		path := filepath.Join(hooksDirectory, phase)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("Stat %s: %v", phase, err)
		}
		if info.Mode().Perm() != 0o755 {
			t.Fatalf("%s mode = %04o; want 0755", phase, info.Mode().Perm())
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile %s: %v", phase, err)
		}
		content := string(data)
		if !strings.Contains(content, managedMarker) {
			t.Fatalf("%s lacks the managed marker: %q", phase, content)
		}
		if !strings.Contains(content, "exec '"+binary+"' git hook "+phase) {
			t.Fatalf("%s does not exec the fixed binary and phase: %q", phase, content)
		}
	}
	if _, err := os.Stat(filepath.Join(hooksDirectory, "update")); !os.IsNotExist(err) {
		t.Fatal("InstallHooks installed the unsafe single-ref update hook")
	}

	report, err = InstallHooks(HooksConfig{RepositoryPath: repository, BinaryPath: binary})
	if err != nil {
		t.Fatalf("second InstallHooks: %v", err)
	}
	if len(report.Unchanged) != 2 || len(report.Installed) != 0 || len(report.Updated) != 0 {
		t.Fatalf("second report = %+v; want both unchanged", report)
	}
}

func TestInstallHooksRefusesForeignHook(t *testing.T) {
	repository := t.TempDir()
	hooksDirectory := filepath.Join(repository, "hooks")
	if err := os.MkdirAll(hooksDirectory, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	foreign := filepath.Join(hooksDirectory, "pre-receive")
	if err := os.WriteFile(foreign, []byte("#!/bin/sh\necho foreign\n"), 0o755); err != nil {
		t.Fatalf("WriteFile foreign: %v", err)
	}
	_, err := InstallHooks(HooksConfig{RepositoryPath: repository, BinaryPath: fakeBinary(t)})
	if !errors.Is(err, ErrForeignHook) {
		t.Fatalf("error = %v; want ErrForeignHook", err)
	}
	data, err := os.ReadFile(foreign)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "#!/bin/sh\necho foreign\n" {
		t.Fatalf("foreign hook was overwritten: %q", data)
	}
}

// TestInstallHooksRefusesSymlinkedHooksDirectory pins that hook installation
// never follows a `hooks` symlink: writing through it would place the managed
// hooks outside the repository, where the adopt path cannot police them and a
// later normalize pass would only reject the tree after the write. Nothing in
// the link target may be created or chmodded.
func TestInstallHooksRefusesSymlinkedHooksDirectory(t *testing.T) {
	repository := t.TempDir()
	outside := t.TempDir()
	sentinel := filepath.Join(outside, "sentinel")
	if err := os.WriteFile(sentinel, []byte("outside repository\n"), 0o640); err != nil {
		t.Fatalf("WriteFile sentinel: %v", err)
	}
	if err := os.Chmod(outside, 0o705); err != nil {
		t.Fatalf("Chmod outside directory: %v", err)
	}
	beforeSentinel, err := os.Stat(sentinel)
	if err != nil {
		t.Fatalf("Stat sentinel: %v", err)
	}
	beforeOutside, err := os.Stat(outside)
	if err != nil {
		t.Fatalf("Stat outside directory: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(repository, "hooks")); err != nil {
		t.Fatalf("Symlink hooks: %v", err)
	}

	if _, err := InstallHooks(HooksConfig{RepositoryPath: repository, BinaryPath: fakeBinary(t)}); !errors.Is(err, ErrHookSymlink) {
		t.Fatalf("error = %v; want ErrHookSymlink", err)
	}
	for _, phase := range []string{"pre-receive", "post-receive", "update"} {
		if _, statErr := os.Lstat(filepath.Join(outside, phase)); !os.IsNotExist(statErr) {
			t.Fatalf("symlink target contains %q after the refused install: %v", phase, statErr)
		}
	}
	data, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatalf("ReadFile sentinel: %v", err)
	}
	if string(data) != "outside repository\n" {
		t.Fatalf("sentinel content = %q; want it untouched", data)
	}
	afterSentinel, err := os.Stat(sentinel)
	if err != nil {
		t.Fatalf("Stat sentinel after install: %v", err)
	}
	if afterSentinel.Mode() != beforeSentinel.Mode() {
		t.Fatalf("sentinel mode = %v; want %v", afterSentinel.Mode(), beforeSentinel.Mode())
	}
	afterOutside, err := os.Stat(outside)
	if err != nil {
		t.Fatalf("Stat outside directory after install: %v", err)
	}
	if afterOutside.Mode() != beforeOutside.Mode() {
		t.Fatalf("outside directory mode = %v; want %v", afterOutside.Mode(), beforeOutside.Mode())
	}
	linkInfo, err := os.Lstat(filepath.Join(repository, "hooks"))
	if err != nil || linkInfo.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("repository hooks path = %v,%v; want the refused symlink left in place", linkInfo, err)
	}
}

// TestInstallHooksRefusesSymlinkedHookFile pins the per-file half of the rule:
// a managed hook name that is a symlink to a file outside the repository is
// refused before it is read through or replaced.
func TestInstallHooksRefusesSymlinkedHookFile(t *testing.T) {
	repository := t.TempDir()
	hooksDirectory := filepath.Join(repository, "hooks")
	if err := os.MkdirAll(hooksDirectory, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	outsideDirectory := t.TempDir()
	outside := filepath.Join(outsideDirectory, "hook")
	content := "#!/bin/sh\n" + managedMarker + "\nexec something-else\n"
	if err := os.WriteFile(outside, []byte(content), 0o750); err != nil {
		t.Fatalf("WriteFile outside hook: %v", err)
	}
	before, err := os.Stat(outside)
	if err != nil {
		t.Fatalf("Stat outside hook: %v", err)
	}
	hookPath := filepath.Join(hooksDirectory, "pre-receive")
	if err := os.Symlink(outside, hookPath); err != nil {
		t.Fatalf("Symlink hook: %v", err)
	}

	if _, err := InstallHooks(HooksConfig{RepositoryPath: repository, BinaryPath: fakeBinary(t)}); !errors.Is(err, ErrHookSymlink) {
		t.Fatalf("error = %v; want ErrHookSymlink", err)
	}
	data, err := os.ReadFile(outside)
	if err != nil {
		t.Fatalf("ReadFile outside hook: %v", err)
	}
	if string(data) != content {
		t.Fatalf("outside hook content = %q; want it untouched", data)
	}
	after, err := os.Stat(outside)
	if err != nil {
		t.Fatalf("Stat outside hook after install: %v", err)
	}
	if after.Mode() != before.Mode() {
		t.Fatalf("outside hook mode = %v; want %v", after.Mode(), before.Mode())
	}
	linkInfo, err := os.Lstat(hookPath)
	if err != nil || linkInfo.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("hook path = %v,%v; want the refused symlink left in place", linkInfo, err)
	}
	if _, statErr := os.Lstat(filepath.Join(hooksDirectory, "post-receive")); !os.IsNotExist(statErr) {
		t.Fatalf("post-receive exists after the refused install: %v", statErr)
	}
}

func TestInitBareRepositorySetsTransportConfigOnly(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is unavailable")
	}
	path := filepath.Join(t.TempDir(), "shop", "web")
	if err := InitBareRepository(context.Background(), BareRepositoryConfig{Path: path}); err != nil {
		t.Fatalf("InitBareRepository: %v", err)
	}
	for _, expected := range [][2]string{
		{"receive.advertiseAtomic", "true"},
		{"http.receivepack", "true"},
	} {
		output, err := exec.Command("git", "-C", path, "config", "--get", expected[0]).Output()
		if err != nil {
			t.Fatalf("git config %s: %v", expected[0], err)
		}
		if strings.TrimSpace(string(output)) != expected[1] {
			t.Fatalf("%s = %q; want %q", expected[0], output, expected[1])
		}
	}
	for _, forbidden := range []string{"receive.denyDeletes", "receive.denyNonFastForwards"} {
		if err := exec.Command("git", "-C", path, "config", "--get", forbidden).Run(); err == nil {
			t.Fatalf("%s was set; ref policy belongs to Kodama rules", forbidden)
		}
	}
	head, err := exec.Command("git", "-C", path, "symbolic-ref", "HEAD").Output()
	if err != nil {
		t.Fatalf("git symbolic-ref HEAD: %v", err)
	}
	if strings.TrimSpace(string(head)) != "refs/heads/main" {
		t.Fatalf("bare HEAD = %q; want refs/heads/main", head)
	}
}
