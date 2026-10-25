import { RGBA } from "./lib/RGBA.js";
import { type ContextEditorViewHandle, type RenderLib, type VisualCursor, type LineInfo, type NativeEditorReplacement } from "./zig.js";
import type { EditBuffer } from "./edit-buffer.js";
import type { NativeResourceOwner, ResourceContext } from "./buffer.js";
import type { SelectionBehavior, SelectionOccupancy } from "./types.js";
export interface Viewport {
    offsetY: number;
    offsetX: number;
    height: number;
    width: number;
}
export type { VisualCursor };
export declare class EditorView {
    private lib;
    private native;
    private editBuffer;
    private _destroyed;
    private _extmarksController?;
    constructor(lib: RenderLib, handle: ContextEditorViewHandle, editBuffer: EditBuffer, source: NativeResourceOwner);
    static create(editBuffer: EditBuffer, viewportWidth: number, viewportHeight: number): EditorView;
    get isDestroyed(): boolean;
    private guard;
    /** @internal Drawing targets must use the view's library and Context. */
    _getOwner(): ResourceContext;
    /** @internal Resources can be shared by scenes in the same Context. */
    _getSceneHandle(scene: NativeResourceOwner): ContextEditorViewHandle;
    private command;
    private position;
    private select;
    setViewportSize(width: number, height: number): void;
    setViewport(x: number, y: number, width: number, height: number, moveCursor?: boolean): void;
    getViewport(): Viewport;
    setScrollMargin(margin: number): void;
    setWrapMode(mode: "none" | "char" | "word"): void;
    getVirtualLineCount(): number;
    getTotalVirtualLineCount(): number;
    setSelection(start: number, end: number, bgColor?: RGBA, fgColor?: RGBA): void;
    updateSelection(end: number, bgColor?: RGBA, fgColor?: RGBA): void;
    resetSelection(): void;
    getSelection(): {
        start: number;
        end: number;
    } | null;
    hasSelection(): boolean;
    setLocalSelection(anchorX: number, anchorY: number, focusX: number, focusY: number, bgColor?: RGBA, fgColor?: RGBA, updateCursor?: boolean, followCursor?: boolean, behavior?: SelectionBehavior): boolean;
    updateLocalSelection(anchorX: number, anchorY: number, focusX: number, focusY: number, bgColor?: RGBA, fgColor?: RGBA, updateCursor?: boolean, followCursor?: boolean, behavior?: SelectionBehavior): boolean;
    resetLocalSelection(): void;
    convertSelectionToCell(): boolean;
    setSelectionOccupancy(occupancy: SelectionOccupancy): void;
    getSelectionOccupancy(): SelectionOccupancy;
    setSelectionInclusive(start: number, end: number, bgColor?: RGBA, fgColor?: RGBA): void;
    setSelectionColors(bgColor?: RGBA, fgColor?: RGBA): void;
    getSelectedText(): string;
    getCursor(): {
        row: number;
        col: number;
    };
    getText(): string;
    getVisualCursor(): VisualCursor;
    moveUpVisual(): void;
    moveDownVisual(): void;
    deleteSelectedText(): void;
    /** @internal The native controller replaces a selection in one checked operation. */
    _replaceSelectedText(text: string): NativeEditorReplacement;
    setCursorByOffset(offset: number): void;
    getNextWordBoundary(): VisualCursor;
    getPrevWordBoundary(): VisualCursor;
    getEOL(): VisualCursor;
    getVisualSOL(): VisualCursor;
    getVisualEOL(): VisualCursor;
    gotoVisualLineEnd(): void;
    getLineInfo(): LineInfo;
    getLogicalLineInfo(): LineInfo;
    get extmarks(): any;
    setPlaceholderStyledText(chunks: {
        text: string;
        fg?: RGBA;
        bg?: RGBA;
        attributes?: number;
    }[]): void;
    setTabIndicator(indicator: string | number): void;
    setTabIndicatorColor(color: RGBA): void;
    measureForDimensions(width: number, height: number): {
        lineCount: number;
        widthColsMax: number;
    } | null;
    destroy(): void;
}
