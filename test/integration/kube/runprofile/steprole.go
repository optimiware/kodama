package runprofile

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// StepUID is the non-root UID/GID of the O-30 `step` role: the `build` user
// of the pinned Buildah image.
const StepUID = 1000

// PodRange is the size of the user-namespace ID range a `hostUsers: false`
// Pod receives (IDs 0..PodRange-1 inside the Pod).
const PodRange = 65536

// stepCapBnd is the container bounding set the role must end with: exactly
// SETGID (bit 6) and SETUID (bit 7).
const stepCapBnd = "00000000000000c0"

// stepOwners are the file owners the Buildah RUN step creates. They must all
// fit the subordinate range inside the Pod (container ID 0 is the build user
// itself; 1..PodRange-2 come from /etc/subuid and /etc/subgid).
var stepOwners = []struct {
	Name     string
	UID, GID int
}{{"root", 0, 0}, {"u1000", 1000, 1000}, {"u4242", 4242, 4343}, {"u60000", 60000, 60001}}

// fallbackSubID is the mapping used only when the pinned image's own
// /etc/subuid and /etc/subgid do not fit inside the Pod range: the build
// user's own ID (1000) is skipped and the rest of the 65,536 IDs are
// subordinate. It is mounted from a Secret, never baked into an image.
const fallbackSubID = "build:1:999\nbuild:1001:64535\n"

// StepRoleSpecViolations returns every way a live (or rendered) Pod differs
// from the S3 step role. An empty result means the role was applied exactly.
func StepRoleSpecViolations(pod map[string]any, unmasked bool) []string {
	var bad []string
	spec, _ := pod["spec"].(map[string]any)
	if spec == nil {
		return []string{"pod has no spec"}
	}
	if b, ok := spec["hostUsers"].(bool); !ok || b {
		bad = append(bad, "hostUsers is not false")
	}
	for _, f := range []string{"hostNetwork", "hostPID", "hostIPC"} {
		if b, _ := spec[f].(bool); b {
			bad = append(bad, f+" is set")
		}
	}
	vols, _ := spec["volumes"].([]any)
	for _, v := range vols {
		if vm, _ := v.(map[string]any); vm != nil {
			if _, ok := vm["hostPath"]; ok {
				bad = append(bad, "hostPath volume")
			}
		}
	}
	psc, _ := spec["securityContext"].(map[string]any)
	ctrs, _ := spec["containers"].([]any)
	if len(ctrs) != 1 {
		return append(bad, fmt.Sprintf("expected one container, got %d", len(ctrs)))
	}
	c, _ := ctrs[0].(map[string]any)
	csc, _ := c["securityContext"].(map[string]any)
	if csc == nil {
		return append(bad, "container has no securityContext")
	}
	for _, f := range []string{"runAsUser", "runAsGroup"} {
		if n, ok := asInt(effective(f, csc, psc)); !ok || n != StepUID {
			bad = append(bad, f+" is not 1000")
		}
	}
	if b, _ := effective("runAsNonRoot", csc, psc).(bool); !b {
		bad = append(bad, "runAsNonRoot is not true")
	}
	if !isLocalhostProfile("seccompProfile", effective("seccompProfile", csc, psc)) {
		bad = append(bad, "seccompProfile is not Localhost "+StepSeccompProfile)
	}
	if !isLocalhostProfile("appArmorProfile", effective("appArmorProfile", csc, psc)) {
		bad = append(bad, "appArmorProfile is not Localhost "+StepAppArmorProfile)
	}
	if b, _ := csc["privileged"].(bool); b {
		bad = append(bad, "container is privileged")
	}
	if b, ok := csc["allowPrivilegeEscalation"].(bool); !ok || !b {
		bad = append(bad, "allowPrivilegeEscalation is not true")
	}
	caps, _ := csc["capabilities"].(map[string]any)
	var drop, add []string
	for _, x := range anyList(caps["drop"]) {
		drop = append(drop, fmt.Sprint(x))
	}
	for _, x := range anyList(caps["add"]) {
		add = append(add, fmt.Sprint(x))
	}
	sort.Strings(add)
	if strings.Join(drop, ",") != "ALL" {
		bad = append(bad, "capabilities.drop is not [ALL]")
	}
	if strings.Join(add, ",") != "SETGID,SETUID" {
		bad = append(bad, fmt.Sprintf("capabilities.add is %v, want [SETGID SETUID]", add))
	}
	pm, _ := csc["procMount"].(string)
	if unmasked != (pm == "Unmasked") || (!unmasked && pm != "" && pm != "Default") {
		bad = append(bad, fmt.Sprintf("procMount is %q, want Unmasked=%t", pm, unmasked))
	}
	return bad
}

func anyList(v any) []any {
	l, _ := v.([]any)
	return l
}

// StepView is what a process inside the step Pod observes about itself.
type StepView struct {
	KV     map[string]string
	SubUID []string
	SubGID []string
}

// ParseStepView reads the key=value output of the inspection script.
func ParseStepView(out string) StepView {
	v := StepView{KV: map[string]string{}}
	for _, l := range lines(out, 200) {
		k, val, ok := strings.Cut(l, "=")
		if !ok {
			continue
		}
		switch k {
		case "subuid_line":
			v.SubUID = append(v.SubUID, val)
		case "subgid_line":
			v.SubGID = append(v.SubGID, val)
		default:
			v.KV[k] = val
		}
	}
	return v
}

// StepViewViolations compares the in-Pod view with the S3 role: the build
// user, the exact bounding set, privilege escalation permitted, the installed
// seccomp filter and AppArmor profile, and a user-namespace mapping of the
// Pod's own range.
// wantUser, when set, is the account name UID 1000 must resolve to (the
// Buildah image's `build` user); other images need only the numeric identity.
func StepViewViolations(v StepView, wantUser string) []string {
	var bad []string
	want := func(key, val string) {
		if v.KV[key] != val {
			bad = append(bad, fmt.Sprintf("%s=%q, want %q", key, v.KV[key], val))
		}
	}
	want("uid", "1000")
	want("gid", "1000")
	if wantUser != "" {
		want("user", wantUser)
	}
	want("cap_bnd", stepCapBnd)
	want("no_new_privs", "0")
	want("seccomp", "2")
	if !strings.HasPrefix(v.KV["apparmor"], StepAppArmorProfile+" (enforce)") {
		bad = append(bad, fmt.Sprintf("apparmor=%q, want %s (enforce)", v.KV["apparmor"], StepAppArmorProfile))
	}
	m := strings.Fields(v.KV["uid_map"])
	if len(m) != 3 || (m[0] == "0" && m[1] == "0") || m[2] != strconv.Itoa(PodRange) {
		bad = append(bad, fmt.Sprintf("uid_map=%q is not a %d-ID user-namespace range", v.KV["uid_map"], PodRange))
	}
	return bad
}

// SubIDFit verifies that the entries of an /etc/subuid or /etc/subgid file
// for user (name or numeric id) lie inside the Pod's ID range, never overlap
// the user's own ID, and cover at least need IDs in total.
func SubIDFit(entries []string, user string, uid, need int64) error {
	type rng struct{ start, count int64 }
	var rs []rng
	var total int64
	for _, e := range entries {
		f := strings.Split(strings.TrimSpace(e), ":")
		if len(f) != 3 || (f[0] != user && f[0] != strconv.FormatInt(uid, 10)) {
			continue
		}
		start, err1 := strconv.ParseInt(f[1], 10, 64)
		count, err2 := strconv.ParseInt(f[2], 10, 64)
		if err1 != nil || err2 != nil || start < 0 || count <= 0 {
			return fmt.Errorf("malformed entry %q", e)
		}
		if start+count > PodRange {
			return fmt.Errorf("entry %q ends at %d, beyond the Pod's %d IDs", e, start+count, PodRange)
		}
		if uid >= start && uid < start+count {
			return fmt.Errorf("entry %q overlaps the user's own ID %d", e, uid)
		}
		rs = append(rs, rng{start, count})
		total += count
	}
	if len(rs) == 0 {
		return fmt.Errorf("no entry for user %q", user)
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].start < rs[j].start })
	for i := 1; i < len(rs); i++ {
		if rs[i].start < rs[i-1].start+rs[i-1].count {
			return fmt.Errorf("entries for %q overlap", user)
		}
	}
	if total < need {
		return fmt.Errorf("entries for %q cover %d IDs, need at least %d", user, total, need)
	}
	return nil
}

// stepOwnersFit reports whether every helper owner is inside the usable
// container ID range (0 plus the subordinate IDs).
func stepOwnersFit(maxID int) error {
	for _, o := range stepOwners {
		if o.UID >= maxID || o.GID >= maxID || o.UID < 0 || o.GID < 0 {
			return fmt.Errorf("owner %s %d:%d outside 0..%d", o.Name, o.UID, o.GID, maxID-1)
		}
	}
	return nil
}
