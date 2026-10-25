import { RGBA, type ColorInput } from "./lib/RGBA.js";
import { type RenderLib, type ContextSyntaxStyleHandle } from "./zig.js";
import type { NativeResourceOwner } from "./buffer.js";
export interface StyleDefinition {
    fg?: RGBA;
    bg?: RGBA;
    bold?: boolean;
    italic?: boolean;
    underline?: boolean;
    dim?: boolean;
}
export interface StyleDefinitionInput {
    fg?: ColorInput;
    bg?: ColorInput;
    bold?: boolean;
    italic?: boolean;
    underline?: boolean;
    dim?: boolean;
}
export interface MergedStyle {
    fg?: RGBA;
    bg?: RGBA;
    attributes: number;
}
export interface ThemeTokenStyle {
    scope: string[];
    style: {
        foreground?: ColorInput;
        background?: ColorInput;
        bold?: boolean;
        italic?: boolean;
        underline?: boolean;
        dim?: boolean;
    };
}
export declare function convertThemeToStyles(theme: ThemeTokenStyle[]): Record<string, StyleDefinition>;
export declare class SyntaxStyle {
    private lib;
    private native;
    private _destroyed;
    private nameCache;
    private styleDefs;
    private mergedCache;
    constructor(lib: RenderLib, handle: ContextSyntaxStyleHandle, source: NativeResourceOwner);
    static create(source: NativeResourceOwner): SyntaxStyle;
    static fromTheme(theme: ThemeTokenStyle[], owner: NativeResourceOwner): SyntaxStyle;
    static fromStyles(styles: Record<string, StyleDefinitionInput> | Map<string, StyleDefinitionInput>, owner: NativeResourceOwner): SyntaxStyle;
    private guard;
    /** @internal Attachment borrows the Context-owned style; it never allocates a copy. */
    _getSceneHandle(scene: NativeResourceOwner): ContextSyntaxStyleHandle;
    registerStyle(name: string, style: StyleDefinitionInput): number;
    resolveStyleId(name: string): number | null;
    getStyleId(name: string): number | null;
    getStyleCount(): number;
    clearNameCache(): void;
    getStyle(name: string): StyleDefinition | undefined;
    mergeStyles(...styleNames: string[]): MergedStyle;
    clearCache(): void;
    getCacheSize(): number;
    getAllStyles(): Map<string, StyleDefinition>;
    getRegisteredNames(): string[];
    destroy(): void;
}
