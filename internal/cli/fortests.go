//go:build istaritest

package cli

// forTests is true in a binary built with the istaritest tag: only such a
// binary accepts istari daemon --stepped (spec section 3.1).
const forTests = true
