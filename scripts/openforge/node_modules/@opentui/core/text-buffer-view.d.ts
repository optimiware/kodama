import { RGBA } from "./lib/RGBA.js";
import { type LineInfo, type MeasureResult, type RenderLib, type ContextTextBufferViewHandle } from "./zig.js";
import type { TextBuffer } from "./text-buffer.js";
import type { NativeResourceOwner, ResourceContext } from "./buffer.js";
import type { SelectionBehavior, SelectionOccupancy } from "./types.js";
export declare class TextBufferView {
    private lib;
    private native;
    private textBuffer;
    private _destroyed;
    private selectionClear;
    constructor(lib: RenderLib, handle: ContextTextBufferViewHandle, textBuffer: TextBuffer, source: NativeResourceOwner);
    static create(textBuffer: TextBuffer): TextBufferView;
    private guard;
    /** @internal Drawing targets must use the view's library and Context. */
    _getOwner(): ResourceContext;
    /** @internal Resources can be shared by scenes in the same Context. */
    _getSceneHandle(scene: NativeResourceOwner): ContextTextBufferViewHandle;
    private select;
    private reset;
    setSelection(start: number, end: number, bgColor?: RGBA, fgColor?: RGBA): void;
    updateSelection(end: number, bgColor?: RGBA, fgColor?: RGBA): void;
    resetSelection(): void;
    getSelection(): {
        start: number;
        end: number;
    } | null;
    hasSelection(): boolean;
    setLocalSelection(anchorX: number, anchorY: number, focusX: number, focusY: number, bgColor?: RGBA, fgColor?: RGBA, behavior?: SelectionBehavior): boolean;
    updateLocalSelection(anchorX: number, anchorY: number, focusX: number, focusY: number, bgColor?: RGBA, fgColor?: RGBA, behavior?: SelectionBehavior): boolean;
    resetLocalSelection(): void;
    setSelectionOccupancy(occupancy: SelectionOccupancy): void;
    getSelectionOccupancy(): SelectionOccupancy;
    private command;
    setWrapWidth(width: number | null): void;
    setWrapMode(mode: "none" | "char" | "word"): void;
    setTextAlign(alignment: "left" | "center" | "right"): void;
    setFirstLineOffset(offset: number): void;
    setViewportSize(width: number, height: number): void;
    setViewport(x: number, y: number, width: number, height: number): void;
    get lineInfo(): LineInfo;
    get logicalLineInfo(): LineInfo;
    getLineSources(startLine: number, lineCount: number): number[];
    getSelectedText(): string;
    getPlainText(): string;
    setTabIndicator(indicator: string | number): void;
    setTabIndicatorColor(color: RGBA): void;
    setTruncate(truncate: boolean): void;
    measureForDimensions(width: number, height: number): MeasureResult | null;
    getVirtualLineCount(): number;
    destroy(): void;
}
