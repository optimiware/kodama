export * as Shell from "./shell.js";
import { Schema } from "effect";
export declare const ID: Schema.brand<Schema.String, "Shell.ID"> & {
    create: () => string & import("effect/Brand").Brand<"Shell.ID">;
    ascending: (id?: string) => string & import("effect/Brand").Brand<"Shell.ID">;
};
export type ID = typeof ID.Type;
export declare const Status: Schema.Literals<readonly ["running", "exited", "timeout", "killed"]>;
export type Status = typeof Status.Type;
export declare const Time: Schema.Struct<{
    readonly started: Schema.Finite;
    readonly completed: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Finite>>, Schema.optionalKey<Schema.Finite>, never, never>;
}>;
export interface Time extends Schema.Schema.Type<typeof Time> {
}
export declare const Metadata: Schema.$Record<Schema.String, Schema.Unknown>;
export type Metadata = typeof Metadata.Type;
export declare const Info: Schema.Struct<{
    readonly id: Schema.brand<Schema.String, "Shell.ID"> & {
        create: () => string & import("effect/Brand").Brand<"Shell.ID">;
        ascending: (id?: string) => string & import("effect/Brand").Brand<"Shell.ID">;
    };
    readonly status: Schema.Literals<readonly ["running", "exited", "timeout", "killed"]>;
    readonly command: Schema.String;
    readonly cwd: Schema.String;
    readonly shell: Schema.String;
    readonly file: Schema.String;
    readonly pid: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Int>>, Schema.optionalKey<Schema.Int>, never, never>;
    readonly exit: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Finite>>, Schema.optionalKey<Schema.Finite>, never, never>;
    readonly signal: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly metadata: Schema.$Record<Schema.String, Schema.Unknown>;
    readonly time: Schema.Struct<{
        readonly started: Schema.Finite;
        readonly completed: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Finite>>, Schema.optionalKey<Schema.Finite>, never, never>;
    }>;
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
        readonly type: Schema.Literal<"shell.created">;
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
                readonly id: Schema.brand<Schema.String, "Shell.ID"> & {
                    create: () => string & import("effect/Brand").Brand<"Shell.ID">;
                    ascending: (id?: string) => string & import("effect/Brand").Brand<"Shell.ID">;
                };
                readonly status: Schema.Literals<readonly ["running", "exited", "timeout", "killed"]>;
                readonly command: Schema.String;
                readonly cwd: Schema.String;
                readonly shell: Schema.String;
                readonly file: Schema.String;
                readonly pid: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Int>>, Schema.optionalKey<Schema.Int>, never, never>;
                readonly exit: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Finite>>, Schema.optionalKey<Schema.Finite>, never, never>;
                readonly signal: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
                readonly metadata: Schema.$Record<Schema.String, Schema.Unknown>;
                readonly time: Schema.Struct<{
                    readonly started: Schema.Finite;
                    readonly completed: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Finite>>, Schema.optionalKey<Schema.Finite>, never, never>;
                }>;
            }>;
        }>;
    }> & {
        type: "shell.created";
        durability: "ephemeral";
        durable: undefined;
        data: Schema.Struct<{
            readonly info: Schema.Struct<{
                readonly id: Schema.brand<Schema.String, "Shell.ID"> & {
                    create: () => string & import("effect/Brand").Brand<"Shell.ID">;
                    ascending: (id?: string) => string & import("effect/Brand").Brand<"Shell.ID">;
                };
                readonly status: Schema.Literals<readonly ["running", "exited", "timeout", "killed"]>;
                readonly command: Schema.String;
                readonly cwd: Schema.String;
                readonly shell: Schema.String;
                readonly file: Schema.String;
                readonly pid: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Int>>, Schema.optionalKey<Schema.Int>, never, never>;
                readonly exit: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Finite>>, Schema.optionalKey<Schema.Finite>, never, never>;
                readonly signal: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
                readonly metadata: Schema.$Record<Schema.String, Schema.Unknown>;
                readonly time: Schema.Struct<{
                    readonly started: Schema.Finite;
                    readonly completed: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Finite>>, Schema.optionalKey<Schema.Finite>, never, never>;
                }>;
            }>;
        }>;
    };
    Exited: Schema.Struct<{
        readonly id: Schema.brand<Schema.String, "Event.ID"> & {
            create: () => string & import("effect/Brand").Brand<"Event.ID">;
        };
        readonly created: Schema.Finite;
        readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
        readonly type: Schema.Literal<"shell.exited">;
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
            readonly id: Schema.brand<Schema.String, "Shell.ID"> & {
                create: () => string & import("effect/Brand").Brand<"Shell.ID">;
                ascending: (id?: string) => string & import("effect/Brand").Brand<"Shell.ID">;
            };
            readonly exit: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Finite>>, Schema.optionalKey<Schema.Finite>, never, never>;
            readonly status: Schema.Literals<readonly ["running", "exited", "timeout", "killed"]>;
        }>;
    }> & {
        type: "shell.exited";
        durability: "ephemeral";
        durable: undefined;
        data: Schema.Struct<{
            readonly id: Schema.brand<Schema.String, "Shell.ID"> & {
                create: () => string & import("effect/Brand").Brand<"Shell.ID">;
                ascending: (id?: string) => string & import("effect/Brand").Brand<"Shell.ID">;
            };
            readonly exit: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Finite>>, Schema.optionalKey<Schema.Finite>, never, never>;
            readonly status: Schema.Literals<readonly ["running", "exited", "timeout", "killed"]>;
        }>;
    };
    Deleted: Schema.Struct<{
        readonly id: Schema.brand<Schema.String, "Event.ID"> & {
            create: () => string & import("effect/Brand").Brand<"Event.ID">;
        };
        readonly created: Schema.Finite;
        readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
        readonly type: Schema.Literal<"shell.deleted">;
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
            readonly id: Schema.brand<Schema.String, "Shell.ID"> & {
                create: () => string & import("effect/Brand").Brand<"Shell.ID">;
                ascending: (id?: string) => string & import("effect/Brand").Brand<"Shell.ID">;
            };
        }>;
    }> & {
        type: "shell.deleted";
        durability: "ephemeral";
        durable: undefined;
        data: Schema.Struct<{
            readonly id: Schema.brand<Schema.String, "Shell.ID"> & {
                create: () => string & import("effect/Brand").Brand<"Shell.ID">;
                ascending: (id?: string) => string & import("effect/Brand").Brand<"Shell.ID">;
            };
        }>;
    };
    Definitions: readonly [Schema.Struct<{
        readonly id: Schema.brand<Schema.String, "Event.ID"> & {
            create: () => string & import("effect/Brand").Brand<"Event.ID">;
        };
        readonly created: Schema.Finite;
        readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
        readonly type: Schema.Literal<"shell.created">;
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
                readonly id: Schema.brand<Schema.String, "Shell.ID"> & {
                    create: () => string & import("effect/Brand").Brand<"Shell.ID">;
                    ascending: (id?: string) => string & import("effect/Brand").Brand<"Shell.ID">;
                };
                readonly status: Schema.Literals<readonly ["running", "exited", "timeout", "killed"]>;
                readonly command: Schema.String;
                readonly cwd: Schema.String;
                readonly shell: Schema.String;
                readonly file: Schema.String;
                readonly pid: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Int>>, Schema.optionalKey<Schema.Int>, never, never>;
                readonly exit: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Finite>>, Schema.optionalKey<Schema.Finite>, never, never>;
                readonly signal: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
                readonly metadata: Schema.$Record<Schema.String, Schema.Unknown>;
                readonly time: Schema.Struct<{
                    readonly started: Schema.Finite;
                    readonly completed: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Finite>>, Schema.optionalKey<Schema.Finite>, never, never>;
                }>;
            }>;
        }>;
    }> & {
        type: "shell.created";
        durability: "ephemeral";
        durable: undefined;
        data: Schema.Struct<{
            readonly info: Schema.Struct<{
                readonly id: Schema.brand<Schema.String, "Shell.ID"> & {
                    create: () => string & import("effect/Brand").Brand<"Shell.ID">;
                    ascending: (id?: string) => string & import("effect/Brand").Brand<"Shell.ID">;
                };
                readonly status: Schema.Literals<readonly ["running", "exited", "timeout", "killed"]>;
                readonly command: Schema.String;
                readonly cwd: Schema.String;
                readonly shell: Schema.String;
                readonly file: Schema.String;
                readonly pid: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Int>>, Schema.optionalKey<Schema.Int>, never, never>;
                readonly exit: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Finite>>, Schema.optionalKey<Schema.Finite>, never, never>;
                readonly signal: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
                readonly metadata: Schema.$Record<Schema.String, Schema.Unknown>;
                readonly time: Schema.Struct<{
                    readonly started: Schema.Finite;
                    readonly completed: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Finite>>, Schema.optionalKey<Schema.Finite>, never, never>;
                }>;
            }>;
        }>;
    }, Schema.Struct<{
        readonly id: Schema.brand<Schema.String, "Event.ID"> & {
            create: () => string & import("effect/Brand").Brand<"Event.ID">;
        };
        readonly created: Schema.Finite;
        readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
        readonly type: Schema.Literal<"shell.exited">;
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
            readonly id: Schema.brand<Schema.String, "Shell.ID"> & {
                create: () => string & import("effect/Brand").Brand<"Shell.ID">;
                ascending: (id?: string) => string & import("effect/Brand").Brand<"Shell.ID">;
            };
            readonly exit: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Finite>>, Schema.optionalKey<Schema.Finite>, never, never>;
            readonly status: Schema.Literals<readonly ["running", "exited", "timeout", "killed"]>;
        }>;
    }> & {
        type: "shell.exited";
        durability: "ephemeral";
        durable: undefined;
        data: Schema.Struct<{
            readonly id: Schema.brand<Schema.String, "Shell.ID"> & {
                create: () => string & import("effect/Brand").Brand<"Shell.ID">;
                ascending: (id?: string) => string & import("effect/Brand").Brand<"Shell.ID">;
            };
            readonly exit: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Finite>>, Schema.optionalKey<Schema.Finite>, never, never>;
            readonly status: Schema.Literals<readonly ["running", "exited", "timeout", "killed"]>;
        }>;
    }, Schema.Struct<{
        readonly id: Schema.brand<Schema.String, "Event.ID"> & {
            create: () => string & import("effect/Brand").Brand<"Event.ID">;
        };
        readonly created: Schema.Finite;
        readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
        readonly type: Schema.Literal<"shell.deleted">;
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
            readonly id: Schema.brand<Schema.String, "Shell.ID"> & {
                create: () => string & import("effect/Brand").Brand<"Shell.ID">;
                ascending: (id?: string) => string & import("effect/Brand").Brand<"Shell.ID">;
            };
        }>;
    }> & {
        type: "shell.deleted";
        durability: "ephemeral";
        durable: undefined;
        data: Schema.Struct<{
            readonly id: Schema.brand<Schema.String, "Shell.ID"> & {
                create: () => string & import("effect/Brand").Brand<"Shell.ID">;
                ascending: (id?: string) => string & import("effect/Brand").Brand<"Shell.ID">;
            };
        }>;
    }];
};
export declare const CreateInput: Schema.Struct<{
    readonly command: Schema.String;
    readonly cwd: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly timeout: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Int>>, Schema.optionalKey<Schema.Int>, never, never>;
    readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
}>;
export interface CreateInput extends Schema.Schema.Type<typeof CreateInput> {
}
export declare const OutputInput: Schema.Struct<{
    readonly cursor: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Int>>, Schema.optionalKey<Schema.Int>, never, never>;
    readonly limit: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Int>>, Schema.optionalKey<Schema.Int>, never, never>;
}>;
export interface OutputInput extends Schema.Schema.Type<typeof OutputInput> {
}
export declare const Output: Schema.Struct<{
    readonly output: Schema.String;
    readonly cursor: Schema.Int;
    readonly size: Schema.Int;
    readonly truncated: Schema.Boolean;
}>;
export interface Output extends Schema.Schema.Type<typeof Output> {
}
