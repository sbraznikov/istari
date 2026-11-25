package runner_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/runner"
	"github.com/sbraznikov/istari/internal/tickets/journal"
)

// The repository's main branch: a test runner that runs every tests/*.sh,
// and the checks in .istari/pipeline.toml.
const pipelineFile = "[checks]\ntypecheck = \"true\"\ntest      = \"sh run-tests.sh\"\nverify_red = true\n"
const runTests = "#!/bin/sh\nfor t in tests/*.sh; do [ -f \"$t\" ] || continue; sh \"$t\" || exit 1; done\n"

type cpFixture struct {
	*wsFixture
	cp   *runner.Checkpoints
	ws   runner.Workspace
	run  runner.Run
	tk   core.Ticket
	head func() string
}

func newCheckpoints(t *testing.T) *cpFixture { return newCheckpointsWith(t, pipelineFile) }

func newCheckpointsWith(t *testing.T, pipelineToml string) *cpFixture {
	t.Helper()
	f := newWorkspaces(t)
	seed := filepath.Join(f.root, "seed")
	write(t, filepath.Join(seed, ".istari", "pipeline.toml"), pipelineToml)
	write(t, filepath.Join(seed, "run-tests.sh"), runTests)
	write(t, filepath.Join(seed, "src.txt"), "old\n")
	gitIn(t, seed, "add", ".")
	gitIn(t, seed, "commit", "-q", "-m", "checks")
	gitIn(t, seed, "push", "-q", "origin", "main")
	tk := f.ticket(t, 21, "Add the feature")
	ws := runner.NewWorkspaces(f.store, runner.WorkspaceOptions{DataDir: f.data, Server: "file://" + filepath.Join(f.root, "remotes")})
	w, err := ws.Ensure(ctx, tk)
	if err != nil {
		t.Fatal(err)
	}
	cp := runner.NewCheckpoints(ws, journal.New(nil, f.store, nil), nil)
	remote := filepath.Join(f.root, "remotes", "acme", "web.git")
	return &cpFixture{wsFixture: f, cp: cp, ws: w, tk: tk,
		run:  runner.Run{ID: "r_1", Ticket: tk.Ref, Role: "implementer", Profile: "implementer-1", Dir: w.Dir},
		head: func() string { return strings.TrimSpace(gitIn(t, remote, "rev-parse", w.Branch)) }}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func (f *cpFixture) checkpoint(t *testing.T, message string) *journal.CheckpointEntry {
	t.Helper()
	e, err := f.cp.Make(ctx, f.run, message)
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	return e.Checkpoint
}

func TestACheckpointCommitsLeftoverWorkWithTheTrailerAndPushesIt(t *testing.T) {
	t.Parallel()
	f := newCheckpoints(t)
	write(t, filepath.Join(f.ws.Dir, "src.txt"), "feature\n")
	write(t, filepath.Join(f.ws.Dir, "tests", "feature.sh"), "grep -q feature src.txt\n")
	c := f.checkpoint(t, "The feature works; next the docs")
	if c.Done != "The feature works" || c.Next != "next the docs" {
		t.Fatalf("done %q next %q", c.Done, c.Next)
	}
	if c.Red || c.Checks["test"] != "green" || c.Checks["typecheck"] != "green" {
		t.Fatalf("checks = %v red %v, want green", c.Checks, c.Red)
	}
	if msg := gitIn(t, f.ws.Dir, "log", "-1", "--format=%B"); !strings.Contains(msg, "Ticket: "+f.tk.Ref) {
		t.Fatalf("the commit lacks the trailer: %q", msg)
	}
	if c.Commit == "" || f.head() != strings.TrimSpace(gitIn(t, f.ws.Dir, "rev-parse", "HEAD")) || !strings.HasPrefix(f.head(), c.Commit) {
		t.Fatalf("pushed %s, entry %s", f.head(), c.Commit)
	}
	if strings.Join(c.Files, " ") != "src.txt tests/feature.sh" {
		t.Fatalf("files = %v", c.Files)
	}
	if strings.Contains(strings.Join(c.Notes, " "), "not counted") {
		t.Fatalf("a red-first slice was not counted: %v", c.Notes)
	}
}

func TestFailingTestsMakeTheCheckpointRedWhateverTheAgentSays(t *testing.T) {
	t.Parallel()
	f := newCheckpoints(t)
	write(t, filepath.Join(f.ws.Dir, "tests", "feature.sh"), "grep -q feature src.txt || { echo MISSING FEATURE; exit 1; }\n")
	c := f.checkpoint(t, "All tests are green")
	if !c.Red || c.Checks["test"] != "red" {
		t.Fatalf("checks = %v red %v, want red", c.Checks, c.Red)
	}
	if !strings.Contains(c.Failures["test"], "MISSING FEATURE") {
		t.Fatalf("failures = %v, want the test's output", c.Failures)
	}
}

func TestANewTestThatPassesOnThePreviousCodeDoesNotCount(t *testing.T) {
	t.Parallel()
	f := newCheckpoints(t)
	write(t, filepath.Join(f.ws.Dir, "tests", "always.sh"), "true\n")
	c := f.checkpoint(t, "Added a test")
	if notes := strings.Join(c.Notes, " "); !strings.Contains(notes, "not counted") || !strings.Contains(notes, "tests/always.sh") || !c.Red {
		t.Fatalf("notes = %v red %v, want the slice not counted and the checkpoint red", c.Notes, c.Red)
	}
}

func TestASliceWithoutATestChangeIsFlagged(t *testing.T) {
	t.Parallel()
	f := newCheckpoints(t)
	write(t, filepath.Join(f.ws.Dir, "src.txt"), "feature\n")
	if c := f.checkpoint(t, "Changed code only"); !strings.Contains(strings.Join(c.Notes, " "), "no test") {
		t.Fatalf("notes = %v, want the slice flagged", c.Notes)
	}
}

func TestCheckCommandsComeOnlyFromTheMainBranch(t *testing.T) {
	t.Parallel()
	f := newCheckpoints(t)
	write(t, filepath.Join(f.ws.Dir, "tests", "feature.sh"), "grep -q feature src.txt\n")
	// The ticket's branch makes its tests always pass: that does not count.
	write(t, filepath.Join(f.ws.Dir, ".istari", "pipeline.toml"), "[checks]\ntypecheck = \"true\"\ntest = \"true\"\n")
	if c := f.checkpoint(t, "Green now"); !c.Red {
		t.Fatalf("checks = %v: the branch's own check command was used", c.Checks)
	}
}

func TestAfterACrashOnlyWorkAfterTheLastCheckpointIsLost(t *testing.T) {
	t.Parallel()
	f := newCheckpoints(t)
	write(t, filepath.Join(f.ws.Dir, "src.txt"), "feature\n")
	write(t, filepath.Join(f.ws.Dir, "tests", "feature.sh"), "grep -q feature src.txt\n")
	f.checkpoint(t, "Saved")
	write(t, filepath.Join(f.ws.Dir, "later.txt"), "not saved\n")
	if err := os.RemoveAll(f.ws.Dir); err != nil { // the machine dies
		t.Fatal(err)
	}
	clone := filepath.Join(f.data, "repos", "acme", "web.git")
	gitIn(t, clone, "worktree", "prune")
	gitIn(t, clone, "branch", "-D", f.ws.Branch)
	w, err := runner.NewWorkspaces(f.store, runner.WorkspaceOptions{DataDir: f.data, Server: "file://" + filepath.Join(f.root, "remotes")}).Ensure(ctx, f.tk)
	if err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(w.Dir, "tests", "feature.sh")) || exists(filepath.Join(w.Dir, "later.txt")) {
		t.Fatal("want the checkpointed work back and nothing after it")
	}
}

func TestACheckpointWithNothingNewIsRefused(t *testing.T) {
	t.Parallel()
	f := newCheckpoints(t)
	write(t, filepath.Join(f.ws.Dir, "src.txt"), "feature\n")
	f.checkpoint(t, "First")
	if _, err := f.cp.Make(ctx, f.run, "Again"); err == nil || !strings.Contains(err.Error(), "nothing") {
		t.Fatalf("a second checkpoint without changes: %v", err)
	}
}

func TestTestDataAloneIsNoTestChange(t *testing.T) {
	t.Parallel()
	f := newCheckpoints(t)
	write(t, filepath.Join(f.ws.Dir, "testdata", "input.txt"), "data\n")
	if c := f.checkpoint(t, "Fixtures"); !strings.Contains(strings.Join(c.Notes, " "), "no test change") {
		t.Fatalf("notes = %v, want the slice flagged", c.Notes)
	}
}

func TestChecksRunWithoutTheDaemonsSecrets(t *testing.T) {
	// Not parallel: sets the process environment's GITHUB_TOKEN and ISTARI_TOKEN.
	t.Setenv("GITHUB_TOKEN", "daemon-secret")
	t.Setenv("ISTARI_TOKEN", "run-secret")
	f := newCheckpointsWith(t, "[checks]\ntypecheck = \"env > env.out; test -z \\\"$GITHUB_TOKEN$ISTARI_TOKEN\\\"\"\ntest = \"true\"\n")
	write(t, filepath.Join(f.ws.Dir, "tests", "x.sh"), "true\n")
	if c := f.checkpoint(t, "Env"); c.Checks["typecheck"] != "green" {
		out, _ := os.ReadFile(filepath.Join(f.ws.Dir, "env.out"))
		t.Fatalf("a check saw the daemon's secrets:\n%s", out)
	}
}

func TestARedPreviousCheckpointCannotJudgeRedFirst(t *testing.T) {
	t.Parallel()
	f := newCheckpoints(t)
	write(t, filepath.Join(f.ws.Dir, "tests", "broken.sh"), "exit 1\n")
	f.checkpoint(t, "Broken on purpose") // red
	os.Remove(filepath.Join(f.ws.Dir, "tests", "broken.sh"))
	write(t, filepath.Join(f.ws.Dir, "tests", "always.sh"), "true\n")
	c := f.checkpoint(t, "A vacuous test")
	if notes := strings.Join(c.Notes, " "); !strings.Contains(notes, "cannot be judged") {
		t.Fatalf("notes = %v, want verify_red reported as not judged", c.Notes)
	}
}

// newLoggedCheckpoints makes a fixture whose test command writes the
// directory of each of its runs to a log outside the repository, as one
// line, and "left <dir>" when it finds the file "leftover" that it leaves
// behind in every run.
func newLoggedCheckpoints(t *testing.T) (*cpFixture, func() []string) {
	t.Helper()
	log := filepath.Join(t.TempDir(), "runs.log")
	test := "pwd -P >> " + log + "; test -e leftover && echo left $(pwd -P) >> " + log + "; touch leftover; sh run-tests.sh"
	f := newCheckpointsWith(t, "[checks]\ntypecheck = \"true\"\ntest = \""+test+"\"\nverify_red = true\n")
	runs := func() []string {
		data, _ := os.ReadFile(log)
		os.Remove(log)
		return strings.Split(strings.TrimSpace(string(data)), "\n")
	}
	return f, runs
}

// elsewhere returns the runs that were not in the ticket's worktree.
func (f *cpFixture) elsewhere(t *testing.T, runs []string) []string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(f.ws.Dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range runs {
		if r != dir && r != "left "+dir {
			out = append(out, r)
		}
	}
	return out
}

// slice writes a code change and a test that fails without it.
func (f *cpFixture) slice(t *testing.T, name string) {
	t.Helper()
	write(t, filepath.Join(f.ws.Dir, name+".txt"), name+"\n")
	write(t, filepath.Join(f.ws.Dir, "tests", name+".sh"), "test -f "+name+".txt\n")
}

func TestAGreenPreviousCheckpointIsNotRunAgainWithoutTheNewTests(t *testing.T) {
	t.Parallel()
	f, runs := newLoggedCheckpoints(t)
	f.slice(t, "first")
	if c := f.checkpoint(t, "First slice"); c.Checks["test"] != "green" || c.Checks["verify_red"] != "green" {
		t.Fatalf("checks = %v, want green", c.Checks)
	}
	runs()
	f.slice(t, "second")
	c := f.checkpoint(t, "Second slice")
	if c.Checks["verify_red"] != "green" {
		t.Fatalf("checks = %v notes %v, want verify_red green", c.Checks, c.Notes)
	}
	if got := f.elsewhere(t, runs()); len(got) != 1 {
		t.Fatalf("the test command ran %d times on the previous code (%v), want once", len(got), got)
	}
}

func TestTheVerifyRedWorktreeIsRemovedWithTheTicketsWorktree(t *testing.T) {
	t.Parallel()
	f := newCheckpoints(t)
	f.slice(t, "first")
	f.checkpoint(t, "First slice")
	clone := filepath.Join(f.data, "repos", "acme", "web.git")
	worktrees := func() []string {
		var out []string
		for _, line := range strings.Split(gitIn(t, clone, "worktree", "list", "--porcelain"), "\n") {
			if dir, ok := strings.CutPrefix(line, "worktree "); ok {
				out = append(out, dir)
			}
		}
		return out
	}
	before := worktrees()
	if len(before) != 3 {
		t.Fatalf("worktrees after a checkpoint = %v, want the clone, the ticket's and verify_red's", before)
	}
	ws := runner.NewWorkspaces(f.store, runner.WorkspaceOptions{DataDir: f.data, Server: "file://" + filepath.Join(f.root, "remotes")})
	if got, err := ws.Remove(ctx, f.tk); err != nil || got.State != runner.WorkspaceRemoved {
		t.Fatalf("Remove = %+v, %v", got, err)
	}
	if after := worktrees(); len(after) != 1 {
		t.Fatalf("worktrees after removal = %v, want only the clone", after)
	}
	for _, dir := range before[1:] {
		if exists(dir) {
			t.Fatalf("%s is still there", dir)
		}
	}
}

func TestVerifyRedRunsInOneCleanWorktreeOfTheTicketNextToItsOwn(t *testing.T) {
	t.Parallel()
	f, runs := newLoggedCheckpoints(t)
	// An ignored file is left behind too: only a full clean removes it.
	write(t, filepath.Join(f.ws.Dir, ".gitignore"), "leftover\n")
	f.slice(t, "first")
	f.checkpoint(t, "First slice")
	first := f.elsewhere(t, runs())
	// The first slice of a branch has no recorded result: the previous code
	// runs as it is, then with the new tests.
	if len(first) < 2 || first[0] != first[1] {
		t.Fatalf("first checkpoint ran on the previous code in %v, want twice in one directory", first)
	}
	red := first[0]
	ticketDir, _ := filepath.EvalSymlinks(f.ws.Dir)
	if filepath.Dir(red) != filepath.Dir(ticketDir) {
		t.Fatalf("verify_red ran in %s, want next to the ticket's worktree %s", red, ticketDir)
	}
	f.slice(t, "second")
	f.checkpoint(t, "Second slice")
	if second := f.elsewhere(t, runs()); strings.Join(second, " ") != red {
		t.Fatalf("second checkpoint ran on the previous code as %v, want once in %s and nothing left over", second, red)
	}
}

func TestAProjectsOwnCheckRunsAtACheckpointByItsName(t *testing.T) {
	t.Parallel()
	f := newCheckpointsWith(t, "[checks]\ntypecheck = \"true\"\ntest = \"sh run-tests.sh\"\nformat = \"! grep -l '  $' *.txt\"\nlint = \"true\"\n")
	f.slice(t, "tidy")
	if c := f.checkpoint(t, "Formatted"); c.Red || c.Checks["format"] != "green" || c.Checks["lint"] != "green" {
		t.Fatalf("checks = %v red %v, want format and lint green", c.Checks, c.Red)
	}
	write(t, filepath.Join(f.ws.Dir, "messy.txt"), "trailing  \n")
	c := f.checkpoint(t, "Messy")
	if !c.Red || c.Checks["format"] != "red" || c.Checks["lint"] != "green" {
		t.Fatalf("checks = %v red %v, want format red", c.Checks, c.Red)
	}
	if !strings.Contains(c.Failures["format"], "messy.txt") {
		t.Fatalf("failures = %v, want the format check's output", c.Failures)
	}
}

func TestACheckAtPRDoesNotRunAtACheckpoint(t *testing.T) {
	t.Parallel()
	marker := filepath.Join(t.TempDir(), "ran")
	f := newCheckpointsWith(t, "[checks]\ntypecheck = \"true\"\ntest = \"sh run-tests.sh\"\ntidy = { run = \"touch "+marker+"; exit 1\", at = [\"pr\"] }\n")
	f.slice(t, "first")
	if c := f.checkpoint(t, "First"); c.Red || c.Checks["tidy"] != "" || exists(marker) {
		t.Fatalf("checks = %v red %v ran %v, want tidy left for the pull request", c.Checks, c.Red, exists(marker))
	}
}

func TestRedFirstJudgedOnTheWholeTestCommandIsNoted(t *testing.T) {
	t.Parallel()
	f := newCheckpoints(t)
	f.slice(t, "first")
	c := f.checkpoint(t, "First slice")
	if c.Checks["verify_red"] != "green" {
		t.Fatalf("checks = %v, want verify_red green", c.Checks)
	}
	if notes := strings.Join(c.Notes, " "); !strings.Contains(notes, "judged on the whole test command") {
		t.Fatalf("notes = %v, want red first noted as judged on the whole test command", c.Notes)
	}
}
