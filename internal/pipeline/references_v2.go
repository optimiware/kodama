package pipeline

import "regexp"

// endpointRE matches the two kodama/v1alpha2 typed endpoint references, always
// as a whole scalar:
//
//	${{ services.NAME.host }}        string
//	${{ services.NAME.ports.PORT }}  integer
var endpointRE = regexp.MustCompile(`^\$\{\{[ \t]*services\.([a-z][a-z0-9-]{0,29})\.(host|ports\.([a-z][a-z0-9-]{0,14}))[ \t]*\}\}$`)

// endpoint is a parsed service endpoint reference.
type endpoint struct {
	service string
	port    string // empty for the host form
}

func (e endpoint) isHost() bool { return e.port == "" }

// valueType is the type the reference resolves to.
func (e endpoint) valueType() ValueType {
	if e.isHost() {
		return TypeString
	}
	return TypeInteger
}

// parseEndpoint reports whether s is exactly one endpoint reference.
func parseEndpoint(s string) (endpoint, bool) {
	m := endpointRE.FindStringSubmatch(s)
	if m == nil {
		return endpoint{}, false
	}
	return endpoint{service: m[1], port: m[3]}, true
}

// checkEndpoint validates an endpoint reference at a step site. Only the
// declared service and port must exist: a step needs no edge or needs path to
// reach a service of its own run.
func (r *resolver) checkEndpoint(site refSite, ep endpoint) {
	if !site.endpoints {
		r.d.add(CodeInterpolation, site.path, "endpoint references are only allowed in container step env values and arguments and in service env values")
		return
	}
	svc, ok := r.doc.Spec.Services[ep.service]
	if !ok {
		r.d.add(CodeUnknownService, site.path, "the referenced service is not declared")
		return
	}
	if !ep.isHost() {
		if _, ok := svc.Ports[ep.port]; !ok {
			r.d.add(CodeUnknownPort, site.path, "the referenced port is not declared by the service")
			return
		}
	}
	if !site.accepts(ep.valueType()) {
		r.d.add(CodeTypeMismatch, site.path, "the referenced value has type %s, which this field does not accept", ep.valueType())
	}
}
