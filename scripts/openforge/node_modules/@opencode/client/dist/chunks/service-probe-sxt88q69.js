// src/service-version.ts
function matchesVersion2(version, options) {
  if (options.version === undefined)
    return true;
  if (version === undefined)
    return false;
  if (typeof options.version === "function")
    return options.version(version);
  return version === options.version;
}

export { matchesVersion2 };
