/** Split an ordered token list into runs of consecutive tokens with the same speaker. */
export declare const group: <Item>(items: ReadonlyArray<Item>, speaker: (item: Item) => unknown) => Item[][];
export * as SpeakerTurns from "./speaker-turns.js";
