// Package identity implements the store-backed adapters for the identity,
// tenancy, resource-binding, service-account and credential ports.
//
// Every mutation port is a thin adapter: the capability package (tenancyadmin,
// resource, token, service) decides the policy and builds one complete domain
// commit unit, and this package maps that unit to exactly one store.Unit — its
// guarded locks, business row writes, receipt, audit rows and event publication
// intent — and calls store.Executor.Execute. Reads map to Executor.Read and
// store.Scanner.Query. The adapter owns no authorization, state machine or SQL:
// it only persists the facts the unit already carries.
//
// # Port to table/column mapping
//
// Table and column names come only from store.SchemaDesign(). The mapping below
// is derived from each unit's fields; a unit field with no column, or a
// required column with no unit field, is recorded here instead of being
// dropped silently.
//
//	port method                     table / columns
//	------------------------------- -------------------------------------------
//	tenancyadmin.CreateTenant       tenants: id, name, version=1,
//	                                derivation_key=JSON(token.TenantKey),
//	                                created_at=unit.At
//	tenancyadmin.RotateTenantKey    tenants: derivation_key=JSON(next key),
//	                                name; version advanced by the adapter under
//	                                ExpectVersion=unit.ExpectedVersion
//	tenancyadmin.CreateProject      projects: id, name, version=1,
//	                                created_at=unit.At
//	tenancyadmin.UpdateProject      projects: name; version advanced under
//	                                ExpectVersion=unit.ExpectedVersion
//	tenancyadmin.GrantMembership    memberships: id (allocated), principal_id,
//	                                roles=JSON, version; created_at=unit.At on
//	                                insert. ExpectedVersion==0 inserts; otherwise
//	                                the existing scoped row is updated under
//	                                ExpectVersion.
//	tenancyadmin.RevokeMembership   memberships: guarded delete under
//	                                ExpectVersion of the scoped row
//	tenancyadmin.ListMemberships    memberships scan (tenant-wide or project
//	                                scope), ordered by id, keyset paged
//	resource.BindingPort.Apply      resource_bindings: id, resource_id,
//	                                service_id, origins=JSON, operations=JSON,
//	                                reportable_events=JSON, metadata=JSON,
//	                                version; created_at=unit.Receipt.At on insert
//	resource.BindingDeletePort      resource_bindings: guarded delete under
//	                                ExpectVersion
//	resource.BindingReadPort        resource_bindings: one scoped read by id
//	resource.BindingQueryPort       resource_bindings scan, ordered by id
//	token.MutationPort              executions or services: token_nonce,
//	                                token_expires_at; services also state/roles from
//	                                the optional ServiceChange. Guarded by
//	                                ExpectVersion and, for an execution, by
//	                                Expect principal_id=unit.PrincipalID.
//	token.CredentialReader          executions or services: token_nonce,
//	                                token_expires_at, state, version and the
//	                                committed principal (executions.principal_id,
//	                                or the service identity)
//	token.TenantKeyReader           tenants: derivation_key JSON
//	service.AdminPort.Create        services: id, name (=id; see gap below),
//	                                state, roles=JSON, version=1,
//	                                created_at=unit.At
//	service.AdminPort.Update        services: roles; version advanced under
//	                                ExpectVersion and Expect state=ExpectedState
//	service.AdminPort.SetStatus     services: state; on disablement also
//	                                token_nonce=NULL, token_expires_at=NULL
//	                                (RevokeAll) under ExpectVersion
//	service.ServiceReader           services: id, state, roles, version,
//	                                token_nonce, token_expires_at
//	identity.PrincipalReader        executions (principal_id, state, version),
//	                                services (id, state, roles, version) or
//	                                memberships (principal_id, roles, version)
//	authorize.AuthorityReader       memberships, services, executions, actions,
//	                                resource_bindings, tasks, pipelines,
//	                                resources as named by each AuthorityRead
//
// # Not a port of this package
//
// authorize.CurrentPrincipal is deliberately not implemented here. Its
// Resolve(ctx, token) orchestration belongs to the token package:
// token.Resolver bootstraps a candidate scope through store.CredentialLookup
// (implemented by the concrete stores) and then verifies the MAC and current
// nonce/expiry/principal state inside that candidate scope. This package
// therefore carries no token parsing, MAC verification or authentication
// policy; it implements authorize.AuthorityReader and identity.PrincipalReader.
//
// # Receipt modes
//
// Every mutation above uses the default store.ReceiptInsert mode (the zero
// ReceiptMode): the store inserts the receipt already completed, so
// receipts.completed_at pairs with the durable result and an equal replay applies
// nothing. No port here inserts an incomplete receipt or completes one, so the
// trigger completion mode is unused here.
//
// # Known mapping gaps
//
//   - service.CreateUnit carries no display name, but services.name is
//     non-nullable and unique per (tenant_id, project_id, name). The adapter
//     stores the service identity as the name so the row is complete and
//     deterministic. Reproducer: service.CreateUnit{Service: service.Service{ID:
//     "svc"}} validates, yet services has no value to write to `name` except an
//     invented one; proposed fix: add Name to service.Service/CreateUnit (and
//     UpdateUnit) or make services.name nullable.
//   - The services row has no principal_id column. By convention of the
//     credential-lookup seam a service credential's principal is the service
//     identity, so the adapter maps token.MutationUnit.PrincipalID exactly for
//     executions (executions.principal_id guard) and requires
//     PrincipalID == ServiceID for a service mutation; token.CredentialReader
//     returns the committed principal. A caller that names a different service
//     principal is refused with invalid_input instead of silently losing it.
//   - service.AdminPort.SetStatus reports RevokedCredentials from the row read
//     before the guarded write; the guard is the row version, so a concurrent
//     change is a version_conflict rather than a wrong count.
package identity
