package main_test

import (
	"os"
	"strings"
	"testing"
)

// An agent's run sets ISTARI_SOCKET and ISTARI_TOKEN, and the CLI finds the
// daemon by ISTARI_SOCKET first: a binary started by a test would otherwise
// act on the real ticket of the run that runs the tests (CODING_STANDARDS.md
// S7.8, #154).
func TestTheTestsDoNotSeeTheIstariEnvironmentOfTheRunThatStartsThem(t *testing.T) {
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "ISTARI_") {
			t.Errorf("the tests' environment has %s, want no ISTARI_ variable", kv)
		}
	}
}
