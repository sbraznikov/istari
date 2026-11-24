package main_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sbraznikov/istari/internal/tickets/github/githubtest"
)

func TestTicketCreateMakesAPlanOnceAndDepsPrintsItsTree(t *testing.T) {
	gh := githubtest.New(t)
	dataDir := newDataDir(t)
	config := filepath.Join(t.TempDir(), "config.toml")
	cfg := fmt.Sprintf("[daemon]\ndata_dir = %q\n\n[[project]]\nname = \"web\"\nrepo = %q\n", dataDir, githubtest.Repo)
	if err := os.WriteFile(config, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	d := startDaemonWithConfig(t, dataDir, config, "PATH="+fakeGH(t, gh.Token), "GITHUB_API_URL="+gh.URL)
	plan := filepath.Join(t.TempDir(), "plan.yaml")
	os.WriteFile(plan, []byte("tickets:\n  - key: base\n    title: The base\n    body: \"## Context\\n\\nFirst.\\n\"\n  - key: top\n    title: On top\n    needs: [base]\n    body: \"## Context\\n\\nNeeds #base.\\n\"\n"), 0o644)
	var refs map[string]string
	for range 2 { // the same key twice: one plan
		d.mustCLI(t, &refs, "ticket", "create", "--file", plan, "--idempotency-key", "plan-1")
	}
	if gh.IssueCount() != 2 || refs["base"] == "" || refs["top"] == "" {
		t.Fatalf("issues %d, refs %v; want two tickets once", gh.IssueCount(), refs)
	}
	out, stderr, code := d.cli(t, "ticket", "deps")
	if code != 0 || !strings.Contains(out, refs["top"]+"  new") || !strings.Contains(out, "needs "+refs["base"]+"  new") {
		t.Fatalf("ticket deps: exit %d %s\n%s", code, stderr, out)
	}
	md := filepath.Join(t.TempDir(), "one.md")
	os.WriteFile(md, []byte("# A single ticket\n\n## Context\n\nOne.\n"), 0o644)
	d.mustCLI(t, &refs, "ticket", "create", "--file", md)
	if gh.IssueCount() != 3 || gh.Issue(3).Title != "A single ticket" {
		t.Fatalf("a single ticket: %d issues, %+v", gh.IssueCount(), gh.Issue(3))
	}
}

func TestPlanSubmitReadsItsPlanFromStandardInput(t *testing.T) {
	d := startDaemon(t, newDataDir(t))
	cmd := exec.Command(binary, "--config", d.config, "plan", "submit", "--file", "-")
	cmd.Stdin = strings.NewReader("summary: s\n")
	out, err := cmd.CombinedOutput()
	// Read and sent: the daemon answers that a plan is the planner's.
	if err == nil || !strings.Contains(string(out), "token_invalid") {
		t.Fatalf("plan submit from stdin: %v\n%s", err, out)
	}
}
