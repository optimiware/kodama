//go:build integration

package native_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// kodemaBinary is the real CLI built once for the native hook processes.
var kodemaBinary string

func TestMain(m *testing.M) {
	directory, err := os.MkdirTemp("", "kodema-native-bin-")
	if err != nil {
		os.Stderr.WriteString("create binary directory: " + err.Error() + "\n")
		os.Exit(1)
	}
	binary := filepath.Join(directory, "kodema")
	command := exec.Command("go", "build", "-o", binary, "./cmd/kodema")
	command.Dir = repositoryRoot()
	output, err := command.CombinedOutput()
	if err != nil {
		os.Stderr.WriteString("build kodema: " + err.Error() + "\n" + string(output))
		os.RemoveAll(directory)
		os.Exit(1)
	}
	kodemaBinary = binary
	code := m.Run()
	os.RemoveAll(directory)
	os.Exit(code)
}

func repositoryRoot() string {
	directory, err := os.Getwd()
	if err != nil {
		return "."
	}
	for {
		if _, err := os.Stat(filepath.Join(directory, "go.mod")); err == nil {
			return directory
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return "."
		}
		directory = parent
	}
}

func TestKodemaBinaryBuilt(t *testing.T) {
	info, err := os.Stat(kodemaBinary)
	if err != nil {
		t.Fatalf("kodema binary: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("kodema binary mode = %04o; want executable", info.Mode().Perm())
	}
}
