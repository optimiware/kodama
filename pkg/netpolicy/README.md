# Outbound address policy

Small standard-library-only helper, adapted from Harness
[`netpolicy/netpolicy.go` at `71005497269fecb8f389f360a75436e305583e9f`](https://github.com/harness/harness/blob/71005497269fecb8f389f360a75436e305583e9f/netpolicy/netpolicy.go).
The source retains its Apache-2.0 attribution; see [LICENSE](LICENSE) and [NOTICE](NOTICE).

- `IsPublic` applies a conservative public-unicast policy, including IPv4-mapped IPv6 normalization.
- `PublicOnly` is a `net.Dialer.Control` hook checking the actual resolved TCP destination before connect.
- No logger, SDK, resolver abstraction, retry loop, configurable bypass, or product dependency.

```go
dialer := &net.Dialer{Timeout: 10 * time.Second, Control: netpolicy.PublicOnly}
transport := http.DefaultTransport.(*http.Transport).Clone()
transport.Proxy = nil
transport.DialContext = dialer.DialContext
client := &http.Client{
    Transport: transport,
    Timeout: 30 * time.Second,
    CheckRedirect: func(*http.Request, []*http.Request) error {
        return http.ErrUseLastResponse
    },
}
```

The consuming adapter still validates the approved HTTPS URL, ports, credentials,
response bounds and redirect behavior. Proxy routing or an overriding TLS dialer
requires its own enforcement. IP classification cannot identify every publicly
addressed internal service; it complements an endpoint allow-list and network policy.
The special-use list is a conservative local policy, not a live routing registry.

This package is not wired into the server. T-041's webhook adapter
(`internal/integrations/chatops/webhook`) uses it for direct public webhook dialing:
`PublicOnly` is the `Control` hook of a per-send `net.Dialer`, the transport has no
proxy and no alternate TLS dialer, redirects are never followed, and the adapter
validates the approved HTTPS URL, ports, credentials and response bounds itself.
Private destinations are possible there only through an explicit, tenant-scoped,
exact-address allowance supplied by the installation, composed in the adapter's
`Control` wrapper that consults `PublicOnly` first; this package has no bypass and
none is added. Later gateway/MCP adapters can reuse it where public-only routing
fits their approved endpoint policy. Private on-premises endpoints need a
separately reviewed scoped policy, not a blanket private-network bypass.

Run `go test -race ./pkg/netpolicy`. Tests cover address families, special-use
ranges, mapped/scoped addresses and a real dial rejected before connect.
