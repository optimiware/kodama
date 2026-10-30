package kernelrt

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/optimiware/kodama/internal/kernel/runtime"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
)

func unverified(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrUnverified, fmt.Sprintf(format, args...))
}

// Adoption verifies the whole security-relevant document the server returned,
// not a typed subset: every field of the submitted shape is checked against the
// permitted value, every field the adapter never writes is refused, and only a
// short list of API-server defaults is tolerated. A new field in a newer API
// version is therefore refused until it is reviewed and allowed here.

type doc = map[string]any

// rule checks one field's value; path names it in the error.
type rule func(path string, v any) error

// walk checks every present, non-null key of the object against its rule. A key
// without a rule is an unexpected field.
func walk(path string, v any, rules map[string]rule) error {
	m, ok := v.(map[string]any)
	if !ok {
		return unverified("%s is not an object", path)
	}
	for k, x := range m {
		if x == nil {
			continue
		}
		r, ok := rules[k]
		if !ok {
			return unverified("unexpected field %s.%s", path, k)
		}
		if err := r(path+"."+k, x); err != nil {
			return err
		}
	}
	return nil
}

// require fails unless every key is present and non-null.
func require(path string, v any, keys ...string) error {
	m, _ := v.(map[string]any)
	for _, k := range keys {
		if m[k] == nil {
			return unverified("%s.%s is required", path, k)
		}
	}
	return nil
}

func isStr(want string) rule {
	return func(p string, v any) error {
		if s, ok := v.(string); !ok || s != want {
			return unverified("%s is %v, want %q", p, v, want)
		}
		return nil
	}
}

func isBool(want bool) rule {
	return func(p string, v any) error {
		if b, ok := v.(bool); !ok || b != want {
			return unverified("%s is %v, want %v", p, v, want)
		}
		return nil
	}
}

func isInt(want int64) rule {
	return func(p string, v any) error {
		if n, ok := v.(float64); !ok || int64(n) != want {
			return unverified("%s is %v, want %d", p, v, want)
		}
		return nil
	}
}

func anyNumber(p string, v any) error {
	if _, ok := v.(float64); !ok {
		return unverified("%s is not a number", p)
	}
	return nil
}

func anyString(p string, v any) error {
	if _, ok := v.(string); !ok {
		return unverified("%s is not a string", p)
	}
	return nil
}

// anyDoc accepts an object whose content is not security relevant (metadata and
// status; ownership is checked from the typed metadata).
func anyDoc(p string, v any) error {
	if _, ok := v.(map[string]any); !ok {
		return unverified("%s is not an object", p)
	}
	return nil
}

func stringMap(p string, v any) error {
	m, ok := v.(map[string]any)
	if !ok {
		return unverified("%s is not an object", p)
	}
	for k, x := range m {
		if _, ok := x.(string); !ok {
			return unverified("%s.%s is not a string", p, k)
		}
	}
	return nil
}

func emptyList(p string, v any) error {
	if l, ok := v.([]any); !ok || len(l) != 0 {
		return unverified("%s must be empty", p)
	}
	return nil
}

func listOf(p string, v any) ([]any, error) {
	l, ok := v.([]any)
	if !ok {
		return nil, unverified("%s is not a list", p)
	}
	return l, nil
}

func strList(p string, v any) ([]string, error) {
	l, err := listOf(p, v)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(l))
	for i, x := range l {
		s, ok := x.(string)
		if !ok {
			return nil, unverified("%s[%d] is not a string", p, i)
		}
		out[i] = s
	}
	return out, nil
}

func eachItem(item func(p string, v any) error) rule {
	return func(p string, v any) error {
		l, err := listOf(p, v)
		if err != nil {
			return err
		}
		for i, x := range l {
			if err := item(p+"["+strconv.Itoa(i)+"]", x); err != nil {
				return err
			}
		}
		return nil
	}
}

func decodeDoc(raw json.RawMessage, what string) (doc, error) {
	if len(raw) == 0 {
		return nil, unverified("the %s has no server document to verify", what)
	}
	var d doc
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, unverified("the %s document is not valid", what)
	}
	return d, nil
}

// quantityValue converts the common Kubernetes quantity forms to a comparable
// number, so a server-canonicalized quantity still matches the configured one.
func quantityValue(s string) (float64, bool) {
	suffixes := []struct {
		s string
		m float64
	}{{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40}, {"k", 1e3}, {"M", 1e6}, {"G", 1e9}, {"T", 1e12}, {"m", 1e-3}}
	mult := 1.0
	for _, x := range suffixes {
		if strings.HasSuffix(s, x.s) {
			s, mult = strings.TrimSuffix(s, x.s), x.m
			break
		}
	}
	f, err := strconv.ParseFloat(s, 64)
	return f * mult, err == nil
}

func sameQuantity(a, b string) bool {
	x, ok1 := quantityValue(a)
	y, ok2 := quantityValue(b)
	return ok1 && ok2 && x == y
}

func quantities(want map[string]string) rule {
	return func(p string, v any) error {
		m, ok := v.(map[string]any)
		if !ok || len(m) != len(want) {
			return unverified("%s does not bound exactly cpu, memory and ephemeral-storage", p)
		}
		for k, w := range want {
			got, _ := m[k].(string)
			if !sameQuantity(got, w) {
				return unverified("%s.%s is %q, want %q", p, k, got, w)
			}
		}
		return nil
	}
}

func (c *core) resourcesRule(b Bounds) rule {
	r := b.resources()
	return func(p string, v any) error {
		if err := walk(p, v, map[string]rule{"requests": quantities(r.Requests), "limits": quantities(r.Limits)}); err != nil {
			return err
		}
		return require(p, v, "requests", "limits")
	}
}

func (c *core) containerSecurityRule() rule {
	return func(p string, v any) error {
		err := walk(p, v, map[string]rule{
			"privileged": isBool(false), "allowPrivilegeEscalation": isBool(false), "readOnlyRootFilesystem": isBool(false),
			"runAsNonRoot": isBool(true), "runAsUser": isInt(c.cfg.RunAsUser), "runAsGroup": isInt(c.cfg.RunAsGroup),
			"capabilities": func(p string, v any) error {
				if err := walk(p, v, map[string]rule{"drop": func(p string, v any) error {
					l, err := strList(p, v)
					if err != nil || !slices.Equal(l, []string{"ALL"}) {
						return unverified("%s must drop ALL", p)
					}
					return nil
				}}); err != nil {
					return err
				}
				return require(p, v, "drop")
			},
			"seccompProfile":  profile,
			"appArmorProfile": profile,
		})
		if err != nil {
			return err
		}
		return require(p, v, "privileged", "allowPrivilegeEscalation", "runAsNonRoot", "runAsUser", "runAsGroup", "capabilities", "seccompProfile")
	}
}

// profile accepts only the runtime default profile: neither Unconfined nor a
// Localhost profile is part of this adapter's shape.
func profile(p string, v any) error {
	return walk(p, v, map[string]rule{"type": isStr("RuntimeDefault")})
}

// podShape is what one Pod specification may be.
type podShape struct {
	c         *core
	t         Target
	live      bool // a scheduled Pod, not a submitted template
	container string
	bounds    Bounds
	claim     string // workspace claim; empty for a service
	material  string // the step secrets Secret name; empty without secrets
}

func (s podShape) verify(path string, spec any) error {
	c := s.c
	rules := map[string]rule{
		"serviceAccountName":            isStr(s.t.ServiceAccount),
		"serviceAccount":                isStr(s.t.ServiceAccount),
		"automountServiceAccountToken":  isBool(false),
		"enableServiceLinks":            isBool(false),
		"restartPolicy":                 isStr("Never"),
		"terminationGracePeriodSeconds": anyNumber,
		"dnsPolicy":                     isStr("ClusterFirst"),
		"schedulerName":                 isStr("default-scheduler"),
		"hostNetwork":                   isBool(false),
		"hostPID":                       isBool(false),
		"hostIPC":                       isBool(false),
		"shareProcessNamespace":         isBool(false),
		"nodeSelector":                  stringMap,
		"initContainers":                emptyList,
		"ephemeralContainers":           emptyList,
		"priority":                      isInt(0),
		"preemptionPolicy":              isStr("PreemptLowerPriority"),
		"tolerations":                   eachItem(defaultToleration),
		// The service account admission controller copies the account's pull
		// secrets into every Pod.
		"imagePullSecrets": eachItem(func(p string, v any) error {
			return walk(p, v, map[string]rule{"name": anyString})
		}),
		"securityContext": func(p string, v any) error {
			if err := walk(p, v, map[string]rule{
				"runAsNonRoot": isBool(true), "runAsUser": isInt(c.cfg.RunAsUser), "runAsGroup": isInt(c.cfg.RunAsGroup),
				"fsGroup": isInt(c.cfg.RunAsGroup), "seccompProfile": profile,
			}); err != nil {
				return err
			}
			return require(p, v, "runAsNonRoot", "runAsUser", "seccompProfile")
		},
		"containers": func(p string, v any) error {
			l, err := listOf(p, v)
			if err != nil || len(l) != 1 {
				return unverified("%s must hold exactly one container", p)
			}
			return s.containerRule(p+"[0]", l[0])
		},
		"volumes": eachItem(s.volumeRule),
	}
	if s.live {
		// The scheduler assigns the node; a submitted template never names one.
		rules["nodeName"] = anyString
	}
	if err := walk(path, spec, rules); err != nil {
		return err
	}
	return require(path, spec, "serviceAccountName", "automountServiceAccountToken", "enableServiceLinks", "restartPolicy", "securityContext", "containers")
}

// defaultToleration allows the two NoExecute tolerations the API server adds to
// every Pod and nothing else.
func defaultToleration(p string, v any) error {
	if err := walk(p, v, map[string]rule{
		"key": func(p string, v any) error {
			if k, _ := v.(string); k != "node.kubernetes.io/not-ready" && k != "node.kubernetes.io/unreachable" {
				return unverified("%s tolerates %v", p, v)
			}
			return nil
		},
		"operator": isStr("Exists"), "effect": isStr("NoExecute"), "tolerationSeconds": anyNumber,
	}); err != nil {
		return err
	}
	return require(p, v, "key", "operator", "effect")
}

func (s podShape) containerRule(p string, v any) error {
	c := s.c
	job := s.claim != ""
	rules := map[string]rule{
		"name":                     isStr(s.container),
		"imagePullPolicy":          isStr(c.cfg.ImagePullPolicy),
		"resources":                c.resourcesRule(s.bounds),
		"securityContext":          c.containerSecurityRule(),
		"terminationMessagePath":   isStr("/dev/termination-log"),
		"terminationMessagePolicy": isStr("File"),
		"env":                      eachItem(envRule),
		"volumeMounts":             eachItem(s.mountRule),
		"resizePolicy": eachItem(func(p string, v any) error {
			return walk(p, v, map[string]rule{"resourceName": anyString, "restartPolicy": isStr("NotRequired")})
		}),
		"image": func(p string, v any) error {
			if img, _ := v.(string); !pinned(img) {
				return unverified("%s is not pinned by digest", p)
			}
			return nil
		},
	}
	if job {
		rules["command"] = func(p string, v any) error {
			l, err := strList(p, v)
			if err != nil || !slices.Equal(l, c.cfg.Shell) {
				return unverified("%s is not the configured interpreter", p)
			}
			return nil
		}
		rules["args"] = func(p string, v any) error {
			if l, err := strList(p, v); err != nil || len(l) != 1 {
				return unverified("%s must be exactly one script", p)
			}
			return nil
		}
		rules["workingDir"] = isStr(pathWorkspace)
	} else {
		rules["command"] = func(p string, v any) error { _, err := strList(p, v); return err }
		rules["ports"] = eachItem(func(p string, v any) error {
			return walk(p, v, map[string]rule{"containerPort": anyNumber, "protocol": isStr("TCP")})
		})
		rules["readinessProbe"] = func(p string, v any) error {
			return walk(p, v, map[string]rule{
				"tcpSocket":     func(p string, v any) error { return walk(p, v, map[string]rule{"port": anyNumber}) },
				"periodSeconds": anyNumber, "timeoutSeconds": anyNumber, "successThreshold": anyNumber, "failureThreshold": anyNumber,
			})
		}
	}
	if err := walk(p, v, rules); err != nil {
		return err
	}
	return require(p, v, "image", "resources", "securityContext")
}

func envRule(p string, v any) error {
	// A literal value only: no Secret, ConfigMap or field reference.
	if err := walk(p, v, map[string]rule{"name": anyString, "value": anyString}); err != nil {
		return err
	}
	return require(p, v, "name")
}

func (s podShape) mountRule(p string, v any) error {
	m, _ := v.(map[string]any)
	name, _ := m["name"].(string)
	path, _ := m["mountPath"].(string)
	rules := map[string]rule{"name": anyString, "mountPath": anyString, "readOnly": isBool(false)}
	switch {
	case name == "workspace" && s.claim != "" && path == pathWorkspace:
	case name == "tmp" && path == pathTmp:
	case name == "secrets" && s.material != "" && validateSecretDir(path) == nil:
		rules["readOnly"] = isBool(true)
	default:
		return unverified("%s mounts %q at %q, which is not part of the shape", p, name, path)
	}
	if err := walk(p, v, rules); err != nil {
		return err
	}
	if name == "secrets" {
		return require(p, v, "readOnly")
	}
	return nil
}

func (s podShape) volumeRule(p string, v any) error {
	m, _ := v.(map[string]any)
	name, _ := m["name"].(string)
	rules := map[string]rule{"name": anyString}
	switch {
	case name == "workspace" && s.claim != "":
		rules["persistentVolumeClaim"] = func(p string, v any) error {
			if err := walk(p, v, map[string]rule{"claimName": isStr(s.claim), "readOnly": isBool(false)}); err != nil {
				return err
			}
			return require(p, v, "claimName")
		}
	case name == "tmp":
		rules["emptyDir"] = func(p string, v any) error {
			return walk(p, v, map[string]rule{"sizeLimit": func(p string, v any) error {
				if got, _ := v.(string); !sameQuantity(got, s.c.cfg.ScratchSize) {
					return unverified("%s is %v, want %s", p, v, s.c.cfg.ScratchSize)
				}
				return nil
			}})
		}
	case name == "secrets" && s.material != "":
		rules["secret"] = func(p string, v any) error {
			if err := walk(p, v, map[string]rule{"secretName": isStr(s.material), "defaultMode": isInt(0o440), "optional": isBool(false)}); err != nil {
				return err
			}
			return require(p, v, "secretName")
		}
	default:
		return unverified("%s is a volume %q that is not part of the shape", p, name)
	}
	return walk(p, v, rules)
}

// jobDoc verifies the Job document and its template.
func (c *core) verifyJobDoc(raw json.RawMessage, t Target, claim, material string) error {
	d, err := decodeDoc(raw, "Job")
	if err != nil {
		return err
	}
	shape := podShape{c: c, t: t, container: containerStep, bounds: c.cfg.Workload, claim: claim, material: material}
	err = walk("job", d, map[string]rule{
		"apiVersion": isStr("batch/v1"), "kind": isStr("Job"), "metadata": anyDoc, "status": anyDoc,
		"spec": func(p string, v any) error {
			if err := walk(p, v, map[string]rule{
				"parallelism": isInt(1), "completions": isInt(1), "backoffLimit": isInt(0), "activeDeadlineSeconds": anyNumber,
				"selector": anyDoc, "completionMode": isStr("NonIndexed"), "suspend": isBool(false), "podReplacementPolicy": anyString,
				"template": func(p string, v any) error {
					return walk(p, v, map[string]rule{
						"metadata": func(p string, v any) error {
							return walk(p, v, map[string]rule{"labels": stringMap, "annotations": stringMap})
						},
						"spec": shape.verify,
					})
				},
			}); err != nil {
				return err
			}
			return require(p, v, "parallelism", "completions", "backoffLimit", "template")
		},
	})
	if err != nil {
		return err
	}
	return require("job", d, "spec")
}

// verifyLivePodDoc verifies a scheduled Pod, for a service or a Job's Pod.
func (c *core) verifyLivePodDoc(raw json.RawMessage, shape podShape) error {
	d, err := decodeDoc(raw, "Pod")
	if err != nil {
		return err
	}
	shape.live = true
	err = walk("pod", d, map[string]rule{
		"apiVersion": isStr("v1"), "kind": isStr("Pod"), "metadata": anyDoc, "status": anyDoc, "spec": shape.verify,
	})
	if err != nil {
		return err
	}
	return require("pod", d, "spec")
}

// verifyJob checks the live Job: owned by id, still the instance this identity
// established, and the complete permitted document, including the workspace
// claim recorded when the step was started.
func (c *core) verifyJob(j kubernetes.Job, id ident, t Target, rec record) error {
	if !id.owns(j.Metadata.Labels, j.Metadata.Annotations) {
		return fmt.Errorf("%w: job %s", ErrForeign, j.Metadata.Name)
	}
	if !id.digestMatches(j.Metadata.Annotations) {
		return fmt.Errorf("%w: the workload exists with another specification digest", runtime.ErrSpecMismatch)
	}
	if err := checkInstance(rec, true, opJob, j.Metadata.UID); err != nil {
		return err
	}
	if rec.workspace == "" {
		return unverified("the workload has no recorded workspace")
	}
	return c.verifyJobDoc(j.Raw, t, rec.workspace, c.materialFor(id, j))
}

// materialFor names the step secrets Secret the Job mounts, if it mounts one.
func (c *core) materialFor(id ident, j kubernetes.Job) string {
	for _, v := range j.Spec.Template.Spec.Volumes {
		if v.Name == "secrets" {
			return id.materialName()
		}
	}
	return ""
}

// verifyJobPod verifies one of the Job's Pods against the same shape.
func (c *core) verifyJobPod(p kubernetes.Pod, id ident, t Target, rec record, material string) error {
	return c.verifyLivePodDoc(p.Raw, podShape{c: c, t: t, container: containerStep, bounds: c.cfg.Workload, claim: rec.workspace, material: material})
}

func (c *core) verifyPod(p kubernetes.Pod, id ident, t Target, rec record) error {
	if !id.owns(p.Metadata.Labels, p.Metadata.Annotations) {
		return fmt.Errorf("%w: pod %s", ErrForeign, p.Metadata.Name)
	}
	if !id.digestMatches(p.Metadata.Annotations) {
		return fmt.Errorf("%w: the service exists with another specification digest", runtime.ErrSpecMismatch)
	}
	if err := checkInstance(rec, true, opPod, p.Metadata.UID); err != nil {
		return err
	}
	return c.verifyLivePodDoc(p.Raw, podShape{c: c, t: t, container: containerSvc, bounds: c.cfg.Service})
}

func (c *core) verifyPVC(p kubernetes.PersistentVolumeClaim, id ident, rec record) error {
	if !id.owns(p.Metadata.Labels, p.Metadata.Annotations) {
		return fmt.Errorf("%w: claim %s", ErrForeign, p.Metadata.Name)
	}
	if !id.digestMatches(p.Metadata.Annotations) {
		return fmt.Errorf("%w: the workspace exists with another specification digest", runtime.ErrSpecMismatch)
	}
	if err := checkInstance(rec, true, opPVC, p.Metadata.UID); err != nil {
		return err
	}
	d, err := decodeDoc(p.Raw, "workspace claim")
	if err != nil {
		return err
	}
	// No data source: a claim cloned from another volume would carry another
	// tenant's data into this run.
	err = walk("claim", d, map[string]rule{
		"apiVersion": isStr("v1"), "kind": isStr("PersistentVolumeClaim"), "metadata": anyDoc, "status": anyDoc,
		"spec": func(p string, v any) error {
			return walk(p, v, map[string]rule{
				"accessModes": func(p string, v any) error {
					if l, err := strList(p, v); err != nil || !slices.Equal(l, []string{"ReadWriteOncePod"}) {
						return unverified("%s must be ReadWriteOncePod", p)
					}
					return nil
				},
				"storageClassName": isStr(c.cfg.StorageClass),
				"volumeMode":       isStr("Filesystem"),
				"volumeName":       anyString,
				"resources": func(p string, v any) error {
					return walk(p, v, map[string]rule{"requests": func(p string, v any) error {
						return walk(p, v, map[string]rule{"storage": func(p string, v any) error {
							if got, _ := v.(string); !sameQuantity(got, c.cfg.WorkspaceSize) {
								return unverified("%s is %v, want %s", p, v, c.cfg.WorkspaceSize)
							}
							return nil
						}})
					}})
				},
			})
		},
	})
	if err != nil {
		return err
	}
	return require("claim", d, "spec")
}

func (c *core) verifyServiceObject(s kubernetes.Service, id ident, rec record) error {
	if !id.owns(s.Metadata.Labels, s.Metadata.Annotations) {
		return fmt.Errorf("%w: service %s", ErrForeign, s.Metadata.Name)
	}
	if err := checkInstance(rec, true, opService, s.Metadata.UID); err != nil {
		return err
	}
	d, err := decodeDoc(s.Raw, "Service")
	if err != nil {
		return err
	}
	selector := func(p string, v any) error {
		m, ok := v.(map[string]any)
		if !ok || len(m) != 2 || m[labelObject] != id.base() || m[labelManaged] != valueManaged {
			return unverified("%s selects more than the service's own Pod", p)
		}
		return nil
	}
	err = walk("service", d, map[string]rule{
		"apiVersion": isStr("v1"), "kind": isStr("Service"), "metadata": anyDoc, "status": anyDoc,
		"spec": func(p string, v any) error {
			if err := walk(p, v, map[string]rule{
				"type": isStr("ClusterIP"), "selector": selector, "clusterIP": anyString,
				"clusterIPs": emptyOrStrings, "ipFamilies": emptyOrStrings, "ipFamilyPolicy": anyString,
				"internalTrafficPolicy": isStr("Cluster"), "sessionAffinity": isStr("None"),
				"ports": eachItem(func(p string, v any) error {
					return walk(p, v, map[string]rule{"name": anyString, "protocol": isStr("TCP"), "port": anyNumber, "targetPort": anyNumber})
				}),
			}); err != nil {
				return err
			}
			return require(p, v, "type", "selector")
		},
	})
	if err != nil {
		return err
	}
	return require("service", d, "spec")
}

func emptyOrStrings(p string, v any) error {
	_, err := strList(p, v)
	return err
}

// ownedPods keeps the Pods whose controlling owner is the named Job (any UID
// when jobUID is empty, for Pods whose Job is already gone) and that carry the
// identity's ownership evidence.
func ownedPods(pods []kubernetes.Pod, id ident, jobName, jobUID string) []kubernetes.Pod {
	var out []kubernetes.Pod
	for _, p := range pods {
		if !id.owns(p.Metadata.Labels, p.Metadata.Annotations) {
			continue
		}
		for _, o := range p.Metadata.OwnerReferences {
			if o.Controlled() && o.Kind == "Job" && o.APIVersion == "batch/v1" && o.Name == jobName && (jobUID == "" || o.UID == jobUID) {
				out = append(out, p)
				break
			}
		}
	}
	return out
}
