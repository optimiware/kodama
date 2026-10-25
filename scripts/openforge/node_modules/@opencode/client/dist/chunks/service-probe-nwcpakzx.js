// src/promise/generated/client-error.ts
class ClientError3 extends Error {
  reason;
  name = "ClientError";
  constructor(reason, options) {
    const detail = options?.detail ?? (options?.cause instanceof Error ? options.cause.message : undefined);
    super(detail ? `${reason}: ${detail}` : reason, options);
    this.reason = reason;
  }
}

export { ClientError3 };
