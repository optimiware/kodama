package runprofile

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"
)

// buildContext packs files into a tar stream for a builder Pod.
func buildContext(files map[string]string, modes map[string]int64) ([]byte, error) {
	var out bytes.Buffer
	tw := tar.NewWriter(&out)
	for name, body := range files {
		mode := int64(0o644)
		if m, ok := modes[name]; ok {
			mode = m
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(body)), Typeflag: tar.TypeReg, ModTime: time.Unix(1700000000, 0), Format: tar.FormatUSTAR}); err != nil {
			return nil, err
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

const koMain = `package main

import (
	"fmt"
	"os"

	"example.com/dep"
)

func main() { fmt.Println("kodama run-profile ko app", dep.Payload, os.Getuid()) }
`

// builderPod is a builder-role Pod (policy label role=builder) with the run
// CA and registry credential mounted read-only.
func (q *Qualifier) builderPod(name, image string, uid int64) Pod {
	p := q.base(name, image, uid)
	p.Labels = map[string]any{"krp/role": "builder"}
	p.Volumes = []any{secretVolume("ca", "registry-ca", 0o444), secretVolume("creds", "builder-docker-config", 0o444),
		emptyDir("tmp", "2Gi", false), emptyDir("work", "256Mi", false)}
	p.Mounts = []any{mount("ca", "/ca", true), mount("creds", "/creds", true), mount("tmp", "/tmp", false), mount("work", "/work", false)}
	p.CPU, p.MemLimit = "500m", "2Gi"
	p.DeadlineS = 1500
	return p
}

// phaseKo builds a Go application with ko and pushes it to the run registry.
func (q *Qualifier) phaseKo(ctx context.Context) error {
	if q.reg == nil || q.reg.client == nil {
		q.add("build.ko", "ko build and push", true, NotRun, "registry fixture unavailable", nil, "")
		return nil
	}
	ns, f := q.nsMain, q.reg
	// O-30: ko is an ordinary run_shared step and receives the same S3 step
	// role as every other step (no restricted-compatible exception).
	p := q.stepPod("ko", q.img("ko"), false, false)
	p.Env = []any{plainEnv("HOME", "/kodama/home"), plainEnv("GOCACHE", "/kodama/home/gocache"), plainEnv("GOPATH", "/kodama/home/gopath"),
		plainEnv("GOFLAGS", "-mod=mod"), plainEnv("GOPROXY", f.deps+"/goproxy"), plainEnv("GOSUMDB", "off"), plainEnv("GOTOOLCHAIN", "local"),
		plainEnv("CGO_ENABLED", "0"), plainEnv("SSL_CERT_FILE", "/ca/ca.crt"), plainEnv("DOCKER_CONFIG", "/creds"),
		plainEnv("KO_DOCKER_REPO", f.host+"/koapp"), plainEnv("KO_DEFAULTBASEIMAGE", f.host+"/base:empty")}
	p.Command = []string{"sleep", "1500"}
	if err := q.c.ApplyAll(ctx, q.c.PodObject(p)); err != nil {
		return err
	}
	defer func() {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		_ = q.c.DeletePod(dctx, ns, p.Name, time.Minute)
	}()
	if err := q.c.WaitReady(ctx, ns, p.Name, 10*minute); err != nil {
		st, h := Fail, ""
		if profileFailure(err.Error()) {
			st, h = Blocked, buildahHandoff
		}
		q.add("build.ko", "ko build and push", true, st, err.Error(), grepLines(q.podEvents(ctx, ns, p.Name), "Warning"), h)
		q.add("step.role_shape.ko", "ko step role applied exactly", true, NotRun, "the ko Pod never became ready", nil, "")
		return nil
	}
	if pj, err := q.c.GetPod(ctx, ns, p.Name); err == nil {
		q.noteImage(pj, p.Image)
	}
	if bad, rev := q.checkStepRole(ctx, ns, p.Name, false, ""); len(bad) > 0 {
		q.add("step.role_shape.ko", "ko step role applied exactly", true, Fail, "the ko Pod does not carry the S3 step role: "+strings.Join(bad, "; "), rev, "")
	} else {
		q.add("step.role_shape.ko", "ko step role applied exactly", true, Pass,
			"ko ran in the same S3 step role as Buildah: non-root 1000, hostUsers false, exactly SETGID/SETUID, escalation allowed, Localhost seccomp and AppArmor enforced; the live Pod object matches", rev, "")
	}
	src, err := buildContext(map[string]string{"go.mod": "module example.com/koapp\n\ngo 1.22\n\nrequire example.com/dep v1.0.0\n", "main.go": koMain}, nil)
	if err != nil {
		return err
	}
	if _, err := q.c.Exec(ctx, ns, p.Name, bytes.NewReader(src), "tar -C /work -xf -"); err != nil {
		return err
	}
	out, err := q.c.Exec(ctx, ns, p.Name, nil, `cd /work && ko version && ko build --bare --platform=linux/amd64 -t latest . 2>&1 | tail -20; echo "dep_cached=$(ls /kodama/home/gopath/pkg/mod/cache/download/example.com/dep/@v 2>&1 | tr '\n' ' ')"`)
	if err != nil {
		q.add("build.ko", "ko build and push", true, Fail, "ko failed: "+tail(out, 400), lines(out, 8), "")
		return nil
	}
	var ref string
	for _, l := range lines(out, 40) {
		if strings.Contains(l, "@sha256:") && strings.Contains(l, f.host) {
			ref = l
		}
	}
	client, err := q.freshClient(ctx)
	if err != nil {
		return err
	}
	man, digest, err := client.Manifest("koapp", "latest")
	if err != nil {
		q.add("build.ko", "ko build and push", true, Fail, "pushed image not found in registry: "+err.Error(), lines(out, 8), "")
		return nil
	}
	entries, err := client.LayerEntries("koapp", man)
	if err != nil {
		q.add("build.ko", "ko build and push", true, Fail, err.Error(), nil, "")
		return nil
	}
	found := false
	for _, e := range entries {
		if strings.HasSuffix(e.Name, "ko-app/koapp") && e.Size > 1<<20 && e.UID == 0 && e.Mode&0o111 != 0 {
			found = true
		}
	}
	status, detail := Pass, "ko built a Go application with a dependency fetched from the run's in-cluster module proxy and pushed it by digest to the authenticated TLS registry"
	switch {
	case !found:
		status, detail = Fail, "pushed image lacks the executable ko-app/koapp"
	case ref == "" || !strings.HasSuffix(ref, digest):
		status, detail = Fail, "ko's reported digest does not match the registry manifest digest"
	case !strings.Contains(out, "dep_cached=") || strings.Contains(out, "dep_cached= \n"):
		status, detail = Fail, "dependency was not fetched through the module proxy"
	}
	q.add("build.ko", "ko build and push", true, status, detail,
		append([]string{"ko ref " + ref, "registry digest " + digest, fmt.Sprintf("layer entries %d", len(entries))}, grepLines(out, "ko version", "dep_cached", "Version:")...), "")
	return nil
}
