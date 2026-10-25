import { Schema } from "effect";
export declare const WorkspaceID: Schema.brand<Schema.String, "Workspace.ID"> & {
    ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
    create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
};
export type WorkspaceID = typeof WorkspaceID.Type;
