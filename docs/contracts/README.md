# Interface contracts

These documents define Kodama's external and internal contracts under [ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md):

| Contract | Owns |
| --- | --- |
| [Pipeline format (`.kodama/<processname>.yaml`)](pipeline-format.md) | Drone CI-inspired YAML pipeline syntax for container steps, agent steps, and scoped services |
| [HTTP & MCP interface](http-api.md) | REST API routes, MCP server tools, authentication, and `kodactl` conventions |
| [Event catalog (NATS JetStream)](event-catalog.md) | NATS subjects and signal payloads for Git triggers, task state transitions, step/agent coordination, releases, and promotions |
| [Git service](git-service.md) | Gerrit-style `/git/<tenant>/<path>.git` smart-HTTP endpoints, automatic project association, and Knowledge Base conventions |

Behavioral requirements and acceptance scenarios remain in the [specifications](../README.md#contributors).
