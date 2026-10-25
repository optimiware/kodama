import * as Arr from "../Array.js";
import * as Cause from "../Cause.js";
import * as Channel from "../Channel.js";
import * as ChannelSchema from "../ChannelSchema.js";
import * as Data from "../Data.js";
import * as Duration from "../Duration.js";
import * as Effect from "../Effect.js";
import { hasProperty } from "../Predicate.js";
import * as Pull from "../Pull.js";
import * as Result from "../Result.js";
import * as Schema from "../Schema.js";
import * as SchemaTransformation from "../SchemaTransformation.js";
const SseErrorTypeId = "~effect/encoding/Sse/SseError";
/**
 * Error reason raised when pending Server-Sent Events state exceeds the
 * configured maximum size.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export class EventTooLarge extends /*#__PURE__*/Data.TaggedError("EventTooLarge") {
  get message() {
    return `Pending SSE event exceeded the maximum size of ${this.maxEventSize}`;
  }
}
/**
 * Error raised when decoding a Server-Sent Events stream fails.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export class SseError extends /*#__PURE__*/Data.TaggedError("SseError") {
  /**
   * Marks this value as an SSE decoding error.
   *
   * @stability unstable
   * @since 4.0.0
   */
  [SseErrorTypeId] = SseErrorTypeId;
  /**
   * Delegates the public message to the underlying SSE error reason.
   *
   * @stability unstable
   * @since 4.0.0
   */
  get message() {
    return this.reason.message;
  }
}
const defaultMaxEventSize = 10 * 1024 * 1024;
/**
 * Creates a channel that parses Server-Sent Events text chunks into `Event` values.
 *
 * **Details**
 *
 * SSE `retry` directives are emitted as `Retry` failures so callers can
 * reconnect with the requested delay.
 *
 * @stability unstable
 * @category decoding
 * @since 4.0.0
 */
export const decode = options => Channel.fromTransform((upstream, _scope) => Effect.sync(() => {
  let buffer = [];
  let retry;
  const parser = makeParser(event => {
    if (event._tag === "Retry") {
      retry = event;
    } else {
      buffer.push(event);
    }
  }, options);
  const pump = Effect.flatMap(upstream, arr => {
    for (let i = 0; i < arr.length; i++) {
      const error = parser.feed(arr[i]);
      if (error !== undefined) {
        return Effect.fail(error);
      }
    }
    return Effect.void;
  });
  return Effect.suspend(function loop() {
    if (Arr.isArrayNonEmpty(buffer)) {
      const out = buffer;
      buffer = [];
      return Effect.succeed(out);
    } else if (retry) {
      return Effect.fail(retry);
    }
    return Effect.flatMap(pump, loop);
  });
}));
/**
 * Creates an SSE decoder channel that decodes each parsed event with a schema.
 *
 * **Details**
 *
 * The schema receives `{ event, data, id? }`, with string `data`. Absent IDs are
 * omitted, even with default options: use `Schema.optional(Schema.String)`, not
 * `Schema.UndefinedOr(Schema.String)`, to accept events without an ID.
 *
 * With `onExcessProperty: "error"`, declare `event` (default: `"message"`) and
 * `id` if the stream carries IDs, including inherited IDs.
 *
 * @stability unstable
 * @category decoding
 * @since 4.0.0
 */
export const decodeSchema = (schema, options, parseOptions) => Channel.pipeTo(decode(options), ChannelSchema.decode(Event.pipe(Schema.decodeTo(schema, transformEvent)), parseOptions)());
/**
 * Creates an SSE decoder channel that JSON-decodes each event `data` field with a schema.
 *
 * **Details**
 *
 * The output preserves the SSE `event` name and optional `id` while replacing
 * `data` with the decoded value.
 *
 * @stability unstable
 * @category decoding
 * @since 4.0.0
 */
export const decodeDataSchema = (schema, options) => {
  const eventSchema = Schema.Struct({
    ...EventEncoded.fields,
    data: Schema.fromJsonString(schema)
  });
  return Channel.pipeTo(decode(options), Channel.map(ChannelSchema.decode(eventSchema)(), Arr.map(event => ({
    ...event,
    id: event.id
  }))));
};
/**
 * Creates a stateful Server-Sent Events parser.
 *
 * **Details**
 *
 * Call `feed` with text chunks to parse `Event` and `Retry` values through the
 * callback, and call `reset` to clear any buffered event state. `feed` returns
 * an `SseError` if the pending event exceeds `maxEventSize`.
 *
 * @stability unstable
 * @category decoding
 * @since 4.0.0
 */
export function makeParser(onParse, options) {
  const maxEventSize = options?.maxEventSize ?? defaultMaxEventSize;
  // Processing state
  let isFirstChunk;
  let buffer;
  let startingPosition;
  let startingFieldLength;
  let discardTrailingNewline;
  // Event state
  let lastEventId;
  let eventName;
  let data;
  reset();
  return {
    feed,
    reset
  };
  function reset() {
    isFirstChunk = true;
    buffer = "";
    startingPosition = 0;
    startingFieldLength = -1;
    discardTrailingNewline = false;
    lastEventId = undefined;
    eventName = undefined;
    data = "";
  }
  function feed(chunk) {
    buffer = buffer ? buffer + chunk : chunk;
    // Strip any UTF-8 byte order mark (BOM) at the start of the stream.
    // Note that we do not strip any non - UTF8 BOM, as eventsource streams are
    // always decoded as UTF8 as per the specification.
    if (isFirstChunk && buffer.startsWith(BOM)) {
      buffer = buffer.slice(BOM.length);
    }
    isFirstChunk = false;
    // Set up chunk-specific processing state
    const length = buffer.length;
    let position = 0;
    // Read the current buffer byte by byte
    while (position < length) {
      // EventSource allows for carriage return + line feed, which means we
      // need to ignore a linefeed character if the previous character was a
      // carriage return
      // @todo refactor to reduce nesting, consider checking previous byte?
      // @todo but consider multiple chunks etc
      if (discardTrailingNewline) {
        if (buffer[position] === "\n") {
          ++position;
        }
        discardTrailingNewline = false;
      }
      let lineLength = -1;
      let fieldLength = startingFieldLength;
      let character;
      for (let index = position + startingPosition; lineLength < 0 && index < length; ++index) {
        character = buffer[index];
        if (character === ":" && fieldLength < 0) {
          fieldLength = index - position;
        } else if (character === "\r") {
          discardTrailingNewline = true;
          lineLength = index - position;
        } else if (character === "\n") {
          lineLength = index - position;
        }
      }
      if (lineLength < 0) {
        startingPosition = length - position;
        startingFieldLength = fieldLength;
        break;
      } else {
        startingPosition = 0;
        startingFieldLength = -1;
      }
      parseEventStreamLine(buffer, position, fieldLength, lineLength);
      position += lineLength + 1;
    }
    if (position === length) {
      // If we consumed the entire buffer to read the event, reset the buffer
      buffer = "";
    } else if (position > 0) {
      // If there are bytes left to process, set the buffer to the unprocessed
      // portion of the buffer only
      buffer = buffer.slice(position);
    }
    if (buffer.length + data.length > maxEventSize) {
      const error = new SseError({
        reason: new EventTooLarge({
          maxEventSize
        })
      });
      reset();
      return error;
    }
  }
  function parseEventStreamLine(lineBuffer, index, fieldLength, lineLength) {
    if (lineLength === 0) {
      // We reached the last line of this event
      if (data.length > 0) {
        onParse({
          _tag: "Event",
          id: lastEventId,
          event: eventName || "message",
          data: data.slice(0, -1) // remove trailing newline
        });
        data = "";
      }
      eventName = undefined;
      return;
    }
    const noValue = fieldLength < 0;
    const field = lineBuffer.slice(index, index + (noValue ? lineLength : fieldLength));
    let step = 0;
    if (noValue) {
      step = lineLength;
    } else if (lineBuffer[index + fieldLength + 1] === " ") {
      step = fieldLength + 2;
    } else {
      step = fieldLength + 1;
    }
    const position = index + step;
    const valueLength = lineLength - step;
    const value = lineBuffer.slice(position, position + valueLength).toString();
    if (field === "data") {
      data += value ? `${value}\n` : "\n";
    } else if (field === "event") {
      eventName = value;
    } else if (field === "id" && !value.includes("\u0000")) {
      lastEventId = value;
    } else if (field === "retry" && /^\d+$/.test(value)) {
      const retry = parseInt(value, 10);
      onParse(new Retry({
        duration: Duration.millis(retry),
        lastEventId
      }));
    }
  }
}
const BOM = "\uFEFF";
/**
 * Creates a channel that encodes `Event` values as Server-Sent Events text.
 *
 * **Details**
 *
 * If the upstream channel fails with `Retry`, the retry directive is written and
 * the encoder completes.
 *
 * @stability unstable
 * @category encoding
 * @since 4.0.0
 */
export const encode = () => Channel.fromTransform((upstream, _scope) => Effect.sync(() => {
  let done = false;
  const pull = upstream.pipe(Effect.map(Arr.map(encoder.write)), Effect.catchFilter(Retry.filter, retry => {
    done = true;
    return Effect.succeed(Arr.of(encoder.write(retry)));
  }), Pull.catchDone(() => Cause.done()));
  return Effect.suspend(() => done ? Cause.done() : pull);
}));
/**
 * Creates an SSE encoder channel for values accepted by a schema.
 *
 * **Details**
 *
 * Values are schema-encoded to the untagged SSE event shape, transformed to
 * `Event`, and then written as Server-Sent Events text.
 *
 * @stability unstable
 * @category encoding
 * @since 4.0.0
 */
export const encodeSchema = schema => ChannelSchema.encode(Event.pipe(Schema.decodeTo(schema, transformEvent)))().pipe(Channel.pipeTo(encode()));
/**
 * Schema for the untagged Server-Sent Events payload shape containing an optional `id`, `event`, and string `data` fields.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export const EventEncoded = /*#__PURE__*/Schema.Struct({
  id: /*#__PURE__*/Schema.optional(Schema.String),
  event: Schema.String,
  data: Schema.String
});
/**
 * Schema for the tagged Server-Sent Events message model that adds `_tag: "Event"` to the event name, optional event ID, and string data payload.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export const Event = /*#__PURE__*/Schema.Struct({
  _tag: /*#__PURE__*/Schema.tag("Event"),
  id: /*#__PURE__*/Schema.UndefinedOr(Schema.String),
  event: Schema.String,
  data: Schema.String
});
/**
 * Schema for transforming untagged SSE event payloads into tagged `Event`
 * models.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export const transformEvent = /*#__PURE__*/SchemaTransformation.transform({
  decode: event => event.id === undefined ? {
    event: event.event,
    data: event.data
  } : {
    id: event.id,
    event: event.event,
    data: event.data
  },
  encode: event => ({
    _tag: "Event",
    id: event.id,
    event: event.event ?? "message",
    data: event.data
  })
});
const RetryTypeId = "~effect/encoding/Sse/Retry";
/**
 * Represents a Server-Sent Events retry directive.
 *
 * **Details**
 *
 * Decoders surface this value as a failure to request reconnection after
 * `duration`; encoders serialize an upstream `Retry` failure as a `retry:` line.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export class Retry extends /*#__PURE__*/Data.TaggedClass("Retry") {
  /**
   * Marks this value as an SSE retry directive for runtime guards.
   *
   * @stability unstable
   * @since 4.0.0
   */
  [RetryTypeId] = RetryTypeId;
  /**
   * Returns `true` when the value is an SSE retry directive.
   *
   * @stability unstable
   * @since 4.0.0
   */
  static is(u) {
    return hasProperty(u, RetryTypeId);
  }
  /**
   * Separates SSE retry directives from regular event values.
   *
   * @stability unstable
   * @since 4.0.0
   */
  static filter(u) {
    return Retry.is(u) ? Result.succeed(u) : Result.fail(u);
  }
}
/**
 * Default Server-Sent Events encoder.
 *
 * **Details**
 *
 * It renders `Event` values as `id`, `event`, and `data` lines and renders
 * `Retry` values as `retry:` directives.
 *
 * @stability unstable
 * @category encoding
 * @since 4.0.0
 */
export const encoder = {
  write(event) {
    switch (event._tag) {
      case "Event":
        {
          let data = "";
          if (event.id !== undefined) {
            data += `id: ${event.id}\n`;
          }
          if (event.event !== "message") {
            data += `event: ${event.event}\n`;
          }
          data += `data: ${event.data.replace(/\n/g, "\ndata: ")}\n`;
          return data + "\n";
        }
      case "Retry":
        {
          return `retry: ${Duration.toMillis(event.duration)}\n\n`;
        }
    }
  }
};
//# sourceMappingURL=Sse.js.map