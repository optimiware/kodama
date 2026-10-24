# Inventory exceptions: domain language

**Application tenant** — a customer organization inside this application. It owns its memberships and
its exceptions. It is unrelated to any tenant of a software factory or hosting platform.

**Membership** — a subject's role in one application tenant (`viewer`, `reviewer`, `admin`, or the service
role `worker`). Membership is the only source of authority; claims in a token grant nothing.

**Exception** — an inventory gap: the ERP says one quantity, a count says another. States: `open`,
`pending`, `resolved`, `failed`, `unknown`.

**Resolution** — a reviewer's decision to adjust the ERP balance to a stated quantity, with a reason.
Requesting it never resolves the exception by itself.

**Intent** — the durable local record that an ERP adjustment must be applied, with a stable operation key.

**Operation key** — the stable ERP idempotency and reconciliation key of one adjustment
(`inv-adj-<exception id>`). Created before the first ERP request and reused for every retry and lookup.

**Acknowledgment** — the ERP's confirmation (with its reference) that the adjustment is applied. Only an
acknowledgment makes an exception `resolved`.

**Unknown** — the ERP outcome cannot be established (lost response, unreachable lookup). It is shown as
unknown, never as resolved, and is reconciled by looking up the same operation key.

**Worker** — a process acting for exactly one tenant under its own service identity.
