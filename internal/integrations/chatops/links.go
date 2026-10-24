package chatops

import (
	"net/url"
	"strings"

	"github.com/optimiware/kodama/internal/domain"
)

// resourceRoutes maps a resource kind to the collection of the existing
// authenticated GET route that returns its current record. A link never names
// anything but that route: no query, no fragment, no token, no decision.
var resourceRoutes = map[string]string{
	"work_item":  "work-items",
	"gate":       "gates",
	"run":        "runs",
	"deployment": "deployments",
}

// LinkBuilder constructs current-record URLs from an explicitly configured
// canonical API origin. It never reads a request Host header or an
// event-provided URL. The zero value refuses to build anything.
type LinkBuilder struct{ origin string }

// NewLinkBuilder validates the origin: an absolute https URL with a host, an
// optional port and no user-info, path, query or fragment.
func NewLinkBuilder(origin string) (LinkBuilder, error) {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Hostname() == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || strings.ContainsAny(origin, " \t\r\n") {
		return LinkBuilder{}, invalidf("the canonical API origin must be an https origin without path, query or user-info")
	}
	return LinkBuilder{origin: "https://" + u.Host}, nil
}

// URL returns the authenticated current-record URL of a resource in a project:
// <origin>/api/v1/tenants/{tenant}/projects/{project}/{collection}/{id}.
// Following it asks the target route to authenticate the follower and authorize
// that follower against the record as it is now; the URL carries no authority.
func (b LinkBuilder) URL(scope domain.Scope, kind, id string) (string, error) {
	collection, ok := resourceRoutes[kind]
	if !ok || b.origin == "" {
		return "", invalidf("no current-record link exists for resource kind %q", kind)
	}
	if err := scope.Validate(); err != nil {
		return "", invalidf("the link scope is not valid")
	}
	if err := domain.WorkItemID(id).Validate(); err != nil { // any opaque identifier
		return "", invalidf("the resource identifier is not valid")
	}
	return b.origin + "/api/v1/tenants/" + url.PathEscape(string(scope.TenantID)) + "/projects/" + url.PathEscape(string(scope.ProjectID)) +
		"/" + collection + "/" + url.PathEscape(id), nil
}
