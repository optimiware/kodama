package podman

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/optimiware/kodama/internal/domain"
	kr "github.com/optimiware/kodama/internal/kernel/runtime"
)

// This file holds what the kernel adapters track for engine objects that are not
// containers: a workspace's named volume, a run's internal network, and the
// durable holder intents that keep a workspace from being released or removed
// while a workload that mounts it may still exist.

// namedObject is a volume or network the adapter creates by name. Its creation
// is announced durably, an unseen outcome is reconciled by name and UID and never
// replayed, and the engine identity (volume creation time, network ID) is bound
// in the record once the object is observed.
type namedObject struct {
	kind    string
	inspect func(ctx context.Context, name string) (labels map[string]string, id string, found bool, err error)
	// create returns the full create command for the record.
	create func(rec krec) []string
}

func (c *kernelCore) volumeObject() namedObject {
	return namedObject{
		kind: "volume",
		inspect: func(ctx context.Context, name string) (map[string]string, string, bool, error) {
			v, found, err := c.pm.inspectVolume(ctx, name)
			return v.Labels, v.CreatedAt, found, err
		},
		create: func(rec krec) []string {
			return append(append([]string{"volume", "create"}, labelArgs(labelsOf(rec))...), rec.Name)
		},
	}
}

func (c *kernelCore) networkObject() namedObject {
	return namedObject{
		kind: "network",
		inspect: func(ctx context.Context, name string) (map[string]string, string, bool, error) {
			n, found, err := c.inspectNetwork(ctx, name)
			return n.Labels, n.ID, found, err
		},
		create: func(rec krec) []string {
			return append(append([]string{"network", "create", "--internal"}, labelArgs(labelsOf(rec))...), rec.Name)
		},
	}
}

// bindObject adopts the record's instance when it is observed under the name:
// it binds the engine identity and resolves an outstanding create. found reports
// that an object holds the name; ours reports that it is the record's instance
// (labels, UID and, once bound, engine identity). An object that is not ours
// settles nothing: the unresolved evidence stays.
func (c *kernelCore) bindObject(ctx context.Context, o namedObject, rec *krec) (found, ours bool, err error) {
	labels, id, found, err := o.inspect(ctx, rec.Name)
	if err != nil || !found {
		return false, false, err
	}
	if !owned(labels, *rec) || (rec.ObjectID != "" && id != rec.ObjectID) {
		return true, false, nil
	}
	if rec.ObjectID == "" || rec.Unanswered {
		rec.ObjectID, rec.Unanswered = id, false
		if err := c.save(*rec); err != nil {
			return true, true, err
		}
	}
	return true, true, nil
}

// ensureObject finds or creates the record's volume or network. The caller holds
// the record's lock and has admitted the record.
func (c *kernelCore) ensureObject(ctx context.Context, o namedObject, rec *krec) error {
	found, ours, err := c.bindObject(ctx, o, rec)
	switch {
	case err != nil:
		return err
	case found && !ours:
		return fmt.Errorf("%w: a foreign %s holds the name %s", kr.ErrSpecMismatch, o.kind, rec.Name)
	case found:
		return nil
	case rec.ObjectID != "":
		return fmt.Errorf("%w: the %s of %s", errLost, o.kind, rec.Identity)
	case rec.Unanswered:
		return uncertain("an earlier create of %s is unresolved; none is replayed", rec.Identity)
	}
	rec.Announced++
	rec.Unanswered = true
	if err := c.save(*rec); err != nil {
		rec.Announced--
		rec.Unanswered = false
		return refuse("announcing the create of %s: %v", rec.Identity, err)
	}
	res, err := c.pm.run(ctx, o.create(*rec)...)
	switch {
	case err != nil && errors.Is(err, kr.ErrNotReached):
		rec.Unanswered = false
		_ = c.save(*rec)
		return err
	case err != nil:
		return err // interrupted: unresolved, reconciled by name
	case res.code != 0:
		found, ours, ierr := c.bindObject(ctx, o, rec)
		switch {
		case ierr != nil:
			return uncertain("podman %s create failed (%s) and the engine could not be asked: %v", o.kind, strings.TrimSpace(res.stderr), ierr)
		case found && ours:
			return nil // a lost response: the object is there and now bound
		case found && ambiguous(res.stderr):
			return uncertain("podman %s create: %s", o.kind, strings.TrimSpace(res.stderr))
		case found:
			// The engine refused the request (the name is taken): the answer resolves it.
			rec.Unanswered = false
			if err := c.save(*rec); err != nil {
				return uncertain("recording the rejected create of %s: %v", rec.Identity, err)
			}
			return fmt.Errorf("%w: a foreign %s holds the name %s", kr.ErrSpecMismatch, o.kind, rec.Name)
		case ambiguous(res.stderr):
			return uncertain("podman %s create: %s", o.kind, strings.TrimSpace(res.stderr))
		}
		rec.Unanswered = false
		if err := c.save(*rec); err != nil {
			return uncertain("recording the rejected create of %s: %v", rec.Identity, err)
		}
		return fmt.Errorf("%w: podman %s create rejected: %s", kr.ErrNotReached, o.kind, strings.TrimSpace(res.stderr))
	}
	found, ours, err = c.bindObject(ctx, o, rec)
	switch {
	case err != nil:
		return uncertain("reading the created %s of %s: %v", o.kind, rec.Identity, err)
	case !found || !ours:
		return uncertain("the created %s of %s is not observed", o.kind, rec.Identity)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Run network
// ---------------------------------------------------------------------------

// knet is the part of `podman network inspect` the adapter decides on.
type knet struct {
	Name     string            `json:"name"`
	ID       string            `json:"id"`
	Labels   map[string]string `json:"labels"`
	Internal bool              `json:"internal"`
}

func networkName(scope domain.Scope, run domain.RunID) string {
	return objectName("kn-", scope, string(run))
}

func (c *kernelCore) inspectNetwork(ctx context.Context, name string) (knet, bool, error) {
	ok, err := c.pm.exists(ctx, "network", name)
	if err != nil || !ok {
		return knet{}, false, err
	}
	res, err := c.pm.query(ctx, "network", "inspect", "--format", "json", name)
	if err != nil {
		return knet{}, false, err
	}
	var all []knet
	if err := json.Unmarshal([]byte(res.stdout), &all); err != nil || len(all) != 1 {
		return knet{}, false, errors.New("podman: unreadable network inspect output")
	}
	return all[0], true, nil
}

// networkRec is the durable record of a run's internal network. Its identity is
// the run: one network per run, shared by its services and workloads.
func networkRec(scope domain.Scope, run domain.RunID, fence kr.Fence) krec {
	return krec{
		Name: networkName(scope, run), Class: classNetwork, Scope: scope, RunID: run,
		Identity: string(run), SpecDigest: "run-network/v1", Fence: fence,
	}
}

// ensureNetwork makes the run's network exist under a durable record: with its
// instance UID, an announced create, unresolved-request tracking and a fence. A
// network whose cleanup was confirmed is fenced for good: nothing of the run
// creates it again.
func (c *kernelCore) ensureNetwork(ctx context.Context, scope domain.Scope, run domain.RunID, fence kr.Fence) error {
	want := networkRec(scope, run, fence)
	unlock, err := c.locks.lock(ctx, want.Name)
	if err != nil {
		return notReached(err)
	}
	defer unlock()
	rec, err := c.admit(want)
	if err != nil {
		return err
	}
	return c.ensureObject(ctx, c.networkObject(), &rec)
}

// verifiedNetwork checks that the run's network is recorded, created and not
// removed: a workload never attaches to a network the adapter does not hold.
func (c *kernelCore) verifiedNetwork(scope domain.Scope, run domain.RunID) (string, error) {
	rec, found, err := c.load(networkName(scope, run))
	switch {
	case err != nil:
		return "", err
	case !found || rec.Class != classNetwork || rec.Scope != scope || rec.RunID != run:
		return "", refuse("the run has no network")
	case rec.fenced():
		return "", fmt.Errorf("%w: the run network was removed", kr.ErrStaleFence)
	case rec.ObjectID == "":
		return "", refuse("the run network was not created")
	}
	return rec.Name, nil
}

// netState is the outcome of a network cleanup attempt.
type netState int

const (
	// netDone: the network is confirmed removed, or the adapter never held one.
	netDone netState = iota
	// netInUse: containers are still attached; the last of them to be removed
	// finishes the cleanup.
	netInUse
	// netUnresolved: a create whose outcome was never seen may still land; the
	// removal cannot be confirmed.
	netUnresolved
)

// settleNetwork removes the run's network when nothing is attached and confirms
// the engine no longer has it. A network the adapter never recorded is not its
// to remove. A removal that fails for a reason other than containers still
// attached is an error. The caller holds a resource lock, never the network's.
func (c *kernelCore) settleNetwork(ctx context.Context, scope domain.Scope, run domain.RunID) (netState, error) {
	name := networkName(scope, run)
	unlock, err := c.locks.lock(ctx, name)
	if err != nil {
		return netDone, err
	}
	defer unlock()
	rec, found, err := c.load(name)
	if err != nil || !found || rec.Removed {
		return netDone, err
	}
	o := c.networkObject()
	_, ours, err := c.bindObject(ctx, o, &rec)
	if err != nil {
		return netDone, err
	}
	switch {
	case ours:
		res, err := c.pm.run(ctx, "network", "rm", name) // by name: removal by ID ignores attached containers
		if err != nil {
			return netDone, err
		}
		if res.code != 0 && !strings.Contains(res.stderr, "not found") {
			used, uerr := c.pm.query(ctx, "ps", "--all", "--quiet", "--filter", "network="+name)
			if uerr != nil {
				return netDone, uerr
			}
			if strings.TrimSpace(used.stdout) != "" {
				return netInUse, nil
			}
			return netDone, uncertain("podman network rm: %s", strings.TrimSpace(res.stderr))
		}
		if _, still, err := c.inspectNetwork(ctx, name); err != nil {
			return netDone, err
		} else if still {
			return netDone, uncertain("the network of %s still exists after removal", rec.Identity)
		}
	case rec.Unanswered:
		// Nothing of ours is observed, but the create may still land (a foreign
		// object at the name does not change that).
		return netUnresolved, nil
	}
	rec.Stopping, rec.Stopped, rec.Removed = true, true, true
	if err := c.save(rec); err != nil {
		return netDone, err
	}
	return netDone, nil
}

// ---------------------------------------------------------------------------
// Workspace holders
// ---------------------------------------------------------------------------

func (c *kernelCore) holderDir(ws string) string { return filepath.Join(c.dir(ws), "holders") }

// markHolder records, durably and before its create is issued, that a workload
// may come to hold the workspace. The caller holds the workspace's lock.
func (c *kernelCore) markHolder(ws, workload string) error {
	dir := c.holderDir(ws)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	file := filepath.Join(dir, workload)
	if _, err := os.Stat(file); err == nil {
		return nil
	}
	err := writeDurably(dir, file, []byte(workload), os.Link)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	return err
}

// unresolvedHolders lists the workloads whose create for the workspace was
// announced and whose outcome is not known: no container is observed for them
// yet one may still land. It prunes intents that settled (the workload was
// removed, or its create was rejected and no container exists). The caller holds
// the workspace's lock, so no launch is in flight.
func (c *kernelCore) unresolvedHolders(ws string) ([]string, error) {
	entries, err := os.ReadDir(c.holderDir(ws))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.Type().IsRegular() || strings.HasPrefix(e.Name(), ".tmp-") {
			continue
		}
		rec, found, err := c.load(e.Name())
		if err != nil {
			return nil, err
		}
		switch {
		case !found || rec.Removed || (rec.ObjectID == "" && !rec.Unanswered):
			if err := os.Remove(filepath.Join(c.holderDir(ws), e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return nil, err
			}
		case rec.ObjectID == "" && rec.Unanswered:
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// resolveHolders resolves, through the owned instance, the holders whose create
// outcome is unresolved: a container that landed is found by name and UID and
// bound, which the engine scan then sees like any other holder. Nothing is
// resolved by absence. The caller holds the workspace's lock; lock order is
// workspace, then workload, as in Start.
func (c *kernelCore) resolveHolders(ctx context.Context, ws string) error {
	entries, err := os.ReadDir(c.holderDir(ws))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.Type().IsRegular() || strings.HasPrefix(e.Name(), ".tmp-") {
			continue
		}
		if err := c.resolveHolder(ctx, e.Name()); err != nil {
			return err
		}
	}
	return nil
}

func (c *kernelCore) resolveHolder(ctx context.Context, workload string) error {
	unlock, err := c.locks.lock(ctx, workload)
	if err != nil {
		return err
	}
	defer unlock()
	rec, found, err := c.load(workload)
	if err != nil || !found || rec.ObjectID != "" || !rec.Unanswered {
		return err
	}
	if _, _, err := c.findContainer(ctx, &rec); err != nil && !errors.Is(err, errForeign) {
		return err
	}
	return nil
}
