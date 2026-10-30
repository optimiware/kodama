# Run the external MCP server

The MCP server is the same `kodama` binary with the `mcp serve` command. It does
not need a database, NATS, S3, KMS or Kubernetes credential.

## Local

```sh
kodama mcp serve \
  --api-url http://127.0.0.1:8080/api/v1 \
  --resource http://127.0.0.1:8090/mcp \
  --audience http://127.0.0.1:8090/mcp \
  --issuer http://127.0.0.1:8181 \
  --api-audience http://127.0.0.1:8080/api/v1 \
  --api-issuer http://127.0.0.1:8181 \
  --exchange-url http://127.0.0.1:8181/token \
  --exchange-client-id kodama-mcp \
  --exchange-client-secret-file /run/secrets/mcp-client \
  --allow-insecure-http
```

`--allow-insecure-http` is only for local development. Use
`--allow-insecure-api-http` for the narrow in-cluster API exception while
keeping issuer, resource and exchange URLs on HTTPS. The issuer must support the
RFC 8693 exchange; the `oauthsonas` fixture does not, so this example describes
the intended local shape rather than a working fixture command.

## Configuration

| Flag | Environment | Meaning |
| --- | --- | --- |
| `--api-url` | `KODAMA_MCP_API_URL` | Kodama API base, including `/api/v1` |
| `--resource` | `KODAMA_MCP_RESOURCE` | Exact public MCP URL; advertised in OAuth metadata |
| `--audience` | `KODAMA_MCP_AUDIENCE` | Inbound MCP audience; defaults to `--resource` |
| `--issuer` | `KODAMA_MCP_ISSUER` | Issuer of MCP-bound access tokens |
| `--api-audience` | `KODAMA_MCP_API_AUDIENCE` | Audience of the delegated Kodama API token |
| `--api-issuer` | `KODAMA_MCP_API_ISSUER` | Issuer of the delegated API token; defaults to `--issuer` |
| `--api-resource` | `KODAMA_MCP_API_RESOURCE` | RFC 8707 resource requested from exchange; defaults to `--api-audience` |
| `--exchange-url` | `KODAMA_MCP_EXCHANGE_URL` | RFC 8693 token-exchange endpoint |
| `--exchange-client-id` | `KODAMA_MCP_EXCHANGE_CLIENT_ID` | Confidential exchange client id |
| `--exchange-client-auth` | `KODAMA_MCP_EXCHANGE_CLIENT_AUTH` | `client_secret_basic` (default) or `client_secret_post` |
| `--exchange-client-secret-file` | `KODAMA_MCP_EXCHANGE_CLIENT_SECRET_FILE` | File containing the exchange client secret |
| `--exchange-scopes` | `KODAMA_MCP_EXCHANGE_SCOPES` | Optional comma-separated exchange scopes |
| `--exchange-ca-file` | `KODAMA_MCP_EXCHANGE_CA_FILE` | PEM trust roots for the exchange endpoint |
| `--allow-insecure-api-http` | `KODAMA_MCP_ALLOW_INSECURE_API_HTTP` | Permit plain HTTP only for the in-cluster API URL |
| `--scopes` | `KODAMA_MCP_SCOPES` | Scopes advertised in protected-resource metadata; default `openid` |
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

1. The issuer must issue MCP access tokens with the MCP audience, not the Kodama
   API audience. The protected-resource metadata `resource` must equal the exact
   public MCP URL, including its path.
2. The MCP client must be able to obtain a token for that resource. Standard MCP
   clients send an RFC 8707 `resource` parameter; the authorization server must
   accept it, or the client must be configured with a pre-registered client.
3. If the authorization server does not support dynamic client registration or
   Client ID Metadata Documents, register a public client for each MCP client
   redirect URI and configure the client with that client ID.
4. The issuer must support the RFC 8693 exchange used by the MCP server, or the
   installation must use a separately qualified broker. The exchange must return
   a separate API-bound token for the same represented subject and identify the
   MCP intermediary.
5. The exchange client is confidential. Its secret is a dedicated Secret or
   mounted file; it is never the server runtime Secret and never a chart value.
6. The API independently verifies the exchanged token. A failed exchange is a
   failed tool call; there is no passthrough fallback.

The current `oauthsonas` developer fixture does not provide the exchange, so the
development chart profile disables MCP. Qualify the chosen authorization server
before claiming a zero-configuration OAuth connection.

## Helm chart (recommended)

The Kodama chart installs the MCP server as a separate Deployment and Service in
the same release. Enable it with `mcp.enabled: true` and expose it on its own
host in the external profile:

```yaml
mcp:
  enabled: true
  resource: "https://mcp.example.invalid/mcp"
  audience: "https://mcp.example.invalid/mcp"
  issuer: "https://issuer.example.invalid"
  apiAudience: "https://kodama.example.invalid/api"
  apiIssuer: "https://issuer.example.invalid"
  apiResource: "https://kodama.example.invalid/api"
  exchange:
    url: "https://issuer.example.invalid/token"
    clientId: "kodama-mcp"
    clientAuth: "client_secret_basic"
    existingSecret: "kodama-mcp-exchange"
    secretKey: "client-secret"
    scopes: "kodama.api"
  ingress:
    enabled: true
    className: nginx
    host: mcp.example.invalid
    path: "/"
    tls:
      enabled: true
      secretName: kodama-mcp-tls
```

A shared MCP/server host is supported only with `mcp.ingress.path: /mcp`; the
chart then routes `/mcp` and `/.well-known/oauth-protected-resource/mcp` to MCP
while `/` remains with Kodama. A dedicated host is the recommended default. The
MCP Pod has no runtime Secret mount and no Kubernetes API token; it mounts only
the exchange Secret and optional CA Secrets. See the
[chart values reference](../../deploy/helm/kodama/README.md#mcp-server) for all
`mcp.*` fields.

## Kubernetes shape (fallback)

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
            - {name: KODAMA_MCP_AUDIENCE, value: "https://mcp.example/mcp"}
            - {name: KODAMA_MCP_ISSUER, value: "https://id.example"}
            - {name: KODAMA_MCP_API_AUDIENCE, value: "https://kodama.example/api/v1"}
            - {name: KODAMA_MCP_API_ISSUER, value: "https://id.example"}
            - {name: KODAMA_MCP_API_RESOURCE, value: "https://kodama.example/api/v1"}
            - {name: KODAMA_MCP_EXCHANGE_URL, value: "https://id.example/token"}
            - {name: KODAMA_MCP_EXCHANGE_CLIENT_ID, value: "kodama-mcp"}
            - {name: KODAMA_MCP_EXCHANGE_CLIENT_SECRET_FILE, value: "/etc/kodama/mcp-exchange/client-secret"}
            - {name: KODAMA_MCP_LISTEN, value: ":8090"}
          volumeMounts:
            - {name: mcp-exchange, mountPath: /etc/kodama/mcp-exchange, readOnly: true}
          ports:
            - {name: http, containerPort: 8090}
          readinessProbe:
            httpGet: {path: /readyz, port: http}
          livenessProbe:
            httpGet: {path: /healthz, port: http}
      volumes:
        - name: mcp-exchange
          secret:
            secretName: kodama-mcp-exchange
```

Expose `/mcp` and the two `/.well-known/oauth-protected-resource` paths through
the Ingress. Network policy needs egress only to the Kodama API, the issuer
(discovery/JWKS) and the exchange endpoint; no database, broker, object store or
KMS access.

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
