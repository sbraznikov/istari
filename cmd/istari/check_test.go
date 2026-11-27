package main_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sbraznikov/istari/internal/tickets/github/githubtest"
)

const conforming = `# Add rate limits

## Context

Limits protect the API.

## What needs to be done

- Change ` + "`go.mod`" + `.

## Acceptance criteria

- [ ] A burst is refused.

## Out of scope

- Per-user limits.

## Dependencies

- Needs: none

---

| Type | Size | Priority | Repository | Spec |
|---|---|---|---|---|
| feature | S | P1 | ` + "`acme/web`" + ` | |
`

func TestTicketCheckPrintsDeviationsAndExitsByThem(t *testing.T) {
	t.Parallel()
	repo := gitRepo(t, "git@github.com:acme/web.git", map[string]string{"go.mod": "module x\n"})
	config := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(config, nil, 0o600); err != nil { // an installation with the defaults
		t.Fatal(err)
	}
	good := filepath.Join(t.TempDir(), "good.md")
	if err := os.WriteFile(good, []byte(conforming), 0o644); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(t.TempDir(), "bad.md")
	if err := os.WriteFile(bad, []byte(strings.Replace(conforming, "## Out of scope\n\n- Per-user limits.\n\n", "", 1)), 0o644); err != nil {
		t.Fatal(err)
	}

	if out, stderr, code := istariIn(t, repo, config, "ticket", "check", good); code != 0 {
		t.Fatalf("a conforming ticket: exit %d\n%s%s", code, out, stderr)
	}
	out, _, code := istariIn(t, repo, config, "ticket", "check", bad)
	if code != 1 || !strings.Contains(out, "Out of scope") {
		t.Fatalf("a ticket without Out of scope: exit %d\n%s", code, out)
	}
	stdout, _, _ := istariIn(t, repo, config, "--json", "ticket", "check", bad)
	var res struct {
		Deviations []struct{ Rule, Message string }
	}
	if json.Unmarshal([]byte(stdout), &res) != nil || len(res.Deviations) != 1 || res.Deviations[0].Rule != "section" {
		t.Fatalf("--json = %s", stdout)
	}
}

func TestTicketCheckOfATrackerTicketWritesNothing(t *testing.T) {
	t.Parallel()
	gh := githubtest.New(t)
	n := gh.Seed("Add rate limits", strings.SplitN(conforming, "\n", 2)[1], "alex", "type:feature", "size:S", "priority:P1")
	repo := gitRepo(t, "git@github.com:acme/web.git", map[string]string{"go.mod": "module x\n"})
	d := startDaemon(t, newDataDir(t)) //nolint:forbidigo // no project and no due timer: no loop pass decides the outcome
	var before []struct{ Cursor string }
	d.mustCLI(t, &before, "tail")
	issueBefore := gh.Issue(n)

	cmd := []string{"ticket", "check", "#" + strconv.Itoa(n)}
	env := []string{"PATH=" + fakeGH(t, gh.Token), "GITHUB_API_URL=" + gh.URL}
	out, stderr, code := istariEnv(t, repo, d.config, env, cmd...)
	if code != 0 {
		t.Fatalf("check #%d: exit %d\n%s%s", n, code, out, stderr)
	}
	var after []struct{ Cursor string }
	d.mustCLI(t, &after, "tail")
	if len(after) != len(before) {
		t.Fatalf("check changed the event log: %d events before, %d after", len(before), len(after))
	}
	if issueAfter := gh.Issue(n); len(issueAfter.Events) != len(issueBefore.Events) || len(issueAfter.Comments) != 0 {
		t.Fatalf("check wrote to the tracker: %+v", issueAfter)
	}
}
