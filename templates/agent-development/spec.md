# Example intent

Add a scoped health endpoint to the inventory service.

## Acceptance criteria

- `GET /healthz` returns `200` and a JSON status.
- The existing test suite passes.
- No credential or environment-specific value is committed.
- The implementation is reviewed by the code and security profiles.
