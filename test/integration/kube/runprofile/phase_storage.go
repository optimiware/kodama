package runprofile

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// phaseStorage proves sequential ReadWriteOncePod sharing of one run volume
// across images and UIDs, idmapped ownership, the RWOP exclusivity fence,
// delete reclaim and actual mount (VolumeAttachment) release.
func (q *Qualifier) phaseStorage(ctx context.Context) error {
	ns := q.nsMain
	pvc := q.c.obj("v1", "PersistentVolumeClaim", ns, "run-workspace", nil, map[string]any{
		"accessModes": []any{"ReadWriteOncePod"}, "storageClassName": q.cfg.StorageClass,
		"resources": map[string]any{"requests": map[string]any{"storage": "1Gi"}},
	})
	if err := q.c.ApplyAll(ctx, pvc); err != nil {
		return err
	}
	vol := []any{map[string]any{"name": "ws", "persistentVolumeClaim": map[string]any{"claimName": "run-workspace"}}}
	mnt := []any{mount("ws", "/w", false)}

	holder := q.base("ws-holder", q.img("httpd"), 10001)
	holder.FSGroup = 4000
	holder.Volumes, holder.Mounts = vol, mnt
	holder.Command = []string{"sh", "-ec", `umask 002; echo one > /w/from-holder; mkdir -p /w/shared; echo 1 > /w/shared/x
echo "holder_uid_map=$(tr -s ' ' < /proc/self/uid_map)"
echo "holder_mount=$(grep ' /w ' /proc/self/mounts)"
echo "holder_owner=$(stat -c '%u:%g' /w/from-holder)"
echo ready; sleep 900`}
	if err := q.c.ApplyAll(ctx, q.c.PodObject(holder)); err != nil {
		return err
	}
	var holderLog string
	if err := Wait(ctx, 8*minute, 4*time.Second, func() (bool, error) {
		holderLog = q.c.logs(ctx, ns, holder.Name)
		return strings.Contains(holderLog, "ready"), fmt.Errorf("holder not ready")
	}); err != nil {
		q.add("storage.rwop_sequential", "RWOP sequential sharing", true, Fail, "holder Pod never mounted the claim: "+err.Error(), grepLines(q.podEvents(ctx, ns, holder.Name), "Warning"), "")
		return nil
	}

	// Exclusivity: a second Pod naming the live claim must not run.
	second := q.base("ws-second", q.img("postgres"), 20002)
	second.FSGroup = 4000
	second.Volumes, second.Mounts = vol, mnt
	second.Command = []string{"sh", "-ec", `echo "second_uid_map=$(tr -s ' ' < /proc/self/uid_map)"
echo "second_mount=$(grep ' /w ' /proc/self/mounts)"
echo "second_sees_holder_file=$(stat -c '%u:%g' /w/from-holder)"
echo "second_sees_holder_dir=$(stat -c '%u:%g' /w/shared)"
cat /w/shared/x
umask 002; echo two > /w/from-second; mkdir /w/shared/second
echo "second_writes=$(stat -c '%u:%g' /w/from-second)"`}
	if err := q.c.ApplyAll(ctx, q.c.PodObject(second)); err != nil {
		return err
	}
	var pendMsg string
	pendErr := Wait(ctx, 3*time.Minute, 5*time.Second, func() (bool, error) {
		p, err := q.c.GetPod(ctx, ns, second.Name)
		if err != nil {
			return false, err
		}
		if p.Status.Phase != "Pending" || p.Spec.NodeName != "" {
			return false, fmt.Errorf("second Pod is %s on node %q while the claim is held", p.Status.Phase, p.Spec.NodeName)
		}
		for _, cd := range p.Status.Conditions {
			if cd.Type == "PodScheduled" && cd.Status == "False" && strings.Contains(cd.Message, "ReadWriteOncePod") {
				pendMsg = cd.Message
				return true, nil
			}
		}
		return false, fmt.Errorf("no RWOP scheduling refusal yet")
	})
	if pendErr != nil {
		q.add("storage.rwop_exclusive", "RWOP exclusivity fence", true, Fail, pendErr.Error(), nil, "")
	} else {
		q.add("storage.rwop_exclusive", "RWOP exclusivity fence", true, Pass, "second Pod stayed unscheduled while the holder held the claim", []string{pendMsg}, "")
	}

	// Release the holder; the same second Pod must then run and see the data.
	if err := q.c.DeletePod(ctx, ns, holder.Name, 3*time.Minute); err != nil {
		q.add("storage.rwop_sequential", "RWOP sequential sharing", true, Fail, "holder deletion: "+err.Error(), nil, "")
		return nil
	}
	if phase, code, err := q.c.WaitDone(ctx, ns, second.Name, 8*minute); err != nil || phase != "Succeeded" || code != 0 {
		q.add("storage.rwop_sequential", "RWOP sequential sharing", true, Fail, fmt.Sprintf("second Pod did not complete after release: phase=%s exit=%d err=%v", phase, code, err), grepLines(q.podEvents(ctx, ns, second.Name), "Warning"), "")
		return nil
	}
	secondLog := q.c.logs(ctx, ns, second.Name)
	if pj, err := q.c.GetPod(ctx, ns, second.Name); err == nil {
		q.noteImage(pj, second.Image)
	}
	if err := q.c.DeletePod(ctx, ns, second.Name, 3*time.Minute); err != nil {
		return err
	}
	third := q.base("ws-third", q.img("httpd"), 30003)
	third.FSGroup = 4000
	third.Volumes, third.Mounts = vol, mnt
	out, code, err := q.oneShot(ctx, ns, third, `echo "third_uid_map=$(tr -s ' ' < /proc/self/uid_map)"
echo "third_sees_holder=$(stat -c '%u:%g' /w/from-holder)"
echo "third_sees_second=$(stat -c '%u:%g' /w/from-second)"
cat /w/from-holder /w/from-second
umask 002; echo three > /w/shared/second/from-third && echo third_write_ok`, 8*minute)
	if err != nil || code != 0 {
		q.add("storage.rwop_sequential", "RWOP sequential sharing", true, Fail, fmt.Sprintf("third Pod failed: exit=%d err=%v", code, err), lines(out, 8), "")
		return nil
	}
	kv := func(log, key string) string {
		for _, l := range strings.Split(log, "\n") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(l), key+"="); ok {
				return v
			}
		}
		return ""
	}
	seqOK := strings.Contains(holderLog, "holder_owner=10001:") &&
		strings.HasPrefix(kv(secondLog, "second_writes"), "20002:") && strings.Contains(out, "third_write_ok") &&
		strings.Contains(out, "one") && strings.Contains(out, "two")
	ev := []string{kv(holderLog, "holder_uid_map"), kv(secondLog, "second_uid_map"), kv(out, "third_uid_map"),
		"second sees holder file " + kv(secondLog, "second_sees_holder_file"), "third sees holder file " + kv(out, "third_sees_holder"),
		"third sees second file " + kv(out, "third_sees_second")}
	status, detail := Pass, "httpd(uid 10001) -> postgres(uid 20002) -> httpd(uid 30003) shared one RWOP claim sequentially with a shared group"
	if !seqOK {
		status, detail = Fail, "ownership or data did not survive the image/UID handoffs"
	}
	q.add("storage.rwop_sequential", "RWOP sequential sharing", true, status, detail, ev, "")

	idmap := strings.Contains(kv(holderLog, "holder_mount"), "idmapped") || strings.Contains(holderLog, "idmapped")
	sees := kv(secondLog, "second_sees_holder_file")
	maps := []string{kv(holderLog, "holder_uid_map"), kv(secondLog, "second_uid_map"), kv(out, "third_uid_map")}
	distinct := maps[0] != "" && maps[0] != maps[1] && maps[1] != maps[2]
	st, det := Pass, "volume mounted idmapped; each Pod saw files under its own distinct user-namespace mapping with the true owner"
	switch {
	case !idmap:
		st, det = Fail, "mountinfo shows no idmapped mount for the volume"
	case !strings.HasPrefix(sees, "10001:"):
		st, det = Fail, "second Pod saw foreign owner "+sees+" instead of 10001: ownership is not idmapped"
	case !distinct:
		st, det = Fail, "user-namespace mappings were not distinct across Pods, so idmapping was not exercised"
	}
	q.add("storage.idmapped_ownership", "idmapped file ownership", true, st, det,
		[]string{kv(holderLog, "holder_mount"), kv(secondLog, "second_mount"), "second sees holder dir " + kv(secondLog, "second_sees_holder_dir")}, "")

	// Mount release and delete reclaim.
	pv, _ := q.c.K.Run(ctx, nil, "-n", ns, "get", "pvc", "run-workspace", "-o", "jsonpath={.spec.volumeName}")
	pv = strings.TrimSpace(pv)
	if pv == "" {
		q.add("storage.release_delete", "mount release and delete reclaim", true, Fail, "claim never bound", nil, "")
		return nil
	}
	relErr := Wait(ctx, 5*time.Minute, 5*time.Second, func() (bool, error) {
		n, err := q.attachments(ctx, pv)
		if err != nil {
			return false, err
		}
		if n > 0 {
			return false, fmt.Errorf("%d VolumeAttachment(s) still reference %s", n, pv)
		}
		return true, nil
	})
	if relErr != nil {
		q.add("storage.release_delete", "mount release and delete reclaim", true, Fail, "volume still attached after all Pods were deleted: "+relErr.Error(), nil, "")
		return nil
	}
	if _, err := q.c.K.Run(ctx, nil, "-n", ns, "delete", "pvc", "run-workspace", "--wait=true", "--timeout=180s"); err != nil {
		q.add("storage.release_delete", "mount release and delete reclaim", true, Fail, "claim deletion: "+err.Error(), nil, "")
		return nil
	}
	delErr := Wait(ctx, 6*time.Minute, 5*time.Second, func() (bool, error) {
		out, err := q.c.K.Run(ctx, nil, "get", "pv", pv, "--ignore-not-found", "-o", "name")
		if err != nil {
			return false, err
		}
		if strings.TrimSpace(out) != "" {
			return false, fmt.Errorf("pv %s still exists", pv)
		}
		return true, nil
	})
	if delErr != nil {
		q.add("storage.release_delete", "mount release and delete reclaim", true, Fail, delErr.Error(), nil, "")
		return nil
	}
	q.add("storage.release_delete", "mount release and delete reclaim", true, Pass,
		"after Pod deletion the VolumeAttachment was released, and deleting the claim removed the PersistentVolume (Delete reclaim)",
		[]string{"pv " + pv + " attachments=0 then removed"}, "")
	return nil
}

func (q *Qualifier) attachments(ctx context.Context, pv string) (int, error) {
	var list struct {
		Items []struct {
			Spec struct {
				Source struct {
					PersistentVolumeName string `json:"persistentVolumeName"`
				} `json:"source"`
			} `json:"spec"`
		} `json:"items"`
	}
	out, err := q.c.K.Run(ctx, nil, "get", "volumeattachments.storage.k8s.io", "-o", "json")
	if err != nil {
		return 0, err
	}
	if i := strings.Index(out, "{"); i >= 0 {
		out = out[i:]
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return 0, err
	}
	n := 0
	for _, it := range list.Items {
		if it.Spec.Source.PersistentVolumeName == pv {
			n++
		}
	}
	return n, nil
}

// podEvents returns the events about a Pod in a namespace, for evidence.
func (q *Qualifier) podEvents(ctx context.Context, ns, pod string) string {
	out, _ := q.c.K.Run(ctx, nil, "-n", ns, "get", "events", "--field-selector", "involvedObject.name="+pod, "-o", "custom-columns=TYPE:.type,REASON:.reason,MSG:.message", "--no-headers")
	return out
}
