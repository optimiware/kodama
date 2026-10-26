package runprofile

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"
)

const wwwPage = `<html><body><script>document.body.innerHTML = '<p id="r">' + (6 * 7) + '</p>';</script></body></html>`

// ensureWWW starts the in-run static page server once.
func (q *Qualifier) ensureWWW(ctx context.Context) error {
	ns := q.nsMain
	if out, _ := q.c.K.Run(ctx, nil, "-n", ns, "get", "pod", "www", "--ignore-not-found", "-o", "name"); strings.TrimSpace(out) != "" {
		return nil
	}
	w := q.base("www", q.img("httpd"), 10001)
	w.Labels = map[string]any{"krp/svc": "www"}
	w.Command = []string{"sh", "-ec", `while [ ! -f /www/.ready ]; do sleep 1; done; ` + serveScript("/www")}
	w.Volumes = []any{emptyDir("www", "8Mi", false)}
	w.Mounts = []any{mount("www", "/www", false)}
	w.Readiness = map[string]any{"tcpSocket": map[string]any{"port": 8080}, "periodSeconds": 3}
	if err := q.c.ApplyAll(ctx, q.c.PodObject(w), q.service(ns, "www", 8080)); err != nil {
		return err
	}
	if err := Wait(ctx, 6*minute, 3*time.Second, func() (bool, error) {
		p, err := q.c.GetPod(ctx, ns, "www")
		if err != nil {
			return false, err
		}
		return p.Status.Phase == "Running", fmt.Errorf("www phase %s", p.Status.Phase)
	}); err != nil {
		return err
	}
	tree, err := buildContext(map[string]string{"index.html": wwwPage}, nil)
	if err != nil {
		return err
	}
	if _, err := q.c.Exec(ctx, ns, "www", bytes.NewReader(tree), "tar -C /www -xf - && touch /www/.ready"); err != nil {
		return err
	}
	return q.c.WaitReady(ctx, ns, "www", 3*time.Minute)
}

// browserPod is a client-role Pod running the headless browser with a
// bounded memory-backed /dev/shm.
func (q *Qualifier) browserPod(name string, caps []string) Pod {
	b := q.base(name, q.img("browser"), 1000)
	b.Labels = map[string]any{"krp/role": "client"}
	b.Caps = caps
	b.MemLimit = "1Gi"
	b.CPU = "250m"
	b.Env = []any{plainEnv("HOME", "/tmp")}
	b.Volumes = []any{emptyDir("tmp", "256Mi", false), emptyDir("shm", "64Mi", true)}
	b.Mounts = []any{mount("tmp", "/tmp", false), mount("shm", "/dev/shm", false)}
	return b
}

const browserScript = `
set +e
BIN=$(command -v headless-shell || ls /headless-shell/headless-shell)
echo "shm_df=$(df -k /dev/shm | tail -1 | tr -s ' ')"
if dd if=/dev/zero of=/dev/shm/fill bs=1M count=100 2>/dev/null; then echo "shm_overfill=allowed"; else echo "shm_overfill=refused"; fi
rm -f /dev/shm/fill
timeout 90 "$BIN" --headless --disable-gpu --user-data-dir=/tmp/profile --dump-dom http://www.%s.svc.cluster.local:8080/ >/tmp/dom.html 2>/tmp/browser.err
echo "browser_exit=$?"
cat /tmp/dom.html
tail -n 12 /tmp/browser.err | cut -c1-240
`

// phaseBrowser proves the browser starts with its sandbox enabled (it is
// never given --no-sandbox) under the user-namespaced profile, that
// /dev/shm is bounded, and records the capability control.
func (q *Qualifier) phaseBrowser(ctx context.Context) error {
	ns := q.nsMain
	if err := q.ensureWWW(ctx); err != nil {
		return fmt.Errorf("www fixture: %w", err)
	}
	script := fmt.Sprintf(browserScript, ns)
	out, code, err := q.oneShot(ctx, ns, q.browserPod("browser", []string{"SYS_ADMIN"}), script, 8*minute)
	ctl, _, cerr := q.oneShot(ctx, ns, q.browserPod("browser-nocap", nil), script, 8*minute)
	ev := append(lines(out, 18), "control without SYS_ADMIN in the bounding set: "+strings.Join(grepLines(ctl, "browser_exit", "andbox", "FATAL"), " | "))
	handoff := ""
	status, detail := Pass, "headless Chromium ran as uid 1000 in a user-namespaced Pod without --no-sandbox, rendered the in-run page, and /dev/shm stayed bounded at 64Mi"
	switch {
	case err != nil || code != 0:
		status, detail = Fail, fmt.Sprintf("browser Pod failed: exit %d err %v", code, err)
	case (strings.Contains(out, "namespace sandbox") || strings.Contains(out, "No usable sandbox")) && !strings.Contains(out, "browser_exit=0"):
		status, detail = Blocked, "Chromium's namespace sandbox cannot start under the node security profile (it is never run with --no-sandbox)"
		handoff = buildahHandoff
	case !strings.Contains(out, `id="r">42<`) || !strings.Contains(out, "browser_exit=0"):
		status, detail = Fail, "browser did not render the page with its sandbox enabled (see evidence)"
	case !strings.Contains(out, "shm_overfill=refused"):
		status, detail = Fail, "/dev/shm accepted more than its limit"
	}
	_ = cerr
	q.add("service.browser", "browser sandbox and bounded shared memory", true, status, detail, ev, handoff)
	return nil
}
