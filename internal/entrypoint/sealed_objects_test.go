package entrypoint

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Review findings 1 and 2 on the sealed workspace Git: the object store of a
// shared scratch is data written by earlier steps, so its content must be shown
// to match its names, and it must be nothing but plain files in plain directories
// (no alternates, no symlinks) before Git is pointed at it. Real Git throughout.

func loosePath(dir, oid string) string {
	return filepath.Join(dir, ".git", "objects", oid[:2], oid[2:])
}

// replaceLoose stores the bytes of loose object from under the name of loose
// object to, as a step with write access to the volume can.
func replaceLoose(t *testing.T, dir, to, from string) {
	t.Helper()
	data, err := os.ReadFile(loosePath(dir, from))
	if err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(loosePath(dir, to), 0o644)
	if err := os.WriteFile(loosePath(dir, to), data, 0o444); err != nil {
		t.Fatal(err)
	}
}

func Test_unit_sealed_git_verifies_object_hashes(t *testing.T) {
	requireGit(t)
	t.Run("a tree replaced under its own name is refused although HEAD is unchanged", func(t *testing.T) {
		su := newShared(t, beganThenRan, sourceBacked("read"))
		dir := su.cfg.WorkspaceDir
		ran := su.ranFile()
		su.v2.ctx.Command.Args = []string{"touch " + ran}
		head := tgit(t, dir, "rev-parse", "HEAD")
		origTree := tgit(t, dir, "rev-parse", "HEAD^{tree}")
		// The step edits a tracked file, stores the tree that records the edit and
		// puts that tree's bytes under the original tree's name.
		writeFile(t, filepath.Join(dir, "main.go"), "package evil\n")
		tgit(t, dir, "add", "-A")
		forged := tgit(t, dir, "write-tree")
		tgit(t, dir, "reset", "-q") // the index is not evidence either
		replaceLoose(t, dir, origTree, forged)
		if got := tgit(t, dir, "rev-parse", "HEAD"); got != head {
			t.Fatal("HEAD moved")
		}
		// The forged tree describes exactly the edited worktree, so a status that
		// trusted it would say "clean".
		sg, err := newSealedGit(context.Background(), su.build().git, dir, su.cfg.ScratchDir, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer sg.Close()
		if _, err := sg.state(context.Background()); !errors.Is(err, errUnsafeWorkspace) {
			t.Fatalf("state over a forged tree: %v", err)
		}
		// Through the step: refused before begin and before the command.
		su.wantExit(su.run(), ExitOK)
		wantReason(t, su.lastResult(), ReasonWorkspaceUnverified)
		if su.v2.count("begin") != 0 {
			t.Fatalf("begin was called over a forged object store: %v", su.v2.calls)
		}
		if _, err := os.Stat(ran); err == nil {
			t.Fatal("the command ran")
		}
	})

	t.Run("a forged commit, subtree and blob are refused too", func(t *testing.T) {
		for name, pick := range map[string]func(dir string) (victim, donor string){
			"commit": func(dir string) (string, string) {
				head := tgit(t, dir, "rev-parse", "HEAD")
				writeFile(t, filepath.Join(dir, "x.txt"), "x")
				tgit(t, dir, "add", "-A")
				tgit(t, dir, "commit", "-q", "--no-gpg-sign", "-m", "other")
				donor := tgit(t, dir, "rev-parse", "HEAD")
				tgit(t, dir, "reset", "-q", "--hard", head)
				return head, donor
			},
			"subtree": func(dir string) (string, string) {
				writeFile(t, filepath.Join(dir, "pkg", "a.go"), "package a\n")
				tgit(t, dir, "add", "-A")
				tgit(t, dir, "commit", "-q", "--no-gpg-sign", "-m", "pkg")
				sub := tgit(t, dir, "rev-parse", "HEAD:pkg")
				donor := tgit(t, dir, "rev-parse", "HEAD^{tree}") // any other tree
				return sub, donor
			},
			"blob": func(dir string) (string, string) {
				return tgit(t, dir, "rev-parse", "HEAD:main.go"), tgit(t, dir, "rev-parse", "HEAD:README.md")
			},
		} {
			t.Run(name, func(t *testing.T) {
				u := newUnit(t, "exit 0")
				dir := u.cfg.WorkspaceDir
				preparedWorkspace(t, dir)
				victim, donor := pick(dir)
				replaceLoose(t, dir, victim, donor)
				_, err := newSealedGit(context.Background(), u.build().git, dir, u.cfg.ScratchDir, nil)
				if err == nil {
					sg, _ := newSealedGit(context.Background(), u.build().git, dir, u.cfg.ScratchDir, nil)
					defer sg.Close()
					_, err = sg.sync(context.Background())
				}
				if !errors.Is(err, errUnsafeWorkspace) {
					t.Fatalf("a forged %s was trusted: %v", name, err)
				}
			})
		}
	})

	t.Run("an intact repository is verified, including packed objects", func(t *testing.T) {
		u := newUnit(t, "exit 0")
		dir := u.cfg.WorkspaceDir
		preparedWorkspace(t, dir)
		tgit(t, dir, "gc", "-q", "--aggressive")
		sg, err := newSealedGit(context.Background(), u.build().git, dir, u.cfg.ScratchDir, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer sg.Close()
		if st, err := sg.state(context.Background()); err != nil || !st.TrackedClean {
			t.Fatalf("%+v %v", st, err)
		}
	})
}

func Test_unit_sealed_git_isolates_the_object_store(t *testing.T) {
	requireGit(t)
	open := func(t *testing.T, mutate func(t *testing.T, u *unit, dir string)) error {
		t.Helper()
		u := newUnit(t, "exit 0")
		dir := u.cfg.WorkspaceDir
		preparedWorkspace(t, dir)
		mutate(t, u, dir)
		sg, err := newSealedGit(context.Background(), u.build().git, dir, u.cfg.ScratchDir, nil)
		if err == nil {
			defer sg.Close()
			_, err = sg.sync(context.Background())
		}
		return err
	}
	outside := func(t *testing.T) string {
		d := t.TempDir()
		writeFile(t, filepath.Join(d, "marker"), "x")
		return d
	}
	for name, mutate := range map[string]func(t *testing.T, u *unit, dir string){
		"alternates": func(t *testing.T, u *unit, dir string) {
			writeFile(t, filepath.Join(dir, ".git", "objects", "info", "alternates"), outside(t)+"\n")
		},
		"http-alternates": func(t *testing.T, u *unit, dir string) {
			writeFile(t, filepath.Join(dir, ".git", "objects", "info", "http-alternates"), "https://elsewhere.invalid/objects\n")
		},
		"a symlinked fan-out directory": func(t *testing.T, u *unit, dir string) {
			fan := filepath.Dir(loosePath(dir, tgit(t, dir, "rev-parse", "HEAD")))
			real := filepath.Join(outside(t), "fan")
			if err := os.Rename(fan, real); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, fan); err != nil {
				t.Fatal(err)
			}
		},
		"a symlinked loose object": func(t *testing.T, u *unit, dir string) {
			p := loosePath(dir, tgit(t, dir, "rev-parse", "HEAD"))
			real := filepath.Join(outside(t), "obj")
			data, _ := os.ReadFile(p)
			writeFile(t, real, string(data))
			_ = os.Remove(p)
			if err := os.Symlink(real, p); err != nil {
				t.Fatal(err)
			}
		},
		"a symlinked pack file": func(t *testing.T, u *unit, dir string) {
			tgit(t, dir, "gc", "-q")
			packs, _ := filepath.Glob(filepath.Join(dir, ".git", "objects", "pack", "*.pack"))
			if len(packs) == 0 {
				t.Fatal("no pack")
			}
			real := filepath.Join(outside(t), "p.pack")
			data, _ := os.ReadFile(packs[0])
			writeFile(t, real, string(data))
			_ = os.Remove(packs[0])
			if err := os.Symlink(real, packs[0]); err != nil {
				t.Fatal(err)
			}
		},
		"a symlinked objects directory": func(t *testing.T, u *unit, dir string) {
			real := filepath.Join(outside(t), "objects")
			if err := os.Rename(filepath.Join(dir, ".git", "objects"), real); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, filepath.Join(dir, ".git", "objects")); err != nil {
				t.Fatal(err)
			}
		},
		"a named pipe in the store": func(t *testing.T, u *unit, dir string) {
			if err := syscallMkfifo(filepath.Join(dir, ".git", "objects", "info", "pipe")); err != nil {
				t.Skip(err)
			}
		},
		"a nested directory Git never writes": func(t *testing.T, u *unit, dir string) {
			writeFile(t, filepath.Join(dir, ".git", "objects", "ab", "cd", "ef", "gh"), "x")
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := open(t, mutate); err == nil {
				t.Fatalf("%s was accepted", name)
			}
		})
	}

	t.Run("publication cannot write through a symlinked child", func(t *testing.T) {
		p := newSealedPub(t, nil)
		out := outside(t)
		// Every fan-out directory a new object could land in is a link out of the volume.
		fan := filepath.Dir(loosePath(p.dir, p.c2))
		real := filepath.Join(out, "fan")
		if err := os.Rename(fan, real); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, fan); err != nil {
			t.Fatal(err)
		}
		before, _ := os.ReadDir(real)
		writeFile(t, filepath.Join(p.dir, "new.txt"), "work\n")
		_, v, ab := p.a.publish()
		p.failed(v, ab, ReasonSourceFailed)
		if after, _ := os.ReadDir(real); len(after) != len(before) {
			t.Fatalf("objects were written through the link: %d -> %d", len(before), len(after))
		}
		if len(p.rt.proposals) != 0 {
			t.Fatal("something was proposed")
		}
	})

	t.Run("an intact store with packs and an info directory is accepted", func(t *testing.T) {
		if err := open(t, func(t *testing.T, u *unit, dir string) {
			tgit(t, dir, "gc", "-q")
			writeFile(t, filepath.Join(dir, ".git", "objects", "info", "packs"), "P pack-x.pack\n")
		}); err != nil {
			t.Fatal(err)
		}
	})
}
