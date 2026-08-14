// Package acquire loads bounded assignment inputs from a local regular file or an
// allowlisted HTTPS origin. It owns transport and source-policy checks, while the
// source and words packages remain the single parsers for their respective formats.
package acquire
