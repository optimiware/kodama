package main

import (
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// prSetChildSubreaper is PR_SET_CHILD_SUBREAPER (linux/prctl.h). With it set,
// every descendant that is orphaned (a `setsid` escapee, a background shell job,
// OpenCode's own private server after the CLI exits) is re-parented to the
// wrapper instead of PID 1, so the whole tree stays enumerable and killable.
const prSetChildSubreaper = 36

func becomeSubreaper() error {
	_, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, prSetChildSubreaper, 1, 0)
	if e != 0 {
		return e
	}
	return nil
}

// descendants lists every process whose ancestry reaches pid, found by
// scanning /proc. OpenCode starts its private server in its own session and
// shell tools start their own process groups, so a process-group signal does
// not reach them; ancestry does.
func descendants(root int) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	parent := map[int]int{}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		// pid (comm) state ppid ... ; comm may contain spaces and parentheses.
		s := string(b)
		i := strings.LastIndexByte(s, ')')
		if i < 0 {
			continue
		}
		f := strings.Fields(s[i+1:])
		if len(f) < 2 {
			continue
		}
		ppid, err := strconv.Atoi(f[1])
		if err != nil {
			continue
		}
		parent[pid] = ppid
	}
	var out []int
	for pid := range parent {
		for p, hops := parent[pid], 0; p > 1 && hops < 4096; p, hops = parent[p], hops+1 {
			if p == root {
				out = append(out, pid)
				break
			}
		}
	}
	return out
}

// killTree signals every descendant of the wrapper, repeating until none is
// left (a killed parent can leave children that are only then re-parented),
// escalating to SIGKILL after grace. It returns the number of processes it had
// to signal at least once and whether the tree is empty afterwards.
func killTree(root int, grace time.Duration, reapOrphans bool) (signalled int, empty bool) {
	seen := map[int]bool{}
	deadline := time.Now().Add(grace)
	hard := false
	for iter := 0; iter < 400; iter++ {
		ds := descendants(root)
		// Reap adopted, exited orphans (only once the harness itself was waited for,
		// so its exit status is never stolen).
		if reapOrphans {
			reap()
		}
		live := ds[:0]
		for _, pid := range ds {
			if !isZombie(pid) {
				live = append(live, pid)
			}
		}
		if len(live) == 0 {
			return len(seen), true
		}
		sig := syscall.SIGTERM
		if hard || time.Now().After(deadline) {
			sig, hard = syscall.SIGKILL, true
		}
		for _, pid := range live {
			seen[pid] = true
			_ = syscall.Kill(pid, sig)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return len(seen), len(descendants(root)) == 0
}

func isZombie(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	return i < 0 || len(s) < i+3 || s[i+2] == 'Z'
}

// reap collects any exited children the wrapper adopted as subreaper.
func reap() {
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
		if pid <= 0 || err != nil {
			return
		}
	}
}
