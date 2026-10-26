package runprofile

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const helperSource = `package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "build" {
		fmt.Println("helper ok")
		return
	}
	resp, err := http.Get(os.Getenv("DEPS_URL") + "/data.txt")
	if err != nil {
		fmt.Fprintln(os.Stderr, "dependency fetch:", err)
		os.Exit(2)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	payload := strings.TrimSpace(string(data))
	if resp.StatusCode != 200 || payload == "" {
		fmt.Fprintln(os.Stderr, "dependency fetch status", resp.Status)
		os.Exit(3)
	}
	if err := os.MkdirAll("/own", 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(4)
	}
	for _, o := range []struct {
		name     string
		uid, gid int
	}{{"root", 0, 0}, {"u1000", 1000, 1000}, {"u65534", 65534, 65534}, {"u4242", 4242, 4343}} {
		path := "/own/" + o.name
		if err := os.WriteFile(path, []byte(o.name), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(5)
		}
		if err := os.Chown(path, o.uid, o.gid); err != nil {
			fmt.Fprintln(os.Stderr, "chown", path, err)
			os.Exit(6)
		}
	}
	if err := os.WriteFile("/dep-"+payload, []byte(payload), 0o644); err != nil {
		os.Exit(7)
	}
	fmt.Println("build step complete", payload)
}
`

const containerfile = `FROM scratch
ARG DEPS_URL
COPY helper /helper
RUN ["/helper", "build"]
ENTRYPOINT ["/helper"]
`

// buildHelper compiles the static helper the RUN step executes. It needs the
// Go toolchain of the host running the qualification and no network.
func buildHelper(dir string) (string, error) {
	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(src, "main.go"), []byte(helperSource), 0o644); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(src, "go.mod"), []byte("module helper\n\ngo 1.22\n"), 0o644); err != nil {
		return "", err
	}
	out := filepath.Join(dir, "helper")
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags=-s -w", "-o", out, ".")
	cmd.Dir = src
	cmd.Env = append(cleanEnv(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64", "GOTOOLCHAIN=local", "GOPROXY=off", "GOFLAGS=-mod=mod")
	if b, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("compile helper: %w: %s", err, tail(string(b), 300))
	}
	return out, nil
}

// variant is one explicit, escalating-but-never-host-privileged Pod shape.
type variant struct {
	Name string
	UID  int64
	Caps []string
	PE   bool
}

// variants are the only shapes ever tried. No variant sets an unconfined
// seccomp or AppArmor profile, mounts a host path or runs privileged; Guard
// refuses those regardless. Later variants add only capabilities inside the
// Pod's own user namespace.
var variants = []variant{
	{"nonroot-no-caps", 1000, nil, false},
	{"nonroot-userns-bounding", 1000, []string{"SETUID", "SETGID", "SYS_ADMIN"}, true},
	{"root-in-userns", 0, []string{"SETUID", "SETGID", "SYS_ADMIN", "SYS_CHROOT"}, false},
}

const diagScript = `
echo "diag_apparmor=$(cat /proc/self/attr/current 2>&1)"
echo "diag_restrict_userns=$(cat /proc/sys/kernel/apparmor_restrict_unprivileged_userns 2>&1)"
echo "diag_unshare_user=$(unshare -U true 2>&1 && echo ok)"
echo "diag_unshare_user_mount=$(unshare -Urm true 2>&1 && echo ok)"
echo "diag_caps=$(grep -E '^Cap(Prm|Bnd):' /proc/self/status | tr -s '\t ' ' ' | tr '\n' ' ')"
`

type attempt struct {
	Variant  string
	OK       bool
	Output   string
	Diag     string
	Blocked  bool // failure attributable to the node security profile
	StartErr string
}

// attemptBuild runs one build+push attempt in a freshly created builder Pod.
func (q *Qualifier) attemptBuild(ctx context.Context, name, image string, v variant, env []any, extraVols, extraMounts []any, buildScript string, ctxTar []byte) attempt {
	ns := q.nsMain
	a := attempt{Variant: v.Name}
	p := q.builderPod(name, image, v.UID)
	p.Caps, p.AllowPE = v.Caps, v.PE
	p.Env = env
	p.Volumes = append(p.Volumes, extraVols...)
	p.Mounts = append(p.Mounts, extraMounts...)
	p.Command = []string{"sleep", "900"}
	if err := q.c.ApplyAll(ctx, q.c.PodObject(p)); err != nil {
		a.StartErr = err.Error()
		return a
	}
	defer func() {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
		defer cancel()
		_ = q.c.DeletePod(dctx, ns, name, 2*time.Minute)
	}()
	if err := q.c.WaitReady(ctx, ns, name, 10*minute); err != nil {
		a.StartErr = err.Error()
		return a
	}
	if pj, err := q.c.GetPod(ctx, ns, name); err == nil {
		q.noteImage(pj, image)
	}
	if _, err := q.c.Exec(ctx, ns, name, bytes.NewReader(ctxTar), "mkdir -p /work && tar -C /work -xf -"); err != nil {
		a.StartErr = err.Error()
		return a
	}
	bctx, cancel := context.WithTimeout(ctx, 6*time.Minute)
	defer cancel()
	out, err := q.c.Exec(bctx, ns, name, nil, buildScript+" 2>&1; echo build_exit=$?")
	a.Output = out
	a.OK = err == nil && strings.Contains(out, "build_exit=0")
	if !a.OK {
		a.Diag = q.execOrEmpty(ctx, ns, name, diagScript)
		a.Blocked = strings.Contains(a.Diag, "diag_unshare_user_mount=unshare:") &&
			(strings.Contains(a.Diag, "Permission denied") || strings.Contains(a.Diag, "Operation not permitted"))
	} else {
		a.Diag = q.execOrEmpty(ctx, ns, name, `echo "processes_before_teardown=$(ps -eo pid,args 2>/dev/null | wc -l)"`)
	}
	return a
}

// verifyBuilt checks the pushed image's contents: the RUN step executed, the
// dependency was fetched, and files carry several distinct numeric owners.
func (q *Qualifier) verifyBuilt(ctx context.Context, repo string) (bool, []string) {
	client, err := q.freshClient(ctx)
	if err != nil {
		return false, []string{err.Error()}
	}
	man, digest, err := client.Manifest(repo, "latest")
	if err != nil {
		return false, []string{err.Error()}
	}
	entries, err := client.LayerEntries(repo, man)
	if err != nil {
		return false, []string{err.Error()}
	}
	owners := map[string][2]int{}
	dep := false
	for _, e := range entries {
		if strings.HasPrefix(e.Name, "own/") {
			owners[strings.TrimPrefix(e.Name, "own/")] = [2]int{e.UID, e.GID}
		}
		if e.Name == "dep-"+depPayloadPrefix+q.c.RunID {
			dep = true
		}
	}
	ok := dep && owners["u1000"] == [2]int{1000, 1000} && owners["u65534"] == [2]int{65534, 65534} &&
		owners["u4242"] == [2]int{4242, 4343} && owners["root"] == [2]int{0, 0}
	return ok, []string{"manifest " + digest, fmt.Sprintf("owners %v", owners), fmt.Sprintf("dependency marker %t", dep)}
}

const buildahHandoff = "T-114/T-116 installation prerequisite: rootless Buildah and BuildKit need to create a mount namespace inside a nested user namespace. On this target the container runtime's enforced default AppArmor profile (cri-containerd.apparmor.d) denies that mount, and Pod Security (restricted/baseline) rejects the SYS_ADMIN bounding capability the runtime's default seccomp profile requires for nested user namespaces. Provide a node-installed, builder-only AppArmor Localhost profile (and its distribution) or a dedicated builder node pool/runtime class without the denial, plus a Pod Security level that admits the builder Pods, then re-run this qualification. Do not use Unconfined profiles or privileged Pods."

// phaseBuildah attempts rootless Buildah with VFS under each permitted Pod
// shape and qualifies only a real build+push.
func (q *Qualifier) phaseBuildah(ctx context.Context) error {
	if q.reg == nil || q.reg.client == nil {
		q.add("build.buildah_vfs", "Buildah VFS build and push", true, NotRun, "registry fixture unavailable", nil, "")
		return nil
	}
	hdir, err := os.MkdirTemp(q.dir, "helper-")
	if err != nil {
		return err
	}
	helper, err := buildHelper(hdir)
	if err != nil {
		q.add("build.buildah_vfs", "Buildah VFS build and push", true, Fail, err.Error(), nil, "")
		return nil
	}
	bin, _ := os.ReadFile(helper)
	ctxTar, err := tarFiles(map[string][]byte{"helper": bin, "Containerfile": []byte(containerfile)}, map[string]int64{"helper": 0o755})
	if err != nil {
		return err
	}
	f := q.reg
	env := []any{plainEnv("HOME", "/tmp/home"), plainEnv("SSL_CERT_FILE", "/ca/ca.crt")}
	script := fmt.Sprintf(`set -e; mkdir -p /tmp/home /tmp/c; cd /work
S="--root /tmp/c/root --runroot /tmp/c/run --storage-driver vfs"
buildah $S version | head -2
buildah $S build --isolation chroot --layers=false --build-arg DEPS_URL=%s -f Containerfile -t localhost/probe:1 .
buildah $S push --authfile /creds/config.json localhost/probe:1 docker://%s/buildah:latest`, f.deps, f.host)

	var tried []string
	var ok *attempt
	var last attempt
	for i, v := range variants {
		a := q.attemptBuild(ctx, fmt.Sprintf("buildah-%d", i), q.img("buildah"), v, env, nil, nil, "("+script+")", ctxTar)
		tried = append(tried, fmt.Sprintf("%s: ok=%t blocked=%t %s", v.Name, a.OK, a.Blocked, firstLine(a.Output+a.StartErr)))
		last = a
		if a.OK {
			ok = &a
			break
		}
	}
	switch {
	case ok != nil:
		good, ev := q.verifyBuilt(ctx, "buildah")
		st, det := Pass, "Buildah (VFS, chroot isolation) ran a RUN step that fetched the dependency over run egress, created files with several numeric owners, and pushed to the registry; variant "+ok.Variant
		if !good {
			st, det = Fail, "pushed image does not show the RUN step, dependency or multi-UID ownership"
		}
		q.add("build.buildah_vfs", "Buildah VFS build and push", true, st, det, append(tried, ev...), "")
	case last.Blocked:
		q.add("build.buildah_vfs", "Buildah VFS build and push", true, Blocked,
			"every permitted rootless Pod shape failed: creating a mount namespace in a nested user namespace is denied by the node security profile",
			append(tried, lines(last.Diag, 8)...), buildahHandoff)
	default:
		q.add("build.buildah_vfs", "Buildah VFS build and push", true, Fail, "Buildah failed without a node-profile denial: "+tail(last.Output+last.StartErr, 300), append(tried, lines(last.Diag, 8)...), "")
	}
	hasFuse := false
	if pv, ok := q.rep.Inventory["podView"].(map[string]string); ok {
		hasFuse = pv["dev_fuse"] == "/dev/fuse"
	}
	q.add("build.buildah_overlay_fuse", "Buildah overlay/FUSE", false, NotAdvertised,
		fmt.Sprintf("not advertised: no evidence collected (/dev/fuse present in a default Pod: %t; the profile exposes no fuse device and the driver never requests one)", hasFuse), nil, "")
	q.psaCheck(ctx)
	return nil
}

func firstLine(s string) string {
	for _, l := range strings.Split(strings.TrimSpace(s), "\n") {
		if strings.TrimSpace(l) != "" {
			return tail(l, 160)
		}
	}
	return ""
}

func tarFiles(files map[string][]byte, modes map[string]int64) ([]byte, error) {
	m := map[string]string{}
	for k, v := range files {
		m[k] = string(v)
	}
	return buildContext(m, modes)
}

// phaseBuildKit tries normal rootless BuildKit, then the one explicit
// alternative (no process sandbox), each in a Pod that is deleted afterwards.
func (q *Qualifier) phaseBuildKit(ctx context.Context) error {
	if q.reg == nil || q.reg.client == nil {
		q.add("build.buildkit_selected", "BuildKit selected mode", true, NotRun, "registry fixture unavailable", nil, "")
		return nil
	}
	hdir, err := os.MkdirTemp(q.dir, "bk-helper-")
	if err != nil {
		return err
	}
	helper, err := buildHelper(hdir)
	if err != nil {
		q.add("build.buildkit_selected", "BuildKit selected mode", true, Fail, err.Error(), nil, "")
		return nil
	}
	bin, _ := os.ReadFile(helper)
	ctxTar, err := tarFiles(map[string][]byte{"helper": bin, "Dockerfile": []byte(containerfile)}, map[string]int64{"helper": 0o755})
	if err != nil {
		return err
	}
	f := q.reg
	env := []any{plainEnv("HOME", "/tmp/home"), plainEnv("XDG_RUNTIME_DIR", "/tmp/run"), plainEnv("SSL_CERT_FILE", "/ca/ca.crt"), plainEnv("DOCKER_CONFIG", "/creds")}
	vols := []any{emptyDir("bkstate", "1Gi", false)}
	mounts := []any{mount("bkstate", "/home/user/.local/share/buildkit", false)}
	build := func(flags string) string {
		return fmt.Sprintf(`set -e; mkdir -p /tmp/run /tmp/home; cd /work
export BUILDKITD_FLAGS=%q
buildctl-daemonless.sh build --frontend dockerfile.v0 --local context=/work --local dockerfile=/work --opt build-arg:DEPS_URL=%s --output type=image,name=%s/buildkit:latest,push=true`, flags, f.deps, f.host)
	}
	modes := []struct{ id, name, flags string }{
		{"build.buildkit_default", "rootless default (process sandbox)", ""},
		{"build.buildkit_selected", "rootless --oci-worker-no-process-sandbox", "--oci-worker-no-process-sandbox"},
	}
	var qualified string
	for mi, mode := range modes {
		var tried []string
		var ok *attempt
		var last attempt
		for i, v := range variants[:2] { // root-in-userns adds nothing for a non-root-image daemon
			a := q.attemptBuild(ctx, fmt.Sprintf("buildkit-%d-%d", mi, i), q.img("buildkit"), v, env, vols, mounts, "("+build(mode.flags)+")", ctxTar)
			tried = append(tried, fmt.Sprintf("%s: ok=%t blocked=%t %s", v.Name, a.OK, a.Blocked, firstLine(a.Output+a.StartErr)))
			last = a
			if a.OK {
				ok = &a
				break
			}
		}
		required := mode.id == "build.buildkit_selected"
		switch {
		case ok != nil:
			good, ev := q.verifyBuilt(ctx, "buildkit")
			st, det := Pass, mode.name+": real RUN build and authenticated push succeeded in Pod shape "+ok.Variant+"; run-exclusive service and whole-process-tree teardown were not separately verified"
			if !good {
				st, det = Fail, mode.name+": pushed image does not show the RUN step, dependency or multi-UID ownership"
			}
			q.add(mode.id, "BuildKit "+mode.name, required, st, det, append(tried, ev...), "")
			if required {
				q.rep.Find(mode.id).Mode = mode.name
			}
			if st == Pass && qualified == "" {
				qualified = mode.name
			}
		case last.Blocked:
			q.add(mode.id, "BuildKit "+mode.name, required, Blocked, mode.name+": rootlesskit cannot create its mount namespace under the node security profile",
				append(tried, lines(last.Diag, 8)...), buildahHandoff)
			if required {
				q.rep.Find(mode.id).Mode = mode.name
			}
		default:
			q.add(mode.id, "BuildKit "+mode.name, required, Fail, mode.name+": "+tail(last.Output+last.StartErr, 300), append(tried, lines(last.Diag, 8)...), "")
			if required {
				q.rep.Find(mode.id).Mode = mode.name
			}
		}
		if qualified != "" {
			break
		}
	}
	if c := q.rep.Find("build.buildkit_selected"); c == nil {
		if d := q.rep.Find("build.buildkit_default"); d != nil && d.Status == Pass {
			q.rep.Add(SelectedFromDefault(*d))
		}
	}
	return nil
}

// SelectedFromDefault projects a passing default-mode BuildKit check onto the
// selected-mode check. The selection is exactly the default mode; no
// alternative mode was exercised and none is claimed.
func SelectedFromDefault(def Check) Check {
	return Check{ID: "build.buildkit_selected", Capability: "BuildKit selected mode", Required: true, Status: def.Status,
		Mode:     "rootless default (process sandbox)",
		Detail:   "selected mode is the default rootless mode, tested by the default-mode check; no alternative mode was exercised and run-exclusive service or process-tree teardown was not verified",
		Evidence: def.Evidence}
}

// psaCheck asks the API server (dry run, admission included) whether the
// builder Pod shapes are admitted at the restricted and baseline Pod
// Security levels the tenant namespaces use. It creates nothing.
func (q *Qualifier) psaCheck(ctx context.Context) {
	ns := q.nsNetB
	v := variants[1]
	var ev []string
	violates := false
	for _, level := range []string{"restricted", "baseline"} {
		if _, err := q.c.K.Run(ctx, nil, "label", "namespace", ns, "pod-security.kubernetes.io/warn="+level, "--overwrite"); err != nil {
			q.add("psa.builder_pod", "builder Pod admitted under Pod Security", true, Fail, err.Error(), nil, "")
			return
		}
		p := q.builderPod("psa-probe", q.img("buildah"), v.UID)
		p.Namespace, p.Caps, p.AllowPE = ns, v.Caps, v.PE
		p.Command = []string{"sleep", "1"}
		out, err := q.c.ApplyDryRun(ctx, q.c.PodObject(p))
		if err != nil {
			q.add("psa.builder_pod", "builder Pod admitted under Pod Security", true, Fail, "dry run: "+err.Error(), nil, "")
			return
		}
		w := grepLines(out, "would violate PodSecurity")
		if len(w) > 0 {
			violates = true
		}
		ev = append(ev, fmt.Sprintf("%s: %d warning(s) %s", level, len(w), firstLine(strings.Join(w, "\n"))))
	}
	// The ko/postgres shape (no added capabilities) is the control.
	if _, err := q.c.K.Run(ctx, nil, "label", "namespace", ns, "pod-security.kubernetes.io/warn=restricted", "--overwrite"); err == nil {
		c := q.builderPod("psa-control", q.img("ko"), 65532)
		c.Namespace = ns
		c.Command = []string{"sleep", "1"}
		out, err := q.c.ApplyDryRun(ctx, q.c.PodObject(c))
		ev = append(ev, fmt.Sprintf("control (no added caps, restricted): err=%v warnings=%d", err != nil, len(grepLines(out, "would violate PodSecurity"))))
	}
	if violates {
		q.add("psa.builder_pod", "builder Pod admitted under Pod Security", true, Blocked,
			"the Pod shape that nested user namespaces require (SYS_ADMIN in the bounding set) violates Pod Security baseline/restricted; tenant namespaces on this target enforce restricted",
			ev, "T-114/T-116 installation prerequisite: builder and browser Pods need a namespace (or exemption) whose Pod Security level admits SYS_ADMIN-in-user-namespace Pods; keep all other tenant Pods restricted.")
		return
	}
	q.add("psa.builder_pod", "builder Pod admitted under Pod Security", true, Pass, "no Pod Security warnings at restricted or baseline", ev, "")
}
