// Package source validates bounded assignment inputs and records their provenance.
// It contains no network behavior: callers provide already-open readers, so local
// files and remote responses follow the same parsing contract.
package source
