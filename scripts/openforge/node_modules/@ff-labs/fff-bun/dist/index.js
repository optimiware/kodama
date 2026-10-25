// @bun
// src/download.ts
import { existsSync } from "fs";
import { createRequire } from "module";
import { dirname, join } from "path";
import { fileURLToPath } from "url";

// src/platform.ts
import { execSync } from "child_process";
function getTriple() {
  const platform = process.platform;
  const arch = process.arch;
  let osName;
  if (platform === "darwin") {
    osName = "apple-darwin";
  } else if (platform === "android") {
    osName = "linux-android";
  } else if (platform === "linux") {
    osName = detectLinuxLibc();
  } else if (platform === "win32") {
    osName = "pc-windows-msvc";
  } else {
    throw new Error(`Unsupported platform: ${platform}`);
  }
  const archName = normalizeArch(arch);
  return `${archName}-${osName}`;
}
function detectLinuxLibc() {
  let output = "";
  try {
    output = execSync("ldd --version 2>&1", {
      encoding: "utf-8",
      timeout: 5000
    });
  } catch (e) {
    const err = e;
    output = String(err?.stdout ?? "") + String(err?.stderr ?? "");
  }
  if (output.toLowerCase().includes("musl")) {
    return "unknown-linux-musl";
  }
  return "unknown-linux-gnu";
}
function normalizeArch(arch) {
  switch (arch) {
    case "x64":
    case "amd64":
      return "x86_64";
    case "arm64":
      return "aarch64";
    case "arm":
      return "arm";
    default:
      throw new Error(`Unsupported architecture: ${arch}`);
  }
}
function getLibExtension() {
  switch (process.platform) {
    case "darwin":
      return "dylib";
    case "win32":
      return "dll";
    default:
      return "so";
  }
}
function getLibPrefix() {
  return process.platform === "win32" ? "" : "lib";
}
function getLibFilename() {
  const prefix = getLibPrefix();
  const ext = getLibExtension();
  return `${prefix}fff_c.${ext}`;
}
var TRIPLE_TO_NPM_PACKAGE = {
  "aarch64-apple-darwin": "@ff-labs/fff-bin-darwin-arm64",
  "x86_64-apple-darwin": "@ff-labs/fff-bin-darwin-x64",
  "x86_64-unknown-linux-gnu": "@ff-labs/fff-bin-linux-x64-gnu",
  "aarch64-unknown-linux-gnu": "@ff-labs/fff-bin-linux-arm64-gnu",
  "x86_64-unknown-linux-musl": "@ff-labs/fff-bin-linux-x64-musl",
  "aarch64-unknown-linux-musl": "@ff-labs/fff-bin-linux-arm64-musl",
  "x86_64-pc-windows-msvc": "@ff-labs/fff-bin-win32-x64",
  "aarch64-pc-windows-msvc": "@ff-labs/fff-bin-win32-arm64",
  "aarch64-linux-android": "@ff-labs/fff-bin-android-arm64"
};
function getNpmPackageName() {
  const triple = getTriple();
  const packageName = TRIPLE_TO_NPM_PACKAGE[triple];
  if (!packageName) {
    throw new Error(`No npm package available for platform: ${triple}`);
  }
  return packageName;
}

// src/download.ts
function getCurrentDir() {
  const url = import.meta.url;
  if (url.includes("$bunfs")) {
    return dirname(process.execPath);
  }
  if (url.startsWith("file://")) {
    return dirname(fileURLToPath(url));
  }
  return dirname(url);
}
function getPackageDir() {
  const currentDir = getCurrentDir();
  return dirname(currentDir);
}
function binaryExists() {
  return findBinary() !== null;
}
function resolveFromNpmPackage() {
  const packageName = getNpmPackageName();
  try {
    const require2 = createRequire(join(getPackageDir(), "package.json"));
    const packageJsonPath = require2.resolve(`${packageName}/package.json`);
    const packageDir = dirname(packageJsonPath);
    const binaryPath = join(packageDir, getLibFilename());
    if (existsSync(binaryPath)) {
      return binaryPath;
    }
  } catch {}
  return null;
}
function getDevBinaryPath() {
  const packageDir = getPackageDir();
  const workspaceRoot = join(packageDir, "..", "..");
  const possiblePaths = [
    join(workspaceRoot, "target", "release", getLibFilename()),
    join(workspaceRoot, "target", "debug", getLibFilename())
  ];
  for (const path of possiblePaths) {
    if (existsSync(path)) {
      return path;
    }
  }
  return null;
}
function isDevWorkspace() {
  const packageDir = getPackageDir();
  const workspaceRoot = join(packageDir, "..", "..");
  return existsSync(join(workspaceRoot, "Cargo.toml"));
}
function findBinary() {
  if (isDevWorkspace()) {
    const binPath = join(getPackageDir(), "bin", getLibFilename());
    if (existsSync(binPath))
      return binPath;
    const devPath = getDevBinaryPath();
    if (devPath)
      return devPath;
    const npmPath2 = resolveFromNpmPackage();
    if (npmPath2)
      return npmPath2;
    return null;
  }
  const npmPath = resolveFromNpmPackage();
  if (npmPath)
    return npmPath;
  return getDevBinaryPath();
}
// src/finder.ts
import { FFIType as FFIType2, JSCallback } from "bun:ffi";

// src/ffi.ts
import {
  CString,
  dlopen,
  FFIType,
  ptr,
  read
} from "bun:ffi";

// src/embedded.ts
async function importFile(promise) {
  try {
    return (await promise).default;
  } catch {
    return null;
  }
}
async function resolveEmbeddedLibPath() {
  if (process.platform === "darwin") {
    return importFile(import(`@ff-labs/fff-bin-darwin-${process.arch}/libfff_c.dylib`, {
      with: { type: "file" }
    }));
  }
  if (process.platform === "win32") {
    return importFile(import(`@ff-labs/fff-bin-win32-${process.arch}/fff_c.dll`, {
      with: { type: "file" }
    }));
  }
  if (process.platform === "linux") {
    return importFile(import(`@ff-labs/fff-bin-linux-${process.arch}-${typeof FFF_LIBC === "string" ? FFF_LIBC : "gnu"}/libfff_c.so`, { with: { type: "file" } }));
  }
  return null;
}
var embeddedLibPath = await resolveEmbeddedLibPath();

// src/fff-api.ts
function err(error) {
  return { ok: false, error };
}
function createGrepCursor(offset) {
  return { __brand: "GrepCursor", _offset: offset };
}

// src/ffi.ts
var GREP_MODE_PLAIN = 0;
var GREP_MODE_REGEX = 1;
var GREP_MODE_FUZZY = 2;
function grepModeToU8(mode) {
  switch (mode) {
    case "regex":
      return GREP_MODE_REGEX;
    case "fuzzy":
      return GREP_MODE_FUZZY;
    default:
      return GREP_MODE_PLAIN;
  }
}
var ffiDefinition = {
  fff_create_instance2: {
    args: [
      FFIType.cstring,
      FFIType.cstring,
      FFIType.cstring,
      FFIType.bool,
      FFIType.bool,
      FFIType.bool,
      FFIType.bool,
      FFIType.bool,
      FFIType.cstring,
      FFIType.cstring,
      FFIType.u64,
      FFIType.u64,
      FFIType.u64
    ],
    returns: FFIType.ptr
  },
  fff_create_instance_with: {
    args: [FFIType.ptr],
    returns: FFIType.ptr
  },
  fff_destroy: {
    args: [FFIType.ptr],
    returns: FFIType.void
  },
  fff_search: {
    args: [
      FFIType.ptr,
      FFIType.cstring,
      FFIType.cstring,
      FFIType.u32,
      FFIType.u32,
      FFIType.u32,
      FFIType.i32,
      FFIType.u32
    ],
    returns: FFIType.ptr
  },
  fff_glob: {
    args: [
      FFIType.ptr,
      FFIType.cstring,
      FFIType.cstring,
      FFIType.u32,
      FFIType.u32,
      FFIType.u32
    ],
    returns: FFIType.ptr
  },
  fff_search_directories: {
    args: [
      FFIType.ptr,
      FFIType.cstring,
      FFIType.cstring,
      FFIType.u32,
      FFIType.u32,
      FFIType.u32
    ],
    returns: FFIType.ptr
  },
  fff_search_mixed: {
    args: [
      FFIType.ptr,
      FFIType.cstring,
      FFIType.cstring,
      FFIType.u32,
      FFIType.u32,
      FFIType.u32,
      FFIType.i32,
      FFIType.u32
    ],
    returns: FFIType.ptr
  },
  fff_live_grep: {
    args: [
      FFIType.ptr,
      FFIType.cstring,
      FFIType.u8,
      FFIType.u64,
      FFIType.u32,
      FFIType.bool,
      FFIType.u32,
      FFIType.u32,
      FFIType.u64,
      FFIType.u32,
      FFIType.u32,
      FFIType.bool
    ],
    returns: FFIType.ptr
  },
  fff_multi_grep: {
    args: [
      FFIType.ptr,
      FFIType.cstring,
      FFIType.cstring,
      FFIType.u64,
      FFIType.u32,
      FFIType.bool,
      FFIType.u32,
      FFIType.u32,
      FFIType.u64,
      FFIType.u32,
      FFIType.u32,
      FFIType.bool
    ],
    returns: FFIType.ptr
  },
  fff_scan_files: {
    args: [FFIType.ptr],
    returns: FFIType.ptr
  },
  fff_is_scanning: {
    args: [FFIType.ptr],
    returns: FFIType.bool
  },
  fff_get_base_path: {
    args: [FFIType.ptr],
    returns: FFIType.ptr
  },
  fff_get_scan_progress: {
    args: [FFIType.ptr],
    returns: FFIType.ptr
  },
  fff_wait_for_scan: {
    args: [FFIType.ptr, FFIType.u64],
    returns: FFIType.ptr
  },
  fff_restart_index: {
    args: [FFIType.ptr, FFIType.cstring],
    returns: FFIType.ptr
  },
  fff_set_watch_callback: {
    args: [
      FFIType.ptr,
      FFIType.function,
      FFIType.ptr
    ],
    returns: FFIType.ptr
  },
  fff_watch: {
    args: [
      FFIType.ptr,
      FFIType.cstring,
      FFIType.ptr
    ],
    returns: FFIType.ptr
  },
  fff_unwatch: {
    args: [FFIType.ptr, FFIType.u64],
    returns: FFIType.ptr
  },
  fff_free_watch_events: {
    args: [FFIType.ptr],
    returns: FFIType.void
  },
  fff_watch_events_count: {
    args: [FFIType.ptr],
    returns: FFIType.u32
  },
  fff_watch_events_get_path: {
    args: [FFIType.ptr, FFIType.u32],
    returns: FFIType.ptr
  },
  fff_watch_events_get_kind: {
    args: [FFIType.ptr, FFIType.u32],
    returns: FFIType.u8
  },
  fff_refresh_git_status: {
    args: [FFIType.ptr],
    returns: FFIType.ptr
  },
  fff_track_query: {
    args: [FFIType.ptr, FFIType.cstring, FFIType.cstring],
    returns: FFIType.ptr
  },
  fff_get_historical_query: {
    args: [FFIType.ptr, FFIType.u64],
    returns: FFIType.ptr
  },
  fff_health_check: {
    args: [FFIType.ptr, FFIType.cstring],
    returns: FFIType.ptr
  },
  fff_free_search_result: {
    args: [FFIType.ptr],
    returns: FFIType.void
  },
  fff_search_result_get_item: {
    args: [FFIType.ptr, FFIType.u32],
    returns: FFIType.ptr
  },
  fff_search_result_get_score: {
    args: [FFIType.ptr, FFIType.u32],
    returns: FFIType.ptr
  },
  fff_free_dir_search_result: {
    args: [FFIType.ptr],
    returns: FFIType.void
  },
  fff_dir_search_result_get_item: {
    args: [FFIType.ptr, FFIType.u32],
    returns: FFIType.ptr
  },
  fff_dir_search_result_get_score: {
    args: [FFIType.ptr, FFIType.u32],
    returns: FFIType.ptr
  },
  fff_free_mixed_search_result: {
    args: [FFIType.ptr],
    returns: FFIType.void
  },
  fff_mixed_search_result_get_item: {
    args: [FFIType.ptr, FFIType.u32],
    returns: FFIType.ptr
  },
  fff_mixed_search_result_get_score: {
    args: [FFIType.ptr, FFIType.u32],
    returns: FFIType.ptr
  },
  fff_free_grep_result: {
    args: [FFIType.ptr],
    returns: FFIType.void
  },
  fff_grep_result_get_match: {
    args: [FFIType.ptr, FFIType.u32],
    returns: FFIType.ptr
  },
  fff_free_result: {
    args: [FFIType.ptr],
    returns: FFIType.void
  },
  fff_free_string: {
    args: [FFIType.ptr],
    returns: FFIType.void
  },
  fff_free_scan_progress: {
    args: [FFIType.ptr],
    returns: FFIType.void
  }
};
var lib = null;
function loadLibrary() {
  if (lib)
    return lib;
  const isEmbedded = embeddedLibPath?.includes("$bunfs") ?? false;
  const binaryPath = isEmbedded ? embeddedLibPath : findBinary() ?? embeddedLibPath;
  if (!binaryPath) {
    throw new Error(libNotFoundMessage());
  }
  lib = dlopen(binaryPath, ffiDefinition);
  return lib;
}
function libNotFoundMessage() {
  if (import.meta.url.includes("$bunfs")) {
    if (process.platform === "linux") {
      return [
        "You are running bun --compile with fff native library which CAN NOT resolve a binary",
        "On Linux the libc must be supplied at compile time so the native lib is bundled.",
        "Rebuild with:",
        `  bun build --compile --define FFF_LIBC='"gnu"'  ...   # glibc`,
        `  bun build --compile --define FFF_LIBC='"musl"' ...   # musl / Alpine`
      ].join(`
`);
    }
    return "fff native library was not embedded into this executable. Rebuild with `bun build --compile` and ensure the @ff-labs/fff-bin-* package for this platform is installed.";
  }
  return "fff native library not found. Build from source with `cargo build --release -p fff-c` or install the platform package.";
}
function encodeString(s) {
  return new TextEncoder().encode(`${s}\x00`);
}
function readCString(pointer) {
  if (pointer === null || pointer === 0)
    return null;
  return new CString(pointer).toString();
}
function snakeToCamel(obj) {
  if (obj === null || obj === undefined)
    return obj;
  if (typeof obj !== "object")
    return obj;
  if (Array.isArray(obj))
    return obj.map(snakeToCamel);
  const result = {};
  for (const [key, value] of Object.entries(obj)) {
    const camelKey = key.replace(/_([a-z])/g, (_, letter) => letter.toUpperCase());
    result[camelKey] = snakeToCamel(value);
  }
  return result;
}
var RES_SUCCESS = 0;
var RES_ERROR = 8;
var RES_HANDLE = 16;
var RES_INT_VALUE = 24;
var FFF_CREATE_OPTIONS_VERSION = 1;
var FFF_CREATE_OPTIONS_SIZE = 88;
var FCO_VERSION = 0;
var FCO_BASE_PATH = 8;
var FCO_FRECENCY_DB_PATH = 16;
var FCO_HISTORY_DB_PATH = 24;
var FCO_ENABLE_MMAP_CACHE = 32;
var FCO_ENABLE_CONTENT_INDEXING = 33;
var FCO_WATCH = 34;
var FCO_AI_MODE = 35;
var FCO_LOG_FILE_PATH = 40;
var FCO_LOG_LEVEL = 48;
var FCO_CACHE_BUDGET_MAX_FILES = 56;
var FCO_CACHE_BUDGET_MAX_BYTES = 64;
var FCO_CACHE_BUDGET_MAX_FILE_SIZE = 72;
var FCO_ENABLE_FS_ROOT_SCANNING = 80;
var FCO_ENABLE_HOME_DIR_SCANNING = 81;
function readResultEnvelope(resultPtr) {
  if (resultPtr === null) {
    return err("FFI returned null pointer");
  }
  const success = read.u8(resultPtr, RES_SUCCESS) !== 0;
  const library = loadLibrary();
  if (!success) {
    const errorPtr = read.ptr(resultPtr, RES_ERROR);
    const errorMsg = readCString(errorPtr) || "Unknown error";
    library.symbols.fff_free_result(resultPtr);
    return err(errorMsg);
  }
  const handlePtr = read.ptr(resultPtr, RES_HANDLE);
  const intValue = Number(read.i64(resultPtr, RES_INT_VALUE));
  library.symbols.fff_free_result(resultPtr);
  return { success: true, handlePtr, intValue };
}
function parseBoolResult(resultPtr) {
  const envelope = readResultEnvelope(resultPtr);
  if (!("success" in envelope))
    return envelope;
  return { ok: true, value: envelope.intValue !== 0 };
}
function parseIntResult(resultPtr) {
  const envelope = readResultEnvelope(resultPtr);
  if (!("success" in envelope))
    return envelope;
  return { ok: true, value: envelope.intValue };
}
function parseStringResult(resultPtr) {
  const envelope = readResultEnvelope(resultPtr);
  if (!("success" in envelope))
    return envelope;
  if (envelope.handlePtr === 0)
    return { ok: true, value: null };
  const library = loadLibrary();
  const str = readCString(envelope.handlePtr);
  library.symbols.fff_free_string(asPtr(envelope.handlePtr));
  return { ok: true, value: str };
}
function parseJsonResult(resultPtr) {
  const envelope = readResultEnvelope(resultPtr);
  if (!("success" in envelope))
    return envelope;
  if (envelope.handlePtr === 0)
    return { ok: true, value: undefined };
  const library = loadLibrary();
  const jsonStr = readCString(envelope.handlePtr);
  library.symbols.fff_free_string(asPtr(envelope.handlePtr));
  if (jsonStr === null || jsonStr === "")
    return { ok: true, value: undefined };
  try {
    return { ok: true, value: snakeToCamel(JSON.parse(jsonStr)) };
  } catch {
    return { ok: true, value: jsonStr };
  }
}
function parseVoidResult(resultPtr) {
  const envelope = readResultEnvelope(resultPtr);
  if (!("success" in envelope))
    return envelope;
  return { ok: true, value: undefined };
}
function ffiCreate(basePath, frecencyDbPath, historyDbPath, _useUnsafeNoLock, enableMmapCache, enableContentIndexing, watch, aiMode, logFilePath, logLevel, cacheBudgetMaxFiles, cacheBudgetMaxBytes, cacheBudgetMaxFileSize, enableFsRootScanning, enableHomeDirScanning) {
  const library = loadLibrary();
  const basePathCStr = encodeCStringBuf(basePath);
  const frecencyCStr = encodeCStringBuf(frecencyDbPath);
  const historyCStr = encodeCStringBuf(historyDbPath);
  const logFileCStr = encodeCStringBuf(logFilePath);
  const logLevelCStr = encodeCStringBuf(logLevel);
  const opts = Buffer.alloc(FFF_CREATE_OPTIONS_SIZE);
  opts.writeUInt32LE(FFF_CREATE_OPTIONS_VERSION, FCO_VERSION);
  writePtrLE(opts, FCO_BASE_PATH, basePathCStr);
  writePtrLE(opts, FCO_FRECENCY_DB_PATH, frecencyCStr);
  writePtrLE(opts, FCO_HISTORY_DB_PATH, historyCStr);
  opts.writeUInt8(enableMmapCache ? 1 : 0, FCO_ENABLE_MMAP_CACHE);
  opts.writeUInt8(enableContentIndexing ? 1 : 0, FCO_ENABLE_CONTENT_INDEXING);
  opts.writeUInt8(watch ? 1 : 0, FCO_WATCH);
  opts.writeUInt8(aiMode ? 1 : 0, FCO_AI_MODE);
  writePtrLE(opts, FCO_LOG_FILE_PATH, logFileCStr);
  writePtrLE(opts, FCO_LOG_LEVEL, logLevelCStr);
  opts.writeBigUInt64LE(cacheBudgetMaxFiles, FCO_CACHE_BUDGET_MAX_FILES);
  opts.writeBigUInt64LE(cacheBudgetMaxBytes, FCO_CACHE_BUDGET_MAX_BYTES);
  opts.writeBigUInt64LE(cacheBudgetMaxFileSize, FCO_CACHE_BUDGET_MAX_FILE_SIZE);
  opts.writeUInt8(enableFsRootScanning ? 1 : 0, FCO_ENABLE_FS_ROOT_SCANNING);
  opts.writeUInt8(enableHomeDirScanning ? 1 : 0, FCO_ENABLE_HOME_DIR_SCANNING);
  const resultPtr = library.symbols.fff_create_instance_with(ptr(opts));
  if (resultPtr === null) {
    return err("FFI returned null pointer");
  }
  const success = read.u8(resultPtr, RES_SUCCESS) !== 0;
  if (success) {
    const handlePtr = read.ptr(resultPtr, RES_HANDLE);
    const handle = handlePtr;
    library.symbols.fff_free_result(resultPtr);
    if (!handle || handle === 0) {
      return err("fff_create_instance_with returned null handle");
    }
    return { ok: true, value: handle };
  } else {
    const errorPtr = read.ptr(resultPtr, RES_ERROR);
    const errorMsg = readCString(errorPtr) || "Unknown error";
    library.symbols.fff_free_result(resultPtr);
    return err(errorMsg);
  }
}
function encodeCStringBuf(s) {
  if (!s)
    return null;
  return Buffer.from(s + "\x00", "utf-8");
}
function writePtrLE(buf, offset, target) {
  if (target == null) {
    buf.writeBigUInt64LE(0n, offset);
    return;
  }
  buf.writeBigUInt64LE(BigInt(ptr(target)), offset);
}
function ffiDestroy(handle) {
  const library = loadLibrary();
  library.symbols.fff_destroy(handle);
}
var SR_ITEMS = 0;
var SR_SCORES = 8;
var SR_COUNT = 16;
var SR_MATCHED = 20;
var SR_TOTAL = 24;
var SR_LOC_TAG = 28;
var SR_LOC_LINE = 32;
var SR_LOC_COL = 36;
var SR_LOC_END_LINE = 40;
var SR_LOC_END_COL = 44;
var FI_RELPATH = 0;
var FI_FNAME = 8;
var FI_GIT = 16;
var FI_SIZE = 24;
var FI_MODIFIED = 32;
var FI_ACCESS = 40;
var FI_MODFR = 48;
var FI_TOTAL_FR = 56;
var FI_SIZE_OF = 72;
var SC_TOTAL = 0;
var SC_BASE = 4;
var SC_FNAME = 8;
var SC_SPECIAL = 12;
var SC_FREC = 16;
var SC_DIST = 20;
var SC_CURFILE = 24;
var SC_COMBO = 28;
var SC_EXACT = 32;
var SC_MTYPE = 40;
var SC_SIZE_OF = 48;
function asPtr(n) {
  return n;
}
function readFileItemStruct(p) {
  const pp = asPtr(p);
  return {
    relativePath: readCString(read.ptr(pp, FI_RELPATH)) ?? "",
    fileName: readCString(read.ptr(pp, FI_FNAME)) ?? "",
    gitStatus: readCString(read.ptr(pp, FI_GIT)) ?? "",
    size: Number(read.u64(pp, FI_SIZE)),
    modified: Number(read.u64(pp, FI_MODIFIED)),
    accessFrecencyScore: Number(read.i64(pp, FI_ACCESS)),
    modificationFrecencyScore: Number(read.i64(pp, FI_MODFR)),
    totalFrecencyScore: Number(read.i64(pp, FI_TOTAL_FR))
  };
}
function readScoreStruct(p) {
  const pp = asPtr(p);
  return {
    total: read.i32(pp, SC_TOTAL),
    baseScore: read.i32(pp, SC_BASE),
    filenameBonus: read.i32(pp, SC_FNAME),
    specialFilenameBonus: read.i32(pp, SC_SPECIAL),
    frecencyBoost: read.i32(pp, SC_FREC),
    distancePenalty: read.i32(pp, SC_DIST),
    currentFilePenalty: read.i32(pp, SC_CURFILE),
    comboMatchBoost: read.i32(pp, SC_COMBO),
    exactMatch: read.u8(pp, SC_EXACT) !== 0,
    matchType: readCString(read.ptr(pp, SC_MTYPE)) ?? ""
  };
}
function parseSearchResult(resultPtr) {
  if (resultPtr === null) {
    return err("FFI returned null pointer");
  }
  const envelope = readResultEnvelope(resultPtr);
  if (!("success" in envelope))
    return envelope;
  if (envelope.handlePtr === 0) {
    return err("fff_search returned null search result");
  }
  const hp = asPtr(envelope.handlePtr);
  const count = read.u32(hp, SR_COUNT);
  const totalMatched = read.u32(hp, SR_MATCHED);
  const totalFiles = read.u32(hp, SR_TOTAL);
  const itemsBase = read.ptr(hp, SR_ITEMS);
  const scoresBase = read.ptr(hp, SR_SCORES);
  const locTag = read.u8(hp, SR_LOC_TAG);
  let location;
  if (locTag === 1) {
    location = { type: "line", line: read.i32(hp, SR_LOC_LINE) };
  } else if (locTag === 2) {
    location = {
      type: "position",
      line: read.i32(hp, SR_LOC_LINE),
      col: read.i32(hp, SR_LOC_COL)
    };
  } else if (locTag === 3) {
    location = {
      type: "range",
      start: { line: read.i32(hp, SR_LOC_LINE), col: read.i32(hp, SR_LOC_COL) },
      end: {
        line: read.i32(hp, SR_LOC_END_LINE),
        col: read.i32(hp, SR_LOC_END_COL)
      }
    };
  }
  const items = [];
  const scores = [];
  for (let i = 0;i < count; i++) {
    items.push(readFileItemStruct(itemsBase + i * FI_SIZE_OF));
    scores.push(readScoreStruct(scoresBase + i * SC_SIZE_OF));
  }
  loadLibrary().symbols.fff_free_search_result(hp);
  const result = { items, scores, totalMatched, totalFiles };
  if (location) {
    result.location = location;
  }
  return { ok: true, value: result };
}
var DSR_COUNT = 16;
var DSR_MATCHED = 20;
var DSR_TOTAL_DIRS = 24;
var DI_RELPATH = 0;
var DI_DIRNAME = 8;
var DI_MAX_FRECENCY = 16;
function readDirItemStruct(p) {
  const pp = asPtr(p);
  return {
    relativePath: readCString(read.ptr(pp, DI_RELPATH)) ?? "",
    dirName: readCString(read.ptr(pp, DI_DIRNAME)) ?? "",
    maxAccessFrecency: read.i32(pp, DI_MAX_FRECENCY)
  };
}
function parseDirSearchResult(resultPtr) {
  if (resultPtr === null) {
    return err("FFI returned null pointer");
  }
  const envelope = readResultEnvelope(resultPtr);
  if (!("success" in envelope))
    return envelope;
  if (envelope.handlePtr === 0) {
    return err("fff_search_directories returned null search result");
  }
  const hp = asPtr(envelope.handlePtr);
  const count = read.u32(hp, DSR_COUNT);
  const totalMatched = read.u32(hp, DSR_MATCHED);
  const totalDirs = read.u32(hp, DSR_TOTAL_DIRS);
  const library = loadLibrary();
  const items = [];
  const scores = [];
  for (let i = 0;i < count; i++) {
    const itemPtr = library.symbols.fff_dir_search_result_get_item(hp, i);
    if (itemPtr !== null && itemPtr !== 0) {
      items.push(readDirItemStruct(itemPtr));
    }
    const scorePtr = library.symbols.fff_dir_search_result_get_score(hp, i);
    if (scorePtr !== null && scorePtr !== 0) {
      scores.push(readScoreStruct(scorePtr));
    }
  }
  library.symbols.fff_free_dir_search_result(hp);
  return { ok: true, value: { items, scores, totalMatched, totalDirs } };
}
var MSR_COUNT = 16;
var MSR_MATCHED = 20;
var MSR_TOTAL_FILES = 24;
var MSR_TOTAL_DIRS = 28;
var MSR_LOC_TAG = 32;
var MSR_LOC_LINE = 36;
var MSR_LOC_COL = 40;
var MSR_LOC_END_LINE = 44;
var MSR_LOC_END_COL = 48;
var MI_TYPE = 0;
var MI_RELPATH = 8;
var MI_DISPLAY = 16;
var MI_GIT = 24;
var MI_SIZE = 32;
var MI_MODIFIED = 40;
var MI_ACCESS = 48;
var MI_MODFR = 56;
var MI_TOTAL_FR = 64;
function readMixedItemStruct(p) {
  const pp = asPtr(p);
  const itemType = read.u8(pp, MI_TYPE);
  if (itemType === 1) {
    return {
      type: "directory",
      item: {
        relativePath: readCString(read.ptr(pp, MI_RELPATH)) ?? "",
        dirName: readCString(read.ptr(pp, MI_DISPLAY)) ?? "",
        maxAccessFrecency: Number(read.i64(pp, MI_ACCESS))
      }
    };
  }
  return {
    type: "file",
    item: {
      relativePath: readCString(read.ptr(pp, MI_RELPATH)) ?? "",
      fileName: readCString(read.ptr(pp, MI_DISPLAY)) ?? "",
      gitStatus: readCString(read.ptr(pp, MI_GIT)) ?? "",
      size: Number(read.u64(pp, MI_SIZE)),
      modified: Number(read.u64(pp, MI_MODIFIED)),
      accessFrecencyScore: Number(read.i64(pp, MI_ACCESS)),
      modificationFrecencyScore: Number(read.i64(pp, MI_MODFR)),
      totalFrecencyScore: Number(read.i64(pp, MI_TOTAL_FR))
    }
  };
}
function parseMixedSearchResult(resultPtr) {
  if (resultPtr === null) {
    return err("FFI returned null pointer");
  }
  const envelope = readResultEnvelope(resultPtr);
  if (!("success" in envelope))
    return envelope;
  if (envelope.handlePtr === 0) {
    return err("fff_search_mixed returned null search result");
  }
  const hp = asPtr(envelope.handlePtr);
  const count = read.u32(hp, MSR_COUNT);
  const totalMatched = read.u32(hp, MSR_MATCHED);
  const totalFiles = read.u32(hp, MSR_TOTAL_FILES);
  const totalDirs = read.u32(hp, MSR_TOTAL_DIRS);
  const locTag = read.u8(hp, MSR_LOC_TAG);
  let location;
  if (locTag === 1) {
    location = { type: "line", line: read.i32(hp, MSR_LOC_LINE) };
  } else if (locTag === 2) {
    location = {
      type: "position",
      line: read.i32(hp, MSR_LOC_LINE),
      col: read.i32(hp, MSR_LOC_COL)
    };
  } else if (locTag === 3) {
    location = {
      type: "range",
      start: {
        line: read.i32(hp, MSR_LOC_LINE),
        col: read.i32(hp, MSR_LOC_COL)
      },
      end: {
        line: read.i32(hp, MSR_LOC_END_LINE),
        col: read.i32(hp, MSR_LOC_END_COL)
      }
    };
  }
  const library = loadLibrary();
  const items = [];
  const scores = [];
  for (let i = 0;i < count; i++) {
    const itemPtr = library.symbols.fff_mixed_search_result_get_item(hp, i);
    if (itemPtr !== null && itemPtr !== 0) {
      items.push(readMixedItemStruct(itemPtr));
    }
    const scorePtr = library.symbols.fff_mixed_search_result_get_score(hp, i);
    if (scorePtr !== null && scorePtr !== 0) {
      scores.push(readScoreStruct(scorePtr));
    }
  }
  library.symbols.fff_free_mixed_search_result(hp);
  const result = {
    items,
    scores,
    totalMatched,
    totalFiles,
    totalDirs
  };
  if (location) {
    result.location = location;
  }
  return { ok: true, value: result };
}
var GM_RELPATH = 0;
var GM_FNAME = 8;
var GM_GIT = 16;
var GM_LINE_CONTENT = 24;
var GM_MATCH_RANGES = 32;
var GM_CTX_BEFORE = 40;
var GM_CTX_AFTER = 48;
var GM_SIZE = 56;
var GM_MODIFIED = 64;
var GM_TOTAL_FR = 72;
var GM_ACCESS_FR = 80;
var GM_MOD_FR = 88;
var GM_LINE_NUM = 96;
var GM_BYTE_OFF = 104;
var GM_COL = 112;
var GM_MR_COUNT = 116;
var GM_CTX_B_COUNT = 120;
var GM_CTX_A_COUNT = 124;
var GM_FUZZY_SCORE = 128;
var GM_HAS_FUZZY = 130;
var GM_IS_BINARY = 131;
var GM_IS_DEFINITION = 132;
var GM_SIZE_OF = 136;
var GR_ITEMS = 0;
var GR_COUNT = 8;
var GR_MATCHED = 12;
var GR_FILES_SEARCHED = 16;
var GR_TOTAL_FILES = 20;
var GR_FILTERED = 24;
var GR_NEXT_OFFSET = 28;
var GR_REGEX_ERR = 32;
var MR_START = 0;
var MR_END = 4;
var MR_SIZE = 8;
function readCStringArray(base, count) {
  if (count === 0 || base === 0)
    return [];
  const result = [];
  const bp = asPtr(base);
  for (let i = 0;i < count; i++) {
    const strPtr = read.ptr(bp, i * 8);
    result.push(readCString(strPtr) ?? "");
  }
  return result;
}
function readGrepMatchStruct(p) {
  const pp = asPtr(p);
  const matchRangesPtr = read.ptr(pp, GM_MATCH_RANGES);
  const matchRangesCount = read.u32(pp, GM_MR_COUNT);
  const matchRanges = [];
  for (let i = 0;i < matchRangesCount; i++) {
    const base = matchRangesPtr + i * MR_SIZE;
    const bp = asPtr(base);
    matchRanges.push([read.u32(bp, MR_START), read.u32(bp, MR_END)]);
  }
  const hasFuzzy = read.u8(pp, GM_HAS_FUZZY) !== 0;
  const ctxBeforeCount = read.u32(pp, GM_CTX_B_COUNT);
  const ctxAfterCount = read.u32(pp, GM_CTX_A_COUNT);
  const match = {
    relativePath: readCString(read.ptr(pp, GM_RELPATH)) ?? "",
    fileName: readCString(read.ptr(pp, GM_FNAME)) ?? "",
    gitStatus: readCString(read.ptr(pp, GM_GIT)) ?? "",
    lineContent: readCString(read.ptr(pp, GM_LINE_CONTENT)) ?? "",
    size: Number(read.u64(pp, GM_SIZE)),
    modified: Number(read.u64(pp, GM_MODIFIED)),
    totalFrecencyScore: Number(read.i64(pp, GM_TOTAL_FR)),
    accessFrecencyScore: Number(read.i64(pp, GM_ACCESS_FR)),
    modificationFrecencyScore: Number(read.i64(pp, GM_MOD_FR)),
    isBinary: read.u8(pp, GM_IS_BINARY) !== 0,
    lineNumber: Number(read.u64(pp, GM_LINE_NUM)),
    col: read.u32(pp, GM_COL),
    byteOffset: Number(read.u64(pp, GM_BYTE_OFF)),
    matchRanges
  };
  if (hasFuzzy) {
    match.fuzzyScore = read.u16(pp, GM_FUZZY_SCORE);
  }
  if (ctxBeforeCount > 0) {
    match.contextBefore = readCStringArray(read.ptr(pp, GM_CTX_BEFORE), ctxBeforeCount);
  }
  if (ctxAfterCount > 0) {
    match.contextAfter = readCStringArray(read.ptr(pp, GM_CTX_AFTER), ctxAfterCount);
  }
  if (read.u8(pp, GM_IS_DEFINITION) !== 0) {
    match.isDefinition = true;
  }
  return match;
}
function parseGrepResult(resultPtr) {
  const envelope = readResultEnvelope(resultPtr);
  if (!("success" in envelope))
    return envelope;
  if (envelope.handlePtr === 0) {
    return err("grep returned null result");
  }
  const hp = asPtr(envelope.handlePtr);
  const count = read.u32(hp, GR_COUNT);
  const totalMatched = read.u32(hp, GR_MATCHED);
  const totalFilesSearched = read.u32(hp, GR_FILES_SEARCHED);
  const totalFiles = read.u32(hp, GR_TOTAL_FILES);
  const filteredFileCount = read.u32(hp, GR_FILTERED);
  const nextFileOffset = read.u32(hp, GR_NEXT_OFFSET);
  const regexErrPtr = read.ptr(hp, GR_REGEX_ERR);
  const regexFallbackError = readCString(regexErrPtr) ?? undefined;
  const itemsBase = read.ptr(hp, GR_ITEMS);
  const items = [];
  for (let i = 0;i < count; i++) {
    items.push(readGrepMatchStruct(itemsBase + i * GM_SIZE_OF));
  }
  loadLibrary().symbols.fff_free_grep_result(hp);
  const grepResult = {
    items,
    totalMatched,
    totalFilesSearched,
    totalFiles,
    filteredFileCount,
    nextCursor: nextFileOffset > 0 ? createGrepCursor(nextFileOffset) : null
  };
  if (regexFallbackError) {
    grepResult.regexFallbackError = regexFallbackError;
  }
  return { ok: true, value: grepResult };
}
function ffiSearch(handle, query, currentFile, maxThreads, pageIndex, pageSize, comboBoostMultiplier, minComboCount) {
  const library = loadLibrary();
  const resultPtr = library.symbols.fff_search(handle, ptr(encodeString(query)), ptr(encodeString(currentFile)), maxThreads, pageIndex, pageSize, comboBoostMultiplier, minComboCount);
  return parseSearchResult(resultPtr);
}
function ffiGlob(handle, pattern, currentFile, maxThreads, pageIndex, pageSize) {
  const library = loadLibrary();
  const resultPtr = library.symbols.fff_glob(handle, ptr(encodeString(pattern)), ptr(encodeString(currentFile)), maxThreads, pageIndex, pageSize);
  return parseSearchResult(resultPtr);
}
function ffiSearchDirectories(handle, query, currentFile, maxThreads, pageIndex, pageSize) {
  const library = loadLibrary();
  const resultPtr = library.symbols.fff_search_directories(handle, ptr(encodeString(query)), ptr(encodeString(currentFile ?? "")), maxThreads, pageIndex, pageSize);
  return parseDirSearchResult(resultPtr);
}
function ffiSearchMixed(handle, query, currentFile, maxThreads, pageIndex, pageSize, comboBoostMultiplier, minComboCount) {
  const library = loadLibrary();
  const resultPtr = library.symbols.fff_search_mixed(handle, ptr(encodeString(query)), ptr(encodeString(currentFile)), maxThreads, pageIndex, pageSize, comboBoostMultiplier, minComboCount);
  return parseMixedSearchResult(resultPtr);
}
function ffiLiveGrep(handle, query, mode, maxFileSize, maxMatchesPerFile, smartCase, fileOffset, pageLimit, timeBudgetMs, beforeContext, afterContext, classifyDefinitions) {
  const library = loadLibrary();
  const resultPtr = library.symbols.fff_live_grep(handle, ptr(encodeString(query)), grepModeToU8(mode), BigInt(maxFileSize), maxMatchesPerFile, smartCase, fileOffset, pageLimit, BigInt(timeBudgetMs), beforeContext, afterContext, classifyDefinitions);
  return parseGrepResult(resultPtr);
}
function ffiMultiGrep(handle, patternsJoined, constraints, maxFileSize, maxMatchesPerFile, smartCase, fileOffset, pageLimit, timeBudgetMs, beforeContext, afterContext, classifyDefinitions) {
  const library = loadLibrary();
  const resultPtr = library.symbols.fff_multi_grep(handle, ptr(encodeString(patternsJoined)), ptr(encodeString(constraints)), BigInt(maxFileSize), maxMatchesPerFile, smartCase, fileOffset, pageLimit, BigInt(timeBudgetMs), beforeContext, afterContext, classifyDefinitions);
  return parseGrepResult(resultPtr);
}
function ffiScanFiles(handle) {
  const library = loadLibrary();
  const resultPtr = library.symbols.fff_scan_files(handle);
  return parseVoidResult(resultPtr);
}
function ffiIsScanning(handle) {
  const library = loadLibrary();
  return library.symbols.fff_is_scanning(handle);
}
function ffiGetBasePath(handle) {
  const library = loadLibrary();
  const resultPtr = library.symbols.fff_get_base_path(handle);
  return parseStringResult(resultPtr);
}
var SP_COUNT = 0;
var SP_SCANNING = 8;
var SP_WATCHER_READY = 9;
var SP_WARMUP_COMPLETE = 10;
function ffiGetScanProgress(handle) {
  const library = loadLibrary();
  const resultPtr = library.symbols.fff_get_scan_progress(handle);
  const envelope = readResultEnvelope(resultPtr);
  if (!("success" in envelope))
    return envelope;
  if (envelope.handlePtr === 0) {
    return err("scan progress returned null");
  }
  const hp = asPtr(envelope.handlePtr);
  const result = {
    scannedFilesCount: Number(read.u64(hp, SP_COUNT)),
    isScanning: read.u8(hp, SP_SCANNING) !== 0,
    isWatcherReady: read.u8(hp, SP_WATCHER_READY) !== 0,
    isWarmupComplete: read.u8(hp, SP_WARMUP_COMPLETE) !== 0
  };
  library.symbols.fff_free_scan_progress(hp);
  return { ok: true, value: result };
}
function ffiWaitForScan(handle, timeoutMs) {
  const library = loadLibrary();
  const resultPtr = library.symbols.fff_wait_for_scan(handle, BigInt(timeoutMs));
  return parseBoolResult(resultPtr);
}
function ffiRestartIndex(handle, newPath) {
  const library = loadLibrary();
  const resultPtr = library.symbols.fff_restart_index(handle, ptr(encodeString(newPath)));
  return parseVoidResult(resultPtr);
}
var FFF_WATCH_OPTIONS_VERSION = 1;
var FFF_WATCH_OPTIONS_SIZE = 24;
var FWO_VERSION = 0;
var FWO_IGNORE = 8;
var FWO_IGNORE_COUNT = 16;
var WATCH_EVENT_KINDS = [
  "created",
  "modified",
  "removed",
  "rescan"
];
function readWatchEventBatch(batchPtr) {
  if (batchPtr === null || batchPtr === 0) {
    return [];
  }
  const bp = batchPtr;
  const symbols = loadLibrary().symbols;
  const count = symbols.fff_watch_events_count(bp);
  const events = [];
  for (let i = 0;i < count; i++) {
    const path = symbols.fff_watch_events_get_path(bp, i);
    const kind = symbols.fff_watch_events_get_kind(bp, i);
    events.push({
      path: readCString(path) ?? "",
      kind: WATCH_EVENT_KINDS[kind] ?? "rescan"
    });
  }
  symbols.fff_free_watch_events(bp);
  return events;
}
function ffiSetWatchCallback(handle, callback) {
  const library = loadLibrary();
  if (callback.ptr === null) {
    return err("watch callback has been closed");
  }
  const resultPtr = library.symbols.fff_set_watch_callback(handle, callback.ptr, null);
  return parseVoidResult(resultPtr);
}
function ffiWatch(handle, pattern, ignore = []) {
  const library = loadLibrary();
  const opts = Buffer.alloc(FFF_WATCH_OPTIONS_SIZE);
  opts.writeUInt32LE(FFF_WATCH_OPTIONS_VERSION, FWO_VERSION);
  const ignoreBuffers = ignore.map((entry) => encodeString(entry));
  const ignorePtrs = Buffer.alloc(Math.max(ignoreBuffers.length, 1) * 8);
  for (let i = 0;i < ignoreBuffers.length; i++) {
    ignorePtrs.writeBigUInt64LE(BigInt(ptr(ignoreBuffers[i])), i * 8);
  }
  opts.writeBigUInt64LE(ignoreBuffers.length > 0 ? BigInt(ptr(ignorePtrs)) : 0n, FWO_IGNORE);
  opts.writeUInt32LE(ignoreBuffers.length, FWO_IGNORE_COUNT);
  const resultPtr = library.symbols.fff_watch(handle, ptr(encodeString(pattern)), ptr(opts));
  return parseIntResult(resultPtr);
}
function ffiUnwatch(handle, watchId) {
  const library = loadLibrary();
  const resultPtr = library.symbols.fff_unwatch(handle, BigInt(watchId));
  return parseBoolResult(resultPtr);
}
function ffiRefreshGitStatus(handle) {
  const library = loadLibrary();
  const resultPtr = library.symbols.fff_refresh_git_status(handle);
  return parseIntResult(resultPtr);
}
function ffiTrackQuery(handle, query, filePath) {
  const library = loadLibrary();
  const resultPtr = library.symbols.fff_track_query(handle, ptr(encodeString(query)), ptr(encodeString(filePath)));
  return parseBoolResult(resultPtr);
}
function ffiGetHistoricalQuery(handle, offset) {
  const library = loadLibrary();
  const resultPtr = library.symbols.fff_get_historical_query(handle, BigInt(offset));
  return parseStringResult(resultPtr);
}
function ffiHealthCheck(handle, testPath) {
  const library = loadLibrary();
  const resultPtr = library.symbols.fff_health_check(handle ?? 0, ptr(encodeString(testPath)));
  return parseJsonResult(resultPtr);
}
function ensureLoaded() {
  loadLibrary();
}
function isAvailable() {
  try {
    loadLibrary();
    return true;
  } catch {
    return false;
  }
}

// src/finder.ts
class FileFinder {
  handle;
  watchHandlers = new Map;
  watchJsCallback = null;
  constructor(handle) {
    this.handle = handle;
  }
  static create(options) {
    const result = ffiCreate(options.basePath, options.frecencyDbPath ?? "", options.historyDbPath ?? "", options.useUnsafeNoLock ?? false, !(options.disableMmapCache ?? false), !(options.disableContentIndexing ?? options.disableMmapCache ?? false), !(options.disableWatch ?? false), options.aiMode ?? false, options.logFilePath ?? "", options.logLevel ?? "", BigInt(options.cacheBudgetMaxFiles ?? 0), BigInt(options.cacheBudgetMaxBytes ?? 0), BigInt(options.cacheBudgetMaxFileSize ?? 0), options.enableFsRootScanning ?? false, options.enableHomeDirScanning ?? false);
    if (!result.ok) {
      return result;
    }
    return { ok: true, value: new FileFinder(result.value) };
  }
  destroy() {
    if (this.handle !== null) {
      this.watchHandlers.clear();
      ffiDestroy(this.handle);
      this.handle = null;
      this.watchJsCallback?.close();
      this.watchJsCallback = null;
    }
  }
  get isDestroyed() {
    return this.handle === null;
  }
  ensureAlive() {
    if (this.handle === null) {
      return err("FileFinder instance has been destroyed.");
    }
    return { ok: true, value: this.handle };
  }
  fileSearch(query, options) {
    const guard = this.ensureAlive();
    if (!guard.ok)
      return guard;
    return ffiSearch(guard.value, query, options?.currentFile ?? "", options?.maxThreads ?? 0, options?.pageIndex ?? 0, options?.pageSize ?? 0, options?.comboBoostMultiplier ?? 0, options?.minComboCount ?? 0);
  }
  glob(pattern, options) {
    const guard = this.ensureAlive();
    if (!guard.ok)
      return guard;
    return ffiGlob(guard.value, pattern, options?.currentFile ?? "", options?.maxThreads ?? 0, options?.pageIndex ?? 0, options?.pageSize ?? 0);
  }
  directorySearch(query, options) {
    const guard = this.ensureAlive();
    if (!guard.ok)
      return guard;
    return ffiSearchDirectories(guard.value, query, options?.currentFile ?? null, options?.maxThreads ?? 0, options?.pageIndex ?? 0, options?.pageSize ?? 0);
  }
  mixedSearch(query, options) {
    const guard = this.ensureAlive();
    if (!guard.ok)
      return guard;
    return ffiSearchMixed(guard.value, query, options?.currentFile ?? "", options?.maxThreads ?? 0, options?.pageIndex ?? 0, options?.pageSize ?? 0, options?.comboBoostMultiplier ?? 0, options?.minComboCount ?? 0);
  }
  grep(query, options) {
    const guard = this.ensureAlive();
    if (!guard.ok)
      return guard;
    return ffiLiveGrep(guard.value, query, options?.mode ?? "plain", options?.maxFileSize ?? 0, options?.maxMatchesPerFile ?? 0, options?.smartCase ?? true, options?.cursor?._offset ?? 0, options?.pageSize ?? 0, options?.timeBudgetMs ?? 0, options?.beforeContext ?? 0, options?.afterContext ?? 0, options?.classifyDefinitions ?? false);
  }
  multiGrep(options) {
    const guard = this.ensureAlive();
    if (!guard.ok)
      return guard;
    if (!options.patterns || options.patterns.length === 0) {
      return err("patterns array must have at least 1 element");
    }
    return ffiMultiGrep(guard.value, options.patterns.join(`
`), options.constraints ?? "", options.maxFileSize ?? 0, options.maxMatchesPerFile ?? 0, options.smartCase ?? true, options.cursor?._offset ?? 0, options.pageSize ?? 0, options.timeBudgetMs ?? 0, options.beforeContext ?? 0, options.afterContext ?? 0, options.classifyDefinitions ?? false);
  }
  scanFiles() {
    const guard = this.ensureAlive();
    if (!guard.ok)
      return guard;
    return ffiScanFiles(guard.value);
  }
  isScanning() {
    if (this.handle === null)
      return false;
    return ffiIsScanning(this.handle);
  }
  getBasePath() {
    const guard = this.ensureAlive();
    if (!guard.ok)
      return guard;
    return ffiGetBasePath(guard.value);
  }
  getScanProgress() {
    const guard = this.ensureAlive();
    if (!guard.ok)
      return guard;
    return ffiGetScanProgress(guard.value);
  }
  async waitForScan(timeoutMs = 5000) {
    const guard = this.ensureAlive();
    if (!guard.ok)
      return guard;
    const deadline = Date.now() + timeoutMs;
    while (this.isScanning()) {
      if (Date.now() >= deadline) {
        return { ok: true, value: false };
      }
      await new Promise((resolve) => setTimeout(resolve, 50));
    }
    return { ok: true, value: true };
  }
  waitForScanBlocking(timeoutMs = 5000) {
    const guard = this.ensureAlive();
    if (!guard.ok)
      return guard;
    return ffiWaitForScan(guard.value, timeoutMs);
  }
  async waitForIndexReady(timeoutMs = 5000) {
    const guard = this.ensureAlive();
    if (!guard.ok)
      return guard;
    const deadline = Date.now() + timeoutMs;
    while (true) {
      const progress = this.getScanProgress();
      if (!progress.ok)
        return progress;
      if (!progress.value.isScanning && progress.value.isWarmupComplete) {
        return { ok: true, value: true };
      }
      if (Date.now() >= deadline) {
        return { ok: true, value: false };
      }
      await new Promise((resolve) => setTimeout(resolve, 50));
    }
  }
  reindex(newPath) {
    const guard = this.ensureAlive();
    if (!guard.ok)
      return guard;
    return ffiRestartIndex(guard.value, newPath);
  }
  refreshGitStatus() {
    const guard = this.ensureAlive();
    if (!guard.ok)
      return guard;
    return ffiRefreshGitStatus(guard.value);
  }
  trackQuery(query, selectedFilePath) {
    const guard = this.ensureAlive();
    if (!guard.ok)
      return guard;
    return ffiTrackQuery(guard.value, query, selectedFilePath);
  }
  getHistoricalQuery(offset) {
    const guard = this.ensureAlive();
    if (!guard.ok)
      return guard;
    return ffiGetHistoricalQuery(guard.value, offset);
  }
  ensureWatchTrampoline(handle) {
    if (this.watchJsCallback !== null)
      return { ok: true, value: undefined };
    const jsCallback = new JSCallback((watchId, batchPtr, _userData) => {
      const events = readWatchEventBatch(batchPtr);
      const handler = this.watchHandlers.get(Number(watchId));
      if (handler !== undefined && events.length > 0) {
        handler(events);
      }
    }, {
      args: [FFIType2.ptr, FFIType2.ptr, FFIType2.ptr],
      returns: FFIType2.void,
      threadsafe: true
    });
    const registered = ffiSetWatchCallback(handle, jsCallback);
    if (!registered.ok) {
      jsCallback.close();
      return registered;
    }
    this.watchJsCallback = jsCallback;
    return { ok: true, value: undefined };
  }
  watch(patternOrCallback, callbackOrOptions, maybeOptions) {
    const noPattern = typeof patternOrCallback === "function";
    const pattern = noPattern ? "" : patternOrCallback;
    const callback = noPattern ? patternOrCallback : callbackOrOptions;
    const options = noPattern ? callbackOrOptions : maybeOptions;
    if (typeof callback !== "function") {
      return err("watch callback must be a function");
    }
    const guard = this.ensureAlive();
    if (!guard.ok)
      return guard;
    const trampoline = this.ensureWatchTrampoline(guard.value);
    if (!trampoline.ok)
      return trampoline;
    const result = ffiWatch(guard.value, pattern, options?.ignore ?? []);
    if (!result.ok)
      return result;
    const watchId = result.value;
    this.watchHandlers.set(watchId, callback);
    return { ok: true, value: () => this.unwatchById(watchId) };
  }
  unwatchById(watchId) {
    if (!this.watchHandlers.delete(watchId))
      return;
    if (this.handle !== null) {
      ffiUnwatch(this.handle, watchId);
    }
  }
  healthCheck(testPath) {
    return ffiHealthCheck(this.handle, testPath || "");
  }
  static isAvailable() {
    return isAvailable();
  }
  static ensureLoaded() {
    ensureLoaded();
  }
  static healthCheckStatic(testPath) {
    return ffiHealthCheck(null, testPath || "");
  }
}
export {
  getTriple,
  getNpmPackageName,
  getLibFilename,
  getLibExtension,
  findBinary,
  binaryExists,
  FileFinder
};

//# debugId=5525184652AF367A64756E2164756E21
