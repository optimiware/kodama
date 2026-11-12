# Context repository knowledge

A context repository holds project knowledge as Markdown. The knowledge lives
next to the code it describes and is referenced from task handoffs, so a fresh
agent session reads the same durable context instead of disposable workspace
state.

## Skills

A skill is a Markdown document under `skills/` whose file name is a lowercase
slug. Skills are reusable instructions: [build-and-test](skills/build-and-test.md)
covers building and testing an image, and
[promote-and-verify](skills/promote-and-verify.md) covers promotion and
readiness verification.

Repository conventions belong in [conventions](conventions.md). Knowledge is
Markdown only: no binary blobs, no embedded credentials and no secret values.
Declare secret names, never their values.
