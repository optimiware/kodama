import { Effect, Schema } from "effect";
import { Rpc, RpcGroup } from "effect/rpc";
export declare namespace JsonRpc {
    const RequestFields: {
        jsonrpc: Schema.Literal<"2.0">;
        id: Schema.optional<Schema.Union<readonly [Schema.String, Schema.Number, Schema.Null]>>;
    };
    const Request: Schema.Struct<{
        readonly jsonrpc: Schema.Literal<"2.0">;
        readonly id: Schema.optional<Schema.Union<readonly [Schema.String, Schema.Number, Schema.Null]>>;
        readonly method: Schema.String;
        readonly params: Schema.optional<Schema.Codec<Schema.Json, Schema.Json, never, never>>;
    }>;
    interface Request extends Schema.Schema.Type<typeof Request> {
    }
    const ErrorObject: Schema.Struct<{
        readonly code: Schema.Number;
        readonly message: Schema.String;
        readonly data: Schema.optional<Schema.Codec<Schema.Json, Schema.Json, never, never>>;
    }>;
    const Response: Schema.Union<readonly [Schema.Struct<{
        readonly jsonrpc: Schema.Literal<"2.0">;
        readonly id: Schema.Union<readonly [Schema.String, Schema.Number, Schema.Null]>;
        readonly result: Schema.Codec<Schema.Json, Schema.Json, never, never>;
        readonly error: Schema.optionalKey<Schema.Never>;
    }>, Schema.Struct<{
        readonly jsonrpc: Schema.Literal<"2.0">;
        readonly id: Schema.Union<readonly [Schema.String, Schema.Number, Schema.Null]>;
        readonly result: Schema.optionalKey<Schema.Never>;
        readonly error: Schema.Struct<{
            readonly code: Schema.Number;
            readonly message: Schema.String;
            readonly data: Schema.optional<Schema.Codec<Schema.Json, Schema.Json, never, never>>;
        }>;
    }>]>;
    type Response = Schema.Schema.Type<typeof Response>;
    const decodeRequest: (input: unknown, options?: import("effect/SchemaAST").ParseOptions) => {
        readonly jsonrpc: "2.0";
        readonly id?: string | number | null | undefined;
        readonly method: string;
        readonly params?: Schema.Json | undefined;
    };
    function success(id: Request["id"], result: unknown): Response | undefined;
    function failure(id: Request["id"], error: unknown): Response;
}
declare const SimulationRequestError_base: Schema.Class<SimulationRequestError, Schema.TaggedStruct<"SimulationRequestError", {
    readonly method: Schema.String;
    readonly code: Schema.Number;
    readonly message: Schema.String;
    readonly data: Schema.optionalKey<Schema.Codec<Schema.Json, Schema.Json, never, never>>;
}>, import("effect/Cause").YieldableError>;
export declare class SimulationRequestError extends SimulationRequestError_base {
}
export declare namespace Handshake {
    export const ProtocolVersion: Schema.Literal<1>;
    export type ProtocolVersion = Schema.Schema.Type<typeof ProtocolVersion>;
    export const Capability: Schema.NonEmptyString;
    export type Capability = Schema.Schema.Type<typeof Capability>;
    export const EndpointRole: Schema.Literals<readonly ["ui", "backend"]>;
    export type EndpointRole = Schema.Schema.Type<typeof EndpointRole>;
    export const Identity: Schema.Struct<{
        readonly name: Schema.NonEmptyString;
        readonly version: Schema.NonEmptyString;
    }>;
    export interface Identity extends Schema.Schema.Type<typeof Identity> {
    }
    export const Params: Schema.Struct<{
        readonly client: Schema.Struct<{
            readonly name: Schema.NonEmptyString;
            readonly version: Schema.NonEmptyString;
        }>;
        readonly expectedRole: Schema.Literals<readonly ["ui", "backend"]>;
        readonly offeredVersions: Schema.$Array<Schema.Int>;
        readonly requiredCapabilities: Schema.$Array<Schema.NonEmptyString>;
        readonly optionalCapabilities: Schema.$Array<Schema.NonEmptyString>;
    }>;
    export interface Params extends Schema.Schema.Type<typeof Params> {
    }
    export const Response: Schema.Struct<{
        readonly protocolVersion: Schema.Literal<1>;
        readonly role: Schema.Literals<readonly ["ui", "backend"]>;
        readonly server: Schema.Struct<{
            readonly name: Schema.NonEmptyString;
            readonly version: Schema.NonEmptyString;
        }>;
        readonly capabilities: Schema.$Array<Schema.NonEmptyString>;
    }>;
    export interface Response extends Schema.Schema.Type<typeof Response> {
    }
    export const Request: Schema.Struct<{
        readonly jsonrpc: Schema.Literal<"2.0">;
        readonly id: Schema.optional<Schema.Union<readonly [Schema.String, Schema.Number, Schema.Null]>>;
        readonly method: Schema.Literal<"simulation.handshake">;
        readonly params: Schema.Struct<{
            readonly client: Schema.Struct<{
                readonly name: Schema.NonEmptyString;
                readonly version: Schema.NonEmptyString;
            }>;
            readonly expectedRole: Schema.Literals<readonly ["ui", "backend"]>;
            readonly offeredVersions: Schema.$Array<Schema.Int>;
            readonly requiredCapabilities: Schema.$Array<Schema.NonEmptyString>;
            readonly optionalCapabilities: Schema.$Array<Schema.NonEmptyString>;
        }>;
    }>;
    export interface Request extends Schema.Schema.Type<typeof Request> {
    }
    export interface DispatchAction {
        readonly role: EndpointRole;
        readonly server: Identity;
        readonly capabilities: ReadonlyArray<Capability>;
    }
    const RoleMismatchError_base: Schema.Class<RoleMismatchError, Schema.TaggedStruct<"SimulationHandshake.RoleMismatchError", {
        readonly expected: Schema.Literals<readonly ["ui", "backend"]>;
        readonly actual: Schema.Literals<readonly ["ui", "backend"]>;
        readonly message: Schema.String;
    }>, import("effect/Cause").YieldableError>;
    export class RoleMismatchError extends RoleMismatchError_base {
    }
    const UnsupportedProtocolError_base: Schema.Class<UnsupportedProtocolError, Schema.TaggedStruct<"SimulationHandshake.UnsupportedProtocolError", {
        readonly offered: Schema.$Array<Schema.Number>;
        readonly supported: Schema.$Array<Schema.Literal<1>>;
        readonly message: Schema.String;
    }>, import("effect/Cause").YieldableError>;
    export class UnsupportedProtocolError extends UnsupportedProtocolError_base {
    }
    const MissingCapabilityError_base: Schema.Class<MissingCapabilityError, Schema.TaggedStruct<"SimulationHandshake.MissingCapabilityError", {
        readonly missing: Schema.$Array<Schema.NonEmptyString>;
        readonly message: Schema.String;
    }>, import("effect/Cause").YieldableError>;
    export class MissingCapabilityError extends MissingCapabilityError_base {
    }
    export function dispatch(action: DispatchAction, params: Params): Effect.Effect<{
        protocolVersion: 1;
        role: "backend" | "ui";
        server: Identity;
        capabilities: string[];
    }, MissingCapabilityError | RoleMismatchError | UnsupportedProtocolError, never>;
    export {};
}
export declare namespace Frontend {
    const Capabilities: readonly ["ui.type", "ui.press", "ui.enter", "ui.arrow", "ui.focus", "ui.click", "ui.click.semantic", "ui.mouse", "ui.recording.pointer", "ui.resize", "ui.matches", "ui.state", "ui.snapshot", "ui.capture", "ui.recording.finish"];
    type Capability = (typeof Capabilities)[number];
    const KeyModifiers: Schema.Struct<{
        readonly ctrl: Schema.optional<Schema.Boolean>;
        readonly shift: Schema.optional<Schema.Boolean>;
        readonly meta: Schema.optional<Schema.Boolean>;
        readonly super: Schema.optional<Schema.Boolean>;
        readonly hyper: Schema.optional<Schema.Boolean>;
    }>;
    interface KeyModifiers extends Schema.Schema.Type<typeof KeyModifiers> {
    }
    const SemanticClickTarget: Schema.Struct<{
        readonly id: Schema.NonEmptyString;
        readonly instance: Schema.optionalKey<Schema.NonEmptyString>;
        readonly element: Schema.Number;
    }>;
    interface SemanticClickTarget extends Schema.Schema.Type<typeof SemanticClickTarget> {
    }
    const MouseParams: Schema.Union<readonly [Schema.Struct<{
        readonly x: Schema.Int;
        readonly y: Schema.Int;
        readonly modifiers: Schema.optionalKey<Schema.Struct<{
            readonly shift: Schema.optionalKey<Schema.Boolean>;
            readonly alt: Schema.optionalKey<Schema.Boolean>;
            readonly ctrl: Schema.optionalKey<Schema.Boolean>;
        }>>;
        readonly action: Schema.Literal<"move">;
    }>, Schema.Struct<{
        readonly x: Schema.Int;
        readonly y: Schema.Int;
        readonly modifiers: Schema.optionalKey<Schema.Struct<{
            readonly shift: Schema.optionalKey<Schema.Boolean>;
            readonly alt: Schema.optionalKey<Schema.Boolean>;
            readonly ctrl: Schema.optionalKey<Schema.Boolean>;
        }>>;
        readonly action: Schema.Literals<readonly ["down", "up"]>;
        readonly button: Schema.optionalKey<Schema.Literals<readonly ["left", "middle", "right"]>>;
    }>, Schema.Struct<{
        readonly x: Schema.Int;
        readonly y: Schema.Int;
        readonly modifiers: Schema.optionalKey<Schema.Struct<{
            readonly shift: Schema.optionalKey<Schema.Boolean>;
            readonly alt: Schema.optionalKey<Schema.Boolean>;
            readonly ctrl: Schema.optionalKey<Schema.Boolean>;
        }>>;
        readonly action: Schema.Literal<"scroll">;
        readonly direction: Schema.Literals<readonly ["up", "down", "left", "right"]>;
    }>]>;
    type MouseParams = Schema.Schema.Type<typeof MouseParams>;
    const Action: Schema.Union<readonly [Schema.Struct<{
        readonly type: Schema.Literal<"ui.type">;
        readonly text: Schema.String;
    }>, Schema.Struct<{
        readonly type: Schema.Literal<"ui.press">;
        readonly key: Schema.String;
        readonly modifiers: Schema.optional<Schema.Struct<{
            readonly ctrl: Schema.optional<Schema.Boolean>;
            readonly shift: Schema.optional<Schema.Boolean>;
            readonly meta: Schema.optional<Schema.Boolean>;
            readonly super: Schema.optional<Schema.Boolean>;
            readonly hyper: Schema.optional<Schema.Boolean>;
        }>>;
    }>, Schema.Struct<{
        readonly type: Schema.Literal<"ui.enter">;
    }>, Schema.Struct<{
        readonly type: Schema.Literal<"ui.arrow">;
        readonly direction: Schema.Literals<readonly ["up", "down", "left", "right"]>;
    }>, Schema.Struct<{
        readonly type: Schema.Literal<"ui.focus">;
        readonly target: Schema.Number;
    }>, Schema.Struct<{
        readonly type: Schema.Literal<"ui.mouse">;
        readonly params: Schema.Union<readonly [Schema.Struct<{
            readonly x: Schema.Int;
            readonly y: Schema.Int;
            readonly modifiers: Schema.optionalKey<Schema.Struct<{
                readonly shift: Schema.optionalKey<Schema.Boolean>;
                readonly alt: Schema.optionalKey<Schema.Boolean>;
                readonly ctrl: Schema.optionalKey<Schema.Boolean>;
            }>>;
            readonly action: Schema.Literal<"move">;
        }>, Schema.Struct<{
            readonly x: Schema.Int;
            readonly y: Schema.Int;
            readonly modifiers: Schema.optionalKey<Schema.Struct<{
                readonly shift: Schema.optionalKey<Schema.Boolean>;
                readonly alt: Schema.optionalKey<Schema.Boolean>;
                readonly ctrl: Schema.optionalKey<Schema.Boolean>;
            }>>;
            readonly action: Schema.Literals<readonly ["down", "up"]>;
            readonly button: Schema.optionalKey<Schema.Literals<readonly ["left", "middle", "right"]>>;
        }>, Schema.Struct<{
            readonly x: Schema.Int;
            readonly y: Schema.Int;
            readonly modifiers: Schema.optionalKey<Schema.Struct<{
                readonly shift: Schema.optionalKey<Schema.Boolean>;
                readonly alt: Schema.optionalKey<Schema.Boolean>;
                readonly ctrl: Schema.optionalKey<Schema.Boolean>;
            }>>;
            readonly action: Schema.Literal<"scroll">;
            readonly direction: Schema.Literals<readonly ["up", "down", "left", "right"]>;
        }>]>;
    }>, Schema.Struct<{
        readonly type: Schema.Literal<"ui.click">;
        readonly target: Schema.Number;
        readonly x: Schema.Number;
        readonly y: Schema.Number;
        readonly semantic: Schema.optionalKey<Schema.Struct<{
            readonly id: Schema.NonEmptyString;
            readonly instance: Schema.optionalKey<Schema.NonEmptyString>;
            readonly element: Schema.Number;
        }>>;
    }>, Schema.Struct<{
        readonly type: Schema.Literal<"ui.resize">;
        readonly cols: Schema.Number;
        readonly rows: Schema.Number;
    }>]>;
    type Action = Schema.Schema.Type<typeof Action>;
    const Element: Schema.Struct<{
        readonly id: Schema.String;
        readonly num: Schema.Number;
        readonly x: Schema.Number;
        readonly y: Schema.Number;
        readonly width: Schema.Number;
        readonly height: Schema.Number;
        readonly focusable: Schema.Boolean;
        readonly focused: Schema.Boolean;
        readonly clickable: Schema.Boolean;
        readonly editor: Schema.Boolean;
    }>;
    interface Element extends Schema.Schema.Type<typeof Element> {
    }
    const State: Schema.Struct<{
        readonly focused: Schema.Struct<{
            readonly renderable: Schema.optional<Schema.Number>;
            readonly editor: Schema.Boolean;
        }>;
        readonly elements: Schema.$Array<Schema.Struct<{
            readonly id: Schema.String;
            readonly num: Schema.Number;
            readonly x: Schema.Number;
            readonly y: Schema.Number;
            readonly width: Schema.Number;
            readonly height: Schema.Number;
            readonly focusable: Schema.Boolean;
            readonly focused: Schema.Boolean;
            readonly clickable: Schema.Boolean;
            readonly editor: Schema.Boolean;
        }>>;
    }>;
    interface State extends Schema.Schema.Type<typeof State> {
    }
    const SemanticNode: Schema.Struct<{
        readonly id: Schema.NonEmptyString;
        readonly instance: Schema.optionalKey<Schema.NonEmptyString>;
        readonly parent: Schema.optionalKey<Schema.NonEmptyString>;
        readonly role: Schema.NonEmptyString;
        readonly label: Schema.optionalKey<Schema.NonEmptyString>;
        readonly element: Schema.Number;
        readonly focused: Schema.optionalKey<Schema.Boolean>;
        readonly selected: Schema.optionalKey<Schema.Boolean>;
        readonly expanded: Schema.optionalKey<Schema.Boolean>;
        readonly disabled: Schema.optionalKey<Schema.Boolean>;
    }>;
    interface SemanticNode extends Schema.Schema.Type<typeof SemanticNode> {
    }
    const SemanticSnapshot: Schema.Struct<{
        readonly format: Schema.Literal<"opencode-ui-snapshot-v1">;
        readonly nodes: Schema.$Array<Schema.Struct<{
            readonly id: Schema.NonEmptyString;
            readonly instance: Schema.optionalKey<Schema.NonEmptyString>;
            readonly parent: Schema.optionalKey<Schema.NonEmptyString>;
            readonly role: Schema.NonEmptyString;
            readonly label: Schema.optionalKey<Schema.NonEmptyString>;
            readonly element: Schema.Number;
            readonly focused: Schema.optionalKey<Schema.Boolean>;
            readonly selected: Schema.optionalKey<Schema.Boolean>;
            readonly expanded: Schema.optionalKey<Schema.Boolean>;
            readonly disabled: Schema.optionalKey<Schema.Boolean>;
        }>>;
    }>;
    interface SemanticSnapshot extends Schema.Schema.Type<typeof SemanticSnapshot> {
    }
    const Color: Schema.Tuple<readonly [Schema.Number, Schema.Number, Schema.Number, Schema.Number]>;
    type Color = Schema.Schema.Type<typeof Color>;
    const CapturedImage: Schema.Struct<{
        readonly x: Schema.Number;
        readonly y: Schema.Number;
        readonly width: Schema.Number;
        readonly height: Schema.Number;
        readonly pixelWidth: Schema.Number;
        readonly pixelHeight: Schema.Number;
        readonly rgba: Schema.String;
    }>;
    interface CapturedImage extends Schema.Schema.Type<typeof CapturedImage> {
    }
    const CapturedFrame: Schema.Struct<{
        readonly cols: Schema.Number;
        readonly rows: Schema.Number;
        readonly cursor: Schema.Tuple<readonly [Schema.Number, Schema.Number]>;
        readonly lines: Schema.$Array<Schema.Struct<{
            readonly spans: Schema.$Array<Schema.Struct<{
                readonly text: Schema.String;
                readonly fg: Schema.Tuple<readonly [Schema.Number, Schema.Number, Schema.Number, Schema.Number]>;
                readonly bg: Schema.Tuple<readonly [Schema.Number, Schema.Number, Schema.Number, Schema.Number]>;
                readonly attributes: Schema.Number;
                readonly width: Schema.Number;
            }>>;
        }>>;
        readonly images: Schema.optionalKey<Schema.$Array<Schema.Struct<{
            readonly x: Schema.Number;
            readonly y: Schema.Number;
            readonly width: Schema.Number;
            readonly height: Schema.Number;
            readonly pixelWidth: Schema.Number;
            readonly pixelHeight: Schema.Number;
            readonly rgba: Schema.String;
        }>>>;
    }>;
    interface CapturedFrame extends Schema.Schema.Type<typeof CapturedFrame> {
    }
    const RecordingFinish: Schema.String;
    type RecordingFinish = Schema.Schema.Type<typeof RecordingFinish>;
    const Matches: Schema.Boolean;
    type Matches = Schema.Schema.Type<typeof Matches>;
    const TypeParams: Schema.Struct<{
        readonly text: Schema.String;
    }>;
    interface TypeParams extends Schema.Schema.Type<typeof TypeParams> {
    }
    const MatchesParams: Schema.Struct<{
        readonly text: Schema.String;
    }>;
    interface MatchesParams extends Schema.Schema.Type<typeof MatchesParams> {
    }
    const PressParams: Schema.Struct<{
        readonly key: Schema.String;
        readonly modifiers: Schema.optional<Schema.Struct<{
            readonly ctrl: Schema.optional<Schema.Boolean>;
            readonly shift: Schema.optional<Schema.Boolean>;
            readonly meta: Schema.optional<Schema.Boolean>;
            readonly super: Schema.optional<Schema.Boolean>;
            readonly hyper: Schema.optional<Schema.Boolean>;
        }>>;
    }>;
    interface PressParams extends Schema.Schema.Type<typeof PressParams> {
    }
    const pressParams: (key: string, modifiers?: KeyModifiers) => PressParams;
    const ArrowParams: Schema.Struct<{
        readonly direction: Schema.Literals<readonly ["up", "down", "left", "right"]>;
    }>;
    interface ArrowParams extends Schema.Schema.Type<typeof ArrowParams> {
    }
    const FocusParams: Schema.Struct<{
        readonly target: Schema.Number;
    }>;
    interface FocusParams extends Schema.Schema.Type<typeof FocusParams> {
    }
    const ClickParams: Schema.Struct<{
        readonly target: Schema.Number;
        readonly x: Schema.Number;
        readonly y: Schema.Number;
        readonly semantic: Schema.optionalKey<Schema.Struct<{
            readonly id: Schema.NonEmptyString;
            readonly instance: Schema.optionalKey<Schema.NonEmptyString>;
            readonly element: Schema.Number;
        }>>;
    }>;
    interface ClickParams extends Schema.Schema.Type<typeof ClickParams> {
    }
    const ResizeParams: Schema.Struct<{
        readonly cols: Schema.Number;
        readonly rows: Schema.Number;
    }>;
    interface ResizeParams extends Schema.Schema.Type<typeof ResizeParams> {
    }
    const Request: Schema.Union<readonly [Schema.Struct<{
        readonly jsonrpc: Schema.Literal<"2.0">;
        readonly id: Schema.optional<Schema.Union<readonly [Schema.String, Schema.Number, Schema.Null]>>;
        readonly method: Schema.Literal<"simulation.handshake">;
        readonly params: Schema.Struct<{
            readonly client: Schema.Struct<{
                readonly name: Schema.NonEmptyString;
                readonly version: Schema.NonEmptyString;
            }>;
            readonly expectedRole: Schema.Literals<readonly ["ui", "backend"]>;
            readonly offeredVersions: Schema.$Array<Schema.Int>;
            readonly requiredCapabilities: Schema.$Array<Schema.NonEmptyString>;
            readonly optionalCapabilities: Schema.$Array<Schema.NonEmptyString>;
        }>;
    }>, Schema.Struct<{
        readonly jsonrpc: Schema.Literal<"2.0">;
        readonly id: Schema.optional<Schema.Union<readonly [Schema.String, Schema.Number, Schema.Null]>>;
        readonly method: Schema.Literal<"ui.type">;
        readonly params: Schema.Struct<{
            readonly text: Schema.String;
        }>;
    }>, Schema.Struct<{
        readonly jsonrpc: Schema.Literal<"2.0">;
        readonly id: Schema.optional<Schema.Union<readonly [Schema.String, Schema.Number, Schema.Null]>>;
        readonly method: Schema.Literal<"ui.press">;
        readonly params: Schema.Struct<{
            readonly key: Schema.String;
            readonly modifiers: Schema.optional<Schema.Struct<{
                readonly ctrl: Schema.optional<Schema.Boolean>;
                readonly shift: Schema.optional<Schema.Boolean>;
                readonly meta: Schema.optional<Schema.Boolean>;
                readonly super: Schema.optional<Schema.Boolean>;
                readonly hyper: Schema.optional<Schema.Boolean>;
            }>>;
        }>;
    }>, Schema.Struct<{
        readonly jsonrpc: Schema.Literal<"2.0">;
        readonly id: Schema.optional<Schema.Union<readonly [Schema.String, Schema.Number, Schema.Null]>>;
        readonly method: Schema.Literal<"ui.arrow">;
        readonly params: Schema.Struct<{
            readonly direction: Schema.Literals<readonly ["up", "down", "left", "right"]>;
        }>;
    }>, Schema.Struct<{
        readonly jsonrpc: Schema.Literal<"2.0">;
        readonly id: Schema.optional<Schema.Union<readonly [Schema.String, Schema.Number, Schema.Null]>>;
        readonly method: Schema.Literal<"ui.focus">;
        readonly params: Schema.Struct<{
            readonly target: Schema.Number;
        }>;
    }>, Schema.Struct<{
        readonly jsonrpc: Schema.Literal<"2.0">;
        readonly id: Schema.optional<Schema.Union<readonly [Schema.String, Schema.Number, Schema.Null]>>;
        readonly method: Schema.Literal<"ui.click">;
        readonly params: Schema.Struct<{
            readonly target: Schema.Number;
            readonly x: Schema.Number;
            readonly y: Schema.Number;
            readonly semantic: Schema.optionalKey<Schema.Struct<{
                readonly id: Schema.NonEmptyString;
                readonly instance: Schema.optionalKey<Schema.NonEmptyString>;
                readonly element: Schema.Number;
            }>>;
        }>;
    }>, Schema.Struct<{
        readonly jsonrpc: Schema.Literal<"2.0">;
        readonly id: Schema.optional<Schema.Union<readonly [Schema.String, Schema.Number, Schema.Null]>>;
        readonly method: Schema.Literal<"ui.mouse">;
        readonly params: Schema.Union<readonly [Schema.Struct<{
            readonly x: Schema.Int;
            readonly y: Schema.Int;
            readonly modifiers: Schema.optionalKey<Schema.Struct<{
                readonly shift: Schema.optionalKey<Schema.Boolean>;
                readonly alt: Schema.optionalKey<Schema.Boolean>;
                readonly ctrl: Schema.optionalKey<Schema.Boolean>;
            }>>;
            readonly action: Schema.Literal<"move">;
        }>, Schema.Struct<{
            readonly x: Schema.Int;
            readonly y: Schema.Int;
            readonly modifiers: Schema.optionalKey<Schema.Struct<{
                readonly shift: Schema.optionalKey<Schema.Boolean>;
                readonly alt: Schema.optionalKey<Schema.Boolean>;
                readonly ctrl: Schema.optionalKey<Schema.Boolean>;
            }>>;
            readonly action: Schema.Literals<readonly ["down", "up"]>;
            readonly button: Schema.optionalKey<Schema.Literals<readonly ["left", "middle", "right"]>>;
        }>, Schema.Struct<{
            readonly x: Schema.Int;
            readonly y: Schema.Int;
            readonly modifiers: Schema.optionalKey<Schema.Struct<{
                readonly shift: Schema.optionalKey<Schema.Boolean>;
                readonly alt: Schema.optionalKey<Schema.Boolean>;
                readonly ctrl: Schema.optionalKey<Schema.Boolean>;
            }>>;
            readonly action: Schema.Literal<"scroll">;
            readonly direction: Schema.Literals<readonly ["up", "down", "left", "right"]>;
        }>]>;
    }>, Schema.Struct<{
        readonly jsonrpc: Schema.Literal<"2.0">;
        readonly id: Schema.optional<Schema.Union<readonly [Schema.String, Schema.Number, Schema.Null]>>;
        readonly method: Schema.Literal<"ui.resize">;
        readonly params: Schema.Struct<{
            readonly cols: Schema.Number;
            readonly rows: Schema.Number;
        }>;
    }>, Schema.Struct<{
        readonly jsonrpc: Schema.Literal<"2.0">;
        readonly id: Schema.optional<Schema.Union<readonly [Schema.String, Schema.Number, Schema.Null]>>;
        readonly method: Schema.Literal<"ui.matches">;
        readonly params: Schema.Struct<{
            readonly text: Schema.String;
        }>;
    }>, Schema.Struct<{
        readonly jsonrpc: Schema.Literal<"2.0">;
        readonly id: Schema.optional<Schema.Union<readonly [Schema.String, Schema.Number, Schema.Null]>>;
        readonly method: Schema.Literals<readonly ["ui.enter", "ui.state", "ui.snapshot", "ui.recording.finish"]>;
    }>, Schema.Struct<{
        readonly jsonrpc: Schema.Literal<"2.0">;
        readonly id: Schema.optional<Schema.Union<readonly [Schema.String, Schema.Number, Schema.Null]>>;
        readonly method: Schema.Literal<"ui.capture">;
    }>]>;
    type Request = Schema.Schema.Type<typeof Request>;
    const decodeRequest: (input: unknown, options?: import("effect/SchemaAST").ParseOptions) => {
        readonly jsonrpc: "2.0";
        readonly id?: string | number | null | undefined;
        readonly method: "simulation.handshake";
        readonly params: {
            readonly client: {
                readonly name: string;
                readonly version: string;
            };
            readonly expectedRole: "backend" | "ui";
            readonly offeredVersions: readonly number[];
            readonly requiredCapabilities: readonly string[];
            readonly optionalCapabilities: readonly string[];
        };
    } | {
        readonly jsonrpc: "2.0";
        readonly id?: string | number | null | undefined;
        readonly method: "ui.type";
        readonly params: {
            readonly text: string;
        };
    } | {
        readonly jsonrpc: "2.0";
        readonly id?: string | number | null | undefined;
        readonly method: "ui.press";
        readonly params: {
            readonly key: string;
            readonly modifiers?: {
                readonly ctrl?: boolean | undefined;
                readonly shift?: boolean | undefined;
                readonly meta?: boolean | undefined;
                readonly super?: boolean | undefined;
                readonly hyper?: boolean | undefined;
            } | undefined;
        };
    } | {
        readonly jsonrpc: "2.0";
        readonly id?: string | number | null | undefined;
        readonly method: "ui.arrow";
        readonly params: {
            readonly direction: "down" | "left" | "right" | "up";
        };
    } | {
        readonly jsonrpc: "2.0";
        readonly id?: string | number | null | undefined;
        readonly method: "ui.focus";
        readonly params: {
            readonly target: number;
        };
    } | {
        readonly jsonrpc: "2.0";
        readonly id?: string | number | null | undefined;
        readonly method: "ui.click";
        readonly params: {
            readonly target: number;
            readonly x: number;
            readonly y: number;
            readonly semantic?: {
                readonly id: string;
                readonly instance?: string | undefined;
                readonly element: number;
            } | undefined;
        };
    } | {
        readonly jsonrpc: "2.0";
        readonly id?: string | number | null | undefined;
        readonly method: "ui.mouse";
        readonly params: {
            readonly x: number;
            readonly y: number;
            readonly modifiers?: {
                readonly shift?: boolean | undefined;
                readonly alt?: boolean | undefined;
                readonly ctrl?: boolean | undefined;
            } | undefined;
            readonly action: "move";
        } | {
            readonly x: number;
            readonly y: number;
            readonly modifiers?: {
                readonly shift?: boolean | undefined;
                readonly alt?: boolean | undefined;
                readonly ctrl?: boolean | undefined;
            } | undefined;
            readonly action: "down" | "up";
            readonly button?: "left" | "middle" | "right" | undefined;
        } | {
            readonly x: number;
            readonly y: number;
            readonly modifiers?: {
                readonly shift?: boolean | undefined;
                readonly alt?: boolean | undefined;
                readonly ctrl?: boolean | undefined;
            } | undefined;
            readonly action: "scroll";
            readonly direction: "down" | "left" | "right" | "up";
        };
    } | {
        readonly jsonrpc: "2.0";
        readonly id?: string | number | null | undefined;
        readonly method: "ui.resize";
        readonly params: {
            readonly cols: number;
            readonly rows: number;
        };
    } | {
        readonly jsonrpc: "2.0";
        readonly id?: string | number | null | undefined;
        readonly method: "ui.matches";
        readonly params: {
            readonly text: string;
        };
    } | {
        readonly jsonrpc: "2.0";
        readonly id?: string | number | null | undefined;
        readonly method: "ui.enter" | "ui.recording.finish" | "ui.snapshot" | "ui.state";
    } | {
        readonly jsonrpc: "2.0";
        readonly id?: string | number | null | undefined;
        readonly method: "ui.capture";
    };
    const decodeRequestEffect: (input: unknown, options?: import("effect/SchemaAST").ParseOptions) => Effect.Effect<{
        readonly jsonrpc: "2.0";
        readonly id?: string | number | null | undefined;
        readonly method: "simulation.handshake";
        readonly params: {
            readonly client: {
                readonly name: string;
                readonly version: string;
            };
            readonly expectedRole: "backend" | "ui";
            readonly offeredVersions: readonly number[];
            readonly requiredCapabilities: readonly string[];
            readonly optionalCapabilities: readonly string[];
        };
    } | {
        readonly jsonrpc: "2.0";
        readonly id?: string | number | null | undefined;
        readonly method: "ui.type";
        readonly params: {
            readonly text: string;
        };
    } | {
        readonly jsonrpc: "2.0";
        readonly id?: string | number | null | undefined;
        readonly method: "ui.press";
        readonly params: {
            readonly key: string;
            readonly modifiers?: {
                readonly ctrl?: boolean | undefined;
                readonly shift?: boolean | undefined;
                readonly meta?: boolean | undefined;
                readonly super?: boolean | undefined;
                readonly hyper?: boolean | undefined;
            } | undefined;
        };
    } | {
        readonly jsonrpc: "2.0";
        readonly id?: string | number | null | undefined;
        readonly method: "ui.arrow";
        readonly params: {
            readonly direction: "down" | "left" | "right" | "up";
        };
    } | {
        readonly jsonrpc: "2.0";
        readonly id?: string | number | null | undefined;
        readonly method: "ui.focus";
        readonly params: {
            readonly target: number;
        };
    } | {
        readonly jsonrpc: "2.0";
        readonly id?: string | number | null | undefined;
        readonly method: "ui.click";
        readonly params: {
            readonly target: number;
            readonly x: number;
            readonly y: number;
            readonly semantic?: {
                readonly id: string;
                readonly instance?: string | undefined;
                readonly element: number;
            } | undefined;
        };
    } | {
        readonly jsonrpc: "2.0";
        readonly id?: string | number | null | undefined;
        readonly method: "ui.mouse";
        readonly params: {
            readonly x: number;
            readonly y: number;
            readonly modifiers?: {
                readonly shift?: boolean | undefined;
                readonly alt?: boolean | undefined;
                readonly ctrl?: boolean | undefined;
            } | undefined;
            readonly action: "move";
        } | {
            readonly x: number;
            readonly y: number;
            readonly modifiers?: {
                readonly shift?: boolean | undefined;
                readonly alt?: boolean | undefined;
                readonly ctrl?: boolean | undefined;
            } | undefined;
            readonly action: "down" | "up";
            readonly button?: "left" | "middle" | "right" | undefined;
        } | {
            readonly x: number;
            readonly y: number;
            readonly modifiers?: {
                readonly shift?: boolean | undefined;
                readonly alt?: boolean | undefined;
                readonly ctrl?: boolean | undefined;
            } | undefined;
            readonly action: "scroll";
            readonly direction: "down" | "left" | "right" | "up";
        };
    } | {
        readonly jsonrpc: "2.0";
        readonly id?: string | number | null | undefined;
        readonly method: "ui.resize";
        readonly params: {
            readonly cols: number;
            readonly rows: number;
        };
    } | {
        readonly jsonrpc: "2.0";
        readonly id?: string | number | null | undefined;
        readonly method: "ui.matches";
        readonly params: {
            readonly text: string;
        };
    } | {
        readonly jsonrpc: "2.0";
        readonly id?: string | number | null | undefined;
        readonly method: "ui.enter" | "ui.recording.finish" | "ui.snapshot" | "ui.state";
    } | {
        readonly jsonrpc: "2.0";
        readonly id?: string | number | null | undefined;
        readonly method: "ui.capture";
    }, Schema.SchemaError, never>;
}
export declare namespace Backend {
    const Capabilities: readonly ["llm.attach", "llm.chunk", "llm.finish", "llm.disconnect", "llm.pending", "llm.request", "llm.tool-input-delta", "tool.attach", "tool.update", "tool.finish", "tool.fail", "tool.invocation", "tool.cancel"];
    const Item: Schema.Union<readonly [Schema.Struct<{
        readonly type: Schema.Literal<"textDelta">;
        readonly text: Schema.String;
    }>, Schema.Struct<{
        readonly type: Schema.Literal<"reasoningDelta">;
        readonly text: Schema.String;
    }>, Schema.Struct<{
        readonly type: Schema.Literal<"toolInputStart">;
        readonly index: Schema.Number;
        readonly id: Schema.String;
        readonly name: Schema.String;
    }>, Schema.Struct<{
        readonly type: Schema.Literal<"toolInputDelta">;
        readonly index: Schema.Number;
        readonly text: Schema.String;
    }>, Schema.Struct<{
        readonly type: Schema.Literal<"toolCall">;
        readonly index: Schema.Number;
        readonly id: Schema.String;
        readonly name: Schema.String;
        readonly input: Schema.Codec<Schema.Json, Schema.Json, never, never>;
    }>, Schema.Struct<{
        readonly type: Schema.Literal<"raw">;
        readonly chunk: Schema.Codec<Schema.Json, Schema.Json, never, never>;
    }>]>;
    type Item = Schema.Schema.Type<typeof Item>;
    const FinishReason: Schema.Literals<readonly ["stop", "tool-calls", "length", "content-filter"]>;
    type FinishReason = Schema.Schema.Type<typeof FinishReason>;
    const ToolContent: Schema.Union<readonly [Schema.Struct<{
        readonly type: Schema.Literal<"text">;
        readonly text: Schema.String;
    }>, Schema.Struct<{
        readonly type: Schema.Literal<"file">;
        readonly data: Schema.String;
        readonly mime: Schema.NonEmptyString;
        readonly name: Schema.optionalKey<Schema.String>;
    }>]>;
    type ToolContent = Schema.Schema.Type<typeof ToolContent>;
    const ToolRegistration: Schema.Struct<{
        readonly name: Schema.NonEmptyString;
        readonly description: Schema.String;
        readonly inputSchema: Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>;
        readonly outputSchema: Schema.optionalKey<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>;
        readonly permission: Schema.optionalKey<Schema.NonEmptyString>;
        readonly options: Schema.optionalKey<Schema.Struct<{
            readonly namespace: Schema.optionalKey<Schema.NonEmptyString>;
            readonly codemode: Schema.optionalKey<Schema.Boolean>;
        }>>;
    }>;
    interface ToolRegistration extends Schema.Schema.Type<typeof ToolRegistration> {
    }
    const ToolAttachParams: Schema.Struct<{
        readonly tools: Schema.$Array<Schema.Struct<{
            readonly name: Schema.NonEmptyString;
            readonly description: Schema.String;
            readonly inputSchema: Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>;
            readonly outputSchema: Schema.optionalKey<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>;
            readonly permission: Schema.optionalKey<Schema.NonEmptyString>;
            readonly options: Schema.optionalKey<Schema.Struct<{
                readonly namespace: Schema.optionalKey<Schema.NonEmptyString>;
                readonly codemode: Schema.optionalKey<Schema.Boolean>;
            }>>;
        }>>;
    }>;
    interface ToolAttachParams extends Schema.Schema.Type<typeof ToolAttachParams> {
    }
    function exposedToolName(registration: ToolRegistration): string;
    const ToolProgress: Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>;
    interface ToolProgress extends Schema.Schema.Type<typeof ToolProgress> {
    }
    const ToolOutput: Schema.Struct<{
        readonly structured: Schema.Codec<Schema.Json, Schema.Json, never, never>;
        readonly content: Schema.$Array<Schema.Union<readonly [Schema.Struct<{
            readonly type: Schema.Literal<"text">;
            readonly text: Schema.String;
        }>, Schema.Struct<{
            readonly type: Schema.Literal<"file">;
            readonly data: Schema.String;
            readonly mime: Schema.NonEmptyString;
            readonly name: Schema.optionalKey<Schema.String>;
        }>]>>;
    }>;
    interface ToolOutput extends Schema.Schema.Type<typeof ToolOutput> {
    }
    const ToolUpdateParams: Schema.Struct<{
        readonly id: Schema.String;
        readonly sequence: Schema.Int;
        readonly update: Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>;
    }>;
    interface ToolUpdateParams extends Schema.Schema.Type<typeof ToolUpdateParams> {
    }
    const ToolFinishParams: Schema.Struct<{
        readonly id: Schema.String;
        readonly output: Schema.Struct<{
            readonly structured: Schema.Codec<Schema.Json, Schema.Json, never, never>;
            readonly content: Schema.$Array<Schema.Union<readonly [Schema.Struct<{
                readonly type: Schema.Literal<"text">;
                readonly text: Schema.String;
            }>, Schema.Struct<{
                readonly type: Schema.Literal<"file">;
                readonly data: Schema.String;
                readonly mime: Schema.NonEmptyString;
                readonly name: Schema.optionalKey<Schema.String>;
            }>]>>;
        }>;
    }>;
    interface ToolFinishParams extends Schema.Schema.Type<typeof ToolFinishParams> {
    }
    const ToolFailParams: Schema.Struct<{
        readonly id: Schema.String;
        readonly message: Schema.String;
    }>;
    interface ToolFailParams extends Schema.Schema.Type<typeof ToolFailParams> {
    }
    const ToolInvocation: Schema.Struct<{
        readonly id: Schema.String;
        readonly name: Schema.String;
        readonly input: Schema.Codec<Schema.Json, Schema.Json, never, never>;
        readonly context: Schema.Struct<{
            readonly sessionID: Schema.String;
            readonly agent: Schema.String;
            readonly messageID: Schema.String;
            readonly id: Schema.String;
        }>;
    }>;
    interface ToolInvocation extends Schema.Schema.Type<typeof ToolInvocation> {
    }
    const ToolCancellation: Schema.Struct<{
        readonly id: Schema.String;
        readonly reason: Schema.Literal<"interrupted">;
    }>;
    interface ToolCancellation extends Schema.Schema.Type<typeof ToolCancellation> {
    }
    const Attached: Schema.Struct<{
        readonly attached: Schema.Literal<true>;
    }>;
    interface Attached extends Schema.Schema.Type<typeof Attached> {
    }
    const Ok: Schema.Struct<{
        readonly ok: Schema.Literal<true>;
    }>;
    interface Ok extends Schema.Schema.Type<typeof Ok> {
    }
    const ChunkParams: Schema.Struct<{
        readonly id: Schema.String;
        readonly items: Schema.$Array<Schema.Union<readonly [Schema.Struct<{
            readonly type: Schema.Literal<"textDelta">;
            readonly text: Schema.String;
        }>, Schema.Struct<{
            readonly type: Schema.Literal<"reasoningDelta">;
            readonly text: Schema.String;
        }>, Schema.Struct<{
            readonly type: Schema.Literal<"toolInputStart">;
            readonly index: Schema.Number;
            readonly id: Schema.String;
            readonly name: Schema.String;
        }>, Schema.Struct<{
            readonly type: Schema.Literal<"toolInputDelta">;
            readonly index: Schema.Number;
            readonly text: Schema.String;
        }>, Schema.Struct<{
            readonly type: Schema.Literal<"toolCall">;
            readonly index: Schema.Number;
            readonly id: Schema.String;
            readonly name: Schema.String;
            readonly input: Schema.Codec<Schema.Json, Schema.Json, never, never>;
        }>, Schema.Struct<{
            readonly type: Schema.Literal<"raw">;
            readonly chunk: Schema.Codec<Schema.Json, Schema.Json, never, never>;
        }>]>>;
    }>;
    interface ChunkParams extends Schema.Schema.Type<typeof ChunkParams> {
    }
    const FinishParams: Schema.Struct<{
        readonly id: Schema.String;
        readonly reason: Schema.withDecodingDefault<Schema.Literals<readonly ["stop", "tool-calls", "length", "content-filter"]>, never>;
    }>;
    interface FinishParams extends Schema.Schema.Type<typeof FinishParams> {
    }
    const FinishPayload: Schema.Struct<{
        readonly id: Schema.String;
        readonly reason: Schema.optionalKey<Schema.Literals<readonly ["stop", "tool-calls", "length", "content-filter"]>>;
    }>;
    interface FinishPayload extends Schema.Schema.Type<typeof FinishPayload> {
    }
    const DisconnectParams: Schema.Struct<{
        readonly id: Schema.String;
    }>;
    interface DisconnectParams extends Schema.Schema.Type<typeof DisconnectParams> {
    }
    const Request: Schema.Union<readonly [Schema.Struct<{
        readonly jsonrpc: Schema.Literal<"2.0">;
        readonly id: Schema.optional<Schema.Union<readonly [Schema.String, Schema.Number, Schema.Null]>>;
        readonly method: Schema.Literal<"simulation.handshake">;
        readonly params: Schema.Struct<{
            readonly client: Schema.Struct<{
                readonly name: Schema.NonEmptyString;
                readonly version: Schema.NonEmptyString;
            }>;
            readonly expectedRole: Schema.Literals<readonly ["ui", "backend"]>;
            readonly offeredVersions: Schema.$Array<Schema.Int>;
            readonly requiredCapabilities: Schema.$Array<Schema.NonEmptyString>;
            readonly optionalCapabilities: Schema.$Array<Schema.NonEmptyString>;
        }>;
    }>, Schema.Struct<{
        readonly jsonrpc: Schema.Literal<"2.0">;
        readonly id: Schema.optional<Schema.Union<readonly [Schema.String, Schema.Number, Schema.Null]>>;
        readonly method: Schema.Literal<"llm.chunk">;
        readonly params: Schema.Struct<{
            readonly id: Schema.String;
            readonly items: Schema.$Array<Schema.Union<readonly [Schema.Struct<{
                readonly type: Schema.Literal<"textDelta">;
                readonly text: Schema.String;
            }>, Schema.Struct<{
                readonly type: Schema.Literal<"reasoningDelta">;
                readonly text: Schema.String;
            }>, Schema.Struct<{
                readonly type: Schema.Literal<"toolInputStart">;
                readonly index: Schema.Number;
                readonly id: Schema.String;
                readonly name: Schema.String;
            }>, Schema.Struct<{
                readonly type: Schema.Literal<"toolInputDelta">;
                readonly index: Schema.Number;
                readonly text: Schema.String;
            }>, Schema.Struct<{
                readonly type: Schema.Literal<"toolCall">;
                readonly index: Schema.Number;
                readonly id: Schema.String;
                readonly name: Schema.String;
                readonly input: Schema.Codec<Schema.Json, Schema.Json, never, never>;
            }>, Schema.Struct<{
                readonly type: Schema.Literal<"raw">;
                readonly chunk: Schema.Codec<Schema.Json, Schema.Json, never, never>;
            }>]>>;
        }>;
    }>, Schema.Struct<{
        readonly jsonrpc: Schema.Literal<"2.0">;
        readonly id: Schema.optional<Schema.Union<readonly [Schema.String, Schema.Number, Schema.Null]>>;
        readonly method: Schema.Literal<"llm.finish">;
        readonly params: Schema.Struct<{
            readonly id: Schema.String;
            readonly reason: Schema.withDecodingDefault<Schema.Literals<readonly ["stop", "tool-calls", "length", "content-filter"]>, never>;
        }>;
    }>, Schema.Struct<{
        readonly jsonrpc: Schema.Literal<"2.0">;
        readonly id: Schema.optional<Schema.Union<readonly [Schema.String, Schema.Number, Schema.Null]>>;
        readonly method: Schema.Literal<"llm.disconnect">;
        readonly params: Schema.Struct<{
            readonly id: Schema.String;
        }>;
    }>, Schema.Struct<{
        readonly jsonrpc: Schema.Literal<"2.0">;
        readonly id: Schema.optional<Schema.Union<readonly [Schema.String, Schema.Number, Schema.Null]>>;
        readonly method: Schema.Literal<"tool.attach">;
        readonly params: Schema.Struct<{
            readonly tools: Schema.$Array<Schema.Struct<{
                readonly name: Schema.NonEmptyString;
                readonly description: Schema.String;
                readonly inputSchema: Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>;
                readonly outputSchema: Schema.optionalKey<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>;
                readonly permission: Schema.optionalKey<Schema.NonEmptyString>;
                readonly options: Schema.optionalKey<Schema.Struct<{
                    readonly namespace: Schema.optionalKey<Schema.NonEmptyString>;
                    readonly codemode: Schema.optionalKey<Schema.Boolean>;
                }>>;
            }>>;
        }>;
    }>, Schema.Struct<{
        readonly jsonrpc: Schema.Literal<"2.0">;
        readonly id: Schema.optional<Schema.Union<readonly [Schema.String, Schema.Number, Schema.Null]>>;
        readonly method: Schema.Literal<"tool.update">;
        readonly params: Schema.Struct<{
            readonly id: Schema.String;
            readonly sequence: Schema.Int;
            readonly update: Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>;
        }>;
    }>, Schema.Struct<{
        readonly jsonrpc: Schema.Literal<"2.0">;
        readonly id: Schema.optional<Schema.Union<readonly [Schema.String, Schema.Number, Schema.Null]>>;
        readonly method: Schema.Literal<"tool.finish">;
        readonly params: Schema.Struct<{
            readonly id: Schema.String;
            readonly output: Schema.Struct<{
                readonly structured: Schema.Codec<Schema.Json, Schema.Json, never, never>;
                readonly content: Schema.$Array<Schema.Union<readonly [Schema.Struct<{
                    readonly type: Schema.Literal<"text">;
                    readonly text: Schema.String;
                }>, Schema.Struct<{
                    readonly type: Schema.Literal<"file">;
                    readonly data: Schema.String;
                    readonly mime: Schema.NonEmptyString;
                    readonly name: Schema.optionalKey<Schema.String>;
                }>]>>;
            }>;
        }>;
    }>, Schema.Struct<{
        readonly jsonrpc: Schema.Literal<"2.0">;
        readonly id: Schema.optional<Schema.Union<readonly [Schema.String, Schema.Number, Schema.Null]>>;
        readonly method: Schema.Literal<"tool.fail">;
        readonly params: Schema.Struct<{
            readonly id: Schema.String;
            readonly message: Schema.String;
        }>;
    }>, Schema.Struct<{
        readonly jsonrpc: Schema.Literal<"2.0">;
        readonly id: Schema.optional<Schema.Union<readonly [Schema.String, Schema.Number, Schema.Null]>>;
        readonly method: Schema.Literals<readonly ["llm.attach", "llm.pending"]>;
    }>]>;
    type Request = Schema.Schema.Type<typeof Request>;
    const decodeRequest: (input: unknown, options?: import("effect/SchemaAST").ParseOptions) => {
        readonly jsonrpc: "2.0";
        readonly id?: string | number | null | undefined;
        readonly method: "simulation.handshake";
        readonly params: {
            readonly client: {
                readonly name: string;
                readonly version: string;
            };
            readonly expectedRole: "backend" | "ui";
            readonly offeredVersions: readonly number[];
            readonly requiredCapabilities: readonly string[];
            readonly optionalCapabilities: readonly string[];
        };
    } | {
        readonly jsonrpc: "2.0";
        readonly id?: string | number | null | undefined;
        readonly method: "llm.chunk";
        readonly params: {
            readonly id: string;
            readonly items: readonly ({
                readonly type: "textDelta";
                readonly text: string;
            } | {
                readonly type: "reasoningDelta";
                readonly text: string;
            } | {
                readonly type: "toolInputStart";
                readonly index: number;
                readonly id: string;
                readonly name: string;
            } | {
                readonly type: "toolInputDelta";
                readonly index: number;
                readonly text: string;
            } | {
                readonly type: "toolCall";
                readonly index: number;
                readonly id: string;
                readonly name: string;
                readonly input: Schema.Json;
            } | {
                readonly type: "raw";
                readonly chunk: Schema.Json;
            })[];
        };
    } | {
        readonly jsonrpc: "2.0";
        readonly id?: string | number | null | undefined;
        readonly method: "llm.finish";
        readonly params: {
            readonly id: string;
            readonly reason: "content-filter" | "length" | "stop" | "tool-calls";
        };
    } | {
        readonly jsonrpc: "2.0";
        readonly id?: string | number | null | undefined;
        readonly method: "llm.disconnect";
        readonly params: {
            readonly id: string;
        };
    } | {
        readonly jsonrpc: "2.0";
        readonly id?: string | number | null | undefined;
        readonly method: "tool.attach";
        readonly params: {
            readonly tools: readonly {
                readonly name: string;
                readonly description: string;
                readonly inputSchema: {
                    readonly [x: string]: Schema.Json;
                };
                readonly outputSchema?: {
                    readonly [x: string]: Schema.Json;
                } | undefined;
                readonly permission?: string | undefined;
                readonly options?: {
                    readonly namespace?: string | undefined;
                    readonly codemode?: boolean | undefined;
                } | undefined;
            }[];
        };
    } | {
        readonly jsonrpc: "2.0";
        readonly id?: string | number | null | undefined;
        readonly method: "tool.update";
        readonly params: {
            readonly id: string;
            readonly sequence: number;
            readonly update: {
                readonly [x: string]: Schema.Json;
            };
        };
    } | {
        readonly jsonrpc: "2.0";
        readonly id?: string | number | null | undefined;
        readonly method: "tool.finish";
        readonly params: {
            readonly id: string;
            readonly output: {
                readonly structured: Schema.Json;
                readonly content: readonly ({
                    readonly type: "text";
                    readonly text: string;
                } | {
                    readonly type: "file";
                    readonly data: string;
                    readonly mime: string;
                    readonly name?: string | undefined;
                })[];
            };
        };
    } | {
        readonly jsonrpc: "2.0";
        readonly id?: string | number | null | undefined;
        readonly method: "tool.fail";
        readonly params: {
            readonly id: string;
            readonly message: string;
        };
    } | {
        readonly jsonrpc: "2.0";
        readonly id?: string | number | null | undefined;
        readonly method: "llm.attach" | "llm.pending";
    };
    const decodeRequestEffect: (input: unknown, options?: import("effect/SchemaAST").ParseOptions) => Effect.Effect<{
        readonly jsonrpc: "2.0";
        readonly id?: string | number | null | undefined;
        readonly method: "simulation.handshake";
        readonly params: {
            readonly client: {
                readonly name: string;
                readonly version: string;
            };
            readonly expectedRole: "backend" | "ui";
            readonly offeredVersions: readonly number[];
            readonly requiredCapabilities: readonly string[];
            readonly optionalCapabilities: readonly string[];
        };
    } | {
        readonly jsonrpc: "2.0";
        readonly id?: string | number | null | undefined;
        readonly method: "llm.chunk";
        readonly params: {
            readonly id: string;
            readonly items: readonly ({
                readonly type: "textDelta";
                readonly text: string;
            } | {
                readonly type: "reasoningDelta";
                readonly text: string;
            } | {
                readonly type: "toolInputStart";
                readonly index: number;
                readonly id: string;
                readonly name: string;
            } | {
                readonly type: "toolInputDelta";
                readonly index: number;
                readonly text: string;
            } | {
                readonly type: "toolCall";
                readonly index: number;
                readonly id: string;
                readonly name: string;
                readonly input: Schema.Json;
            } | {
                readonly type: "raw";
                readonly chunk: Schema.Json;
            })[];
        };
    } | {
        readonly jsonrpc: "2.0";
        readonly id?: string | number | null | undefined;
        readonly method: "llm.finish";
        readonly params: {
            readonly id: string;
            readonly reason: "content-filter" | "length" | "stop" | "tool-calls";
        };
    } | {
        readonly jsonrpc: "2.0";
        readonly id?: string | number | null | undefined;
        readonly method: "llm.disconnect";
        readonly params: {
            readonly id: string;
        };
    } | {
        readonly jsonrpc: "2.0";
        readonly id?: string | number | null | undefined;
        readonly method: "tool.attach";
        readonly params: {
            readonly tools: readonly {
                readonly name: string;
                readonly description: string;
                readonly inputSchema: {
                    readonly [x: string]: Schema.Json;
                };
                readonly outputSchema?: {
                    readonly [x: string]: Schema.Json;
                } | undefined;
                readonly permission?: string | undefined;
                readonly options?: {
                    readonly namespace?: string | undefined;
                    readonly codemode?: boolean | undefined;
                } | undefined;
            }[];
        };
    } | {
        readonly jsonrpc: "2.0";
        readonly id?: string | number | null | undefined;
        readonly method: "tool.update";
        readonly params: {
            readonly id: string;
            readonly sequence: number;
            readonly update: {
                readonly [x: string]: Schema.Json;
            };
        };
    } | {
        readonly jsonrpc: "2.0";
        readonly id?: string | number | null | undefined;
        readonly method: "tool.finish";
        readonly params: {
            readonly id: string;
            readonly output: {
                readonly structured: Schema.Json;
                readonly content: readonly ({
                    readonly type: "text";
                    readonly text: string;
                } | {
                    readonly type: "file";
                    readonly data: string;
                    readonly mime: string;
                    readonly name?: string | undefined;
                })[];
            };
        };
    } | {
        readonly jsonrpc: "2.0";
        readonly id?: string | number | null | undefined;
        readonly method: "tool.fail";
        readonly params: {
            readonly id: string;
            readonly message: string;
        };
    } | {
        readonly jsonrpc: "2.0";
        readonly id?: string | number | null | undefined;
        readonly method: "llm.attach" | "llm.pending";
    }, Schema.SchemaError, never>;
    const ProviderInvocation: Schema.Struct<{
        readonly id: Schema.String;
        readonly url: Schema.String;
        readonly body: Schema.Codec<Schema.Json, Schema.Json, never, never>;
    }>;
    interface ProviderInvocation extends Schema.Schema.Type<typeof ProviderInvocation> {
    }
    const Pending: Schema.Struct<{
        readonly invocations: Schema.$Array<Schema.Struct<{
            readonly id: Schema.String;
            readonly url: Schema.String;
            readonly body: Schema.Codec<Schema.Json, Schema.Json, never, never>;
        }>>;
    }>;
    interface Pending extends Schema.Schema.Type<typeof Pending> {
    }
    const NetworkLogEntry: Schema.Struct<{
        readonly time: Schema.Number;
        readonly method: Schema.String;
        readonly url: Schema.String;
        readonly matched: Schema.Boolean;
    }>;
    interface NetworkLogEntry extends Schema.Schema.Type<typeof NetworkLogEntry> {
    }
    const Notification: Schema.Union<readonly [Schema.Struct<{
        readonly jsonrpc: Schema.Literal<"2.0">;
        readonly method: Schema.Literal<"llm.request">;
        readonly params: Schema.Struct<{
            readonly id: Schema.String;
            readonly url: Schema.String;
            readonly body: Schema.Codec<Schema.Json, Schema.Json, never, never>;
        }>;
    }>, Schema.Struct<{
        readonly jsonrpc: Schema.Literal<"2.0">;
        readonly method: Schema.Literal<"tool.invocation">;
        readonly params: Schema.Struct<{
            readonly id: Schema.String;
            readonly name: Schema.String;
            readonly input: Schema.Codec<Schema.Json, Schema.Json, never, never>;
            readonly context: Schema.Struct<{
                readonly sessionID: Schema.String;
                readonly agent: Schema.String;
                readonly messageID: Schema.String;
                readonly id: Schema.String;
            }>;
        }>;
    }>, Schema.Struct<{
        readonly jsonrpc: Schema.Literal<"2.0">;
        readonly method: Schema.Literal<"tool.cancel">;
        readonly params: Schema.Struct<{
            readonly id: Schema.String;
            readonly reason: Schema.Literal<"interrupted">;
        }>;
    }>]>;
    type Notification = Schema.Schema.Type<typeof Notification>;
    const decodeNotification: (input: unknown, options?: import("effect/SchemaAST").ParseOptions) => {
        readonly jsonrpc: "2.0";
        readonly method: "llm.request";
        readonly params: {
            readonly id: string;
            readonly url: string;
            readonly body: Schema.Json;
        };
    } | {
        readonly jsonrpc: "2.0";
        readonly method: "tool.invocation";
        readonly params: {
            readonly id: string;
            readonly name: string;
            readonly input: Schema.Json;
            readonly context: {
                readonly sessionID: string;
                readonly agent: string;
                readonly messageID: string;
                readonly id: string;
            };
        };
    } | {
        readonly jsonrpc: "2.0";
        readonly method: "tool.cancel";
        readonly params: {
            readonly id: string;
            readonly reason: "interrupted";
        };
    };
    const decodeNotificationEffect: (input: unknown, options?: import("effect/SchemaAST").ParseOptions) => Effect.Effect<{
        readonly jsonrpc: "2.0";
        readonly method: "llm.request";
        readonly params: {
            readonly id: string;
            readonly url: string;
            readonly body: Schema.Json;
        };
    } | {
        readonly jsonrpc: "2.0";
        readonly method: "tool.invocation";
        readonly params: {
            readonly id: string;
            readonly name: string;
            readonly input: Schema.Json;
            readonly context: {
                readonly sessionID: string;
                readonly agent: string;
                readonly messageID: string;
                readonly id: string;
            };
        };
    } | {
        readonly jsonrpc: "2.0";
        readonly method: "tool.cancel";
        readonly params: {
            readonly id: string;
            readonly reason: "interrupted";
        };
    }, Schema.SchemaError, never>;
}
export declare const UiRpcs: RpcGroup.RpcGroup<Rpc.Rpc<"simulation.handshake", Schema.Struct<{
    readonly client: Schema.Struct<{
        readonly name: Schema.NonEmptyString;
        readonly version: Schema.NonEmptyString;
    }>;
    readonly expectedRole: Schema.Literals<readonly ["ui", "backend"]>;
    readonly offeredVersions: Schema.$Array<Schema.Int>;
    readonly requiredCapabilities: Schema.$Array<Schema.NonEmptyString>;
    readonly optionalCapabilities: Schema.$Array<Schema.NonEmptyString>;
}>, Schema.Struct<{
    readonly protocolVersion: Schema.Literal<1>;
    readonly role: Schema.Literals<readonly ["ui", "backend"]>;
    readonly server: Schema.Struct<{
        readonly name: Schema.NonEmptyString;
        readonly version: Schema.NonEmptyString;
    }>;
    readonly capabilities: Schema.$Array<Schema.NonEmptyString>;
}>, typeof SimulationRequestError, never, never> | Rpc.Rpc<"ui.arrow", Schema.Struct<{
    readonly direction: Schema.Literals<readonly ["up", "down", "left", "right"]>;
}>, Schema.Struct<{
    readonly focused: Schema.Struct<{
        readonly renderable: Schema.optional<Schema.Number>;
        readonly editor: Schema.Boolean;
    }>;
    readonly elements: Schema.$Array<Schema.Struct<{
        readonly id: Schema.String;
        readonly num: Schema.Number;
        readonly x: Schema.Number;
        readonly y: Schema.Number;
        readonly width: Schema.Number;
        readonly height: Schema.Number;
        readonly focusable: Schema.Boolean;
        readonly focused: Schema.Boolean;
        readonly clickable: Schema.Boolean;
        readonly editor: Schema.Boolean;
    }>>;
}>, typeof SimulationRequestError, never, never> | Rpc.Rpc<"ui.capture", Schema.Void, Schema.Struct<{
    readonly cols: Schema.Number;
    readonly rows: Schema.Number;
    readonly cursor: Schema.Tuple<readonly [Schema.Number, Schema.Number]>;
    readonly lines: Schema.$Array<Schema.Struct<{
        readonly spans: Schema.$Array<Schema.Struct<{
            readonly text: Schema.String;
            readonly fg: Schema.Tuple<readonly [Schema.Number, Schema.Number, Schema.Number, Schema.Number]>;
            readonly bg: Schema.Tuple<readonly [Schema.Number, Schema.Number, Schema.Number, Schema.Number]>;
            readonly attributes: Schema.Number;
            readonly width: Schema.Number;
        }>>;
    }>>;
    readonly images: Schema.optionalKey<Schema.$Array<Schema.Struct<{
        readonly x: Schema.Number;
        readonly y: Schema.Number;
        readonly width: Schema.Number;
        readonly height: Schema.Number;
        readonly pixelWidth: Schema.Number;
        readonly pixelHeight: Schema.Number;
        readonly rgba: Schema.String;
    }>>>;
}>, typeof SimulationRequestError, never, never> | Rpc.Rpc<"ui.click", Schema.Struct<{
    readonly target: Schema.Number;
    readonly x: Schema.Number;
    readonly y: Schema.Number;
    readonly semantic: Schema.optionalKey<Schema.Struct<{
        readonly id: Schema.NonEmptyString;
        readonly instance: Schema.optionalKey<Schema.NonEmptyString>;
        readonly element: Schema.Number;
    }>>;
}>, Schema.Struct<{
    readonly focused: Schema.Struct<{
        readonly renderable: Schema.optional<Schema.Number>;
        readonly editor: Schema.Boolean;
    }>;
    readonly elements: Schema.$Array<Schema.Struct<{
        readonly id: Schema.String;
        readonly num: Schema.Number;
        readonly x: Schema.Number;
        readonly y: Schema.Number;
        readonly width: Schema.Number;
        readonly height: Schema.Number;
        readonly focusable: Schema.Boolean;
        readonly focused: Schema.Boolean;
        readonly clickable: Schema.Boolean;
        readonly editor: Schema.Boolean;
    }>>;
}>, typeof SimulationRequestError, never, never> | Rpc.Rpc<"ui.enter", Schema.Void, Schema.Struct<{
    readonly focused: Schema.Struct<{
        readonly renderable: Schema.optional<Schema.Number>;
        readonly editor: Schema.Boolean;
    }>;
    readonly elements: Schema.$Array<Schema.Struct<{
        readonly id: Schema.String;
        readonly num: Schema.Number;
        readonly x: Schema.Number;
        readonly y: Schema.Number;
        readonly width: Schema.Number;
        readonly height: Schema.Number;
        readonly focusable: Schema.Boolean;
        readonly focused: Schema.Boolean;
        readonly clickable: Schema.Boolean;
        readonly editor: Schema.Boolean;
    }>>;
}>, typeof SimulationRequestError, never, never> | Rpc.Rpc<"ui.focus", Schema.Struct<{
    readonly target: Schema.Number;
}>, Schema.Struct<{
    readonly focused: Schema.Struct<{
        readonly renderable: Schema.optional<Schema.Number>;
        readonly editor: Schema.Boolean;
    }>;
    readonly elements: Schema.$Array<Schema.Struct<{
        readonly id: Schema.String;
        readonly num: Schema.Number;
        readonly x: Schema.Number;
        readonly y: Schema.Number;
        readonly width: Schema.Number;
        readonly height: Schema.Number;
        readonly focusable: Schema.Boolean;
        readonly focused: Schema.Boolean;
        readonly clickable: Schema.Boolean;
        readonly editor: Schema.Boolean;
    }>>;
}>, typeof SimulationRequestError, never, never> | Rpc.Rpc<"ui.matches", Schema.Struct<{
    readonly text: Schema.String;
}>, Schema.Boolean, typeof SimulationRequestError, never, never> | Rpc.Rpc<"ui.mouse", Schema.Union<readonly [Schema.Struct<{
    readonly x: Schema.Int;
    readonly y: Schema.Int;
    readonly modifiers: Schema.optionalKey<Schema.Struct<{
        readonly shift: Schema.optionalKey<Schema.Boolean>;
        readonly alt: Schema.optionalKey<Schema.Boolean>;
        readonly ctrl: Schema.optionalKey<Schema.Boolean>;
    }>>;
    readonly action: Schema.Literal<"move">;
}>, Schema.Struct<{
    readonly x: Schema.Int;
    readonly y: Schema.Int;
    readonly modifiers: Schema.optionalKey<Schema.Struct<{
        readonly shift: Schema.optionalKey<Schema.Boolean>;
        readonly alt: Schema.optionalKey<Schema.Boolean>;
        readonly ctrl: Schema.optionalKey<Schema.Boolean>;
    }>>;
    readonly action: Schema.Literals<readonly ["down", "up"]>;
    readonly button: Schema.optionalKey<Schema.Literals<readonly ["left", "middle", "right"]>>;
}>, Schema.Struct<{
    readonly x: Schema.Int;
    readonly y: Schema.Int;
    readonly modifiers: Schema.optionalKey<Schema.Struct<{
        readonly shift: Schema.optionalKey<Schema.Boolean>;
        readonly alt: Schema.optionalKey<Schema.Boolean>;
        readonly ctrl: Schema.optionalKey<Schema.Boolean>;
    }>>;
    readonly action: Schema.Literal<"scroll">;
    readonly direction: Schema.Literals<readonly ["up", "down", "left", "right"]>;
}>]>, Schema.Struct<{
    readonly focused: Schema.Struct<{
        readonly renderable: Schema.optional<Schema.Number>;
        readonly editor: Schema.Boolean;
    }>;
    readonly elements: Schema.$Array<Schema.Struct<{
        readonly id: Schema.String;
        readonly num: Schema.Number;
        readonly x: Schema.Number;
        readonly y: Schema.Number;
        readonly width: Schema.Number;
        readonly height: Schema.Number;
        readonly focusable: Schema.Boolean;
        readonly focused: Schema.Boolean;
        readonly clickable: Schema.Boolean;
        readonly editor: Schema.Boolean;
    }>>;
}>, typeof SimulationRequestError, never, never> | Rpc.Rpc<"ui.press", Schema.Struct<{
    readonly key: Schema.String;
    readonly modifiers: Schema.optional<Schema.Struct<{
        readonly ctrl: Schema.optional<Schema.Boolean>;
        readonly shift: Schema.optional<Schema.Boolean>;
        readonly meta: Schema.optional<Schema.Boolean>;
        readonly super: Schema.optional<Schema.Boolean>;
        readonly hyper: Schema.optional<Schema.Boolean>;
    }>>;
}>, Schema.Struct<{
    readonly focused: Schema.Struct<{
        readonly renderable: Schema.optional<Schema.Number>;
        readonly editor: Schema.Boolean;
    }>;
    readonly elements: Schema.$Array<Schema.Struct<{
        readonly id: Schema.String;
        readonly num: Schema.Number;
        readonly x: Schema.Number;
        readonly y: Schema.Number;
        readonly width: Schema.Number;
        readonly height: Schema.Number;
        readonly focusable: Schema.Boolean;
        readonly focused: Schema.Boolean;
        readonly clickable: Schema.Boolean;
        readonly editor: Schema.Boolean;
    }>>;
}>, typeof SimulationRequestError, never, never> | Rpc.Rpc<"ui.recording.finish", Schema.Void, Schema.String, typeof SimulationRequestError, never, never> | Rpc.Rpc<"ui.resize", Schema.Struct<{
    readonly cols: Schema.Number;
    readonly rows: Schema.Number;
}>, Schema.Struct<{
    readonly focused: Schema.Struct<{
        readonly renderable: Schema.optional<Schema.Number>;
        readonly editor: Schema.Boolean;
    }>;
    readonly elements: Schema.$Array<Schema.Struct<{
        readonly id: Schema.String;
        readonly num: Schema.Number;
        readonly x: Schema.Number;
        readonly y: Schema.Number;
        readonly width: Schema.Number;
        readonly height: Schema.Number;
        readonly focusable: Schema.Boolean;
        readonly focused: Schema.Boolean;
        readonly clickable: Schema.Boolean;
        readonly editor: Schema.Boolean;
    }>>;
}>, typeof SimulationRequestError, never, never> | Rpc.Rpc<"ui.snapshot", Schema.Void, Schema.Struct<{
    readonly format: Schema.Literal<"opencode-ui-snapshot-v1">;
    readonly nodes: Schema.$Array<Schema.Struct<{
        readonly id: Schema.NonEmptyString;
        readonly instance: Schema.optionalKey<Schema.NonEmptyString>;
        readonly parent: Schema.optionalKey<Schema.NonEmptyString>;
        readonly role: Schema.NonEmptyString;
        readonly label: Schema.optionalKey<Schema.NonEmptyString>;
        readonly element: Schema.Number;
        readonly focused: Schema.optionalKey<Schema.Boolean>;
        readonly selected: Schema.optionalKey<Schema.Boolean>;
        readonly expanded: Schema.optionalKey<Schema.Boolean>;
        readonly disabled: Schema.optionalKey<Schema.Boolean>;
    }>>;
}>, typeof SimulationRequestError, never, never> | Rpc.Rpc<"ui.state", Schema.Void, Schema.Struct<{
    readonly focused: Schema.Struct<{
        readonly renderable: Schema.optional<Schema.Number>;
        readonly editor: Schema.Boolean;
    }>;
    readonly elements: Schema.$Array<Schema.Struct<{
        readonly id: Schema.String;
        readonly num: Schema.Number;
        readonly x: Schema.Number;
        readonly y: Schema.Number;
        readonly width: Schema.Number;
        readonly height: Schema.Number;
        readonly focusable: Schema.Boolean;
        readonly focused: Schema.Boolean;
        readonly clickable: Schema.Boolean;
        readonly editor: Schema.Boolean;
    }>>;
}>, typeof SimulationRequestError, never, never> | Rpc.Rpc<"ui.type", Schema.Struct<{
    readonly text: Schema.String;
}>, Schema.Struct<{
    readonly focused: Schema.Struct<{
        readonly renderable: Schema.optional<Schema.Number>;
        readonly editor: Schema.Boolean;
    }>;
    readonly elements: Schema.$Array<Schema.Struct<{
        readonly id: Schema.String;
        readonly num: Schema.Number;
        readonly x: Schema.Number;
        readonly y: Schema.Number;
        readonly width: Schema.Number;
        readonly height: Schema.Number;
        readonly focusable: Schema.Boolean;
        readonly focused: Schema.Boolean;
        readonly clickable: Schema.Boolean;
        readonly editor: Schema.Boolean;
    }>>;
}>, typeof SimulationRequestError, never, never>>;
export declare const BackendRpcs: RpcGroup.RpcGroup<Rpc.Rpc<"llm.attach", Schema.Void, Schema.Struct<{
    readonly attached: Schema.Literal<true>;
}>, typeof SimulationRequestError, never, never> | Rpc.Rpc<"llm.chunk", Schema.Struct<{
    readonly id: Schema.String;
    readonly items: Schema.$Array<Schema.Union<readonly [Schema.Struct<{
        readonly type: Schema.Literal<"textDelta">;
        readonly text: Schema.String;
    }>, Schema.Struct<{
        readonly type: Schema.Literal<"reasoningDelta">;
        readonly text: Schema.String;
    }>, Schema.Struct<{
        readonly type: Schema.Literal<"toolInputStart">;
        readonly index: Schema.Number;
        readonly id: Schema.String;
        readonly name: Schema.String;
    }>, Schema.Struct<{
        readonly type: Schema.Literal<"toolInputDelta">;
        readonly index: Schema.Number;
        readonly text: Schema.String;
    }>, Schema.Struct<{
        readonly type: Schema.Literal<"toolCall">;
        readonly index: Schema.Number;
        readonly id: Schema.String;
        readonly name: Schema.String;
        readonly input: Schema.Codec<Schema.Json, Schema.Json, never, never>;
    }>, Schema.Struct<{
        readonly type: Schema.Literal<"raw">;
        readonly chunk: Schema.Codec<Schema.Json, Schema.Json, never, never>;
    }>]>>;
}>, Schema.Struct<{
    readonly ok: Schema.Literal<true>;
}>, typeof SimulationRequestError, never, never> | Rpc.Rpc<"llm.disconnect", Schema.Struct<{
    readonly id: Schema.String;
}>, Schema.Struct<{
    readonly ok: Schema.Literal<true>;
}>, typeof SimulationRequestError, never, never> | Rpc.Rpc<"llm.finish", Schema.Struct<{
    readonly id: Schema.String;
    readonly reason: Schema.optionalKey<Schema.Literals<readonly ["stop", "tool-calls", "length", "content-filter"]>>;
}>, Schema.Struct<{
    readonly ok: Schema.Literal<true>;
}>, typeof SimulationRequestError, never, never> | Rpc.Rpc<"llm.pending", Schema.Void, Schema.Struct<{
    readonly invocations: Schema.$Array<Schema.Struct<{
        readonly id: Schema.String;
        readonly url: Schema.String;
        readonly body: Schema.Codec<Schema.Json, Schema.Json, never, never>;
    }>>;
}>, typeof SimulationRequestError, never, never> | Rpc.Rpc<"simulation.handshake", Schema.Struct<{
    readonly client: Schema.Struct<{
        readonly name: Schema.NonEmptyString;
        readonly version: Schema.NonEmptyString;
    }>;
    readonly expectedRole: Schema.Literals<readonly ["ui", "backend"]>;
    readonly offeredVersions: Schema.$Array<Schema.Int>;
    readonly requiredCapabilities: Schema.$Array<Schema.NonEmptyString>;
    readonly optionalCapabilities: Schema.$Array<Schema.NonEmptyString>;
}>, Schema.Struct<{
    readonly protocolVersion: Schema.Literal<1>;
    readonly role: Schema.Literals<readonly ["ui", "backend"]>;
    readonly server: Schema.Struct<{
        readonly name: Schema.NonEmptyString;
        readonly version: Schema.NonEmptyString;
    }>;
    readonly capabilities: Schema.$Array<Schema.NonEmptyString>;
}>, typeof SimulationRequestError, never, never> | Rpc.Rpc<"tool.attach", Schema.Struct<{
    readonly tools: Schema.$Array<Schema.Struct<{
        readonly name: Schema.NonEmptyString;
        readonly description: Schema.String;
        readonly inputSchema: Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>;
        readonly outputSchema: Schema.optionalKey<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>;
        readonly permission: Schema.optionalKey<Schema.NonEmptyString>;
        readonly options: Schema.optionalKey<Schema.Struct<{
            readonly namespace: Schema.optionalKey<Schema.NonEmptyString>;
            readonly codemode: Schema.optionalKey<Schema.Boolean>;
        }>>;
    }>>;
}>, Schema.Struct<{
    readonly attached: Schema.Literal<true>;
}>, typeof SimulationRequestError, never, never> | Rpc.Rpc<"tool.fail", Schema.Struct<{
    readonly id: Schema.String;
    readonly message: Schema.String;
}>, Schema.Struct<{
    readonly ok: Schema.Literal<true>;
}>, typeof SimulationRequestError, never, never> | Rpc.Rpc<"tool.finish", Schema.Struct<{
    readonly id: Schema.String;
    readonly output: Schema.Struct<{
        readonly structured: Schema.Codec<Schema.Json, Schema.Json, never, never>;
        readonly content: Schema.$Array<Schema.Union<readonly [Schema.Struct<{
            readonly type: Schema.Literal<"text">;
            readonly text: Schema.String;
        }>, Schema.Struct<{
            readonly type: Schema.Literal<"file">;
            readonly data: Schema.String;
            readonly mime: Schema.NonEmptyString;
            readonly name: Schema.optionalKey<Schema.String>;
        }>]>>;
    }>;
}>, Schema.Struct<{
    readonly ok: Schema.Literal<true>;
}>, typeof SimulationRequestError, never, never> | Rpc.Rpc<"tool.update", Schema.Struct<{
    readonly id: Schema.String;
    readonly sequence: Schema.Int;
    readonly update: Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>;
}>, Schema.Struct<{
    readonly ok: Schema.Literal<true>;
}>, typeof SimulationRequestError, never, never>>;
export {};
