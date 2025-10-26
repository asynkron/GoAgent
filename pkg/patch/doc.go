// Package patch provides helpers for parsing and applying unified-diff style patches.
//
// The package is extracted from GoAgent's internal command implementation so that it can be
// reused by other tools. It exposes primitives to parse patch payloads, apply them to the
// filesystem, or operate on in-memory documents which makes it straightforward to embed in
// editors and testing utilities.
package patch
