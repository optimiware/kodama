import { Schema } from "effect";
export declare const SessionID: Schema.brand<Schema.String, "SessionID"> & {
    create: () => string & import("effect/Brand").Brand<"SessionID">;
    descending: (id?: string) => string & import("effect/Brand").Brand<"SessionID">;
};
export type SessionID = typeof SessionID.Type;
