import { type RenderLib } from "./zig.js";
import type { Pointer } from "./platform/ffi.js";
import { type ValueInput } from "./yoga.internal.js";
export declare enum Align {
    Auto,
    FlexStart,
    Center,
    FlexEnd,
    Stretch,
    Baseline,
    SpaceBetween,
    SpaceAround,
    SpaceEvenly
}
export declare enum BoxSizing {
    BorderBox,
    ContentBox
}
export declare enum Dimension {
    Width,
    Height
}
export declare enum Direction {
    Inherit,
    LTR,
    RTL
}
export declare enum Display {
    Flex = 0,
    None = 1,
    Contents = 2
}
export declare enum Edge {
    Left,
    Top,
    Right,
    Bottom,
    Start,
    End,
    Horizontal,
    Vertical,
    All
}
export declare enum Errata {
    None = 0,
    StretchFlexBasis = 1,
    AbsolutePositionWithoutInsetsExcludesPadding = 2,
    AbsolutePercentAgainstInnerSize = 4,
    All = 2147483647,
    Classic = 2147483646
}
export declare enum ExperimentalFeature {
    WebFlexBasis = 0
}
export declare enum FlexDirection {
    Column,
    ColumnReverse,
    Row,
    RowReverse
}
export declare enum Gutter {
    Column,
    Row,
    All
}
export declare enum Justify {
    FlexStart,
    Center,
    FlexEnd,
    SpaceBetween,
    SpaceAround,
    SpaceEvenly
}
export declare enum LogLevel {
    Error = 0,
    Warn = 1,
    Info = 2,
    Debug = 3,
    Verbose = 4,
    Fatal = 5
}
export declare enum MeasureMode {
    Undefined,
    Exactly,
    AtMost
}
export declare enum NodeType {
    Default = 0,
    Text = 1
}
export declare enum Overflow {
    Visible,
    Hidden,
    Scroll
}
export declare enum PositionType {
    Static,
    Relative,
    Absolute
}
export declare enum Unit {
    Undefined,
    Point,
    Percent,
    Auto
}
export declare enum Wrap {
    NoWrap,
    Wrap,
    WrapReverse
}
export declare const ALIGN_AUTO = Align.Auto;
export declare const ALIGN_FLEX_START = Align.FlexStart;
export declare const ALIGN_CENTER = Align.Center;
export declare const ALIGN_FLEX_END = Align.FlexEnd;
export declare const ALIGN_STRETCH = Align.Stretch;
export declare const ALIGN_BASELINE = Align.Baseline;
export declare const ALIGN_SPACE_BETWEEN = Align.SpaceBetween;
export declare const ALIGN_SPACE_AROUND = Align.SpaceAround;
export declare const ALIGN_SPACE_EVENLY = Align.SpaceEvenly;
export declare const BOX_SIZING_BORDER_BOX = BoxSizing.BorderBox;
export declare const BOX_SIZING_CONTENT_BOX = BoxSizing.ContentBox;
export declare const DIMENSION_WIDTH = Dimension.Width;
export declare const DIMENSION_HEIGHT = Dimension.Height;
export declare const DIRECTION_INHERIT = Direction.Inherit;
export declare const DIRECTION_LTR = Direction.LTR;
export declare const DIRECTION_RTL = Direction.RTL;
export declare const DISPLAY_FLEX = Display.Flex;
export declare const DISPLAY_NONE = Display.None;
export declare const DISPLAY_CONTENTS = Display.Contents;
export declare const EDGE_LEFT = Edge.Left;
export declare const EDGE_TOP = Edge.Top;
export declare const EDGE_RIGHT = Edge.Right;
export declare const EDGE_BOTTOM = Edge.Bottom;
export declare const EDGE_START = Edge.Start;
export declare const EDGE_END = Edge.End;
export declare const EDGE_HORIZONTAL = Edge.Horizontal;
export declare const EDGE_VERTICAL = Edge.Vertical;
export declare const EDGE_ALL = Edge.All;
export declare const ERRATA_NONE = Errata.None;
export declare const ERRATA_STRETCH_FLEX_BASIS = Errata.StretchFlexBasis;
export declare const ERRATA_ABSOLUTE_POSITION_WITHOUT_INSETS_EXCLUDES_PADDING = Errata.AbsolutePositionWithoutInsetsExcludesPadding;
export declare const ERRATA_ABSOLUTE_PERCENT_AGAINST_INNER_SIZE = Errata.AbsolutePercentAgainstInnerSize;
export declare const ERRATA_ALL = Errata.All;
export declare const ERRATA_CLASSIC = Errata.Classic;
export declare const EXPERIMENTAL_FEATURE_WEB_FLEX_BASIS = ExperimentalFeature.WebFlexBasis;
export declare const FLEX_DIRECTION_COLUMN = FlexDirection.Column;
export declare const FLEX_DIRECTION_COLUMN_REVERSE = FlexDirection.ColumnReverse;
export declare const FLEX_DIRECTION_ROW = FlexDirection.Row;
export declare const FLEX_DIRECTION_ROW_REVERSE = FlexDirection.RowReverse;
export declare const GUTTER_COLUMN = Gutter.Column;
export declare const GUTTER_ROW = Gutter.Row;
export declare const GUTTER_ALL = Gutter.All;
export declare const JUSTIFY_FLEX_START = Justify.FlexStart;
export declare const JUSTIFY_CENTER = Justify.Center;
export declare const JUSTIFY_FLEX_END = Justify.FlexEnd;
export declare const JUSTIFY_SPACE_BETWEEN = Justify.SpaceBetween;
export declare const JUSTIFY_SPACE_AROUND = Justify.SpaceAround;
export declare const JUSTIFY_SPACE_EVENLY = Justify.SpaceEvenly;
export declare const LOG_LEVEL_ERROR = LogLevel.Error;
export declare const LOG_LEVEL_WARN = LogLevel.Warn;
export declare const LOG_LEVEL_INFO = LogLevel.Info;
export declare const LOG_LEVEL_DEBUG = LogLevel.Debug;
export declare const LOG_LEVEL_VERBOSE = LogLevel.Verbose;
export declare const LOG_LEVEL_FATAL = LogLevel.Fatal;
export declare const MEASURE_MODE_UNDEFINED = MeasureMode.Undefined;
export declare const MEASURE_MODE_EXACTLY = MeasureMode.Exactly;
export declare const MEASURE_MODE_AT_MOST = MeasureMode.AtMost;
export declare const NODE_TYPE_DEFAULT = NodeType.Default;
export declare const NODE_TYPE_TEXT = NodeType.Text;
export declare const OVERFLOW_VISIBLE = Overflow.Visible;
export declare const OVERFLOW_HIDDEN = Overflow.Hidden;
export declare const OVERFLOW_SCROLL = Overflow.Scroll;
export declare const POSITION_TYPE_STATIC = PositionType.Static;
export declare const POSITION_TYPE_RELATIVE = PositionType.Relative;
export declare const POSITION_TYPE_ABSOLUTE = PositionType.Absolute;
export declare const UNIT_UNDEFINED = Unit.Undefined;
export declare const UNIT_POINT = Unit.Point;
export declare const UNIT_PERCENT = Unit.Percent;
export declare const UNIT_AUTO = Unit.Auto;
export declare const WRAP_NO_WRAP = Wrap.NoWrap;
export declare const WRAP_WRAP = Wrap.Wrap;
export declare const WRAP_WRAP_REVERSE = Wrap.WrapReverse;
export interface Layout {
    left: number;
    right: number;
    top: number;
    bottom: number;
    width: number;
    height: number;
}
export interface Size {
    width: number;
    height: number;
}
export interface Value {
    unit: Unit;
    value: number;
}
export type MeasureFunction = (width: number, widthMode: MeasureMode, height: number, heightMode: MeasureMode) => Size;
export type DirtiedFunction = (node: Node) => void;
type ValueInputNoAuto = number | `${number}%` | Value | undefined;
export declare enum YogaStatus {
    Ok = 0,
    InvalidArgument = 1,
    OutOfMemory = 2,
    Exception = 3,
    Poisoned = 4,
    Busy = 5,
    DepthLimit = 6
}
export declare class YogaError extends Error {
    readonly operation: string;
    readonly status: YogaStatus;
    readonly name = "YogaError";
    constructor(operation: string, status: YogaStatus);
}
export declare class Config {
    readonly renderLib: RenderLib;
    private readonly ownsConfig;
    readonly ptr: Pointer;
    readonly nodes: Map<Pointer, Node>;
    readonly measures: Map<Pointer, MeasureFunction>;
    readonly dirtied: Map<Pointer, {
        node: Node;
        callback: DirtiedFunction;
    }>;
    private freed;
    private measureCallback?;
    private dirtiedCallback?;
    private constructor();
    static create(renderLib?: RenderLib): Config;
    static fromBorrowedPointer(ptr: Pointer, renderLib?: RenderLib): Config;
    static destroy(config: Config): void;
    free(): void;
    assertUnused(): void;
    assertAlive(): void;
    ensureCallbacks(): void;
    setUseWebDefaults(useWebDefaults: boolean): void;
    useWebDefaults(): boolean;
    setPointScaleFactor(pointScaleFactor: number): void;
    getPointScaleFactor(): number;
    setErrata(errata: Errata): void;
    getErrata(): Errata;
    setExperimentalFeatureEnabled(feature: ExperimentalFeature, enabled: boolean): void;
    isExperimentalFeatureEnabled(feature: ExperimentalFeature): boolean;
}
export declare class Node {
    readonly ptr: Pointer;
    private readonly config;
    private freed;
    private constructor();
    private get renderLib();
    static create(config?: Config): Node;
    static createForOpenTUI(): Node;
    static createDefault(): Node;
    static createWithConfig(config: Config): Node;
    static destroy(node: Node): void;
    private static fromPointer;
    isFreed(): boolean;
    assertMutable(): void;
    runMutation<T>(operation: () => T): T;
    free(): void;
    freeRecursive(): void;
    reset(): void;
    copyStyle(node: Node): void;
    insertChild(child: Node, index: number): void;
    removeChild(child: Node): void;
    removeAllChildren(): void;
    getChild(index: number): Node | null;
    getChildCount(): number;
    getParent(): Node | null;
    calculateLayout(width?: number | "auto", height?: number | "auto", direction?: Direction): void;
    hasNewLayout(): boolean;
    markLayoutSeen(): void;
    markDirty(): void;
    isDirty(): boolean;
    getComputedLayout(): Layout;
    getComputedLeft(): number;
    getComputedTop(): number;
    getComputedRight(): number;
    getComputedBottom(): number;
    getComputedWidth(): number;
    getComputedHeight(): number;
    getComputedMargin(edge: Edge): number;
    getComputedPadding(edge: Edge): number;
    getComputedBorder(edge: Edge): number;
    setDirection(direction: Direction): void;
    getDirection(): Direction;
    setFlexDirection(flexDirection: FlexDirection): void;
    getFlexDirection(): FlexDirection;
    setJustifyContent(justifyContent: Justify): void;
    getJustifyContent(): Justify;
    setAlignContent(alignContent: Align): void;
    getAlignContent(): Align;
    setAlignItems(alignItems: Align): void;
    getAlignItems(): Align;
    setAlignSelf(alignSelf: Align): void;
    getAlignSelf(): Align;
    setPositionType(positionType: PositionType): void;
    getPositionType(): PositionType;
    setFlexWrap(flexWrap: Wrap): void;
    getFlexWrap(): Wrap;
    setOverflow(overflow: Overflow): void;
    getOverflow(): Overflow;
    setDisplay(display: Display): void;
    getDisplay(): Display;
    setBoxSizing(boxSizing: BoxSizing): void;
    getBoxSizing(): BoxSizing;
    setFlex(flex: number | undefined): void;
    getFlex(): number;
    setFlexGrow(flexGrow: number | undefined): void;
    getFlexGrow(): number;
    setFlexShrink(flexShrink: number | undefined): void;
    getFlexShrink(): number;
    setAspectRatio(aspectRatio: number | undefined): void;
    getAspectRatio(): number;
    setFlexBasis(flexBasis: ValueInput): void;
    setFlexBasisPercent(flexBasis: number | undefined): void;
    setFlexBasisAuto(): void;
    getFlexBasis(): Value;
    setWidth(width: ValueInput): void;
    setWidthPercent(width: number | undefined): void;
    setWidthAuto(): void;
    getWidth(): Value;
    setHeight(height: ValueInput): void;
    setHeightPercent(height: number | undefined): void;
    setHeightAuto(): void;
    getHeight(): Value;
    setMinWidth(minWidth: ValueInputNoAuto): void;
    setMinWidthPercent(minWidth: number | undefined): void;
    getMinWidth(): Value;
    setMinHeight(minHeight: ValueInputNoAuto): void;
    setMinHeightPercent(minHeight: number | undefined): void;
    getMinHeight(): Value;
    setMaxWidth(maxWidth: ValueInputNoAuto): void;
    setMaxWidthPercent(maxWidth: number | undefined): void;
    getMaxWidth(): Value;
    setMaxHeight(maxHeight: ValueInputNoAuto): void;
    setMaxHeightPercent(maxHeight: number | undefined): void;
    getMaxHeight(): Value;
    setMargin(edge: Edge, margin: ValueInput): void;
    setMarginPercent(edge: Edge, margin: number | undefined): void;
    setMarginAuto(edge: Edge): void;
    getMargin(edge: Edge): Value;
    setPadding(edge: Edge, padding: ValueInputNoAuto): void;
    setPaddingPercent(edge: Edge, padding: number | undefined): void;
    getPadding(edge: Edge): Value;
    setPosition(edge: Edge, position: ValueInput): void;
    setPositionPercent(edge: Edge, position: number | undefined): void;
    setPositionAuto(edge: Edge): void;
    getPosition(edge: Edge): Value;
    setGap(gutter: Gutter, gap: ValueInputNoAuto): void;
    setGapPercent(gutter: Gutter, gap: number | undefined): void;
    getGap(gutter: Gutter): Value;
    setBorder(edge: Edge, border: number | undefined): void;
    getBorder(edge: Edge): number;
    setIsReferenceBaseline(isReferenceBaseline: boolean): void;
    isReferenceBaseline(): boolean;
    setAlwaysFormsContainingBlock(alwaysFormsContainingBlock: boolean): void;
    getAlwaysFormsContainingBlock(): boolean;
    setMeasureFunc(measureFunc: MeasureFunction | null): void;
    unsetMeasureFunc(): void;
    hasMeasureFunc(): boolean;
    setDirtiedFunc(dirtiedFunc: DirtiedFunction | null): void;
    unsetDirtiedFunc(): void;
    private setEnum;
    private getEnum;
    private setFloat;
    private getFloat;
    private setValue;
    private getValue;
    private collectSubtree;
    private unregisterCallbacks;
    private markFreed;
    private assertSameLibrary;
}
declare const Yoga: {
    Config: typeof Config;
    Node: typeof Node;
    Align: typeof Align;
    BoxSizing: typeof BoxSizing;
    Dimension: typeof Dimension;
    Direction: typeof Direction;
    Display: typeof Display;
    Edge: typeof Edge;
    Errata: typeof Errata;
    ExperimentalFeature: typeof ExperimentalFeature;
    FlexDirection: typeof FlexDirection;
    Gutter: typeof Gutter;
    Justify: typeof Justify;
    LogLevel: typeof LogLevel;
    MeasureMode: typeof MeasureMode;
    NodeType: typeof NodeType;
    Overflow: typeof Overflow;
    PositionType: typeof PositionType;
    Unit: typeof Unit;
    Wrap: typeof Wrap;
    ALIGN_AUTO: Align;
    ALIGN_FLEX_START: Align;
    ALIGN_CENTER: Align;
    ALIGN_FLEX_END: Align;
    ALIGN_STRETCH: Align;
    ALIGN_BASELINE: Align;
    ALIGN_SPACE_BETWEEN: Align;
    ALIGN_SPACE_AROUND: Align;
    ALIGN_SPACE_EVENLY: Align;
    BOX_SIZING_BORDER_BOX: BoxSizing;
    BOX_SIZING_CONTENT_BOX: BoxSizing;
    DIMENSION_WIDTH: Dimension;
    DIMENSION_HEIGHT: Dimension;
    DIRECTION_INHERIT: Direction;
    DIRECTION_LTR: Direction;
    DIRECTION_RTL: Direction;
    DISPLAY_FLEX: Display;
    DISPLAY_NONE: Display;
    DISPLAY_CONTENTS: Display;
    EDGE_LEFT: Edge;
    EDGE_TOP: Edge;
    EDGE_RIGHT: Edge;
    EDGE_BOTTOM: Edge;
    EDGE_START: Edge;
    EDGE_END: Edge;
    EDGE_HORIZONTAL: Edge;
    EDGE_VERTICAL: Edge;
    EDGE_ALL: Edge;
    ERRATA_NONE: Errata;
    ERRATA_STRETCH_FLEX_BASIS: Errata;
    ERRATA_ABSOLUTE_POSITION_WITHOUT_INSETS_EXCLUDES_PADDING: Errata;
    ERRATA_ABSOLUTE_PERCENT_AGAINST_INNER_SIZE: Errata;
    ERRATA_ALL: Errata;
    ERRATA_CLASSIC: Errata;
    EXPERIMENTAL_FEATURE_WEB_FLEX_BASIS: ExperimentalFeature;
    FLEX_DIRECTION_COLUMN: FlexDirection;
    FLEX_DIRECTION_COLUMN_REVERSE: FlexDirection;
    FLEX_DIRECTION_ROW: FlexDirection;
    FLEX_DIRECTION_ROW_REVERSE: FlexDirection;
    GUTTER_COLUMN: Gutter;
    GUTTER_ROW: Gutter;
    GUTTER_ALL: Gutter;
    JUSTIFY_FLEX_START: Justify;
    JUSTIFY_CENTER: Justify;
    JUSTIFY_FLEX_END: Justify;
    JUSTIFY_SPACE_BETWEEN: Justify;
    JUSTIFY_SPACE_AROUND: Justify;
    JUSTIFY_SPACE_EVENLY: Justify;
    LOG_LEVEL_ERROR: LogLevel;
    LOG_LEVEL_WARN: LogLevel;
    LOG_LEVEL_INFO: LogLevel;
    LOG_LEVEL_DEBUG: LogLevel;
    LOG_LEVEL_VERBOSE: LogLevel;
    LOG_LEVEL_FATAL: LogLevel;
    MEASURE_MODE_UNDEFINED: MeasureMode;
    MEASURE_MODE_EXACTLY: MeasureMode;
    MEASURE_MODE_AT_MOST: MeasureMode;
    NODE_TYPE_DEFAULT: NodeType;
    NODE_TYPE_TEXT: NodeType;
    OVERFLOW_VISIBLE: Overflow;
    OVERFLOW_HIDDEN: Overflow;
    OVERFLOW_SCROLL: Overflow;
    POSITION_TYPE_STATIC: PositionType;
    POSITION_TYPE_RELATIVE: PositionType;
    POSITION_TYPE_ABSOLUTE: PositionType;
    UNIT_UNDEFINED: Unit;
    UNIT_POINT: Unit;
    UNIT_PERCENT: Unit;
    UNIT_AUTO: Unit;
    WRAP_NO_WRAP: Wrap;
    WRAP_WRAP: Wrap;
    WRAP_WRAP_REVERSE: Wrap;
};
export default Yoga;
