package runprofile

import (
	"context"
	"fmt"
	"strings"
	"time"
)

func secretEnv(name, secret, key string) map[string]any {
	return map[string]any{"name": name, "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": secret, "key": key}}}
}

func plainEnv(name, value string) map[string]any { return map[string]any{"name": name, "value": value} }

func emptyDir(name, size string, memory bool) map[string]any {
	ed := map[string]any{"sizeLimit": size}
	if memory {
		ed["medium"] = "Memory"
	}
	return map[string]any{"name": name, "emptyDir": ed}
}

func mount(name, path string, readOnly bool) map[string]any {
	m := map[string]any{"name": name, "mountPath": path}
	if readOnly {
		m["readOnly"] = true
	}
	return m
}

func secretVolume(name, secret string, mode int) map[string]any {
	return map[string]any{"name": name, "secret": map[string]any{"secretName": secret, "defaultMode": mode}}
}

// logs returns a Pod's container log.
func (c *Cluster) logs(ctx context.Context, ns, pod string) string {
	out, _ := c.K.Run(ctx, nil, "-n", ns, "logs", pod, "--tail=400")
	return out
}

// noteImage records the digest the node actually resolved for a Pod.
func (q *Qualifier) noteImage(p *PodJSON, ref string) {
	for _, cs := range p.Status.ContainerStatuses {
		if cs.ImageID != "" {
			q.rep.ImageIDs[ref] = cs.ImageID
		}
	}
}

// oneShot runs a script to completion in a guarded Pod and returns its log
// and exit code. The Pod is always removed.
func (q *Qualifier) oneShot(ctx context.Context, ns string, p Pod, script string, timeout time.Duration) (string, int, error) {
	p.Namespace = ns
	p.Command = []string{"sh", "-ec", script}
	if err := q.c.ApplyAll(ctx, q.c.PodObject(p)); err != nil {
		return "", -1, err
	}
	defer func() {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		_ = q.c.DeletePod(dctx, ns, p.Name, time.Minute)
	}()
	phase, code, err := q.c.WaitDone(ctx, ns, p.Name, timeout)
	out := q.c.logs(ctx, ns, p.Name)
	if pj, gerr := q.c.GetPod(ctx, ns, p.Name); gerr == nil {
		q.noteImage(pj, p.Image)
	}
	if err != nil {
		return out, code, fmt.Errorf("pod %s: %w", p.Name, err)
	}
	_ = phase
	return out, code, nil
}

func lines(out string, max int) []string {
	var res []string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		l = strings.TrimSpace(l)
		if l != "" {
			res = append(res, l)
		}
		if len(res) == max {
			break
		}
	}
	return res
}

// grepLines returns the log lines containing any needle.
func grepLines(out string, needles ...string) []string {
	var res []string
	for _, l := range strings.Split(out, "\n") {
		for _, n := range needles {
			if strings.Contains(l, n) {
				res = append(res, strings.TrimSpace(l))
				break
			}
		}
	}
	return res
}

func (q *Qualifier) add(id, capability string, required bool, status Status, detail string, evidence []string, handoff string) {
	for i := range evidence {
		evidence[i] = q.c.K.Rec.Redact(evidence[i])
		if len(evidence[i]) > 400 {
			evidence[i] = evidence[i][:400] + "…"
		}
	}
	q.rep.Add(Check{ID: id, Capability: capability, Required: required, Status: status,
		Detail: q.c.K.Rec.Redact(detail), Evidence: evidence, Handoff: handoff})
}

// hardened returns the baseline shape of a non-root user-namespaced probe.
func (q *Qualifier) base(name, image string, uid int64) Pod {
	return Pod{Name: name, Namespace: q.nsMain, Image: image, UID: uid, GID: uid, Labels: map[string]any{"krp/probe": name}}
}

const minute = time.Minute

// serveScript is a shell snippet that serves root over HTTP on 8080 as a
// non-root user with the httpd image's Apache (the image's busybox has no
// httpd applet).
func serveScript(root string) string {
	return fmt.Sprintf(`mkdir -p %[1]s
cat > %[1]s/.httpd.conf <<'CONF'
ServerRoot /usr/local/apache2
Listen 8080
LoadModule mpm_event_module modules/mod_mpm_event.so
LoadModule authz_core_module modules/mod_authz_core.so
LoadModule unixd_module modules/mod_unixd.so
LoadModule dir_module modules/mod_dir.so
LoadModule mime_module modules/mod_mime.so
PidFile %[1]s/.httpd.pid
DocumentRoot "%[1]s"
<Directory "%[1]s">
  Require all granted
</Directory>
ErrorLog /dev/stderr
TypesConfig conf/mime.types
CONF
exec httpd -f %[1]s/.httpd.conf -DFOREGROUND`, root)
}
