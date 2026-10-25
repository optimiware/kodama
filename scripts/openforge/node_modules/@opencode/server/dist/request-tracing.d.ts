import { Effect, Scope } from "effect";
export declare const withoutParentSpan: <A, E>(effect: Effect.Effect<A, E, Scope.Scope>) => Effect.Effect<A, E, Scope.Scope>;
