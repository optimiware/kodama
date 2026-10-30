package gitservice

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Runtime fence. A Git process group the worker started (a receive's
// http-backend, an update-ref, a maintenance) is killed with its whole group at a
// deadline or cancellation. Its ref locks may stay behind, so the repository is
// repaired only after the group is verified gone: the leader was reaped by
// cmd.Wait and kill(-pgid, 0) reports no member left. While a group is not
// verified gone the repository is fenced (see gate.fence): reads report it not
// quiescent, a killed receive still reports running, and receives, server ref
// operations, maintenance and the repair itself are refused with unavailable.
// Locks of a repository with a live group are never deleted.
//
// Across a worker restart the generation lock (lockDataRoot) provides the same
// guarantee: a new worker cannot start until every process of the previous one
// has exited.

// groupSettle is how long a finished group may take to disappear before the
// repository is fenced for it; fenceCheck is how often a fenced group is
// re-checked (and re-killed).
const (
	groupSettle = time.Second
	fenceCheck  = 50 * time.Millisecond
)

// groupGone reports whether no process of the group exists.
func (s *Service) groupGone(pgid int) bool {
	if s.groupGoneHook != nil {
		return s.groupGoneHook(pgid)
	}
	return !processGroupAlive(pgid)
}

func repositoryOf(s *Service, dir string) (string, bool) {
	id, ok := strings.CutSuffix(filepath.Base(dir), ".git")
	if !ok || filepath.Dir(dir) != s.layout.repositories || !repositoryIDPattern.MatchString(id) {
		return "", false
	}
	return id, true
}

// groupExited is called after the leader of a Git process group was reaped. It
// kills any member still in the group and verifies the group gone; if it is not
// gone within groupSettle the repository is fenced and a background check keeps
// killing and checking until it is. A killed group (or one that outlived its
// leader) schedules the repair of its repository's locks once it is gone.
func (s *Service) groupExited(repositoryID, dir string, cmd *exec.Cmd, receiveID string, killed bool) {
	if cmd.Process == nil {
		return
	}
	pgid := cmd.Process.Pid
	_ = killProcessGroup(cmd)
	deadline := time.Now().Add(groupSettle)
	for !s.groupGone(pgid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	g := s.gates.of(repositoryID)
	if s.groupGone(pgid) {
		if killed {
			g.markDirty()
			s.scheduleRepair(repositoryID, dir)
		}
		return
	}
	g.fence(pgid, receiveID)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		t := time.NewTicker(fenceCheck)
		defer t.Stop()
		for {
			_ = killProcessGroup(cmd)
			if s.groupGone(pgid) {
				break
			}
			select {
			case <-s.ctx.Done():
				return
			case <-t.C:
			}
		}
		if g.unfence(pgid) {
			s.scheduleRepair(repositoryID, dir)
		}
	}()
}

// scheduleRepair repairs the repository once it is idle and not fenced.
func (s *Service) scheduleRepair(repositoryID, dir string) {
	s.gates.of(repositoryID).markDirty()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if err := s.repairRepository(repositoryID, dir); err != nil && err != errFenced {
			s.cfg.Logger.Warn("repair after a terminated Git process did not run", "repositoryId", repositoryID, "error", err)
		}
	}()
}

// repairRepository removes the ref locks, the packed-refs lock and the incoming
// quarantines a killed Git left. It takes the gate exclusively, so no receive or
// operation is alive to own such a file, and refuses (errFenced) while any
// terminated group of the repository is not verified gone. The journal and the
// refs are untouched: prepared entries of the killed transactions stay for
// settlement.
func (s *Service) repairRepository(repositoryID, dir string) error {
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Minute)
	defer cancel()
	g := s.gates.of(repositoryID)
	release, err := g.acquireExclusiveMode(ctx, false)
	if err != nil {
		return err
	}
	defer release()
	if g.isFenced() {
		return errFenced
	}
	if !g.takeDirty() {
		return nil
	}
	if removed := removeStaleLocks(dir); len(removed) > 0 {
		s.cfg.Logger.Warn("removed what a terminated Git process left", "repositoryId", repositoryID, "paths", removed)
	}
	return nil
}
