import type { NonEmptyReadonlyArray } from "../Array.ts";
import type * as Brand from "../Brand.ts";
import * as Cause from "../Cause.ts";
import * as Effect from "../Effect.ts";
import * as Exit from "../Exit.ts";
import * as Schema from "../Schema.ts";
import * as Workflow from "./Workflow.ts";
import type { WorkflowEngine, WorkflowInstance } from "./WorkflowEngine.ts";
declare const TypeId = "~effect/workflow/DurableDeferred";
/**
 * Named durable deferred value whose completion is persisted by the workflow
 * engine and encoded with success and error schemas.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface DurableDeferred<Success extends Schema.Constraint, Error extends Schema.Constraint = Schema.Never> {
    readonly [TypeId]: typeof TypeId;
    readonly name: string;
    readonly successSchema: Success;
    readonly errorSchema: Error;
    readonly exitSchema: Schema.Exit<Schema.Top, Schema.Top, Schema.Top>;
    readonly withActivityAttempt: Effect.Effect<DurableDeferred<Success, Error>>;
}
/**
 * Type-erased durable deferred shape for APIs that only need the deferred
 * identity and name.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface Any {
    readonly [TypeId]: typeof TypeId;
    readonly name: string;
}
/**
 * Type-erased durable deferred shape that also exposes success, error, and
 * exit schemas.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface AnyWithProps {
    readonly [TypeId]: typeof TypeId;
    readonly name: string;
    readonly successSchema: Schema.Top;
    readonly errorSchema: Schema.Top;
    readonly exitSchema: Schema.Exit<any, any, any>;
}
/**
 * Creates a named durable deferred with optional success and error schemas for
 * persisted completion.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const make: <Success extends Schema.Constraint = Schema.Void, Error extends Schema.Constraint = Schema.Never>(name: string, options?: {
    readonly success?: Success | undefined;
    readonly error?: Error | undefined;
}) => DurableDeferred<Success, Error>;
declare const await_: <Success extends Schema.Constraint, Error extends Schema.Constraint>(self: DurableDeferred<Success, Error>) => Effect.Effect<Success["Type"], Error["Type"], WorkflowEngine | WorkflowInstance | Success["DecodingServices"] | Error["DecodingServices"]>;
/**
 * @stability unstable
 */
export { 
/**
 * Waits for the durable deferred, suspending the current workflow when no
 * persisted completion is available.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
await_ as await };
/**
 * Runs an effect and records its exit into the durable deferred, resuming
 * workflows that are waiting on that deferred.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const into: {
    /**
     * Runs an effect and records its exit into the durable deferred, resuming
     * workflows that are waiting on that deferred.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    <Success extends Schema.Constraint, Error extends Schema.Constraint>(self: DurableDeferred<Success, Error>): <R>(effect: Effect.Effect<Success["Type"], Error["Type"], R>) => Effect.Effect<Success["Type"], Error["Type"], R | WorkflowEngine | WorkflowInstance | Success["DecodingServices"] | Error["DecodingServices"] | Success["EncodingServices"] | Error["EncodingServices"]>;
    /**
     * Runs an effect and records its exit into the durable deferred, resuming
     * workflows that are waiting on that deferred.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    <Success extends Schema.Constraint, Error extends Schema.Constraint, R>(effect: Effect.Effect<Success["Type"], Error["Type"], R>, self: DurableDeferred<Success, Error>): Effect.Effect<Success["Type"], Error["Type"], R | WorkflowEngine | WorkflowInstance | Success["DecodingServices"] | Error["DecodingServices"] | Success["EncodingServices"] | Error["EncodingServices"]>;
};
/**
 * Runs effects as a durable race, returning a previously persisted result when
 * present or completing a named deferred with the first result.
 *
 * @stability unstable
 * @category racing
 * @since 4.0.0
 */
export declare const raceAll: <const Effects extends NonEmptyReadonlyArray<Effect.Effect<any, any, any>>, Success extends Schema.Schema<Effect.Success<Effects[number]>>, Error extends Schema.Schema<Effect.Error<Effects[number]>>>(options: {
    name: string;
    success: Success;
    error: Error;
    effects: Effects;
}) => Effect.Effect<Effect.Success<Effects[number]>, Effect.Error<Effects[number]>, Effect.Services<Effects[number]> | Success["DecodingServices"] | Success["EncodingServices"] | Error["DecodingServices"] | Error["EncodingServices"] | WorkflowEngine | WorkflowInstance>;
/**
 * Runtime brand identifier for durable deferred tokens.
 *
 * @stability unstable
 * @category type IDs
 * @since 4.0.0
 */
export declare const TokenTypeId = "~effect/workflow/DurableDeferred/Token";
/**
 * Type-level brand identifier for `Token` values.
 *
 * @stability unstable
 * @category type IDs
 * @since 4.0.0
 */
export type TokenTypeId = typeof TokenTypeId;
/**
 * Branded string token identifying a durable deferred for a workflow
 * execution.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export type Token = Brand.Branded<string, TokenTypeId>;
/**
 * Schema for branded durable deferred tokens.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare const Token: Schema.brand<Schema.String, TokenTypeId>;
declare const TokenParsed_base: Schema.Class<TokenParsed, Schema.Struct<{
    readonly workflowName: Schema.String;
    readonly executionId: Schema.String;
    readonly deferredName: Schema.String;
}>, {}>;
/**
 * Schema for a decoded durable deferred token containing the workflow
 * name, execution ID, and deferred name.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare class TokenParsed extends TokenParsed_base {
    /**
     * Encodes the parsed workflow, execution, and deferred names back into a token.
     *
     * @stability unstable
     * @since 4.0.0
     */
    get asToken(): Token;
    /**
     * Schema for decoding and encoding durable deferred tokens as strings.
     *
     * @stability unstable
     * @since 4.0.0
     */
    static readonly FromString: Schema.decodeTo<typeof TokenParsed, Schema.decodeTo<Schema.fromJsonString<Schema.Tuple<readonly [Schema.String, Schema.String, Schema.String]>>, Schema.String, never, never>, never, never>;
    /**
     * Decodes a durable deferred token string into its parsed components.
     *
     * @stability unstable
     * @since 4.0.0
     */
    static readonly fromString: (input: string, options?: import("../SchemaAST.ts").ParseOptions) => TokenParsed;
    /**
     * Encodes parsed durable deferred token components into a token string.
     *
     * @stability unstable
     * @since 4.0.0
     */
    static readonly encode: (input: TokenParsed, options?: import("../SchemaAST.ts").ParseOptions) => string;
}
/**
 * Creates a token for a durable deferred using the current workflow instance's
 * workflow name and execution ID.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const token: <Success extends Schema.Constraint, Error extends Schema.Constraint>(self: DurableDeferred<Success, Error>) => Effect.Effect<Token, never, WorkflowInstance>;
/**
 * Creates a durable deferred token from an explicit workflow, execution ID,
 * and deferred name.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const tokenFromExecutionId: {
    /**
     * Creates a durable deferred token from an explicit workflow, execution ID,
     * and deferred name.
     *
     * @stability unstable
     * @category constructors
     * @since 4.0.0
     */
    (options: {
        readonly workflow: Workflow.Any;
        readonly executionId: string;
    }): <Success extends Schema.Constraint, Error extends Schema.Constraint>(self: DurableDeferred<Success, Error>) => Token;
    /**
     * Creates a durable deferred token from an explicit workflow, execution ID,
     * and deferred name.
     *
     * @stability unstable
     * @category constructors
     * @since 4.0.0
     */
    <Success extends Schema.Constraint, Error extends Schema.Constraint>(self: DurableDeferred<Success, Error>, options: {
        readonly workflow: Workflow.Any;
        readonly executionId: string;
    }): Token;
};
/**
 * Creates a durable deferred token by deriving the workflow execution ID from
 * the supplied workflow payload.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const tokenFromPayload: {
    /**
     * Creates a durable deferred token by deriving the workflow execution ID from
     * the supplied workflow payload.
     *
     * @stability unstable
     * @category constructors
     * @since 4.0.0
     */
    <W extends Workflow.Any>(options: {
        readonly workflow: W;
        readonly payload: Workflow.PayloadSchema<W>["~type.make.in"];
    }): <Success extends Schema.Constraint, Error extends Schema.Constraint>(self: DurableDeferred<Success, Error>) => Effect.Effect<Token>;
    /**
     * Creates a durable deferred token by deriving the workflow execution ID from
     * the supplied workflow payload.
     *
     * @stability unstable
     * @category constructors
     * @since 4.0.0
     */
    <Success extends Schema.Constraint, Error extends Schema.Constraint, W extends Workflow.Any>(self: DurableDeferred<Success, Error>, options: {
        readonly workflow: W;
        readonly payload: Workflow.PayloadSchema<W>["~type.make.in"];
    }): Effect.Effect<Token>;
};
/**
 * Completes the durable deferred identified by a token with the supplied exit,
 * encoding the result through the deferred schemas.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const done: {
    /**
     * Completes the durable deferred identified by a token with the supplied exit,
     * encoding the result through the deferred schemas.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    <Success extends Schema.Constraint, Error extends Schema.Constraint>(options: {
        readonly token: Token;
        readonly exit: Exit.Exit<Success["Type"], Error["Type"]>;
    }): (self: DurableDeferred<Success, Error>) => Effect.Effect<void, never, WorkflowEngine | Success["EncodingServices"] | Error["EncodingServices"]>;
    /**
     * Completes the durable deferred identified by a token with the supplied exit,
     * encoding the result through the deferred schemas.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    <Success extends Schema.Constraint, Error extends Schema.Constraint>(self: DurableDeferred<Success, Error>, options: {
        readonly token: Token;
        readonly exit: Exit.Exit<Success["Type"], Error["Type"]>;
    }): Effect.Effect<void, never, WorkflowEngine | Success["EncodingServices"] | Error["EncodingServices"]>;
};
/**
 * Completes the durable deferred identified by a token with a successful
 * value.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const succeed: {
    /**
     * Completes the durable deferred identified by a token with a successful
     * value.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    <Success extends Schema.Constraint, Error extends Schema.Constraint>(options: {
        readonly token: Token;
        readonly value: Success["Type"];
    }): (self: DurableDeferred<Success, Error>) => Effect.Effect<void, never, WorkflowEngine | Success["EncodingServices"]>;
    /**
     * Completes the durable deferred identified by a token with a successful
     * value.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    <Success extends Schema.Constraint, Error extends Schema.Constraint>(self: DurableDeferred<Success, Error>, options: {
        readonly token: Token;
        readonly value: Success["Type"];
    }): Effect.Effect<void, never, WorkflowEngine | Success["EncodingServices"]>;
};
/**
 * Completes the durable deferred identified by a token with a typed failure.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const fail: {
    /**
     * Completes the durable deferred identified by a token with a typed failure.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    <Success extends Schema.Constraint, Error extends Schema.Constraint>(options: {
        readonly token: Token;
        readonly error: Error["Type"];
    }): (self: DurableDeferred<Success, Error>) => Effect.Effect<void, never, WorkflowEngine | Error["EncodingServices"]>;
    /**
     * Completes the durable deferred identified by a token with a typed failure.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    <Success extends Schema.Constraint, Error extends Schema.Constraint>(self: DurableDeferred<Success, Error>, options: {
        readonly token: Token;
        readonly error: Error["Type"];
    }): Effect.Effect<void, never, WorkflowEngine | Error["EncodingServices"]>;
};
/**
 * Completes the durable deferred identified by a token with a failure cause.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const failCause: {
    /**
     * Completes the durable deferred identified by a token with a failure cause.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    <Success extends Schema.Constraint, Error extends Schema.Constraint>(options: {
        readonly token: Token;
        readonly cause: Cause.Cause<Error["Type"]>;
    }): (self: DurableDeferred<Success, Error>) => Effect.Effect<void, never, WorkflowEngine | Error["EncodingServices"]>;
    /**
     * Completes the durable deferred identified by a token with a failure cause.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    <Success extends Schema.Constraint, Error extends Schema.Constraint>(self: DurableDeferred<Success, Error>, options: {
        readonly token: Token;
        readonly cause: Cause.Cause<Error["Type"]>;
    }): Effect.Effect<void, never, WorkflowEngine | Error["EncodingServices"]>;
};
//# sourceMappingURL=DurableDeferred.d.ts.map