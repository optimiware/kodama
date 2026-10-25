// src/effect/generated/client-error.ts
import { Schema } from "effect";

class ClientError2 extends Schema.TaggedError()("ClientError", {
  cause: Schema.Defect()
}) {
}

export { ClientError2 };
