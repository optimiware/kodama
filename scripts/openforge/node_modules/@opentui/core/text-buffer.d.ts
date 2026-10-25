import { StyledText } from "./lib/styled-text.js";
import { RGBA } from "./lib/RGBA.js";
import { type RenderLib, type ContextTextBufferHandle, type NativeEncodedStyledText } from "./zig.js";
import { type WidthMethod, type Highlight } from "./types.js";
import type { SyntaxStyle } from "./syntax-style.js";
import type { NativeResourceOwner, ResourceContext } from "./buffer.js";
import type { TextBufferView } from "./text-buffer-view.js";
export interface TextChunk {
    __isChunk: true;
    text: string;
    fg?: RGBA;
    bg?: RGBA;
    attributes?: number;
    link?: {
        url: string;
    };
}
export declare class TextBuffer {
    private lib;
    private native;
    private _length;
    private _byteSize;
    private _destroyed;
    private _syntaxStyle?;
    constructor(lib: RenderLib, handle: ContextTextBufferHandle, source: NativeResourceOwner);
    static create(widthMethod: WidthMethod, source: NativeResourceOwner): TextBuffer;
    private guard;
    /** @internal Dependent views must use the buffer's library and Context. */
    _getOwner(): ResourceContext;
    /** @internal Resources can be shared by scenes in the same Context. */
    _getSceneHandle(scene: NativeResourceOwner): ContextTextBufferHandle;
    private updateLengths;
    setText(text: string): void;
    append(text: string): void;
    loadFile(path: string): void;
    setStyledText(text: StyledText): void;
    /** @internal Uses an input snapshot without invoking chunk getters again. */
    _setEncodedStyledText(text: NativeEncodedStyledText): void;
    /** @internal Returns false for oversized batches without changing any resource. */
    static _replaceStyledTextBatch(replacements: {
        textBuffer: TextBuffer;
        textBufferView: TextBufferView;
        text: NativeEncodedStyledText;
    }[], beforeNative: () => void): boolean;
    private setDefaults;
    /** @internal Apply a complete default style through one checked native mutation. */
    _setDefaults(fg: RGBA | null, bg: RGBA | null, attributes: number | null): void;
    setDefaultFg(fg: RGBA | null): void;
    setDefaultBg(bg: RGBA | null): void;
    setDefaultAttributes(attributes: number | null): void;
    resetDefaults(): void;
    getLineCount(): number;
    get length(): number;
    get byteSize(): number;
    getPlainText(): string;
    getTextRange(startOffset: number, endOffset: number): string;
    private highlight;
    /**
     * Add a highlight using character offsets into the full text.
     * start/end in highlight represent absolute character positions.
     */
    addHighlightByCharRange(highlight: Highlight): void;
    /**
     * Add a highlight to a specific line by column positions.
     * start/end in highlight represent column offsets.
     */
    addHighlight(lineIdx: number, highlight: Highlight): void;
    removeHighlightsByRef(hlRef: number): void;
    clearLineHighlights(lineIdx: number): void;
    clearAllHighlights(): void;
    getLineHighlights(lineIdx: number): Array<Highlight>;
    getHighlightCount(): number;
    setSyntaxStyle(style: SyntaxStyle | null): void;
    getSyntaxStyle(): SyntaxStyle | null;
    setTabWidth(width: number): void;
    getTabWidth(): number;
    clear(): void;
    reset(): void;
    destroy(): void;
}
