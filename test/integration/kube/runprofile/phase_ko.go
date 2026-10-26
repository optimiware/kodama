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
	p := q.builderPod("ko", q.img("ko"), 65532)
	p.ReadOnlyFS = true
	p.Env = []any{plainEnv("HOME", "/tmp/home"), plainEnv("GOCACHE", "/tmp/gocache"), plainEnv("GOPATH", "/tmp/gopath"),
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
		q.add("build.ko", "ko build and push", true, Fail, err.Error(), grepLines(q.podEvents(ctx, ns, p.Name), "Warning"), "")
		return nil
	}
	if pj, err := q.c.GetPod(ctx, ns, p.Name); err == nil {
		q.noteImage(pj, p.Image)
	}
	src, err := buildContext(map[string]string{"go.mod": "module example.com/koapp\n\ngo 1.22\n\nrequire example.com/dep v1.0.0\n", "main.go": koMain}, nil)
	if err != nil {
		return err
	}
	if _, err := q.c.Exec(ctx, ns, p.Name, bytes.NewReader(src), "tar -C /work -xf -"); err != nil {
		return err
	}
	out, err := q.c.Exec(ctx, ns, p.Name, nil, `cd /work && ko version && ko build --bare --platform=linux/amd64 -t latest . 2>&1 | tail -20; echo "dep_cached=$(ls /tmp/gopath/pkg/mod/cache/download/example.com/dep/@v 2>&1 | tr '\n' ' ')"`)
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

	q.builderEgress(ctx, ns, p.Name)
	return nil
}

// builderEgress exercises the explicit builder allowances and the denials
// from inside a builder-role Pod, with in-run controls.
func (q *Qualifier) builderEgress(ctx context.Context, ns, pod string) {
	f := q.reg
	// Control: the denied in-run target is live for a client-role Pod.
	if err := q.ensureWWW(ctx); err != nil {
		q.add("network.builder_egress", "explicit registry/dependency egress", true, Fail, "live control target unavailable: "+err.Error(), nil, "")
		return
	}
	probe := func(host string, port int) string {
		return fmt.Sprintf(`if timeout 5 bash -c 'exec 3<>/dev/tcp/%s/%d' 2>/dev/null; then echo "open %s:%d"; else echo "closed %s:%d"; fi`, host, port, host, port, host, port)
	}
	script := strings.Join([]string{
		probe(strings.Split(f.host, ":")[0], 5000),
		probe("deps."+ns+".svc.cluster.local", 8080),
		probe("www."+ns+".svc.cluster.local", 8080),
		probe("kubernetes.default.svc.cluster.local", 443),
		probe("1.1.1.1", 443),
	}, "\n")
	out, err := q.c.Exec(ctx, ns, pod, nil, script)
	if err != nil {
		q.add("network.builder_egress", "explicit registry/dependency egress", true, Fail, err.Error(), nil, "")
		return
	}
	want := map[string]string{
		strings.Split(f.host, ":")[0] + ":5000":    "open",
		"deps." + ns + ".svc.cluster.local:8080":   "open",
		"www." + ns + ".svc.cluster.local:8080":    "closed",
		"kubernetes.default.svc.cluster.local:443": "closed",
		"1.1.1.1:443": "closed",
	}
	ok := true
	var ev []string
	for _, l := range lines(out, 10) {
		fields := strings.SplitN(l, " ", 2)
		if len(fields) == 2 {
			ev = append(ev, l)
			if w, found := want[fields[1]]; found && w != fields[0] {
				ok = false
			}
		}
	}
	if len(ev) != len(want) {
		ok = false
	}
	status, detail := Pass, "builder role reached only the registry and dependency server; the live www service, Kubernetes API and public address were denied"
	if !ok {
		status, detail = Fail, "builder egress did not match the explicit allowances"
	}
	q.add("network.builder_egress", "explicit registry/dependency egress", true, status, detail, ev, "")
}
