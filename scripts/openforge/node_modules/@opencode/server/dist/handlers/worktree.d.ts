import { Worktree } from "@opencode/core/worktree";
export declare const WorktreeHandler: import("effect/Layer").Layer<import("effect/http-api/HttpApiGroup").Service<"server", "server.worktree">, never, import("@opencode/protocol/middleware/authorization").Authorization | import("@opencode/protocol/middleware/schema-error").SchemaErrorMiddleware | Worktree.Service>;
