export * as PtyTicket from "./pty-ticket.js";
import { Schema } from "effect";
export declare const ConnectToken: Schema.Struct<{
    readonly ticket: Schema.String;
    readonly expires_in: Schema.Int;
}>;
export interface ConnectToken extends Schema.Schema.Type<typeof ConnectToken> {
}
