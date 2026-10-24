// Package adapters bridges the agent-profile module to its peers: the pipeline
// module's profile lookup, the execution module's agent resolver, admission,
// keyed creation and planner seams, the identity authority, and read-only
// pinned-policy and attempt-metadata readers for the gateway and tool proxy.
//
// The execution bridges are one chain (see Chain): expanding profiles before
// durable admission, on both the ordinary and the keyed path, and populating the
// runtime context's profile and design baseline from the frozen step at claim
// time. Enabling the agent step kind with only part of the chain would admit
// steps with no image or leave attempts without their pins, so Chain is the only
// source of agent-enabled hooks and capabilities. Nothing here makes a network
// call inside a transaction or queries a profile from Planner.Plan.
package adapters
