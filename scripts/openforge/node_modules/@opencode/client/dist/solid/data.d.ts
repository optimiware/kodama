import type { AgentInfo, CommandInfo, ConfigEntry, SessionFormCancelInput, FormInfo, SessionFormReplyInput, IntegrationInfo, LocationRef, McpResource, McpServer, ModelInfo, ModelRef, PermissionSavedInfo, PermissionRequest, PermissionReplyInput, Project, ProviderInfo, ReferenceInfo, SessionMessageInfo, SessionInfo, SessionInboxInfo, SessionInboxCompaction, ShellInfo, SkillInfo, VcsInfo, OpenCodeEvent, OpenCodeClient, WebSearchProvider } from "../promise";
import { type SessionPromptInput } from "../promise";
export type DataSessionStatus = "idle" | "running";
/**
 * Where a pending user prompt stands relative to the runner: `starting` is promoted at the next idle boundary,
 * `steering` waits for the next step boundary of an execution that already delivered input, `stranded` outlived an
 * execution that ended without delivering it, and `queued` waits for every steer to drain.
 */
export type DataPendingStatus = "starting" | "steering" | "stranded" | "queued";
type OpenCodeEventMap = {
    [Type in OpenCodeEvent["type"]]: Extract<OpenCodeEvent, {
        type: Type;
    }>;
};
export type CreateDataInput = {
    readonly api: () => OpenCodeClient;
    readonly directory: string;
    /** Raw-message window used for an initial transcript read. Older pages retain their normal size. */
    readonly initialMessageLimit?: () => number;
    readonly event: {
        readonly on: <Type extends OpenCodeEvent["type"]>(type: Type, handler: (event: OpenCodeEventMap[Type]) => void) => () => void;
        readonly listen: (handler: (event: {
            name: OpenCodeEvent["type"];
            details: OpenCodeEvent;
        }) => void) => () => void;
    };
    readonly connection?: {
        readonly status: () => "connected" | "connecting" | "reconnecting";
    };
    /** Receives failed event-driven reads. Explicit reads still reject to their caller. */
    readonly onError?: (error: unknown) => void;
};
export declare const settleMs = 150;
export type FormWithLocation = FormInfo & {
    readonly location?: LocationRef;
};
type ShellWithLocation = ShellInfo & {
    readonly location: LocationRef;
};
export declare function locationKey(location: LocationRef): string;
export declare function createData(config: CreateDataInput): {
    on: <Type extends OpenCodeEvent["type"]>(type: Type, handler: (event: OpenCodeEventMap[Type]) => void) => () => void;
    listen: (handler: (event: {
        name: OpenCodeEvent["type"];
        details: OpenCodeEvent;
    }) => void) => () => void;
    session: {
        list(): SessionInfo[];
        get(sessionID: string): SessionInfo;
        creating(sessionID: string): boolean;
        remember(info: SessionInfo): void;
        setStatus(sessionID: string, status: DataSessionStatus): void;
        root(sessionID: string): string;
        family(sessionID: string): string[];
        /** Clear heavy cached data for the root and all known descendants. */
        evict(sessionID: string): void;
        cost(sessionID: string): number;
        status(sessionID: string): DataSessionStatus;
        active(): string[];
        input: {
            list(sessionID: string): string[];
            has(sessionID: string, inboxID: string): boolean;
        };
        pending: {
            list(sessionID: string): SessionInboxInfo[];
            status(sessionID: string, inboxID: string): DataPendingStatus | undefined;
            sync(sessionID: string): Promise<void>;
            invalidate(sessionID: string): void;
        };
        create(input: {
            id?: string;
            title?: string;
            agent?: string;
            model?: ModelRef;
            location?: LocationRef;
            projectID?: string;
        }): {
            id: string;
            request: Promise<SessionInfo>;
        };
        compact(input: {
            sessionID: string;
            model?: ModelRef;
        }): Promise<SessionInboxCompaction>;
        prompt(input: SessionPromptInput & {
            gate?: Promise<unknown>;
            prepare?: () => Promise<unknown>;
        }): Promise<import("../promise").SessionInboxUser>;
        sync(sessionID: string, options?: {
            children?: boolean;
        }): Promise<void>;
        invalidate(sessionID: string): void;
        message: {
            list(sessionID: string): SessionMessageInfo[];
            get(sessionID: string, messageID: string): SessionMessageInfo | undefined;
            sync(sessionID: string): Promise<void>;
            more(sessionID: string): boolean;
            loading(sessionID: string): boolean;
            loadMore(sessionID: string, options?: {
                all?: boolean;
                signal?: AbortSignal;
                /** Runs synchronously inside the store-publication batch. */
                beforePublish?: () => void;
            }): Promise<void>;
            invalidate(sessionID: string): void;
        };
        permission: {
            list(sessionID: string): PermissionRequest[];
            sessions(): string[];
            sync(sessionID: string): Promise<void>;
            invalidate(sessionID: string): void;
            reply(input: PermissionReplyInput): Promise<void>;
        };
        form: {
            list(sessionID: string, ref?: LocationRef): FormWithLocation[] | undefined;
            sessions(): string[];
            sync(sessionID: string, ref?: LocationRef): Promise<void>;
            invalidate(sessionID: string, ref?: LocationRef): void;
            reply(input: SessionFormReplyInput, ref?: LocationRef): Promise<void>;
            cancel(input: SessionFormCancelInput, ref?: LocationRef): Promise<void>;
        };
    };
    project: {
        list(): Project[];
        get(projectID: string): Project;
        sync(): Promise<void>;
        invalidate(): void;
        permission: {
            list(projectID: string): PermissionSavedInfo[];
            sync(projectID: string): Promise<void>;
            invalidate(projectID: string): void;
        };
    };
    shell: {
        list(location?: LocationRef): ShellWithLocation[];
        listBySession(sessionID: string): ShellWithLocation[];
        get(id: string): ShellWithLocation | undefined;
        sync: (ref?: LocationRef) => Promise<void>;
        invalidate: (ref?: LocationRef) => void;
    };
    location: {
        info(ref?: LocationRef): import("../promise").LocationPublicInfo | undefined;
        default(): LocationRef;
        syncInfo(ref?: LocationRef): Promise<void>;
        sync(ref?: LocationRef): Promise<void>;
        invalidate(ref?: LocationRef): void;
        vcs: {
            info: (ref?: LocationRef) => VcsInfo | undefined;
            sync: (ref?: LocationRef) => Promise<void>;
            invalidate: (ref?: LocationRef) => void;
        };
        agent: {
            list: (ref?: LocationRef) => AgentInfo[] | undefined;
            sync: (ref?: LocationRef) => Promise<void>;
            invalidate: (ref?: LocationRef) => void;
        };
        command: {
            list: (ref?: LocationRef) => CommandInfo[] | undefined;
            sync: (ref?: LocationRef) => Promise<void>;
            invalidate: (ref?: LocationRef) => void;
        };
        config: {
            list: (ref?: LocationRef) => ConfigEntry[] | undefined;
            sync: (ref?: LocationRef) => Promise<void>;
            invalidate: (ref?: LocationRef) => void;
        };
        integration: {
            list: (ref?: LocationRef) => IntegrationInfo[] | undefined;
            sync: (ref?: LocationRef) => Promise<void>;
            invalidate: (ref?: LocationRef) => void;
        };
        mcp: {
            server: {
                list: (ref?: LocationRef) => McpServer[] | undefined;
                sync: (ref?: LocationRef) => Promise<void>;
                invalidate: (ref?: LocationRef) => void;
            };
            resource: {
                list: (ref?: LocationRef) => McpResource[] | undefined;
                sync: (ref?: LocationRef) => Promise<void>;
                invalidate: (ref?: LocationRef) => void;
            };
        };
        model: {
            list: (ref?: LocationRef) => ModelInfo[] | undefined;
            sync: (ref?: LocationRef) => Promise<void>;
            invalidate: (ref?: LocationRef) => void;
        };
        provider: {
            list: (ref?: LocationRef) => ProviderInfo[] | undefined;
            sync: (ref?: LocationRef) => Promise<void>;
            invalidate: (ref?: LocationRef) => void;
        };
        reference: {
            list: (ref?: LocationRef) => ReferenceInfo[] | undefined;
            sync: (ref?: LocationRef) => Promise<void>;
            invalidate: (ref?: LocationRef) => void;
        };
        websearch: {
            list(location?: LocationRef): WebSearchProvider[] | undefined;
            refresh(ref?: LocationRef): Promise<void>;
        };
        skill: {
            list: (ref?: LocationRef) => SkillInfo[] | undefined;
            sync: (ref?: LocationRef) => Promise<void>;
            invalidate: (ref?: LocationRef) => void;
        };
    };
};
export type Data = ReturnType<typeof createData>;
export {};
