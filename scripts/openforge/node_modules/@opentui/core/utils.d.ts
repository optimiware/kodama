import { Renderable } from "./Renderable.js";
export declare function createTextAttributes({ bold, italic, underline, dim, blink, inverse, reverse, hidden, strikethrough, }?: {
    bold?: boolean;
    italic?: boolean;
    underline?: boolean;
    dim?: boolean;
    blink?: boolean;
    inverse?: boolean;
    reverse?: boolean;
    hidden?: boolean;
    strikethrough?: boolean;
}): number;
export declare function attributesWithLink(baseAttributes: number, linkId: number): number;
export declare function getLinkId(attributes: number): number;
export declare function visualizeRenderableTree(renderable: Renderable, maxDepth?: number): void;
