package runprofile

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// NetKind is the verdict rule of one probed destination.
type NetKind int

const (
	// AllowTenant must be open before and after policy: the tenant model lets
	// workloads of one tenant namespace reach each other whatever their run.
	AllowTenant NetKind = iota
	// DenyCore must be open in the control and closed afterwards; a missing
	// control fails the check (the target is a fixture the run owns).
	DenyCore
	// DenyProbe is a deny claim about a destination the run does not own
	// (Kubernetes API, cloud metadata). It needs an open live control; without
	// one the claim is unproven and the check is blocked, never passed.
	DenyProbe
	// AllowPublic must be open before and after policy (public HTTPS).
	AllowPublic
)

// Network claims the tenant model makes; every one must be tested.
const (
	ClaimSameTenant    = "same-tenant other-run reachable"
	ClaimForeignTenant = "foreign tenant denied"
	ClaimAPI           = "kubernetes api denied"
	ClaimMetadata      = "cloud metadata denied"
	ClaimPublic        = "public https open"
)

var requiredNetClaims = []string{ClaimSameTenant, ClaimForeignTenant, ClaimAPI, ClaimMetadata, ClaimPublic}

// Probe modes. http is a plain TCP+HTTP request (busybox wget), used for the
// allow claims between the run's own servers; tls is a complete HTTPS exchange
// with certificate verification (curl), which is what the public-HTTPS claim
// needs; tcp is connect-only (see ProbeTCP).
const (
	ProbeHTTP = "http"
	ProbeTLS  = "tls"
	// ProbeTCP is a connect-only probe (busybox `nc -z`): it reports whether
	// the TCP connection was established and exchanges nothing afterwards, so a
	// timeout can only be a connection-establishment timeout. Every deny claim
	// uses it; an accepted connection followed by a stalled response is `open`.
	ProbeTCP = "tcp"
)

// NetTarget is one destination of the traffic matrix, probed from Client.
type NetTarget struct {
	Name   string
	Client string
	URL    string
	Kind   NetKind
	Claim  string
	Mode   string // ProbeHTTP (default) or ProbeTLS
}

// probeFn runs one probe and reports its exit status with its output, so the
// driver classifies on both (an output pattern alone, or an ignored exit
// status, once turned unknown failures into "open"). Fields are tab separated.
const probeFn = `probe() {
  name=$1; mode=$2; url=$3
  if [ "$mode" = tcp ]; then
    hp=${url#*://}; hp=${hp%%/*}; host=${hp%:*}; port=${hp##*:}
    if command -v nc >/dev/null 2>&1; then
      out=$(nc -z -v -w 4 "$host" "$port" 2>&1); rc=$?
    else
      out="nc is not installed"; rc=127
    fi
  elif [ "$mode" = tls ]; then
    if command -v curl >/dev/null 2>&1; then
      out=$(curl -sS --connect-timeout 5 --max-time 10 -o /dev/null -w 'http=%{http_code} verify=%{ssl_verify_result}' "$url" 2>&1); rc=$?
    else
      out="curl is not installed"; rc=127
    fi
  else
    out=$(wget -q -T 4 -O /dev/null "$url" 2>&1); rc=$?
  fi
  out=$(printf '%s' "$out" | tr '\n\t' '  ' | cut -c1-240)
  printf 'probe\t%s\t%s\t%s\n' "$name" "$rc" "$out"
}
`

// Probe states.
const (
	StateOpen   = "open"
	StateClosed = "closed"
	StateError  = "error"
)

// ClassifyProbe turns one probe's exit status and output into open, closed or
// error. Only an outcome that proves the destination answered is open, only an
// outcome that proves the connection was refused, unroutable or dropped is
// closed, and everything else (DNS failure, TLS failure, missing tool, an
// unrecognised message) is an error, never open.
func ClassifyProbe(mode string, rc int, out string) (state, reason string) {
	o := strings.ToLower(out)
	has := func(subs ...string) bool {
		for _, s := range subs {
			if strings.Contains(o, s) {
				return true
			}
		}
		return false
	}
	if rc == 127 || has("not installed", ": not found", "command not found") {
		return StateError, "probe tool unavailable: " + tail(out, 100)
	}
	if mode == ProbeTCP {
		// nc -z performs no I/O after connect(): rc 0 means the connection
		// was accepted (whatever the peer then does), a timeout is a
		// connection-establishment timeout, a refusal or unroutable network
		// prints nothing or its own message, and anything else is undecided.
		switch {
		case rc == 0:
			return StateOpen, "TCP connection established: " + tail(out, 100)
		case has("bad address"):
			return StateError, "DNS failure: " + tail(out, 100)
		case has("operation timed out", "connection timed out"):
			return StateClosed, "connection establishment timed out: " + tail(out, 100)
		case has("connection refused", "no route to host", "network is unreachable"):
			return StateClosed, "connection refused or unroutable: " + tail(out, 100)
		case rc == 1 && strings.TrimSpace(out) == "":
			return StateClosed, "connection refused or unroutable (nc reports no message)"
		}
		return StateError, fmt.Sprintf("unrecognised nc result (exit %d): %s", rc, tail(out, 100))
	}
	if mode == ProbeTLS {
		switch rc {
		case 0:
			if has("verify=0") && !has("http=000") {
				return StateOpen, "TLS handshake verified and HTTP answered: " + tail(out, 100)
			}
			return StateError, "TLS exchange succeeded without a verified certificate or response: " + tail(out, 100)
		case 7:
			return StateClosed, "connect failed: " + tail(out, 100)
		case 28:
			if has("connection timed out", "connection time-out", "connect timed out") {
				return StateClosed, "connect timed out: " + tail(out, 100)
			}
			return StateError, "operation timed out after connecting: " + tail(out, 100)
		case 6:
			return StateError, "DNS failure: " + tail(out, 100)
		case 35, 51, 53, 54, 58, 59, 60, 77, 80, 83, 90, 91:
			return StateError, "TLS failure: " + tail(out, 100)
		}
		return StateError, fmt.Sprintf("unrecognised curl exit %d: %s", rc, tail(out, 100))
	}
	if rc == 0 {
		return StateOpen, "HTTP exchange succeeded"
	}
	switch {
	case has("bad address"):
		return StateError, "DNS failure: " + tail(out, 100)
	case has("server returned error", "http/1."):
		return StateOpen, "peer answered at HTTP level: " + tail(out, 100)
	case has("connection refused", "network is unreachable", "no route to host"):
		return StateClosed, "connection refused or unroutable: " + tail(out, 100)
	}
	// BusyBox "download timed out" (and any other timeout) can occur after the
	// connection was accepted, while waiting for the response: it cannot
	// distinguish a drop from a stalled peer, so it is undecided, never closed.
	return StateError, fmt.Sprintf("unrecognised failure (exit %d): %s", rc, tail(out, 100))
}

// parseProbeLines reads the tab-separated probe results of one client.
func parseProbeLines(out string, modes map[string]string) (states, details map[string]string) {
	states, details = map[string]string{}, map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		f := strings.Split(strings.TrimRight(l, "\r"), "\t")
		if len(f) < 3 || f[0] != "probe" {
			continue
		}
		rc, err := strconv.Atoi(strings.TrimSpace(f[2]))
		text := ""
		if len(f) > 3 {
			text = f[3]
		}
		if err != nil {
			states[f[1]], details[f[1]] = StateError, "unparseable exit status "+f[2]
			continue
		}
		states[f[1]], details[f[1]] = ClassifyProbe(modes[f[1]], rc, text)
	}
	return states, details
}

// NetObservations are the measured open/closed results of the traffic matrix,
// keyed by target name, before (Control) and after (After) the tenant policy.
type NetObservations struct {
	Control, After map[string]string
	// Detail explains each classification (evidence only).
	Detail map[string]string
}

// EvaluateNetwork decides the tenant-model verdict. A deny claim is made
// only for a destination whose positive control was observed open in the same
// run: a core destination without a live control fails; a probe destination
// (Kubernetes API, cloud metadata) whose control is absent or closed leaves
// that claim unproven, so the check is blocked and the detail narrows what is
// claimed. An allow claim needs the destination open after the policy. Every
// claim of the tenant model must be present among the targets, and an
// execution error or unknown observation never passes.
func EvaluateNetwork(targets []NetTarget, o NetObservations) (Status, string) {
	var failures, proven, unproven []string
	have := map[string]bool{}
	for _, m := range []map[string]string{o.Control, o.After} {
		for k, v := range m {
			if strings.HasPrefix(k, "error") {
				failures = append(failures, "probe execution error "+k+": "+v)
			}
		}
	}
	for _, t := range targets {
		have[t.Claim] = true
		c, af := o.Control[t.Name], o.After[t.Name]
		if c == StateError || af == StateError {
			// A probe that could not decide is never open, never closed and
			// never an unproven-but-tolerated claim: the check fails.
			failures = append(failures, fmt.Sprintf("probe error for %s (control=%s: %s; policy=%s: %s)", t.Name, c, o.Detail["control:"+t.Name], af, o.Detail["after:"+t.Name]))
			continue
		}
		if (t.Kind == DenyCore || t.Kind == DenyProbe) && t.Mode != ProbeTCP {
			failures = append(failures, "a deny claim needs a connect-only TCP probe, not mode "+t.Mode+": "+t.Name)
			continue
		}
		if t.Kind == AllowPublic && t.Mode != ProbeTLS {
			failures = append(failures, "public HTTPS claim needs a verified TLS exchange, not mode "+t.Mode+": "+t.Name)
			continue
		}
		switch t.Kind {
		case AllowTenant, AllowPublic:
			switch {
			case c != "open":
				failures = append(failures, "control not live for "+t.Name)
			case af != "open":
				failures = append(failures, "allowed traffic blocked: "+t.Name)
			default:
				proven = append(proven, "allow "+t.Name)
			}
		case DenyCore:
			switch {
			case c != "open":
				failures = append(failures, "control not live for "+t.Name)
			case af != "closed":
				failures = append(failures, "not denied: "+t.Name)
			default:
				proven = append(proven, "deny "+t.Name)
			}
		case DenyProbe:
			switch {
			case c != "open":
				unproven = append(unproven, fmt.Sprintf("deny %s (no live positive control: %q)", t.Name, c))
			case af != "closed":
				failures = append(failures, "not denied: "+t.Name)
			default:
				proven = append(proven, "deny "+t.Name)
			}
		default:
			failures = append(failures, "unknown rule for "+t.Name)
		}
	}
	for _, claim := range requiredNetClaims {
		if !have[claim] {
			failures = append(failures, "claim not tested: "+claim)
		}
	}
	switch {
	case len(failures) > 0:
		return Fail, strings.Join(failures, "; ")
	case len(unproven) > 0:
		return Blocked, "tenant-model claims not fully proven. Proven with live controls: " + strings.Join(proven, ", ") + ". Unproven: " + strings.Join(unproven, "; ")
	}
	return Pass, "with live positive controls, the tenant model held: workloads of the tenant namespace reached each other's services whatever their run label (pod IP and Service DNAT), a foreign tenant namespace, the Kubernetes API and cloud metadata were denied, and public HTTPS stayed open; proven: " + strings.Join(proven, ", ")
}

// phaseNetwork qualifies the tenant network model (seam S8) on the outer CNI:
// with known-live positive controls in the same run, the tenant policy lets
// a client of the same tenant namespace but another run label reach the
// service (pod IP and Service DNAT), denies a foreign tenant namespace, the
// Kubernetes API and cloud metadata, and leaves public HTTPS open.
func (q *Qualifier) phaseNetwork(ctx context.Context) error {
	a, b := q.nsNetA, q.nsNetB
	server := func(ns, name, run string) Pod {
		p := q.base(name, q.img("httpd"), 10001)
		p.Namespace = ns
		p.Labels = map[string]any{"krp/svc": name, "krp/run": run}
		p.Command = []string{"sh", "-ec", `mkdir -p /tmp/www; echo ` + name + ` > /tmp/www/index.html; ` + serveScript("/tmp/www")}
		p.Volumes = []any{emptyDir("tmp", "8Mi", false)}
		p.Mounts = []any{mount("tmp", "/tmp", false)}
		p.Readiness = map[string]any{"tcpSocket": map[string]any{"port": 8080}, "periodSeconds": 3}
		return p
	}
	client := func(ns, name, run string) Pod {
		p := q.base(name, q.img("httpd"), 10001)
		p.Namespace = ns
		p.Labels = map[string]any{"krp/role": "client", "krp/run": run}
		p.Command = []string{"sleep", "1200"}
		return p
	}
	// The public-HTTPS claim needs a real TLS exchange with certificate
	// verification: the ko image (a full Go toolchain image) carries curl and
	// the system CA bundle; a missing tool is a probe error, never "open".
	tlsClient := client(a, "cli-tls", "r1")
	tlsClient.Image, tlsClient.UID, tlsClient.GID = q.img("ko"), 10001, 10001
	pods := []Pod{
		server(a, "srv-same", "r1"), server(a, "srv-other", "r2"), server(b, "srv-foreign", "r1"),
		client(a, "cli-same", "r1"), client(a, "cli-other", "r2"), client(b, "cli-foreign", "r1"), tlsClient,
	}
	svc := func(ns, name string) map[string]any {
		return q.c.obj("v1", "Service", ns, name, nil, map[string]any{"type": "ClusterIP", "selector": map[string]any{"krp/svc": name},
			"ports": []any{map[string]any{"port": 8080, "targetPort": 8080, "protocol": "TCP"}}})
	}
	for _, p := range pods {
		if err := q.c.ApplyAll(ctx, q.c.PodObject(p)); err != nil {
			return err
		}
	}
	if err := q.c.ApplyAll(ctx, svc(a, "srv-same"), svc(a, "srv-other"), svc(b, "srv-foreign")); err != nil {
		return err
	}
	for _, p := range pods {
		if strings.HasPrefix(p.Name, "cli-") {
			// Clients have no readiness probe: running is enough.
			if err := Wait(ctx, 8*minute, 3*time.Second, func() (bool, error) {
				pj, e := q.c.GetPod(ctx, p.Namespace, p.Name)
				if e != nil {
					return false, e
				}
				return pj.Status.Phase == "Running", fmt.Errorf("phase %s", pj.Status.Phase)
			}); err != nil {
				return err
			}
			continue
		}
		if err := q.c.WaitReady(ctx, p.Namespace, p.Name, 8*minute); err != nil {
			return err
		}
	}
	ip := func(ns, name string) string {
		out, _ := q.c.K.Run(ctx, nil, "-n", ns, "get", "pod", name, "-o", "jsonpath={.status.podIP}")
		return strings.TrimSpace(out)
	}
	ipSame, ipOther, ipForeign := ip(a, "srv-same"), ip(a, "srv-other"), ip(b, "srv-foreign")
	podURL := func(ipa string) string { return "http://" + ipa + ":8080/" }
	svcURL := func(ns, name string) string { return "http://" + name + "." + ns + ".svc.cluster.local:8080/" }
	targets := []NetTarget{
		{"same-run srv (pod ip)", "cli-same", podURL(ipSame), AllowTenant, ClaimSameTenant, ProbeHTTP},
		{"same-run srv (service)", "cli-same", svcURL(a, "srv-same"), AllowTenant, ClaimSameTenant, ProbeHTTP},
		{"other-run srv (pod ip)", "cli-same", podURL(ipOther), AllowTenant, ClaimSameTenant, ProbeHTTP},
		{"other-run srv (service)", "cli-same", svcURL(a, "srv-other"), AllowTenant, ClaimSameTenant, ProbeHTTP},
		{"other-run client -> same-run srv (pod ip)", "cli-other", podURL(ipSame), AllowTenant, ClaimSameTenant, ProbeHTTP},
		{"other-run client -> same-run srv (service)", "cli-other", svcURL(a, "srv-same"), AllowTenant, ClaimSameTenant, ProbeHTTP},
		{"foreign-tenant srv (pod ip)", "cli-same", podURL(ipForeign), DenyCore, ClaimForeignTenant, ProbeTCP},
		{"foreign-tenant srv (service)", "cli-same", svcURL(b, "srv-foreign"), DenyCore, ClaimForeignTenant, ProbeTCP},
		{"foreign-tenant client -> srv (pod ip)", "cli-foreign", podURL(ipSame), DenyCore, ClaimForeignTenant, ProbeTCP},
		{"foreign-tenant client -> srv (service)", "cli-foreign", svcURL(a, "srv-same"), DenyCore, ClaimForeignTenant, ProbeTCP},
		{"kubernetes-api", "cli-same", "http://kubernetes.default.svc.cluster.local:443/", DenyProbe, ClaimAPI, ProbeTCP},
		{"cloud-metadata 169.254.169.254", "cli-same", "http://169.254.169.254:80/", DenyProbe, ClaimMetadata, ProbeTCP},
		{"public-https 1.1.1.1:443 (verified TLS)", "cli-tls", "https://1.1.1.1/", AllowPublic, ClaimPublic, ProbeTLS},
	}
	clientNS := map[string]string{"cli-same": a, "cli-other": a, "cli-foreign": b, "cli-tls": a}
	measure := func(only func(NetTarget) bool) (res, detail map[string]string) {
		res, detail = map[string]string{}, map[string]string{}
		byClient := map[string][]NetTarget{}
		for _, t := range targets {
			if only == nil || only(t) {
				byClient[t.Client] = append(byClient[t.Client], t)
			}
		}
		for cli, tg := range byClient {
			var sb strings.Builder
			sb.WriteString(probeFn)
			modes := map[string]string{}
			for _, t := range tg {
				mode := t.Mode
				if mode == "" {
					mode = ProbeHTTP
				}
				modes[t.Name] = mode
				fmt.Fprintf(&sb, "probe %q %q %q\n", t.Name, mode, t.URL)
			}
			out, err := q.c.Exec(ctx, clientNS[cli], cli, nil, sb.String())
			if err != nil {
				res["error:"+cli] = tail(err.Error(), 200)
			}
			st, dt := parseProbeLines(out, modes)
			for _, t := range tg {
				if s, ok := st[t.Name]; ok {
					res[t.Name], detail[t.Name] = s, dt[t.Name]
				} else {
					res[t.Name], detail[t.Name] = StateError, "no result line from the probe"
				}
			}
		}
		return res, detail
	}
	// 1. Positive controls with no policy anywhere.
	ctl, ctlDetail := measure(nil)

	// 2. The tenant model in namespace A (namespace B stays open: the foreign
	// tenant's own rules are not under test, and its pods are controls).
	if err := q.c.ApplyAll(ctx, q.tenantPolicies(a, []int{8080})...); err != nil {
		return err
	}
	// Enforcement is programmed asynchronously; poll until the first denial
	// shows or the bound expires, then take the authoritative measurement.
	_ = Wait(ctx, 90*time.Second, 5*time.Second, func() (bool, error) {
		r, _ := measure(func(t NetTarget) bool { return t.Name == "foreign-tenant srv (pod ip)" })
		return r["foreign-tenant srv (pod ip)"] == StateClosed, nil
	})
	after, afterDetail := measure(nil)
	obsDetail := map[string]string{}
	for k, v := range ctlDetail {
		obsDetail["control:"+k] = v
	}
	for k, v := range afterDetail {
		obsDetail["after:"+k] = v
	}

	var ev []string
	for _, t := range targets {
		ev = append(ev, fmt.Sprintf("%s -> %s: control=%s (%s) policy=%s (%s)", t.Client, t.Name, ctl[t.Name], ctlDetail[t.Name], after[t.Name], afterDetail[t.Name]))
	}
	for _, m := range []map[string]string{ctl, after} {
		for k, v := range m {
			if strings.HasPrefix(k, "error") {
				ev = append(ev, k+": "+v)
			}
		}
	}
	status, detail := EvaluateNetwork(targets, NetObservations{Control: ctl, After: after, Detail: obsDetail})
	q.add("network.matrix", "tenant network model with live controls", true, status, detail, ev, "")
	return nil
}
