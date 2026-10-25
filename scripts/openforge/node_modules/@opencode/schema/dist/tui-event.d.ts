export * as TuiEvent from "./tui-event.js";
import { Schema } from "effect";
export declare const PromptAppend: Schema.Struct<{
    readonly id: Schema.brand<Schema.String, "Event.ID"> & {
        create: () => string & import("effect/Brand").Brand<"Event.ID">;
    };
    readonly created: Schema.Finite;
    readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
    readonly type: Schema.Literal<"tui.prompt.append">;
    readonly location: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Struct<{
        readonly directory: Schema.brand<Schema.String, "AbsolutePath">;
        readonly workspaceID: Schema.decodeTo<Schema.optional<Schema.toType<Schema.brand<Schema.String, "Workspace.ID"> & {
            ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
            create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
        }>>, Schema.optionalKey<Schema.brand<Schema.String, "Workspace.ID"> & {
            ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
            create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
        }>, never, never>;
    }>>>, Schema.optionalKey<Schema.Struct<{
        readonly directory: Schema.brand<Schema.String, "AbsolutePath">;
        readonly workspaceID: Schema.decodeTo<Schema.optional<Schema.toType<Schema.brand<Schema.String, "Workspace.ID"> & {
            ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
            create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
        }>>, Schema.optionalKey<Schema.brand<Schema.String, "Workspace.ID"> & {
            ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
            create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
        }>, never, never>;
    }>>, never, never>;
    readonly data: Schema.Struct<{
        readonly text: Schema.String;
    }>;
}> & {
    type: "tui.prompt.append";
    durability: "ephemeral";
    durable: undefined;
    data: Schema.Struct<{
        readonly text: Schema.String;
    }>;
};
export declare const CommandExecute: Schema.Struct<{
    readonly id: Schema.brand<Schema.String, "Event.ID"> & {
        create: () => string & import("effect/Brand").Brand<"Event.ID">;
    };
    readonly created: Schema.Finite;
    readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
    readonly type: Schema.Literal<"tui.command.execute">;
    readonly location: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Struct<{
        readonly directory: Schema.brand<Schema.String, "AbsolutePath">;
        readonly workspaceID: Schema.decodeTo<Schema.optional<Schema.toType<Schema.brand<Schema.String, "Workspace.ID"> & {
            ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
            create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
        }>>, Schema.optionalKey<Schema.brand<Schema.String, "Workspace.ID"> & {
            ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
            create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
        }>, never, never>;
    }>>>, Schema.optionalKey<Schema.Struct<{
        readonly directory: Schema.brand<Schema.String, "AbsolutePath">;
        readonly workspaceID: Schema.decodeTo<Schema.optional<Schema.toType<Schema.brand<Schema.String, "Workspace.ID"> & {
            ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
            create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
        }>>, Schema.optionalKey<Schema.brand<Schema.String, "Workspace.ID"> & {
            ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
            create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
        }>, never, never>;
    }>>, never, never>;
    readonly data: Schema.Struct<{
        readonly command: Schema.Union<readonly [Schema.Literals<readonly ["session.list", "session.new", "session.interrupt", "session.background", "session.compact", "session.page.up", "session.page.down", "session.line.up", "session.line.down", "session.half.page.up", "session.half.page.down", "session.first", "session.last", "prompt.clear", "prompt.submit", "agent.cycle"]>, Schema.String]>;
    }>;
}> & {
    type: "tui.command.execute";
    durability: "ephemeral";
    durable: undefined;
    data: Schema.Struct<{
        readonly command: Schema.Union<readonly [Schema.Literals<readonly ["session.list", "session.new", "session.interrupt", "session.background", "session.compact", "session.page.up", "session.page.down", "session.line.up", "session.line.down", "session.half.page.up", "session.half.page.down", "session.first", "session.last", "prompt.clear", "prompt.submit", "agent.cycle"]>, Schema.String]>;
    }>;
};
export declare const ToastShow: Schema.Struct<{
    readonly id: Schema.brand<Schema.String, "Event.ID"> & {
        create: () => string & import("effect/Brand").Brand<"Event.ID">;
    };
    readonly created: Schema.Finite;
    readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
    readonly type: Schema.Literal<"tui.toast.show">;
    readonly location: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Struct<{
        readonly directory: Schema.brand<Schema.String, "AbsolutePath">;
        readonly workspaceID: Schema.decodeTo<Schema.optional<Schema.toType<Schema.brand<Schema.String, "Workspace.ID"> & {
            ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
            create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
        }>>, Schema.optionalKey<Schema.brand<Schema.String, "Workspace.ID"> & {
            ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
            create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
        }>, never, never>;
    }>>>, Schema.optionalKey<Schema.Struct<{
        readonly directory: Schema.brand<Schema.String, "AbsolutePath">;
        readonly workspaceID: Schema.decodeTo<Schema.optional<Schema.toType<Schema.brand<Schema.String, "Workspace.ID"> & {
            ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
            create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
        }>>, Schema.optionalKey<Schema.brand<Schema.String, "Workspace.ID"> & {
            ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
            create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
        }>, never, never>;
    }>>, never, never>;
    readonly data: Schema.Struct<{
        readonly title: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
        readonly message: Schema.String;
        readonly variant: Schema.Literals<readonly ["info", "success", "warning", "error"]>;
        readonly duration: Schema.withDecodingDefault<Schema.Int, never>;
    }>;
}> & {
    type: "tui.toast.show";
    durability: "ephemeral";
    durable: undefined;
    data: Schema.Struct<{
        readonly title: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
        readonly message: Schema.String;
        readonly variant: Schema.Literals<readonly ["info", "success", "warning", "error"]>;
        readonly duration: Schema.withDecodingDefault<Schema.Int, never>;
    }>;
};
export declare const SessionSelect: Schema.Struct<{
    readonly id: Schema.brand<Schema.String, "Event.ID"> & {
        create: () => string & import("effect/Brand").Brand<"Event.ID">;
    };
    readonly created: Schema.Finite;
    readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
    readonly type: Schema.Literal<"tui.session.select">;
    readonly location: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Struct<{
        readonly directory: Schema.brand<Schema.String, "AbsolutePath">;
        readonly workspaceID: Schema.decodeTo<Schema.optional<Schema.toType<Schema.brand<Schema.String, "Workspace.ID"> & {
            ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
            create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
        }>>, Schema.optionalKey<Schema.brand<Schema.String, "Workspace.ID"> & {
            ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
            create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
        }>, never, never>;
    }>>>, Schema.optionalKey<Schema.Struct<{
        readonly directory: Schema.brand<Schema.String, "AbsolutePath">;
        readonly workspaceID: Schema.decodeTo<Schema.optional<Schema.toType<Schema.brand<Schema.String, "Workspace.ID"> & {
            ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
            create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
        }>>, Schema.optionalKey<Schema.brand<Schema.String, "Workspace.ID"> & {
            ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
            create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
        }>, never, never>;
    }>>, never, never>;
    readonly data: Schema.Struct<{
        readonly sessionID: Schema.brand<Schema.String, "SessionID">;
    }>;
}> & {
    type: "tui.session.select";
    durability: "ephemeral";
    durable: undefined;
    data: Schema.Struct<{
        readonly sessionID: Schema.brand<Schema.String, "SessionID">;
    }>;
};
export declare const Definitions: readonly [Schema.Struct<{
    readonly id: Schema.brand<Schema.String, "Event.ID"> & {
        create: () => string & import("effect/Brand").Brand<"Event.ID">;
    };
    readonly created: Schema.Finite;
    readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
    readonly type: Schema.Literal<"tui.prompt.append">;
    readonly location: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Struct<{
        readonly directory: Schema.brand<Schema.String, "AbsolutePath">;
        readonly workspaceID: Schema.decodeTo<Schema.optional<Schema.toType<Schema.brand<Schema.String, "Workspace.ID"> & {
            ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
            create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
        }>>, Schema.optionalKey<Schema.brand<Schema.String, "Workspace.ID"> & {
            ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
            create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
        }>, never, never>;
    }>>>, Schema.optionalKey<Schema.Struct<{
        readonly directory: Schema.brand<Schema.String, "AbsolutePath">;
        readonly workspaceID: Schema.decodeTo<Schema.optional<Schema.toType<Schema.brand<Schema.String, "Workspace.ID"> & {
            ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
            create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
        }>>, Schema.optionalKey<Schema.brand<Schema.String, "Workspace.ID"> & {
            ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
            create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
        }>, never, never>;
    }>>, never, never>;
    readonly data: Schema.Struct<{
        readonly text: Schema.String;
    }>;
}> & {
    type: "tui.prompt.append";
    durability: "ephemeral";
    durable: undefined;
    data: Schema.Struct<{
        readonly text: Schema.String;
    }>;
}, Schema.Struct<{
    readonly id: Schema.brand<Schema.String, "Event.ID"> & {
        create: () => string & import("effect/Brand").Brand<"Event.ID">;
    };
    readonly created: Schema.Finite;
    readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
    readonly type: Schema.Literal<"tui.command.execute">;
    readonly location: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Struct<{
        readonly directory: Schema.brand<Schema.String, "AbsolutePath">;
        readonly workspaceID: Schema.decodeTo<Schema.optional<Schema.toType<Schema.brand<Schema.String, "Workspace.ID"> & {
            ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
            create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
        }>>, Schema.optionalKey<Schema.brand<Schema.String, "Workspace.ID"> & {
            ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
            create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
        }>, never, never>;
    }>>>, Schema.optionalKey<Schema.Struct<{
        readonly directory: Schema.brand<Schema.String, "AbsolutePath">;
        readonly workspaceID: Schema.decodeTo<Schema.optional<Schema.toType<Schema.brand<Schema.String, "Workspace.ID"> & {
            ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
            create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
        }>>, Schema.optionalKey<Schema.brand<Schema.String, "Workspace.ID"> & {
            ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
            create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
        }>, never, never>;
    }>>, never, never>;
    readonly data: Schema.Struct<{
        readonly command: Schema.Union<readonly [Schema.Literals<readonly ["session.list", "session.new", "session.interrupt", "session.background", "session.compact", "session.page.up", "session.page.down", "session.line.up", "session.line.down", "session.half.page.up", "session.half.page.down", "session.first", "session.last", "prompt.clear", "prompt.submit", "agent.cycle"]>, Schema.String]>;
    }>;
}> & {
    type: "tui.command.execute";
    durability: "ephemeral";
    durable: undefined;
    data: Schema.Struct<{
        readonly command: Schema.Union<readonly [Schema.Literals<readonly ["session.list", "session.new", "session.interrupt", "session.background", "session.compact", "session.page.up", "session.page.down", "session.line.up", "session.line.down", "session.half.page.up", "session.half.page.down", "session.first", "session.last", "prompt.clear", "prompt.submit", "agent.cycle"]>, Schema.String]>;
    }>;
}, Schema.Struct<{
    readonly id: Schema.brand<Schema.String, "Event.ID"> & {
        create: () => string & import("effect/Brand").Brand<"Event.ID">;
    };
    readonly created: Schema.Finite;
    readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
    readonly type: Schema.Literal<"tui.toast.show">;
    readonly location: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Struct<{
        readonly directory: Schema.brand<Schema.String, "AbsolutePath">;
        readonly workspaceID: Schema.decodeTo<Schema.optional<Schema.toType<Schema.brand<Schema.String, "Workspace.ID"> & {
            ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
            create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
        }>>, Schema.optionalKey<Schema.brand<Schema.String, "Workspace.ID"> & {
            ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
            create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
        }>, never, never>;
    }>>>, Schema.optionalKey<Schema.Struct<{
        readonly directory: Schema.brand<Schema.String, "AbsolutePath">;
        readonly workspaceID: Schema.decodeTo<Schema.optional<Schema.toType<Schema.brand<Schema.String, "Workspace.ID"> & {
            ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
            create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
        }>>, Schema.optionalKey<Schema.brand<Schema.String, "Workspace.ID"> & {
            ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
            create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
        }>, never, never>;
    }>>, never, never>;
    readonly data: Schema.Struct<{
        readonly title: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
        readonly message: Schema.String;
        readonly variant: Schema.Literals<readonly ["info", "success", "warning", "error"]>;
        readonly duration: Schema.withDecodingDefault<Schema.Int, never>;
    }>;
}> & {
    type: "tui.toast.show";
    durability: "ephemeral";
    durable: undefined;
    data: Schema.Struct<{
        readonly title: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
        readonly message: Schema.String;
        readonly variant: Schema.Literals<readonly ["info", "success", "warning", "error"]>;
        readonly duration: Schema.withDecodingDefault<Schema.Int, never>;
    }>;
}, Schema.Struct<{
    readonly id: Schema.brand<Schema.String, "Event.ID"> & {
        create: () => string & import("effect/Brand").Brand<"Event.ID">;
    };
    readonly created: Schema.Finite;
    readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
    readonly type: Schema.Literal<"tui.session.select">;
    readonly location: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Struct<{
        readonly directory: Schema.brand<Schema.String, "AbsolutePath">;
        readonly workspaceID: Schema.decodeTo<Schema.optional<Schema.toType<Schema.brand<Schema.String, "Workspace.ID"> & {
            ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
            create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
        }>>, Schema.optionalKey<Schema.brand<Schema.String, "Workspace.ID"> & {
            ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
            create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
        }>, never, never>;
    }>>>, Schema.optionalKey<Schema.Struct<{
        readonly directory: Schema.brand<Schema.String, "AbsolutePath">;
        readonly workspaceID: Schema.decodeTo<Schema.optional<Schema.toType<Schema.brand<Schema.String, "Workspace.ID"> & {
            ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
            create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
        }>>, Schema.optionalKey<Schema.brand<Schema.String, "Workspace.ID"> & {
            ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
            create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
        }>, never, never>;
    }>>, never, never>;
    readonly data: Schema.Struct<{
        readonly sessionID: Schema.brand<Schema.String, "SessionID">;
    }>;
}> & {
    type: "tui.session.select";
    durability: "ephemeral";
    durable: undefined;
    data: Schema.Struct<{
        readonly sessionID: Schema.brand<Schema.String, "SessionID">;
    }>;
}];
