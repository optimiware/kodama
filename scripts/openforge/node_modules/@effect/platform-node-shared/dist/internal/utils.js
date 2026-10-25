import * as PlatformError from "effect/PlatformError";
/** @internal */
export const handleErrnoException = (module, method) => (err, [path]) => {
  let reason = "Unknown";
  switch (err.code) {
    case "ENOENT":
      reason = "NotFound";
      break;
    case "EACCES":
      reason = "PermissionDenied";
      break;
    case "EEXIST":
      reason = "AlreadyExists";
      break;
    case "EISDIR":
      reason = "BadResource";
      break;
    case "ENOTDIR":
      reason = "BadResource";
      break;
    case "EBUSY":
      reason = "Busy";
      break;
    case "ELOOP":
      reason = "BadResource";
      break;
  }
  return PlatformError.systemError({
    _tag: reason,
    module,
    method,
    pathOrDescriptor: path,
    syscall: err.syscall,
    cause: err
  });
};
//# sourceMappingURL=utils.js.map