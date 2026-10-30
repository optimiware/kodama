# Run the external MCP server

The MCP server is the same `kodama` binary with the `mcp serve` command. It does
not need a database, NATS, S3, KMS or Kubernetes credential.

## Local

```sh
kodama mcp serve \
  --api-url http://127.0.0.1:8080/api/v1 \
  --resource http://127.0.0.1:8090/mcp \
  --audience http://127.0.0.1:8080/api/v1 \
  --issuer http://127.0.0.1:8181 \
  --allow-insecure-http
```

`--allow-insecure-http` is only for loopback development. Use HTTPS for every
URL in a real installation.

## Configuration

| Flag | Environment | Meaning |
| --- | --- | --- |
| `--api-url` | `KODAMA_MCP_API_URL` | Kodama API base, including `/api/v1` |
| `--resource` | `KODAMA_MCP_RESOURCE` | Exact public MCP URL; advertised in OAuth metadata |
| `--audience` | `KODAMA_MCP_AUDIENCE` | Audience Kodama accepts; defaults to `--resource` |
| `--issuer` | `KODAMA_MCP_ISSUER` | OAuth/OIDC issuer that issues Kodama access tokens |
| `--listen` | `KODAMA_MCP_LISTEN` | Listen address, default `:8090` |
| `--read-only` | `KODAMA_MCP_READ_ONLY` | Expose only GET tools |
| `--toolsets` | `KODAMA_MCP_TOOLSETS` | Comma-separated groups: `identity`, `work`, `workspace`, `pipeline`, `runs`, `gates`, `delivery`, `configuration`, `artifacts`, `model`, `repo`, `chatops` |
| `--enable-secret-writes` | `KODAMA_MCP_ENABLE_SECRET_WRITES` | Expose `createSecret`/`createSecretVersion` |
| `--enable-credential-tools` | `KODAMA_MCP_ENABLE_CREDENTIAL_TOOLS` | Expose tools that return Git credentials |
| `--api-ca-file` | `KODAMA_MCP_API_CA_FILE` | PEM trust roots for the API |
| `--oidc-ca-file` | `KODAMA_MCP_OIDC_CA_FILE` | PEM trust roots for discovery and JWKS |
| `--default-tenant` | `KODAMA_MCP_DEFAULT_TENANT` | Optional default tenant argument |
| `--default-project` | `KODAMA_MCP_DEFAULT_PROJECT` | Optional default project argument |
| `--http-timeout` | `KODAMA_MCP_HTTP_TIMEOUT` | Per-call timeout |
| `--max-response-bytes` | `KODAMA_MCP_MAX_RESPONSE_BYTES` | Response bound |

## OAuth deployment check

1. The issuer must issue access tokens with the audience Kodama checks
   (`KODAMA_OIDC_AUDIENCE`).
2. The MCP client must be able to obtain a token for the MCP resource. Standard
   MCP clients send an RFC 8707 `resource` parameter; the authorization server
   must accept it or the client must be configured with a pre-registered client.
3. If the authorization server does not support dynamic client registration,
   register a public client for each MCP client redirect URI and configure the
   client with that client ID.
4. The protected-resource metadata `resource` must equal the exact public MCP
   URL, including its path.

The current `oauthsonas` developer fixture does not yet accept `resource` and has
no registration endpoint. Qualify the chosen authorization server before
claiming a zero-configuration OAuth connection.

## Kubernetes shape

Use a separate Deployment in the same namespace as the Kodama server:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: kodama-mcp
spec:
  replicas: 1
  selector:
    matchLabels: {app: kodama-mcp}
  template:
    metadata:
      labels: {app: kodama-mcp}
    spec:
      containers:
        - name: mcp
          image: ghcr.io/example/kodama@sha256:REPLACE
          args: ["mcp", "serve"]
          env:
            - {name: KODAMA_MCP_API_URL, value: "https://kodama.example/api/v1"}
            - {name: KODAMA_MCP_RESOURCE, value: "https://mcp.example/mcp"}
            - {name: KODAMA_MCP_AUDIENCE, value: "https://kodama.example/api/v1"}
            - {name: KODAMA_MCP_ISSUER, value: "https://id.example"}
            - {name: KODAMA_MCP_LISTEN, value: ":8090"}
          ports:
            - {name: http, containerPort: 8090}
          readinessProbe:
            httpGet: {path: /readyz, port: http}
          livenessProbe:
            httpGet: {path: /healthz, port: http}
```

Expose `/mcp` and the two `/.well-known/oauth-protected-resource` paths through
the Ingress. Network policy needs egress only to the Kodama API and the issuer
(discovery/JWKS); no database, broker, object store or KMS access.

## Client shape

```json
{
  "mcpServers": {
    "kodama": {
      "type": "http",
      "url": "https://mcp.example/mcp"
    }
  }
}
```

The client discovers the authorization server from the 401 challenge and the
protected-resource metadata. It must support OAuth authorization code + PKCE.
