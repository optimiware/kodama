package app

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func spoolConfig(root string) CoreConfig {
	c := validConfig()
	c.Git.ScratchRoot = root
	return c
}

func Test_core_spool_directories(t *testing.T) {
	t.Run("fresh scratch root gets both private directories", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "fresh", "scratch")
		if err := prepareSpools(spoolConfig(root)); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"artifact-spool", "log-spool"} {
			info, err := os.Stat(filepath.Join(root, name))
			if err != nil || !info.IsDir() {
				t.Fatalf("%s was not created: %v", name, err)
			}
			if runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
				t.Fatalf("%s mode = %o, want 0700", name, info.Mode().Perm())
			}
		}
	})

	t.Run("repeated startup keeps the directories and their content", func(t *testing.T) {
		root := t.TempDir()
		cfg := spoolConfig(root)
		if err := prepareSpools(cfg); err != nil {
			t.Fatal(err)
		}
		keep := filepath.Join(artifactSpoolDir(cfg), "kodama-artifact-leftover")
		if err := os.WriteFile(keep, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 3; i++ {
			if err := prepareSpools(cfg); err != nil {
				t.Fatalf("restart %d: %v", i, err)
			}
		}
		if _, err := os.Stat(keep); err != nil {
			t.Fatalf("an existing spool file must survive a restart: %v", err)
		}
		entries, _ := os.ReadDir(logSpoolDir(cfg))
		if len(entries) != 0 {
			t.Fatalf("the writability probe leaked %d entries", len(entries))
		}
	})

	t.Run("a widened mode is narrowed again", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("POSIX modes")
		}
		root := t.TempDir()
		cfg := spoolConfig(root)
		if err := os.MkdirAll(logSpoolDir(cfg), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(logSpoolDir(cfg), 0o777); err != nil {
			t.Fatal(err)
		}
		if err := prepareSpools(cfg); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(logSpoolDir(cfg))
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("mode = %v (%v), want 0700", info.Mode().Perm(), err)
		}
	})

	t.Run("unusable paths are reported", func(t *testing.T) {
		cases := map[string]func(cfg CoreConfig) error{
			"a file where the directory belongs": func(cfg CoreConfig) error {
				return os.WriteFile(artifactSpoolDir(cfg), []byte("x"), 0o600)
			},
			"a symbolic link in place of the directory": func(cfg CoreConfig) error {
				return os.Symlink(t.TempDir(), logSpoolDir(cfg))
			},
			"a scratch root that is a file": func(cfg CoreConfig) error {
				if err := os.RemoveAll(cfg.Git.ScratchRoot); err != nil {
					return err
				}
				return os.WriteFile(cfg.Git.ScratchRoot, []byte("x"), 0o600)
			},
		}
		for name, setup := range cases {
			t.Run(name, func(t *testing.T) {
				cfg := spoolConfig(filepath.Join(t.TempDir(), "scratch"))
				if err := os.MkdirAll(cfg.Git.ScratchRoot, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := setup(cfg); err != nil {
					t.Fatal(err)
				}
				err := prepareSpools(cfg)
				if err == nil {
					t.Fatal("an unusable spool path must be reported")
				}
				if !strings.Contains(err.Error(), "spool directory") || strings.Contains(err.Error(), cfg.Git.ScratchRoot) {
					t.Fatalf("error %q must name the spool and not the host path", err)
				}
			})
		}
	})

	t.Run("an unwritable directory is reported", func(t *testing.T) {
		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("permission bits do not bind this user")
		}
		cfg := spoolConfig(t.TempDir())
		if err := os.Mkdir(artifactSpoolDir(cfg), 0o500); err != nil {
			t.Fatal(err)
		}
		if err := prepareSpools(cfg); err == nil || !strings.Contains(err.Error(), "not writable") {
			t.Fatalf("err = %v, want not writable", err)
		}
	})

	t.Run("NewCore refuses before opening any connection", func(t *testing.T) {
		cfg := spoolConfig(filepath.Join(t.TempDir(), "scratch"))
		if err := os.MkdirAll(cfg.Git.ScratchRoot, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(logSpoolDir(cfg), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		// The database and broker of validConfig are unreachable names: a
		// connection attempt would fail with a different error.
		_, err := NewCore(context.Background(), cfg, nil)
		if err == nil || !strings.Contains(err.Error(), "spool directory") {
			t.Fatalf("err = %v, want a spool directory refusal", err)
		}
	})
}
