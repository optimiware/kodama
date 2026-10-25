import type { AnyAuthClient } from "google-auth-library";
import { Auth } from "../route/auth.js";
export declare const loadADCClient: (project?: string) => Promise<AnyAuthClient>;
export type OAuthOptions = {
    readonly accessToken?: string;
    readonly auth?: never;
} | {
    readonly accessToken?: never;
    readonly auth?: Auth.Definition;
};
export type ApiKeyOptions = (OAuthOptions & {
    readonly apiKey?: never;
}) | {
    readonly accessToken?: never;
    readonly apiKey?: string;
    readonly auth?: never;
};
export declare const project: (value?: string) => string | undefined;
export declare const location: (value: string | undefined) => string;
export declare const host: (location: string) => string;
export declare const requireProject: (value: string | undefined) => string;
export declare const apiKey: (input: ApiKeyOptions & {
    readonly project?: string;
    readonly location?: string;
}) => string | undefined;
export declare const oauth: (input: OAuthOptions, project?: string) => Auth.Definition;
export * as GoogleVertexShared from "./google-vertex-shared.js";
