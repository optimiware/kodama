package runprofile

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type nsMeta struct {
	Metadata struct {
		UID             string            `json:"uid"`
		ResourceVersion string            `json:"resourceVersion"`
		Labels          map[string]string `json:"labels"`
	} `json:"metadata"`
}

// listPVNamespaces returns PV name -> claim namespace. An error is never
// swallowed: cleanup that cannot list PersistentVolumes is not verified.
func (q *Qualifier) listPVNamespaces(ctx context.Context) (map[string]string, error) {
	out, err := q.c.K.Run(ctx, nil, "get", "pv", "-o", "jsonpath={range .items[*]}{.metadata.name}{\" \"}{.spec.claimRef.namespace}{\"\\n\"}{end}")
	if err != nil {
		return nil, err
	}
	res := map[string]string{}
	for _, l := range lines(out, 5000) {
		if f := strings.Fields(l); len(f) == 2 {
			res[f[0]] = f[1]
		}
	}
	return res, nil
}

// deleteChecked deletes one namespace this run created. It refuses anything
// whose UID or ownership labels differ from what the run recorded, and the
// delete itself carries uid and resourceVersion preconditions, so a foreign
// object that replaced ours between the check and the delete is not removed
// (the API answers 409 and the identity is re-read and re-checked).
func (q *Qualifier) deleteChecked(ctx context.Context, hc *http.Client, base, ns, uid string) (deleted, pending bool, detail string) {
	for attempt := 0; attempt < 3; attempt++ {
		var meta nsMeta
		if err := q.c.K.JSON(ctx, &meta, "get", "namespace", ns); err != nil {
			if strings.Contains(err.Error(), "NotFound") {
				return true, false, "already absent"
			}
			return false, false, "lookup failed: " + tail(err.Error(), 160)
		}
		m := meta.Metadata
		if m.UID != uid || m.Labels[LabelTask] != TaskValue || m.Labels[LabelRun] != q.c.RunID || !strings.HasPrefix(ns, NamespacePrefix) {
			return false, false, "refused: identity or ownership labels do not match this run; not deleted"
		}
		status, err := DeleteNamespacePrecondition(ctx, hc, base, ns, m.UID, m.ResourceVersion)
		switch {
		case err != nil:
			return false, false, "delete failed: " + tail(err.Error(), 160)
		case status == http.StatusOK || status == http.StatusAccepted:
			return false, true, ""
		case status == http.StatusNotFound:
			return true, false, "already absent"
		case status == http.StatusConflict:
			continue // changed underneath us: re-read and re-verify identity
		default:
			return false, false, fmt.Sprintf("delete refused by API: HTTP %d", status)
		}
	}
	return false, false, "refused: namespace kept changing identity or version during deletion; not deleted"
}

// teardown removes only the namespaces this run created and then proves
// nothing of the run remains. Any lookup that fails (PersistentVolumes,
// VolumeAttachments, the final namespace list) is recorded as a leftover:
// unverifiable cleanup is never reported as verified.
func (q *Qualifier) teardown() {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	cl := &q.rep.Cleanup
	if q.reg != nil && q.reg.stop != nil {
		q.reg.stop()
	}
	leftover := func(s string) { cl.Leftovers = append(cl.Leftovers, s) }

	before, pvErr := q.listPVNamespaces(ctx)
	if pvErr != nil {
		leftover("unverifiable: PersistentVolume listing before deletion failed: " + tail(pvErr.Error(), 160))
	}
	var pvs []string
	for pv, ns := range before {
		if _, ours := q.c.Namespaces[ns]; ours {
			pvs = append(pvs, pv)
		}
	}

	base, stop, perr := q.c.K.StartProxy(ctx)
	hc := &http.Client{Timeout: 60 * time.Second}
	pending := map[string]bool{}
	for ns, uid := range q.c.Namespaces {
		item := CleanupItem{Name: ns, UID: uid}
		if perr != nil {
			item.Detail = "delete refused: cannot open an API proxy for a precondition delete: " + tail(perr.Error(), 120)
			cl.Namespaces = append(cl.Namespaces, item)
			continue
		}
		deleted, pend, detail := q.deleteChecked(ctx, hc, base, ns, uid)
		item.Deleted, item.Detail = deleted, detail
		if pend {
			pending[ns] = true
		}
		cl.Namespaces = append(cl.Namespaces, item)
	}
	if stop != nil {
		defer stop()
	}
	for i := range cl.Namespaces {
		it := &cl.Namespaces[i]
		if !pending[it.Name] {
			continue
		}
		err := Wait(ctx, 8*time.Minute, 5*time.Second, func() (bool, error) {
			out, err := q.c.K.Run(ctx, nil, "get", "namespace", it.Name, "--ignore-not-found", "-o", "name")
			if err != nil {
				return false, err
			}
			if strings.TrimSpace(out) != "" {
				return false, fmt.Errorf("namespace %s still terminating", it.Name)
			}
			return true, nil
		})
		it.Deleted = err == nil
		if err != nil {
			it.Detail = tail(err.Error(), 160)
		}
	}

	// Post-deletion inventory: PVs bound in our namespaces (including any
	// provisioned during the run after the first listing) and attachments.
	after, aerr := q.listPVNamespaces(ctx)
	if aerr != nil {
		leftover("unverifiable: PersistentVolume listing after deletion failed: " + tail(aerr.Error(), 160))
	}
	for pv, ns := range after {
		if _, ours := q.c.Namespaces[ns]; ours {
			seen := false
			for _, p := range pvs {
				seen = seen || p == pv
			}
			if !seen {
				pvs = append(pvs, pv)
			}
		}
	}
	for _, pv := range pvs {
		err := Wait(ctx, 6*time.Minute, 5*time.Second, func() (bool, error) {
			out, err := q.c.K.Run(ctx, nil, "get", "pv", pv, "--ignore-not-found", "-o", "name")
			if err != nil {
				return false, err
			}
			if strings.TrimSpace(out) != "" {
				return false, fmt.Errorf("pv %s remains", pv)
			}
			return true, nil
		})
		if err != nil {
			leftover("pv/" + pv)
		} else {
			cl.Evidence = append(cl.Evidence, "pv/"+pv+" removed")
		}
		if n, err := q.attachments(ctx, pv); err != nil {
			leftover("unverifiable: VolumeAttachment lookup for " + pv + " failed: " + tail(err.Error(), 120))
		} else if n > 0 {
			leftover("volumeattachment for " + pv)
		}
	}
	out, err := q.c.K.Run(ctx, nil, "get", "namespace", "-l", LabelRun+"="+q.c.RunID, "-o", "name")
	if err != nil {
		leftover("unverifiable: final namespace listing failed: " + tail(err.Error(), 160))
	}
	for _, l := range lines(out, 20) {
		leftover(l)
	}
	cl.Verified = len(cl.Namespaces) > 0
	for _, n := range cl.Namespaces {
		if !n.Deleted {
			cl.Verified = false
			leftover("namespace/" + n.Name + ": " + n.Detail)
		}
	}
	if len(cl.Leftovers) > 0 {
		cl.Verified = false
	}
}
