// Package bridges contains the thin composition adapters that connect the
// consumer-owned ports to their concrete Shell, Userland and
// infrastructure seams.
//
// Every bridge validates its dependencies at construction, refuses a partial
// composition, propagates context cancellation, maps failures to the seam's
// stable fault vocabulary and never logs, renders or retains credential or key
// material. The package holds no business rule: policy decisions stay in the
// capability packages and the composition root only wires the chosen
// production or test adapter.
package bridges
