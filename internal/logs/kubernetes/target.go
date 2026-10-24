package kubernetes

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/optimiware/kodama/internal/logs"
)

// Kubernetes object-name syntax (DNS-1123). Every identifier that reaches a
// request path or query is validated against these before use, so a value that
// came from persisted state or configuration still cannot carry a separator,
// a dot segment or a selector.
var (
	labelRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	nameRe  = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`)
	uidRe   = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	// selector label keys/values as Kubernetes defines them (qualified name /
	// label value), bounded.
	selKeyRe = regexp.MustCompile(`^([a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?/)?[A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?$`)
	selValRe = regexp.MustCompile(`^([A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?)?$`)
)

func validTarget(t logs.Target) error {
	switch {
	case !labelRe.MatchString(t.Namespace):
		return fmt.Errorf("%w: namespace", logs.ErrInvalid)
	case !nameRe.MatchString(t.JobName):
		return fmt.Errorf("%w: job name", logs.ErrInvalid)
	case !uidRe.MatchString(t.JobUID):
		return fmt.Errorf("%w: job uid", logs.ErrInvalid)
	case !labelRe.MatchString(t.Container):
		return fmt.Errorf("%w: container", logs.ErrInvalid)
	}
	return nil
}

func (r *Reader) path(parts ...string) string {
	esc := make([]string, len(parts))
	for i, p := range parts {
		esc[i] = url.PathEscape(p)
	}
	return r.base.JoinPath(esc...).String()
}

func (r *Reader) jobURL(t logs.Target) string {
	return r.path("apis", "batch", "v1", "namespaces", t.Namespace, "jobs", t.JobName)
}

// podsURL lists candidate Pods by a selector built here from the Job's own
// selector labels, never from a caller value.
func (r *Reader) podsURL(t logs.Target, labels map[string]string) (string, error) {
	if len(labels) == 0 || len(labels) > 8 {
		return "", fmt.Errorf("%w: job selector", logs.ErrWorkloadMismatch)
	}
	keys := make([]string, 0, len(labels))
	for k, v := range labels {
		if !selKeyRe.MatchString(k) || !selValRe.MatchString(v) {
			return "", fmt.Errorf("%w: job selector", logs.ErrWorkloadMismatch)
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + labels[k]
	}
	q := url.Values{"labelSelector": {strings.Join(parts, ",")}, "limit": {"50"}}
	return r.path("api", "v1", "namespaces", t.Namespace, "pods") + "?" + q.Encode(), nil
}

func (r *Reader) logURL(t logs.Target, pod string, q logs.LiveQuery) string {
	v := url.Values{"container": {t.Container}}
	if q.TailLines > 0 {
		v.Set("tailLines", fmt.Sprint(q.TailLines))
	}
	if q.LimitBytes > 0 {
		v.Set("limitBytes", fmt.Sprint(q.LimitBytes))
	}
	if q.Follow {
		v.Set("follow", "true")
	}
	return r.path("api", "v1", "namespaces", t.Namespace, "pods", pod, "log") + "?" + v.Encode()
}
