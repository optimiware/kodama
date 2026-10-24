// Package repochk holds the repository checks that used to be
// scripts/dev/check.py and scripts/dev/go_results.py: formatting, pinned
// configuration, documentation links and scoped Go results.
package repochk

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// skipDirs are never searched for sources.
var skipDirs = map[string]bool{
	"node_modules": true, ".git": true, ".work": true, ".state": true, ".harness": true, "__pycache__": true,
}

// SourceFiles walks directory for files with suffix, never descending into
// dependencies or local state.
func SourceFiles(directory, suffix string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if entry.IsDir() {
			if path != directory && skipDirs[entry.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(entry.Name(), suffix) {
			files = append(files, path)
		}
		return nil
	})
	return files, err
}

// Fmt checks gofmt over the named packages. With no packages it checks every
// development source tree.
func Fmt(root string, packages []string) error {
	files := []string{}
	if len(packages) == 0 {
		for _, base := range []string{"cmd", "internal", "dev", "contracts", "test"} {
			found, err := SourceFiles(filepath.Join(root, base), ".go")
			if err != nil {
				return err
			}
			files = append(files, found...)
		}
	}
	for _, pkg := range packages {
		recursive := strings.HasSuffix(pkg, "/...")
		clean := strings.TrimSuffix(pkg, "/...")
		directory := filepath.Join(root, clean)
		relative, err := filepath.Rel(root, directory)
		if err != nil || strings.HasPrefix(relative, "..") {
			return fmt.Errorf("package %q is outside the repository", pkg)
		}
		if _, err := os.Stat(directory); os.IsNotExist(err) {
			continue
		}
		if recursive {
			found, err := SourceFiles(directory, ".go")
			if err != nil {
				return err
			}
			files = append(files, found...)
			continue
		}
		entries, err := os.ReadDir(directory)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".go") {
				files = append(files, filepath.Join(directory, entry.Name()))
			}
		}
	}
	if len(files) == 0 {
		return nil
	}
	args := append([]string{"-l"}, files...)
	command := exec.Command("gofmt", args...)
	command.Dir = root
	output, err := command.Output()
	if err != nil {
		return fmt.Errorf("gofmt: %w", err)
	}
	if len(output) > 0 {
		return fmt.Errorf("Run make fmt:\n%s", output)
	}
	return nil
}
