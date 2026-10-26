package runprofile

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

const inventoryScript = `
echo "kernel=$(uname -r)"
echo "uid_map=$(tr -s ' ' < /proc/self/uid_map)"
echo "gid_map=$(tr -s ' ' < /proc/self/gid_map)"
echo "cgroup_version=$([ -f /sys/fs/cgroup/cgroup.controllers ] && echo v2 || echo v1-or-hidden)"
echo "cgroup_controllers=$(cat /sys/fs/cgroup/cgroup.controllers 2>&1)"
echo "seccomp=$(grep '^Seccomp:' /proc/self/status | tr -s '\t ' ' ')"
echo "no_new_privs=$(grep '^NoNewPrivs:' /proc/self/status | tr -s '\t ' ' ')"
echo "apparmor_profile=$(cat /proc/self/attr/current 2>&1)"
echo "apparmor_restrict_unprivileged_userns=$(cat /proc/sys/kernel/apparmor_restrict_unprivileged_userns 2>&1)"
echo "max_user_namespaces=$(cat /proc/sys/user/max_user_namespaces 2>&1)"
echo "lsm=$(cat /sys/kernel/security/lsm 2>&1)"
echo "dev_fuse=$(ls /dev/fuse 2>&1 | head -1)"
echo "dev_net_tun=$(ls /dev/net/tun 2>&1 | head -1)"
`

// phaseInventory records the exact platform the qualification ran on.
func (q *Qualifier) phaseInventory(ctx context.Context) error {
	inv := q.rep.Inventory
	var ver struct {
		ClientVersion struct {
			GitVersion string `json:"gitVersion"`
		} `json:"clientVersion"`
		ServerVersion struct {
			GitVersion string `json:"gitVersion"`
			Platform   string `json:"platform"`
		} `json:"serverVersion"`
	}
	if err := q.c.K.JSON(ctx, &ver, "version"); err != nil {
		return err
	}
	inv["kubernetes"] = map[string]string{"server": ver.ServerVersion.GitVersion, "platform": ver.ServerVersion.Platform, "kubectl": ver.ClientVersion.GitVersion}

	var nodes struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Status struct {
				NodeInfo map[string]any `json:"nodeInfo"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := q.c.K.JSON(ctx, &nodes, "get", "nodes"); err != nil {
		return err
	}
	var nodeInv []map[string]string
	for _, n := range nodes.Items {
		ni := n.Status.NodeInfo
		str := func(k string) string { v, _ := ni[k].(string); return v }
		nodeInv = append(nodeInv, map[string]string{
			"name": n.Metadata.Name, "kernel": str("kernelVersion"), "os": str("osImage"),
			"cri": str("containerRuntimeVersion"), "kubelet": str("kubeletVersion"), "arch": str("architecture"),
		})
	}
	inv["nodes"] = nodeInv
	if len(nodes.Items) > 0 {
		var cfg struct {
			KubeletConfig map[string]any `json:"kubeletconfig"`
		}
		if out, err := q.c.K.Run(ctx, nil, "get", "--raw", "/api/v1/nodes/"+nodes.Items[0].Metadata.Name+"/proxy/configz"); err == nil {
			if json.Unmarshal([]byte(out), &cfg) == nil {
				kc := map[string]any{}
				for _, k := range []string{"cgroupDriver", "seccompDefault", "featureGates", "containerRuntimeEndpoint", "maxPods"} {
					kc[k] = cfg.KubeletConfig[k]
				}
				inv["kubeletConfig"] = kc
			}
		}
	}

	// CNI and CSI component images (read-only).
	for _, ds := range []struct{ key, name string }{{"cni", "cilium"}, {"csi", "csi-node"}} {
		out, err := q.c.K.Run(ctx, nil, "-n", "kube-system", "get", "ds", ds.name, "-o", "jsonpath={range .spec.template.spec.containers[*]}{.image}{\"\\n\"}{end}")
		if err != nil {
			inv[ds.key] = "unavailable: " + tail(err.Error(), 120)
			continue
		}
		inv[ds.key] = lines(out, 8)
	}
	if out, err := q.c.K.Run(ctx, nil, "-n", "kube-system", "get", "cm", "cilium-config", "-o", "jsonpath={.data.enable-policy} {.data.routing-mode} {.data.kube-proxy-replacement}"); err == nil {
		inv["ciliumPolicyMode/routing/kubeProxyReplacement"] = strings.TrimSpace(out)
	}
	var sc struct {
		Provisioner       string            `json:"provisioner"`
		ReclaimPolicy     string            `json:"reclaimPolicy"`
		VolumeBindingMode string            `json:"volumeBindingMode"`
		Parameters        map[string]string `json:"parameters"`
	}
	if err := q.c.K.JSON(ctx, &sc, "get", "storageclass", q.cfg.StorageClass); err != nil {
		return fmt.Errorf("storage class %q: %w", q.cfg.StorageClass, err)
	}
	inv["storageClass"] = map[string]any{"name": q.cfg.StorageClass, "provisioner": sc.Provisioner, "reclaimPolicy": sc.ReclaimPolicy, "bindingMode": sc.VolumeBindingMode}

	// In-pod view: user mapping, cgroup, LSM, seccomp, devices.
	p := q.base("inventory", q.img("httpd"), 10001)
	out, code, err := q.oneShot(ctx, q.nsMain, p, inventoryScript, 6*minute)
	if err != nil || code != 0 {
		return fmt.Errorf("inventory probe pod failed (exit %d): %v", code, err)
	}
	pod := map[string]string{}
	for _, l := range lines(out, 40) {
		if k, v, ok := strings.Cut(l, "="); ok {
			pod[k] = v
		}
	}
	inv["podView"] = pod
	status := Pass
	detail := fmt.Sprintf("kubernetes %s, %d nodes, cri %s, kernel %s", ver.ServerVersion.GitVersion, len(nodeInv), nodeInv[0]["cri"], nodeInv[0]["kernel"])
	if pod["uid_map"] == "" || strings.HasPrefix(pod["uid_map"], "0 0 ") {
		status = Fail
		detail += "; probe Pod did not receive a user-namespace mapping"
	}
	q.add("inventory", "platform inventory", true, status, detail, lines(out, 14), "")
	return nil
}
