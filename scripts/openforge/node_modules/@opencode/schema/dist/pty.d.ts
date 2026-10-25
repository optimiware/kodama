export * as Pty from "./pty.js";
import { Schema } from "effect";
export declare const ID: Schema.brand<Schema.String, "PtyID"> & {
    create: () => string & import("effect/Brand").Brand<"PtyID">;
    ascending: (id?: string) => string & import("effect/Brand").Brand<"PtyID">;
};
export type ID = typeof ID.Type;
export declare const Info: Schema.Struct<{
    readonly id: Schema.brand<Schema.String, "PtyID"> & {
        create: () => string & import("effect/Brand").Brand<"PtyID">;
        ascending: (id?: string) => string & import("effect/Brand").Brand<"PtyID">;
    };
    readonly title: Schema.String;
    readonly command: Schema.String;
    readonly args: Schema.$Array<Schema.String>;
    readonly cwd: Schema.String;
    readonly status: Schema.Literals<readonly ["running", "exited"]>;
    readonly pid: Schema.Int;
    readonly exitCode: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Int>>, Schema.optionalKey<Schema.Int>, never, never>;
}>;
export interface Info extends Schema.Schema.Type<typeof Info> {
}
export declare const Event: {
    Created: Schema.Struct<{
        readonly id: Schema.brand<Schema.String, "Event.ID"> & {
            create: () => string & import("effect/Brand").Brand<"Event.ID">;
        };
        readonly created: Schema.Finite;
        readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
        readonly type: Schema.Literal<"pty.created">;
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
            readonly info: Schema.Struct<{
                readonly id: Schema.brand<Schema.String, "PtyID"> & {
                    create: () => string & import("effect/Brand").Brand<"PtyID">;
                    ascending: (id?: string) => string & import("effect/Brand").Brand<"PtyID">;
                };
                readonly title: Schema.String;
                readonly command: Schema.String;
                readonly args: Schema.$Array<Schema.String>;
                readonly cwd: Schema.String;
                readonly status: Schema.Literals<readonly ["running", "exited"]>;
                readonly pid: Schema.Int;
                readonly exitCode: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Int>>, Schema.optionalKey<Schema.Int>, never, never>;
            }>;
        }>;
    }> & {
        type: "pty.created";
        durability: "ephemeral";
        durable: undefined;
        data: Schema.Struct<{
            readonly info: Schema.Struct<{
                readonly id: Schema.brand<Schema.String, "PtyID"> & {
                    create: () => string & import("effect/Brand").Brand<"PtyID">;
                    ascending: (id?: string) => string & import("effect/Brand").Brand<"PtyID">;
                };
                readonly title: Schema.String;
                readonly command: Schema.String;
                readonly args: Schema.$Array<Schema.String>;
                readonly cwd: Schema.String;
                readonly status: Schema.Literals<readonly ["running", "exited"]>;
                readonly pid: Schema.Int;
                readonly exitCode: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Int>>, Schema.optionalKey<Schema.Int>, never, never>;
            }>;
        }>;
    };
    Updated: Schema.Struct<{
        readonly id: Schema.brand<Schema.String, "Event.ID"> & {
            create: () => string & import("effect/Brand").Brand<"Event.ID">;
        };
        readonly created: Schema.Finite;
        readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
        readonly type: Schema.Literal<"pty.updated">;
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
            readonly info: Schema.Struct<{
                readonly id: Schema.brand<Schema.String, "PtyID"> & {
                    create: () => string & import("effect/Brand").Brand<"PtyID">;
                    ascending: (id?: string) => string & import("effect/Brand").Brand<"PtyID">;
                };
                readonly title: Schema.String;
                readonly command: Schema.String;
                readonly args: Schema.$Array<Schema.String>;
                readonly cwd: Schema.String;
                readonly status: Schema.Literals<readonly ["running", "exited"]>;
                readonly pid: Schema.Int;
                readonly exitCode: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Int>>, Schema.optionalKey<Schema.Int>, never, never>;
            }>;
        }>;
    }> & {
        type: "pty.updated";
        durability: "ephemeral";
        durable: undefined;
        data: Schema.Struct<{
            readonly info: Schema.Struct<{
                readonly id: Schema.brand<Schema.String, "PtyID"> & {
                    create: () => string & import("effect/Brand").Brand<"PtyID">;
                    ascending: (id?: string) => string & import("effect/Brand").Brand<"PtyID">;
                };
                readonly title: Schema.String;
                readonly command: Schema.String;
                readonly args: Schema.$Array<Schema.String>;
                readonly cwd: Schema.String;
                readonly status: Schema.Literals<readonly ["running", "exited"]>;
                readonly pid: Schema.Int;
                readonly exitCode: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Int>>, Schema.optionalKey<Schema.Int>, never, never>;
            }>;
        }>;
    };
    Exited: Schema.Struct<{
        readonly id: Schema.brand<Schema.String, "Event.ID"> & {
            create: () => string & import("effect/Brand").Brand<"Event.ID">;
        };
        readonly created: Schema.Finite;
        readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
        readonly type: Schema.Literal<"pty.exited">;
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
            readonly id: Schema.brand<Schema.String, "PtyID"> & {
                create: () => string & import("effect/Brand").Brand<"PtyID">;
                ascending: (id?: string) => string & import("effect/Brand").Brand<"PtyID">;
            };
            readonly exitCode: Schema.Int;
        }>;
    }> & {
        type: "pty.exited";
        durability: "ephemeral";
        durable: undefined;
        data: Schema.Struct<{
            readonly id: Schema.brand<Schema.String, "PtyID"> & {
                create: () => string & import("effect/Brand").Brand<"PtyID">;
                ascending: (id?: string) => string & import("effect/Brand").Brand<"PtyID">;
            };
            readonly exitCode: Schema.Int;
        }>;
    };
    Deleted: Schema.Struct<{
        readonly id: Schema.brand<Schema.String, "Event.ID"> & {
            create: () => string & import("effect/Brand").Brand<"Event.ID">;
        };
        readonly created: Schema.Finite;
        readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
        readonly type: Schema.Literal<"pty.deleted">;
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
            readonly id: Schema.brand<Schema.String, "PtyID"> & {
                create: () => string & import("effect/Brand").Brand<"PtyID">;
                ascending: (id?: string) => string & import("effect/Brand").Brand<"PtyID">;
            };
        }>;
    }> & {
        type: "pty.deleted";
        durability: "ephemeral";
        durable: undefined;
        data: Schema.Struct<{
            readonly id: Schema.brand<Schema.String, "PtyID"> & {
                create: () => string & import("effect/Brand").Brand<"PtyID">;
                ascending: (id?: string) => string & import("effect/Brand").Brand<"PtyID">;
            };
        }>;
    };
    Definitions: readonly [Schema.Struct<{
        readonly id: Schema.brand<Schema.String, "Event.ID"> & {
            create: () => string & import("effect/Brand").Brand<"Event.ID">;
        };
        readonly created: Schema.Finite;
        readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
        readonly type: Schema.Literal<"pty.created">;
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
            readonly info: Schema.Struct<{
                readonly id: Schema.brand<Schema.String, "PtyID"> & {
                    create: () => string & import("effect/Brand").Brand<"PtyID">;
                    ascending: (id?: string) => string & import("effect/Brand").Brand<"PtyID">;
                };
                readonly title: Schema.String;
                readonly command: Schema.String;
                readonly args: Schema.$Array<Schema.String>;
                readonly cwd: Schema.String;
                readonly status: Schema.Literals<readonly ["running", "exited"]>;
                readonly pid: Schema.Int;
                readonly exitCode: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Int>>, Schema.optionalKey<Schema.Int>, never, never>;
            }>;
        }>;
    }> & {
        type: "pty.created";
        durability: "ephemeral";
        durable: undefined;
        data: Schema.Struct<{
            readonly info: Schema.Struct<{
                readonly id: Schema.brand<Schema.String, "PtyID"> & {
                    create: () => string & import("effect/Brand").Brand<"PtyID">;
                    ascending: (id?: string) => string & import("effect/Brand").Brand<"PtyID">;
                };
                readonly title: Schema.String;
                readonly command: Schema.String;
                readonly args: Schema.$Array<Schema.String>;
                readonly cwd: Schema.String;
                readonly status: Schema.Literals<readonly ["running", "exited"]>;
                readonly pid: Schema.Int;
                readonly exitCode: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Int>>, Schema.optionalKey<Schema.Int>, never, never>;
            }>;
        }>;
    }, Schema.Struct<{
        readonly id: Schema.brand<Schema.String, "Event.ID"> & {
            create: () => string & import("effect/Brand").Brand<"Event.ID">;
        };
        readonly created: Schema.Finite;
        readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
        readonly type: Schema.Literal<"pty.updated">;
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
            readonly info: Schema.Struct<{
                readonly id: Schema.brand<Schema.String, "PtyID"> & {
                    create: () => string & import("effect/Brand").Brand<"PtyID">;
                    ascending: (id?: string) => string & import("effect/Brand").Brand<"PtyID">;
                };
                readonly title: Schema.String;
                readonly command: Schema.String;
                readonly args: Schema.$Array<Schema.String>;
                readonly cwd: Schema.String;
                readonly status: Schema.Literals<readonly ["running", "exited"]>;
                readonly pid: Schema.Int;
                readonly exitCode: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Int>>, Schema.optionalKey<Schema.Int>, never, never>;
            }>;
        }>;
    }> & {
        type: "pty.updated";
        durability: "ephemeral";
        durable: undefined;
        data: Schema.Struct<{
            readonly info: Schema.Struct<{
                readonly id: Schema.brand<Schema.String, "PtyID"> & {
                    create: () => string & import("effect/Brand").Brand<"PtyID">;
                    ascending: (id?: string) => string & import("effect/Brand").Brand<"PtyID">;
                };
                readonly title: Schema.String;
                readonly command: Schema.String;
                readonly args: Schema.$Array<Schema.String>;
                readonly cwd: Schema.String;
                readonly status: Schema.Literals<readonly ["running", "exited"]>;
                readonly pid: Schema.Int;
                readonly exitCode: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Int>>, Schema.optionalKey<Schema.Int>, never, never>;
            }>;
        }>;
    }, Schema.Struct<{
        readonly id: Schema.brand<Schema.String, "Event.ID"> & {
            create: () => string & import("effect/Brand").Brand<"Event.ID">;
        };
        readonly created: Schema.Finite;
        readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
        readonly type: Schema.Literal<"pty.exited">;
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
            readonly id: Schema.brand<Schema.String, "PtyID"> & {
                create: () => string & import("effect/Brand").Brand<"PtyID">;
                ascending: (id?: string) => string & import("effect/Brand").Brand<"PtyID">;
            };
            readonly exitCode: Schema.Int;
        }>;
    }> & {
        type: "pty.exited";
        durability: "ephemeral";
        durable: undefined;
        data: Schema.Struct<{
            readonly id: Schema.brand<Schema.String, "PtyID"> & {
                create: () => string & import("effect/Brand").Brand<"PtyID">;
                ascending: (id?: string) => string & import("effect/Brand").Brand<"PtyID">;
            };
            readonly exitCode: Schema.Int;
        }>;
    }, Schema.Struct<{
        readonly id: Schema.brand<Schema.String, "Event.ID"> & {
            create: () => string & import("effect/Brand").Brand<"Event.ID">;
        };
        readonly created: Schema.Finite;
        readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
        readonly type: Schema.Literal<"pty.deleted">;
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
            readonly id: Schema.brand<Schema.String, "PtyID"> & {
                create: () => string & import("effect/Brand").Brand<"PtyID">;
                ascending: (id?: string) => string & import("effect/Brand").Brand<"PtyID">;
            };
        }>;
    }> & {
        type: "pty.deleted";
        durability: "ephemeral";
        durable: undefined;
        data: Schema.Struct<{
            readonly id: Schema.brand<Schema.String, "PtyID"> & {
                create: () => string & import("effect/Brand").Brand<"PtyID">;
                ascending: (id?: string) => string & import("effect/Brand").Brand<"PtyID">;
            };
        }>;
    }];
};
export declare const CreateInput: Schema.Struct<{
    readonly command: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly args: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Array<Schema.String>>>, Schema.optionalKey<Schema.$Array<Schema.String>>, never, never>;
    readonly cwd: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly title: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly env: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.String>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.String>>, never, never>;
}>;
export interface CreateInput extends Schema.Schema.Type<typeof CreateInput> {
}
export declare const UpdateInput: Schema.Struct<{
    readonly title: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly size: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Struct<{
        readonly rows: Schema.Int;
        readonly cols: Schema.Int;
    }>>>, Schema.optionalKey<Schema.Struct<{
        readonly rows: Schema.Int;
        readonly cols: Schema.Int;
    }>>, never, never>;
}>;
export interface UpdateInput extends Schema.Schema.Type<typeof UpdateInput> {
}
