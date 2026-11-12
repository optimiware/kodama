package tenancy

// No effectful port is declared in this neutral package.
//
// Tenant, project and membership administration commits are expressed by the
// operation registry (tenant.create, project.create, project.update,
// membership.grant, membership.revoke, membership.list) and their complete
// commit payloads live in the capability-local tenancyadmin subpackage. That
// subpackage imports the authority/event/receipt values a complete unit needs;
// this package cannot, because authorize and event already import tenancy and
// importing them here would create a cycle.
//
// Current membership facts reach the authorizer through the consumer-owned
// authorize.AuthorityReader port, not through a tenancy repository. Adding a
// generic Save/Update split here would recreate the handoff boundary S01
// forbids.
