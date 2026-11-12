package kube

import (
	"net/url"
	"sort"
	"strings"
)

// REST paths pinned by the seam. Names are path-escaped so an object name can
// never change the request path.

func namespacePath(namespace string) string {
	return "/api/v1/namespaces/" + url.PathEscape(namespace)
}

func jobsPath(namespace string) string {
	return "/apis/batch/v1/namespaces/" + url.PathEscape(namespace) + "/jobs"
}

func jobPath(namespace, name string) string {
	return jobsPath(namespace) + "/" + url.PathEscape(name)
}

func podsPath(namespace string) string {
	return namespacePath(namespace) + "/pods"
}

func podLogPath(namespace, pod string) string {
	return podsPath(namespace) + "/" + url.PathEscape(pod) + "/log"
}

func configMapsPath(namespace string) string {
	return namespacePath(namespace) + "/configmaps"
}

func configMapPath(namespace, name string) string {
	return configMapsPath(namespace) + "/" + url.PathEscape(name)
}

func secretsPath(namespace string) string {
	return namespacePath(namespace) + "/secrets"
}

func secretPath(namespace, name string) string {
	return secretsPath(namespace) + "/" + url.PathEscape(name)
}

func claimsPath(namespace string) string {
	return namespacePath(namespace) + "/persistentvolumeclaims"
}

func claimPath(namespace, name string) string {
	return claimsPath(namespace) + "/" + url.PathEscape(name)
}

// labelSelector encodes a label map as the single labelSelector query value.
// Keys are sorted so a retry sends the identical query.
func labelSelector(labels map[string]string) url.Values {
	if len(labels) == 0 {
		return nil
	}
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+labels[key])
	}
	query := url.Values{}
	query.Set("labelSelector", strings.Join(parts, ","))
	return query
}
