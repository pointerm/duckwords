// Package evidence validates a successful DuckWords run and publishes a compact,
// sanitized submission bundle. It treats captured output and operational logs as
// untrusted inputs: every persisted value is allowlisted, reconciled, and bounded.
package evidence
