package schema

import (
	_ "embed"
	"slices"
)

// patched is the schema with the SPEC.md 16.2 fix applied, as `make schema`
// writes it. The drift tests keep it equal to the schema repository's copy.
//
//go:embed schema.patched.json
var patched []byte

// Patched returns the embedded, patched schema. Each call returns a fresh copy,
// so no caller can change what another sees.
func Patched() []byte { return slices.Clone(patched) }
