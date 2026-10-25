import * as ByteSize from "../../../ByteSize.js";
import * as CT from "./contentType.js";
import * as HP from "./headers.js";
import * as Search from "./search.js";
const State = {
  headers: 0,
  body: 1
};
const errInvalidDisposition = {
  _tag: "InvalidDisposition"
};
const errEndNotReached = {
  _tag: "EndNotReached"
};
const errMaxParts = {
  _tag: "ReachedLimit",
  limit: "MaxParts"
};
const errMaxTotalSize = {
  _tag: "ReachedLimit",
  limit: "MaxTotalSize"
};
const errMaxPartSize = {
  _tag: "ReachedLimit",
  limit: "MaxPartSize"
};
const errMaxFieldSize = {
  _tag: "ReachedLimit",
  limit: "MaxFieldSize"
};
const constCR = /*#__PURE__*/new TextEncoder().encode("\r\n");
export function defaultIsFile(info) {
  return info.filename !== undefined || info.contentType === "application/octet-stream";
}
function parseBoundary(headers) {
  const contentType = CT.parse(headers["content-type"]);
  return contentType.parameters.boundary;
}
function noopOnChunk(_chunk) {}
const toLimit = input => input === Infinity ? Infinity : Number(ByteSize.fromInputUnsafe(input));
export function make({
  headers,
  onFile: onPart,
  onField,
  onError,
  onDone,
  isFile = defaultIsFile,
  maxParts = Infinity,
  maxTotalSize: maxTotalSizeInput = Infinity,
  maxPartSize: maxPartSizeInput = Infinity,
  maxFieldSize: maxFieldSizeInput = 1024 * 1024
}) {
  const maxTotalSize = toLimit(maxTotalSizeInput);
  const maxPartSize = toLimit(maxPartSizeInput);
  const maxFieldSize = toLimit(maxFieldSizeInput);
  const boundary = parseBoundary(headers);
  if (boundary === undefined) {
    onError({
      _tag: "InvalidBoundary"
    });
    return {
      write: noopOnChunk,
      end() {}
    };
  }
  const state = {
    state: State.headers,
    index: 0,
    parts: 0,
    onChunk: noopOnChunk,
    info: undefined,
    headerSkip: 0,
    partSize: 0,
    totalSize: 0,
    isFile: false,
    fieldChunks: [],
    fieldSize: 0,
    done: false,
    stopped: false
  };
  function skipBody() {
    state.state = State.body;
    state.isFile = true;
    state.onChunk = noopOnChunk;
  }
  function stop(error) {
    state.stopped = true;
    if (state.state === State.body && state.isFile) {
      state.onChunk(null);
    }
    onError(error);
  }
  const headerParser = HP.make();
  const split = Search.make(`\r\n--${boundary}`, function (index, chunk) {
    if (state.stopped) {
      return;
    }
    if (index === 0) {
      // data before the first boundary
      skipBody();
      return;
    } else if (index !== state.index) {
      if (state.index > 0) {
        if (state.isFile) {
          state.onChunk(null);
        } else {
          if (state.fieldChunks.length === 1) {
            onField(state.info, state.fieldChunks[0]);
          } else {
            const buf = new Uint8Array(state.fieldSize);
            let offset = 0;
            for (let i = 0; i < state.fieldChunks.length; i++) {
              const chunk = state.fieldChunks[i];
              buf.set(chunk, offset);
              offset += chunk.length;
            }
            onField(state.info, buf);
          }
          state.fieldSize = 0;
          state.fieldChunks = [];
        }
      }
      state.partSize = 0;
      state.state = State.headers;
      state.index = index;
      state.headerSkip = 2; // skip the first \r\n
      // trailing --
      if (chunk[0] === 45 && chunk[1] === 45) {
        state.done = true;
        return onDone();
      }
      state.parts++;
      if (state.parts > maxParts) {
        return stop(errMaxParts);
      }
    }
    if ((state.partSize += chunk.length) > maxPartSize) {
      return stop(errMaxPartSize);
    }
    if (state.state === State.headers) {
      const result = headerParser(chunk, state.headerSkip);
      state.headerSkip = 0;
      if (result._tag === "Continue") {
        return;
      } else if (result._tag === "Failure") {
        skipBody();
        return onError({
          _tag: "BadHeaders",
          error: result
        });
      }
      const contentType = CT.parse(result.headers["content-type"]);
      const contentDisposition = CT.parse(result.headers["content-disposition"], true);
      if ("form-data" === contentDisposition.value && !("name" in contentDisposition.parameters)) {
        skipBody();
        return onError(errInvalidDisposition);
      }
      let encodedFilename;
      if ("filename*" in contentDisposition.parameters) {
        const parts = contentDisposition.parameters["filename*"].split("''");
        if (parts.length === 2) {
          try {
            encodedFilename = decodeURIComponent(parts[1]);
          } catch {
            encodedFilename = parts[1];
          }
        }
      }
      state.info = {
        name: contentDisposition.parameters.name ?? "",
        filename: encodedFilename ?? contentDisposition.parameters.filename,
        contentType: contentType.value === "" ? contentDisposition.parameters.filename !== undefined ? "application/octet-stream" : "text/plain" : contentType.value,
        contentTypeParameters: contentType.parameters,
        contentDisposition: contentDisposition.value,
        contentDispositionParameters: contentDisposition.parameters,
        headers: result.headers
      };
      state.state = State.body;
      state.isFile = isFile(state.info);
      if (state.isFile) {
        state.onChunk = onPart(state.info);
      }
      if (result.endPosition < chunk.length) {
        if (state.isFile) {
          state.onChunk(chunk.subarray(result.endPosition));
        } else {
          const buf = chunk.subarray(result.endPosition);
          if ((state.fieldSize += buf.length) > maxFieldSize) {
            return stop(errMaxFieldSize);
          }
          state.fieldChunks.push(buf);
        }
      }
    } else if (state.isFile) {
      state.onChunk(chunk);
    } else {
      if ((state.fieldSize += chunk.length) > maxFieldSize) {
        return stop(errMaxFieldSize);
      }
      state.fieldChunks.push(chunk);
    }
  }, constCR, 2);
  return {
    write(chunk) {
      if (state.stopped) {
        return;
      }
      if ((state.totalSize += chunk.length) > maxTotalSize) {
        return stop(errMaxTotalSize);
      }
      return split.write(chunk);
    },
    end() {
      split.end();
      if (!state.done && !state.stopped) {
        stop(errEndNotReached);
      }
      state.state = State.headers;
      state.index = 0;
      state.parts = 0;
      state.onChunk = noopOnChunk;
      state.info = undefined;
      state.totalSize = 0;
      state.partSize = 0;
      state.fieldChunks = [];
      state.fieldSize = 0;
      state.done = false;
      state.stopped = false;
    }
  };
}
const utf8Decoder = /*#__PURE__*/new TextDecoder("utf-8");
function getDecoder(charset) {
  if (charset === "utf-8" || charset === "utf8" || charset === "") {
    return utf8Decoder;
  }
  try {
    return new TextDecoder(charset);
  } catch (error) {
    return utf8Decoder;
  }
}
export function decodeField(info, value) {
  return getDecoder(info.contentTypeParameters.charset ?? "utf-8").decode(value);
}
//# sourceMappingURL=multipart.js.map