package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Spool directories under the scratch root. The artifact service verifies an
// upload from a private temporary copy, and the log writer and controller
// stage retained logs the same way; both fail every request with 503 when
// their directory is missing, so the assembly creates them before use.
const (
	artifactSpoolName = "artifact-spool"
	logSpoolName      = "log-spool"
	// spoolMode is private to the server's user: spooled bytes are tenant
	// artifact and log content before redaction and verification.
	spoolMode os.FileMode = 0o700
)

func artifactSpoolDir(cfg CoreConfig) string {
	return filepath.Join(cfg.Git.ScratchRoot, artifactSpoolName)
}

func logSpoolDir(cfg CoreConfig) string {
	return filepath.Join(cfg.Git.ScratchRoot, logSpoolName)
}

// prepareSpools creates (or adopts, on a restart) both spool directories and
// proves each is usable. A path that is a file, a symbolic link, unreadable or
// not writable fails construction, so no service starts that would answer every
// artifact completion with an error.
func prepareSpools(cfg CoreConfig) error {
	for _, dir := range []string{artifactSpoolDir(cfg), logSpoolDir(cfg)} {
		if err := prepareSpool(dir); err != nil {
			return fmt.Errorf("spool directory: %w", err)
		}
	}
	return nil
}

// prepareSpool is idempotent: an existing private directory is reused as it is,
// and one whose mode a previous run or an operator widened is narrowed again.
func prepareSpool(dir string) error {
	if err := os.MkdirAll(filepath.Dir(dir), spoolMode); err != nil {
		return fmt.Errorf("cannot create the parent of %s: %w", filepath.Base(dir), pathFree(err))
	}
	if err := os.Mkdir(dir, spoolMode); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("cannot create %s: %w", filepath.Base(dir), pathFree(err))
	}
	// Lstat, not Stat: a symbolic link planted at the spool name could redirect
	// spooled content elsewhere.
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("cannot inspect %s: %w", filepath.Base(dir), pathFree(err))
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", filepath.Base(dir))
	}
	if info.Mode().Perm()&^spoolMode != 0 {
		if err := os.Chmod(dir, spoolMode); err != nil {
			return fmt.Errorf("cannot restrict %s: %w", filepath.Base(dir), pathFree(err))
		}
	}
	probe, err := os.CreateTemp(dir, ".probe-*")
	if err != nil {
		return fmt.Errorf("%s is not writable: %w", filepath.Base(dir), pathFree(err))
	}
	name := probe.Name()
	closeErr := probe.Close()
	if err := os.Remove(name); err != nil {
		return fmt.Errorf("%s is not usable: %w", filepath.Base(dir), pathFree(err))
	}
	if closeErr != nil {
		return fmt.Errorf("%s is not usable: %w", filepath.Base(dir), pathFree(closeErr))
	}
	return nil
}

// pathFree returns the operating-system cause without the path, which is
// already named by its base in the message.
func pathFree(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}
