import * as Runtime from "effect/Runtime";
/**
 * Runs an Effect as the Node process main program, interrupting the fiber on
 * `SIGINT` or `SIGTERM` and invoking the configured teardown to determine the
 * process exit code.
 *
 * @category running
 * @since 4.0.0
 */
export const runMain = /*#__PURE__*/Runtime.makeRunMain(({
  fiber,
  teardown
}) => {
  let receivedSignal = false;
  fiber.addObserver(exit => {
    process.removeListener("SIGINT", onSigint);
    process.removeListener("SIGTERM", onSigint);
    teardown(exit, code => {
      if (receivedSignal || code !== 0) {
        process.exit(code);
      }
    });
  });
  function onSigint() {
    receivedSignal = true;
    fiber.interruptUnsafe(fiber.id);
  }
  process.on("SIGINT", onSigint);
  process.on("SIGTERM", onSigint);
});
//# sourceMappingURL=NodeRuntime.js.map