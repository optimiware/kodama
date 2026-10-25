// src/service-contender.ts
import { spawn } from "node:child_process";
var stderrLimit = 8 * 1024;
function spawnServiceContender2(command, args, env) {
  const child = spawn(command, args, {
    detached: true,
    windowsHide: true,
    stdio: ["ignore", "ignore", "pipe"],
    env: { ...process.env, ...env }
  });
  let error;
  let closed = false;
  let stderr = Buffer.alloc(0);
  const onStderr = (chunk) => {
    const tail = chunk.subarray(-stderrLimit);
    stderr = tail.length === stderrLimit ? Buffer.from(tail) : Buffer.concat([stderr.subarray(-(stderrLimit - tail.length)), tail]);
  };
  child.stderr?.on("data", onStderr);
  if (child.stderr !== null && "unref" in child.stderr && typeof child.stderr.unref === "function")
    child.stderr.unref();
  child.once("error", (cause) => {
    error = new Error("Failed to start server", { cause });
  });
  child.once("close", () => {
    closed = true;
  });
  child.unref();
  return {
    child,
    error: () => error,
    closed: () => closed,
    stderr: () => stderr.toString("utf8").trim(),
    release: () => {
      child.stderr?.off("data", onStderr);
      child.stderr?.resume();
      stderr = Buffer.alloc(0);
    }
  };
}
function contenderPool2(timing) {
  const contenders = new Set;
  let failure;
  let spawnDelay = timing.spawnDelay;
  let lastSpawn = 0;
  return {
    failure: () => failure,
    serviceAnswered() {
      spawnDelay = timing.spawnDelay;
    },
    evict(pid) {
      for (const item of contenders) {
        if (item.child.pid === pid || contenderFinished2(item)) {
          item.release();
          contenders.delete(item);
        }
      }
      failure = undefined;
      lastSpawn = 0;
    },
    recruitNow() {
      lastSpawn = Date.now() - spawnDelay;
    },
    reap() {
      const finished = [...contenders].filter(contenderFinished2);
      failure ??= finished.map(contenderFailure).find((error) => error !== undefined);
      if (finished.some((item) => item.child.exitCode === 0))
        spawnDelay = Math.min(spawnDelay * 2, timing.maxSpawnDelay);
      finished.forEach((item) => contenders.delete(item));
      return contenders.size === 0 ? failure : undefined;
    },
    shouldRecruit(registered) {
      if (lastSpawn === 0 && registered)
        lastSpawn = Date.now();
      return failure === undefined && contenders.size < 2 && Date.now() - lastSpawn >= spawnDelay;
    },
    add(contender) {
      contenders.add(contender);
      lastSpawn = Date.now();
    },
    releaseAll() {
      contenders.forEach((contender) => contender.release());
    }
  };
}
function contenderFailure(contender) {
  const error = contender.error();
  if (error !== undefined)
    return error;
  if (contender.child.exitCode !== null && contender.child.exitCode !== 0)
    return startupError(`Server process exited with code ${contender.child.exitCode}`, contender.stderr());
  if (contender.child.signalCode !== null)
    return startupError(`Server process terminated by ${contender.child.signalCode}`, contender.stderr());
  return;
}
function contenderFinished2(contender) {
  return contender.error() !== undefined || contender.closed();
}
function startupError(message, stderr) {
  return new Error(stderr ? `${message}
${stderr}` : message);
}

export { spawnServiceContender2, contenderPool2, contenderFinished2 };
