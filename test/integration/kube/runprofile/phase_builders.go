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

const helperTemplate = `package main

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
	}{{{OWNERS}}} {
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

// helperSource renders the helper with the owners of stepOwners, all of which
// fit the subordinate range inside the Pod.
func helperSource() string {
	var owners []string
	for _, o := range stepOwners {
		owners = append(owners, fmt.Sprintf("{%q, %d, %d}", o.Name, o.UID, o.GID))
	}
	return strings.Replace(helperTemplate, "{{OWNERS}}", strings.Join(owners, ", "), 1)
}

// buildHelper compiles the static helper the RUN step executes. It needs the
// Go toolchain of the host running the qualification and no network.
func buildHelper(dir string) (string, error) {
	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(src, "main.go"), []byte(helperSource()), 0o644); err != nil {
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

type attempt struct {
	Variant   string
	OK        bool
	Output    string
	Diag      string
	Blocked   bool     // failure attributable to the node security profile
	StartErr  string   // the Pod never started
	SpecBad   []string // differences between the live Pod and the S3 role
	Inspected bool     // the live Pod object was read back
	Digest    string   // digest the builder reports it pushed
}

const diagScript = `
echo "diag_apparmor=$(cat /proc/self/attr/current 2>&1)"
echo "diag_restrict_userns=$(cat /proc/sys/kernel/apparmor_restrict_unprivileged_userns 2>&1)"
echo "diag_unshare_user=$(unshare -U true 2>&1 && echo ok)"
echo "diag_unshare_user_mount=$(unshare -Urm true 2>&1 && echo ok)"
echo "diag_caps=$(grep -E '^Cap(Prm|Bnd):' /proc/self/status | tr -s '\t ' ' ' | tr '\n' ' ')"
`

// inspectScript prints the in-Pod view of the step role and the subordinate
// ID files of the pinned image.
const inspectScript = `
echo "uid=$(id -u)"
echo "gid=$(id -g)"
echo "user=$(id -un 2>&1)"
echo "cap_bnd=$(grep '^CapBnd:' /proc/self/status | tr -s '\t ' ' ' | cut -d' ' -f2)"
echo "no_new_privs=$(grep '^NoNewPrivs:' /proc/self/status | tr -s '\t ' ' ' | cut -d' ' -f2)"
echo "seccomp=$(grep '^Seccomp:' /proc/self/status | tr -s '\t ' ' ' | cut -d' ' -f2)"
echo "apparmor=$(cat /proc/self/attr/current 2>&1)"
echo "uid_map=$(tr -s ' ' < /proc/self/uid_map)"
echo "newuidmap=$(ls -l /usr/bin/newuidmap 2>&1)"
sed 's/^/subuid_line=/' /etc/subuid 2>/dev/null || true
sed 's/^/subgid_line=/' /etc/subgid 2>/dev/null || true
`

// stepPod is a builder-role Pod in the exact S3 step role with HOME-based
// builder storage. subid mounts the fallback subordinate-ID files.
func (q *Qualifier) stepPod(name, image string, unmasked, subid bool) Pod {
	p := q.builderPod(name, image, StepUID)
	p.StepRole, p.Unmasked = true, unmasked
	p.Env = []any{plainEnv("HOME", "/kodama/home"), plainEnv("XDG_RUNTIME_DIR", "/kodama/run"), plainEnv("TMPDIR", "/tmp"),
		plainEnv("SSL_CERT_FILE", "/ca/ca.crt"), plainEnv("DOCKER_CONFIG", "/creds"), plainEnv("BUILDAH_ISOLATION", "chroot")}
	p.Volumes = append(p.Volumes, emptyDir("home", "2Gi", false), emptyDir("run", "64Mi", false))
	p.Mounts = append(p.Mounts, mount("home", "/kodama/home", false), mount("run", "/kodama/run", false))
	if subid {
		p.Volumes = append(p.Volumes, secretVolume("subid", "buildah-subid", 0o444))
		p.Mounts = append(p.Mounts, mountSub("subid", "/etc/subuid", "subuid"), mountSub("subid", "/etc/subgid", "subgid"))
	}
	return p
}

// attemptBuild runs one build+push attempt in a freshly created step Pod and
// records the live Pod's conformance to the role.
func (q *Qualifier) attemptBuild(ctx context.Context, p Pod, variant, buildScript string, ctxTar []byte) attempt {
	ns := q.nsMain
	a := attempt{Variant: variant}
	p.Command = []string{"sleep", "900"}
	if err := q.c.ApplyAll(ctx, q.c.PodObject(p)); err != nil {
		a.StartErr = err.Error()
		return a
	}
	defer func() {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
		defer cancel()
		_ = q.c.DeletePod(dctx, ns, p.Name, 2*time.Minute)
	}()
	if err := q.c.WaitReady(ctx, ns, p.Name, 10*minute); err != nil {
		a.StartErr = err.Error()
		a.Blocked = profileFailure(a.StartErr)
		return a
	}
	if pj, err := q.c.GetPod(ctx, ns, p.Name); err == nil {
		q.noteImage(pj, p.Image)
	}
	var live map[string]any
	a.Inspected = true
	if err := q.c.K.JSON(ctx, &live, "-n", ns, "get", "pod", p.Name); err == nil {
		a.SpecBad = StepRoleSpecViolations(live, p.Unmasked)
	} else {
		a.SpecBad = []string{"live Pod unreadable: " + err.Error()}
	}
	if _, err := q.c.Exec(ctx, ns, p.Name, bytes.NewReader(ctxTar), "mkdir -p /work && tar -C /work -xf -"); err != nil {
		a.StartErr = err.Error()
		return a
	}
	bctx, cancel := context.WithTimeout(ctx, 8*time.Minute)
	defer cancel()
	out, err := q.c.Exec(bctx, ns, p.Name, nil, buildScript+" 2>&1; echo build_exit=$?")
	a.Output = out
	a.OK = err == nil && strings.Contains(out, "build_exit=0")
	for _, l := range lines(out, 400) {
		if d, ok := strings.CutPrefix(l, "pushed_digest="); ok {
			a.Digest = strings.TrimSpace(d)
		}
	}
	if !a.OK {
		a.Diag = q.execOrEmpty(ctx, ns, p.Name, diagScript)
		a.Blocked = strings.Contains(a.Diag, "diag_unshare_user_mount=unshare:") &&
			(strings.Contains(a.Diag, "Permission denied") || strings.Contains(a.Diag, "Operation not permitted"))
	}
	return a
}

// verifyBuilt checks the pushed image's contents: the RUN step executed, the
// dependency was fetched, files carry several distinct numeric owners, and the
// registry's manifest digest is the one the builder pushed.
func (q *Qualifier) verifyBuilt(ctx context.Context, repo, pushedDigest string) (bool, []string) {
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
	ownersOK := true
	for _, o := range stepOwners {
		if owners[o.Name] != [2]int{o.UID, o.GID} {
			ownersOK = false
		}
	}
	match := pushedDigest != "" && pushedDigest == digest
	ok := dep && ownersOK && match
	return ok, []string{"registry manifest " + digest, "builder pushed digest " + pushedDigest, fmt.Sprintf("digest match %t", match),
		fmt.Sprintf("owners %v", owners), fmt.Sprintf("dependency marker %t", dep)}
}

const buildahHandoff = "T-120/T-121 installation prerequisite: the O-30 step role needs the installed node profiles. Install the seccomp profile kodama/kodama-rootless-build-v1.json under the kubelet seccomp root and load the AppArmor profile kodama-rootless-build-v2 (flags=(unconfined) plus userns, listed as \"kodama-rootless-build-v2 (unconfined)\") on every worker that runs tenant steps (T-120 installer), then re-run this qualification. Do not use the generic Unconfined profile type, SYS_ADMIN or privileged Pods."

// buildahScript builds the fixture with Buildah's documented rootless storage
// (VFS under HOME, chroot isolation), pushes with the run credential and
// reports the digest it pushed.
func buildahScript(depsURL, registryHost string) string {
	return fmt.Sprintf(`set -e; mkdir -p "$HOME/containers" /work; cd /work
S="--root $HOME/containers/root --runroot $HOME/containers/run --storage-driver=vfs"
buildah $S version | head -2
buildah $S build --isolation=chroot --layers=false --build-arg DEPS_URL=%s -f Containerfile -t localhost/probe:1 .
buildah $S push --authfile /creds/config.json --digestfile "$HOME/digest" localhost/probe:1 docker://%s/buildah:latest
echo "pushed_digest=$(cat "$HOME/digest")"`, depsURL, registryHost)
}

// phaseBuildah qualifies Buildah in the exact S3 step role and nothing wider.
func (q *Qualifier) phaseBuildah(ctx context.Context) error {
	if q.reg == nil || q.reg.client == nil {
		q.add("build.buildah_vfs", "Buildah VFS build and push", true, NotRun, "registry fixture unavailable", nil, "")
		q.add("step.role_shape", "step role applied exactly", true, NotRun, "registry fixture unavailable", nil, "")
		q.add("psa.builder_pod", "step role admitted under Pod Security", true, NotRun, "registry fixture unavailable", nil, "")
		return nil
	}
	q.add("build.buildah_overlay_fuse", "Buildah overlay/FUSE", false, NotAdvertised,
		"not advertised: the step role exposes no devices (no /dev/fuse) and the driver never requests one", nil, "")
	q.psaCheck(ctx)

	hdir, err := os.MkdirTemp(q.dir, "helper-")
	if err != nil {
		return err
	}
	helper, err := buildHelper(hdir)
	if err != nil {
		q.add("build.buildah_vfs", "Buildah VFS build and push", true, Fail, err.Error(), nil, "")
		q.add("step.role_shape", "step role applied exactly", true, NotRun, "no builder ran", nil, "")
		return nil
	}
	bin, _ := os.ReadFile(helper)
	ctxTar, err := tarFiles(map[string][]byte{"helper": bin, "Containerfile": []byte(containerfile)}, map[string]int64{"helper": 0o755})
	if err != nil {
		return err
	}
	f := q.reg
	img := q.img("buildah")

	// Inspect the pinned image's user and subordinate ID files under the role.
	view, perr := q.inspectStep(ctx, img)
	if perr != nil {
		st, h := Fail, ""
		if profileFailure(perr.Error()) {
			st, h = Blocked, buildahHandoff
		}
		q.add("step.role_shape", "step role applied exactly", true, st, "the step-role Pod could not run its inspection: "+tail(perr.Error(), 300), nil, h)
		q.add("build.buildah_vfs", "Buildah VFS build and push", true, NotRun, "step-role Pod did not start; no build was attempted", nil, "")
		return nil
	}
	viewBad := StepViewViolations(view, "build")
	ev := []string{"in-Pod view: " + fmt.Sprint(view.KV), fmt.Sprintf("subuid %v subgid %v", view.SubUID, view.SubGID)}
	subid := false
	for _, k := range []struct {
		name    string
		entries []string
	}{{"/etc/subuid", view.SubUID}, {"/etc/subgid", view.SubGID}} {
		if err := SubIDFit(k.entries, "build", StepUID, 60000); err != nil {
			subid = true
			ev = append(ev, k.name+" of the pinned image does not fit the Pod range ("+err.Error()+"): the build mounts a fallback")
		}
	}
	if subid {
		if err := q.c.ApplyAll(ctx, q.secret(q.nsMain, "buildah-subid", map[string]string{"subuid": fallbackSubID, "subgid": fallbackSubID})); err != nil {
			return err
		}
	} else {
		ev = append(ev, "the pinned image's own /etc/subuid and /etc/subgid fit inside the Pod range")
	}
	if len(viewBad) > 0 {
		q.add("step.role_shape", "step role applied exactly", true, Fail, "the process inside the Pod does not see the S3 role: "+strings.Join(viewBad, "; "), ev, "")
		q.add("build.buildah_vfs", "Buildah VFS build and push", true, NotRun, "the role was not applied as specified; no build evidence would describe it", nil, "")
		return nil
	}

	script := "(" + buildahScript(f.deps, f.host) + ")"
	variants := []struct {
		name     string
		unmasked bool
	}{{"step", false}, {"step+procMount-unmasked", true}}
	var tried []string
	var results []attempt
	var specBad []string
	var goodDefault, goodUnmasked bool
	var detailEv []string
	for i, v := range variants {
		if i > 0 && (results[0].OK || (results[0].Blocked && results[0].StartErr != "")) {
			break // the optional variant only runs when the frozen role's build failed to start a nested namespace, never when its Pod could not start
		}
		a := q.attemptBuild(ctx, q.stepPod(fmt.Sprintf("buildah-%d", i), img, v.unmasked, subid), v.name, script, ctxTar)
		tried = append(tried, fmt.Sprintf("%s: ok=%t blocked=%t %s", v.name, a.OK, a.Blocked, firstLine(a.Output+a.StartErr)))
		results = append(results, a)
		specBad = append(specBad, a.SpecBad...)
		if a.OK {
			good, bev := q.verifyBuilt(ctx, "buildah", a.Digest)
			detailEv = append(detailEv, bev...)
			if good && !v.unmasked {
				goodDefault = true
			}
			if good && v.unmasked {
				goodUnmasked = true
			}
			if !good {
				tried = append(tried, v.name+": pushed image failed verification")
			}
		}
	}
	// The role check combines the in-Pod view with the live Pod objects.
	if len(specBad) > 0 {
		q.add("step.role_shape", "step role applied exactly", true, Fail, "the live Pod differs from the S3 role: "+strings.Join(specBad, "; "), ev, "")
	} else if !results[0].Inspected {
		st, h := Fail, ""
		if results[0].Blocked {
			st, h = Blocked, buildahHandoff
		}
		q.add("step.role_shape", "step role applied exactly", true, st, "the step-role build Pod never started, so its live object could not be compared: "+tail(results[0].StartErr, 300), ev, h)
	} else {
		q.add("step.role_shape", "step role applied exactly", true, Pass,
			"non-root 1000, hostUsers false, capabilities [ALL] dropped and exactly SETGID/SETUID in the bounding set, allowPrivilegeEscalation true, Localhost seccomp profile enforced, the Localhost AppArmor profile kodama-rootless-build-v2 loaded unconfined with userns, a 65,536-ID user-namespace mapping; the live Pod object matches", ev, "")
	}

	last := results[len(results)-1]
	allEv := append(append(tried, detailEv...), lines(last.Diag, 8)...)
	switch {
	case goodDefault:
		q.add("build.buildah_vfs", "Buildah VFS build and push", true, Pass,
			"Buildah (VFS under HOME, chroot isolation) ran a RUN step in the S3 step role that fetched the dependency over run egress, created files with several numeric owners inside the Pod range, and pushed to the authenticated registry with the manifest digest it reported", allEv, "")
	case goodUnmasked:
		q.add("build.buildah_vfs", "Buildah VFS build and push", true, Blocked,
			"the frozen step role failed but the same role with procMount: Unmasked built and pushed: the role needs `procMount: unmasked` to qualify, which the S3 default does not set",
			allEv, "Policy decision: the default step role did not build; only the optional procMount: Unmasked variant did. Record the policy's step role with procMount: unmasked (seam S3 supports it) or fix the node profile, then re-run. Nothing is qualified for the default role.")
		q.add("build.buildah_procmount_unmasked", "Buildah with procMount: Unmasked", false, Pass, "built and pushed in the optional variant", nil, "")
	case results[0].OK && !goodUnmasked:
		q.add("build.buildah_vfs", "Buildah VFS build and push", true, Fail,
			"Buildah finished but the pushed image does not show the RUN step, the dependency, the multi-UID ownership or a manifest digest equal to the pushed one", allEv, "")
	case anyBlocked(results):
		q.add("build.buildah_vfs", "Buildah VFS build and push", true, Blocked,
			"the node security profile denies the step role (profile not installed/loaded, or a nested user-namespace mount is denied)", allEv, buildahHandoff)
	default:
		q.add("build.buildah_vfs", "Buildah VFS build and push", true, Fail, "Buildah failed without a node-profile denial: "+tail(last.Output+last.StartErr, 300), allEv, "")
	}
	return nil
}

func anyBlocked(rs []attempt) bool {
	for _, r := range rs {
		if r.Blocked {
			return true
		}
	}
	return false
}

// checkStepRole compares a running step-role Pod with seam S3 twice: the live
// Pod object the API server holds and what the process inside observes.
// wantUser, when set, is the account UID 1000 must resolve to.
func (q *Qualifier) checkStepRole(ctx context.Context, ns, name string, unmasked bool, wantUser string) (bad []string, evidence []string) {
	var live map[string]any
	if err := q.c.K.JSON(ctx, &live, "-n", ns, "get", "pod", name); err != nil {
		return []string{"live Pod unreadable: " + err.Error()}, nil
	}
	bad = StepRoleSpecViolations(live, unmasked)
	view := ParseStepView(q.execOrEmpty(ctx, ns, name, inspectScript))
	for _, v := range StepViewViolations(view, wantUser) {
		bad = append(bad, "in-Pod "+v)
	}
	evidence = []string{"in-Pod view: " + fmt.Sprint(view.KV)}
	return bad, evidence
}

// inspectStep runs the inspection script in a one-shot step-role Pod.
func (q *Qualifier) inspectStep(ctx context.Context, image string) (StepView, error) {
	p := q.stepPod("buildah-inspect", image, false, false)
	out, code, err := q.oneShot(ctx, q.nsMain, p, inspectScript, 10*minute)
	if err != nil {
		return StepView{}, err
	}
	if code != 0 {
		return StepView{}, fmt.Errorf("inspection exited %d: %s", code, tail(out, 200))
	}
	return ParseStepView(out), nil
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

// psaCheck asks the API server (dry run, admission included) whether the step
// role Pod is admitted at the Pod Security levels. The role adds only
// SETUID/SETGID with Localhost profiles and allows privilege escalation, so
// baseline is its floor; restricted is recorded as evidence. It creates
// nothing.
func (q *Qualifier) psaCheck(ctx context.Context) {
	ns := q.nsNetB
	var ev []string
	warnings := map[string]int{}
	for _, level := range []string{"restricted", "baseline"} {
		if _, err := q.c.K.Run(ctx, nil, "label", "namespace", ns, "pod-security.kubernetes.io/warn="+level, "--overwrite"); err != nil {
			q.add("psa.builder_pod", "step role admitted under Pod Security", true, Fail, err.Error(), nil, "")
			return
		}
		p := q.stepPod("psa-probe", q.img("buildah"), false, false)
		p.Namespace = ns
		p.Command = []string{"sleep", "1"}
		out, err := q.c.ApplyDryRun(ctx, q.c.PodObject(p))
		if err != nil {
			q.add("psa.builder_pod", "step role admitted under Pod Security", true, Fail, "dry run: "+err.Error(), nil, "")
			return
		}
		w := grepLines(out, "would violate PodSecurity")
		warnings[level] = len(w)
		ev = append(ev, fmt.Sprintf("%s: %d warning(s) %s", level, len(w), firstLine(strings.Join(w, "\n"))))
	}
	// The ko/postgres shape (no added capabilities) is the control.
	if _, err := q.c.K.Run(ctx, nil, "label", "namespace", ns, "pod-security.kubernetes.io/warn=restricted", "--overwrite"); err == nil {
		c := q.builderPod("psa-control", q.img("ko"), 65532)
		c.Namespace = ns
		c.Command = []string{"sleep", "1"}
		out, err := q.c.ApplyDryRun(ctx, q.c.PodObject(c))
		ev = append(ev, fmt.Sprintf("control (restricted-compatible ko shape, restricted): err=%v warnings=%d", err != nil, len(grepLines(out, "would violate PodSecurity"))))
	}
	if warnings["baseline"] > 0 {
		q.add("psa.builder_pod", "step role admitted under Pod Security", true, Blocked,
			"the S3 step role violates Pod Security baseline; the tenant namespace could not admit run_shared step Pods at any standard level below privileged",
			ev, "T-116/T-120 installation prerequisite: tenant namespaces for run_shared steps must enforce a Pod Security level that admits the step role (baseline admits SETUID/SETGID and Localhost profiles); keep attempt_private namespaces restricted.")
		return
	}
	detail := "the step role produces no Pod Security warning at baseline"
	if warnings["restricted"] > 0 {
		detail += fmt.Sprintf(" but %d at restricted: tenant namespaces that run run_shared steps must enforce baseline (the stage tenant chart renders restricted today)", warnings["restricted"])
	}
	q.add("psa.builder_pod", "step role admitted under Pod Security", true, Pass, detail, ev, "")
}
