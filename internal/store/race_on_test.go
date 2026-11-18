//go:build race

package store

// The race detector slows memory-bound loops several times over.
const raceBuild = true
