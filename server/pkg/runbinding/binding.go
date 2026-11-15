// Package runbinding publishes launcher-observed identities for cooperative
// same-UID consumers. Records are not authentication against the host owner.
package runbinding

// Spec is supplied by the daemon, never by agent custom environment or RPC.
// Dir must be an existing private directory outside task writable roots.
type Spec struct{ Dir, AgentID, TaskID, IssueID string }
