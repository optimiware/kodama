package kube

import (
	"regexp"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
)

// profileNameRe bounds a Localhost seccomp or AppArmor profile name: a plain
// name, never a path, so a pinned role cannot select a file outside the
// installation's profile directory.
var profileNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// Role values the adapter applies. Anything else is refused: a policy this
// adapter cannot render exactly is never approximated.
const (
	roleUserNonRoot        = "nonroot"
	roleUserNamespacePod   = "pod"
	roleSeccompDefault     = "RuntimeDefault"
	roleProcMountUnmasked  = "unmasked"
	seccompLocalhostPrefix = "kodama/"
	seccompLocalhostSuffix = ".json"
)

// roleSecurity is the complete security shape of one policy role (seam S3):
// non-root with the installation's workload UID/GID, a Pod user namespace, every
// capability dropped and then only the role's additions, privilege escalation
// exactly when additions exist, the pinned seccomp and AppArmor profiles and an
// unmasked /proc only when the role asks for it. It sets no other field. The
// Pod-level part carries the non-root identity and the group that owns volumes.
type roleSecurity struct {
	hostUsers *bool
	pod       *kubernetes.PodSecurity
	container *kubernetes.ContainerSecurity
}

// roleSecurityOf renders the role under the installation's workload identity.
// It refuses every shape it cannot express: another user, a host user namespace,
// a device, a capability outside the mapping helpers or an unusable profile.
func roleSecurityOf(r execution.RoleShape, c PolicyConfig) (roleSecurity, error) {
	if r.User != roleUserNonRoot {
		return roleSecurity{}, wrap(ErrPolicy, "the role runs as an unsupported user")
	}
	if r.UserNamespace != roleUserNamespacePod {
		return roleSecurity{}, wrap(ErrPolicy, "the role does not run in a Pod user namespace")
	}
	if len(r.Devices) != 0 {
		return roleSecurity{}, wrap(ErrPolicy, "the role lists a device, which this milestone does not grant")
	}
	for _, capName := range r.CapabilitiesAdd {
		if capName != "SETUID" && capName != "SETGID" {
			return roleSecurity{}, wrap(ErrPolicy, "the role adds a capability beyond the mapping helpers")
		}
	}
	sec := &kubernetes.ContainerSecurity{
		Privileged: ptr(false), AllowPrivilegeEscalation: ptr(len(r.CapabilitiesAdd) > 0),
		RunAsNonRoot: ptr(true), RunAsUser: ptr(c.RunAsUser), RunAsGroup: ptr(c.RunAsGroup),
		Capabilities: &kubernetes.Capabilities{Drop: []string{"ALL"}},
	}
	if len(r.CapabilitiesAdd) > 0 {
		sec.Capabilities.Add = append([]string(nil), r.CapabilitiesAdd...)
	}
	switch {
	case r.Seccomp == "" || r.Seccomp == roleSeccompDefault:
		sec.SeccompProfile = &kubernetes.SeccompProfile{Type: "RuntimeDefault"}
	case profileNameRe.MatchString(r.Seccomp):
		sec.SeccompProfile = &kubernetes.SeccompProfile{Type: "Localhost", LocalhostProfile: seccompLocalhostPrefix + r.Seccomp + seccompLocalhostSuffix}
	default:
		return roleSecurity{}, wrap(ErrPolicy, "the role names an unusable seccomp profile")
	}
	switch {
	case r.LSM == "":
		sec.AppArmorProfile = &kubernetes.AppArmorProfile{Type: "RuntimeDefault"}
	case profileNameRe.MatchString(r.LSM):
		sec.AppArmorProfile = &kubernetes.AppArmorProfile{Type: "Localhost", LocalhostProfile: r.LSM}
	default:
		return roleSecurity{}, wrap(ErrPolicy, "the role names an unusable AppArmor profile")
	}
	switch r.ProcMount {
	case "":
	case roleProcMountUnmasked:
		sec.ProcMount = "Unmasked"
	default:
		return roleSecurity{}, wrap(ErrPolicy, "the role names an unusable proc mount")
	}
	return roleSecurity{
		hostUsers: ptr(false), container: sec,
		pod: &kubernetes.PodSecurity{RunAsNonRoot: ptr(true), RunAsUser: ptr(c.RunAsUser), RunAsGroup: ptr(c.RunAsGroup), FSGroup: ptr(c.RunAsGroup)},
	}, nil
}
