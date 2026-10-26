package runprofile

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// NetTarget is one destination of the traffic matrix.
type NetTarget struct {
	Name string
	URL  string
	Core bool // must be live in the control and denied afterwards
}

const probeFn = `probe() {
  out=$(wget -q -T 4 -O- "$2" 2>&1); rc=$?
  case "$out" in
    *"timed out"*|*"Connection refused"*|*"can't connect"*|*"bad address"*|*"Network is unreachable"*) echo "closed $1";;
    *) echo "open $1";;
  esac
}
`

// NetObservations are the measured open/closed results of the traffic matrix.
type NetObservations struct {
	Control, After               map[string]string // cli-same, before and after policy
	ControlOther, AfterOther     map[string]string // other-run client toward the reverse targets
	ControlForeign, AfterForeign map[string]string // foreign-namespace client toward the reverse targets
}

// EvaluateNetwork decides the network matrix verdict. A deny claim is made
// only for a destination whose positive control was observed open in the same
// run; a core destination without a live control fails, and a non-core one
// (Kubernetes API, public address) whose control is absent or closed leaves
// that claim unproven, so the check is blocked and the detail narrows what
// is claimed. Nothing passes by omission.
func EvaluateNetwork(targets, reverse []NetTarget, o NetObservations) (Status, string) {
	var failures, proven, unproven []string
	for _, t := range targets {
		c, af := o.Control[t.Name], o.After[t.Name]
		sameRun := strings.HasPrefix(t.Name, "srv-same")
		switch {
		case sameRun && c != "open":
			failures = append(failures, "control not live for "+t.Name)
		case sameRun && af != "open":
			failures = append(failures, "same-run traffic blocked: "+t.Name)
		case sameRun:
			proven = append(proven, "allow "+t.Name)
		case c != "open" && t.Core:
			failures = append(failures, "control not live for "+t.Name)
		case c != "open":
			unproven = append(unproven, fmt.Sprintf("deny %s (no live positive control: %q)", t.Name, c))
		case af != "closed":
			failures = append(failures, "not denied: "+t.Name)
		default:
			proven = append(proven, "deny "+t.Name)
		}
	}
	for _, t := range reverse {
		if o.ControlOther[t.Name] != "open" || o.ControlForeign[t.Name] != "open" {
			failures = append(failures, "reverse control not live for "+t.Name)
			continue
		}
		if o.AfterOther[t.Name] != "closed" || o.AfterForeign[t.Name] != "closed" {
			failures = append(failures, "foreign client reached "+t.Name)
			continue
		}
		proven = append(proven, "deny other-run and foreign-namespace clients to "+t.Name)
	}
	switch {
	case len(failures) > 0:
		return Fail, strings.Join(failures, "; ")
	case len(unproven) > 0:
		return Blocked, "default-deny claims not fully proven. Proven with live controls: " + strings.Join(proven, ", ") + ". Unproven: " + strings.Join(unproven, "; ")
	}
	return Pass, "with live positive controls, default deny plus the run-scoped edge admitted only same-run traffic (pod IP and Service DNAT) and denied other runs, other namespaces, the Kubernetes API and public addresses; proven: " + strings.Join(proven, ", ")
}

// phaseNetwork qualifies the outer CNI: with known-live positive controls
// in the same run, a default-deny policy plus run-scoped allows lets only
// same-run traffic through, including via Service DNAT, and denies other
// runs of the same tenant, other namespaces, the Kubernetes API and public
// addresses.
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
	client := func(ns, name, run string, caps []string) Pod {
		p := q.base(name, q.img("httpd"), 10001)
		p.Namespace = ns
		p.Labels = map[string]any{"krp/role": "client", "krp/run": run}
		p.Command = []string{"sleep", "1200"}
		p.Caps = caps
		return p
	}
	pods := []Pod{
		server(a, "srv-same", "r1"), server(a, "srv-other", "r2"), server(b, "srv-foreign", "r1"),
		client(a, "cli-same", "r1", nil), client(a, "cli-other", "r2", nil), client(b, "cli-foreign", "r1", nil),
		client(a, "cli-nested", "r1", []string{"SYS_ADMIN"}),
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
		if err := q.c.WaitReady(ctx, p.Namespace, p.Name, 8*minute); err != nil && !strings.HasPrefix(p.Name, "cli-") {
			return err
		} else if err != nil {
			if err := Wait(ctx, 5*minute, 3*time.Second, func() (bool, error) {
				pj, e := q.c.GetPod(ctx, p.Namespace, p.Name)
				if e != nil {
					return false, e
				}
				return pj.Status.Phase == "Running", fmt.Errorf("phase %s", pj.Status.Phase)
			}); err != nil {
				return err
			}
		}
	}
	ip := func(ns, name string) string {
		out, _ := q.c.K.Run(ctx, nil, "-n", ns, "get", "pod", name, "-o", "jsonpath={.status.podIP}")
		return strings.TrimSpace(out)
	}
	ipSame, ipOther, ipForeign := ip(a, "srv-same"), ip(a, "srv-other"), ip(b, "srv-foreign")
	targets := []NetTarget{
		{"srv-same(pod ip)", "http://" + ipSame + ":8080/", true},
		{"srv-same(service)", "http://srv-same." + a + ".svc.cluster.local:8080/", true},
		{"srv-other-run(pod ip)", "http://" + ipOther + ":8080/", true},
		{"srv-other-run(service)", "http://srv-other." + a + ".svc.cluster.local:8080/", true},
		{"srv-foreign-ns(pod ip)", "http://" + ipForeign + ":8080/", true},
		{"srv-foreign-ns(service)", "http://srv-foreign." + b + ".svc.cluster.local:8080/", true},
		{"kubernetes-api", "http://kubernetes.default.svc.cluster.local:443/", false},
		{"public-1.1.1.1", "http://1.1.1.1:443/", false},
	}
	run := func(ns, pod string, tg []NetTarget) map[string]string {
		var sb strings.Builder
		sb.WriteString(probeFn)
		for _, t := range tg {
			fmt.Fprintf(&sb, "probe %q %q\n", t.Name, t.URL)
		}
		out, err := q.c.Exec(ctx, ns, pod, nil, sb.String())
		res := map[string]string{}
		if err != nil {
			res["error"] = err.Error()
		}
		for _, l := range lines(out, 40) {
			if st, name, ok := strings.Cut(l, " "); ok && (st == "open" || st == "closed") {
				res[name] = st
			}
		}
		return res
	}
	reverse := []NetTarget{
		{"srv-same(pod ip)", "http://" + ipSame + ":8080/", true},
		{"srv-same(service)", "http://srv-same." + a + ".svc.cluster.local:8080/", true},
	}
	// 1. Positive controls with no policy anywhere.
	ctl := run(a, "cli-same", targets)
	ctlOther, ctlForeign := run(a, "cli-other", reverse), run(b, "cli-foreign", reverse)

	// 2. Run-scoped policy in namespace A: default deny, DNS, same-run edge.
	sel := func(kv map[string]any) map[string]any { return map[string]any{"matchLabels": kv} }
	r1 := map[string]any{"krp/run": "r1"}
	pol := func(name string, spec map[string]any) map[string]any {
		return q.c.obj("networking.k8s.io/v1", "NetworkPolicy", a, name, nil, spec)
	}
	port := []any{map[string]any{"protocol": "TCP", "port": 8080}}
	if err := q.c.ApplyAll(ctx,
		pol("default-deny", map[string]any{"podSelector": map[string]any{}, "policyTypes": []any{"Ingress", "Egress"}}),
		pol("dns-egress", map[string]any{"podSelector": map[string]any{}, "policyTypes": []any{"Egress"}, "egress": []any{map[string]any{
			"to":    []any{map[string]any{"namespaceSelector": sel(map[string]any{"kubernetes.io/metadata.name": "kube-system"}), "podSelector": sel(map[string]any{"k8s-app": "kube-dns"})}},
			"ports": []any{map[string]any{"protocol": "UDP", "port": 53}, map[string]any{"protocol": "TCP", "port": 53}}}}}),
		pol("same-run-egress", map[string]any{"podSelector": sel(r1), "policyTypes": []any{"Egress"},
			"egress": []any{map[string]any{"to": []any{map[string]any{"podSelector": sel(r1)}}, "ports": port}}}),
		pol("same-run-ingress", map[string]any{"podSelector": sel(r1), "policyTypes": []any{"Ingress"},
			"ingress": []any{map[string]any{"from": []any{map[string]any{"podSelector": sel(r1)}}, "ports": port}}}),
	); err != nil {
		return err
	}
	// Enforcement is programmed asynchronously; poll until the first denial
	// shows or the bound expires, then take the authoritative measurement.
	_ = Wait(ctx, 90*time.Second, 5*time.Second, func() (bool, error) {
		r := run(a, "cli-same", []NetTarget{targets[2]})
		return r[targets[2].Name] == "closed", nil
	})
	after := run(a, "cli-same", targets)
	afterOther, afterForeign := run(a, "cli-other", reverse), run(b, "cli-foreign", reverse)

	ev := []string{}
	for _, t := range targets {
		ev = append(ev, fmt.Sprintf("cli-same -> %s: control=%s policy=%s", t.Name, ctl[t.Name], after[t.Name]))
	}
	for _, t := range reverse {
		ev = append(ev, fmt.Sprintf("cli-other-run -> %s: control=%s policy=%s", t.Name, ctlOther[t.Name], afterOther[t.Name]),
			fmt.Sprintf("cli-foreign-ns -> %s: control=%s policy=%s", t.Name, ctlForeign[t.Name], afterForeign[t.Name]))
	}
	status, detail := EvaluateNetwork(targets, reverse, NetObservations{Control: ctl, After: after,
		ControlOther: ctlOther, AfterOther: afterOther, ControlForeign: ctlForeign, AfterForeign: afterForeign})
	q.add("network.matrix", "outer CNI same-run / foreign / default deny", true, status, detail, ev, "")

	// 3. Nested network namespace under the same outer policy.
	nested := q.execOrEmpty(ctx, a, "cli-nested", probeFn+`
echo "outer_same_run=$(probe same http://`+ipSame+`:8080/)"
echo "outer_other_run=$(probe other http://`+ipOther+`:8080/)"
unshare -Urn sh -c 'echo "nested_ifaces=$(tail -n +3 /proc/net/dev | cut -d: -f1 | tr -d " " | tr "\n" ",")"; if wget -q -T 3 -O- http://`+ipSame+`:8080/ >/dev/null 2>&1; then echo nested_same_run=open; else echo nested_same_run=closed; fi' 2>&1`)
	nst, nd := NotRun, "nested namespace probe produced no result: "+tail(nested, 160)
	if strings.Contains(nested, "outer_same_run=open same") && strings.Contains(nested, "outer_other_run=closed other") {
		switch {
		case strings.Contains(nested, "nested_same_run=closed") && strings.Contains(nested, "nested_ifaces=lo,"):
			nst, nd = Pass, "a nested user+network namespace inside the policed Pod has only loopback and cannot reach the run peer, while the outer Pod keeps exactly its run-scoped access; NAT'd nested networking (slirp4netns/pasta/CNI-in-Pod) is not qualified: the profile exposes no /dev/net/tun"
		default:
			nst, nd = Fail, "nested namespace behaved unexpectedly: "+tail(nested, 200)
		}
	}
	q.add("network.nested", "nested network namespace under outer default deny", false, nst, nd, lines(nested, 8), "")
	return nil
}
