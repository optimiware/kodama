# test/support/mcp

`mcpfixture` is the local MCP protocol server of the T-033 tests: a real Streamable HTTP server built on the
official Go SDK, served over TLS on the loopback interface (`httptest.NewTLSServer`).

```go
fx := mcpfixture.New(t, mcpfixture.Options{ExtraTools: 5, PageSize: 2})
fx.URL()        // https://127.0.0.1:port/mcp
fx.Roots()      // pool that trusts the fixture's certificate
fx.Token()      // the one bearer the server accepts (recipient-bound check)
```

Tools: `echo` (visible), `admin_secret` (a tool no registration or profile exposes), `mutate` (a counted side
effect, `Mutations`), `slow` (blocks until cancelled and records it: `Started`/`Cancelled`), `fail` (a protocol
error reply) and `filler_NN` (pagination).

Fault controls: `LoseNextReplies(n)` runs the tool and then drops the connection without replying (a side effect
followed by response loss); `BlockListing(true)` makes `tools/list` block until cancelled (`ListingStarted`/`ListingCancelled` count it);
`RedirectTo(sink)` serves `/redirect` as a 307 to a `Sink` that records anything it
receives; `Options.Protocols` restricts the negotiated protocol revision; `Options.Token` sets the accepted bearer.
`Requests()` records every HTTP request (method, path, Authorization, session header, body) so tests can prove what
did and did not reach the server.

The fixture disables the SDK's localhost DNS-rebinding protection so tests can address it by name through a fake
resolver. It proves the declared SDK/protocol combination only and establishes no production or harness support.
Resource identifiers use the task prefix `t033`.
