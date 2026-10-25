import * as Cause from "../../Cause.js";
import * as SchemaIssue from "../../SchemaIssue.js";
/** @internal */
export function getSchemaIssue(cause) {
  let issue;
  for (const reason of cause.reasons) {
    if (!Cause.isFailReason(reason) || !SchemaIssue.isIssue(reason.error)) {
      return undefined;
    }
    issue ??= reason.error;
  }
  return issue;
}
/** @internal */
export function getSchemaIssueOrThrow(cause, message) {
  const issue = getSchemaIssue(cause);
  if (issue === undefined) {
    throw new Error(message, {
      cause
    });
  }
  return issue;
}
//# sourceMappingURL=cause.js.map