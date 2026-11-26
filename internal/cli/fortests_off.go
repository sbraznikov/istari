//go:build !istaritest

package cli

// forTests is false in a production binary: it refuses istari daemon
// --stepped (spec section 3.1).
const forTests = false
