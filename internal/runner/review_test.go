package runner_test

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/harness"
	"github.com/sbraznikov/istari/internal/harness/claudecode"
	"github.com/sbraznikov/istari/internal/runner"
	"github.com/sbraznikov/istari/internal/tickets"
	"github.com/sbraznikov/istari/internal/tickets/journal"
)

type reviewFixture struct {
	*wsFixture
	reviews *runner.Reviews
	run     runner.Run
	ref     tickets.Ref
}

const reviewTicket = `## What needs to be done

- Add a cache in front of the store.

## Acceptance criteria

- [ ] A second read of the same key does not reach the store.`

// newReviews makes a ticket whose branch changes cache/cache.go (lines 3–5
// new) on top of main, which has CODING_STANDARDS.md.
func newReviews(t *testing.T) *reviewFixture {
	t.Helper()
	f := newWorkspaces(t)
	seed := filepath.Join(f.root, "seed")
	write(t, filepath.Join(seed, "cache", "cache.go"), "package cache\n\n")
	write(t, filepath.Join(seed, "CODING_STANDARDS.md"), "# Standards\n\n- Every exported\n  function has a doc comment.\n")
	gitIn(t, seed, "add", ".")
	gitIn(t, seed, "commit", "-q", "-m", "cache")
	gitIn(t, seed, "push", "-q", "origin", "main")
	tk := f.ticket(t, 31, "Add a cache")
	ws, err := f.ws.Ensure(ctx, tk)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(ws.Dir, "cache", "cache.go"), "package cache\n\nfunc Get(key string) string {\n\treturn key\n}\n")
	gitIn(t, ws.Dir, "add", ".")
	gitIn(t, ws.Dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "get")
	ref, _ := tickets.ParseRef(tk.Ref)
	return &reviewFixture{wsFixture: f, ref: ref,
		reviews: runner.NewReviews(f.ws, journal.New(nil, f.store, nil)),
		run:     runner.Run{ID: "r_r", Ticket: tk.Ref, Task: "s_x_r1_review", Role: "reviewer", Profile: "reviewer-1", Dir: ws.Dir}}
}

func (f *reviewFixture) submit(t *testing.T, report string) runner.ReviewCheck {
	t.Helper()
	res, err := runner.ParseReviewResult([]byte(report))
	if err != nil {
		t.Fatal(err)
	}
	checked, err := f.reviews.Check(ctx, f.run, res, reviewTicket)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.reviews.Record(ctx, f.run, checked); err != nil {
		t.Fatal(err)
	}
	return checked
}

func (f *reviewFixture) review(t *testing.T) journal.Entry {
	t.Helper()
	entries, _ := journal.New(nil, f.store, nil).Stored(ctx, f.ref)
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Kind == journal.Review {
			return entries[i]
		}
	}
	t.Fatalf("no review in the journal: %+v", entries)
	return journal.Entry{}
}

func TestAFindingWhoseQuoteIsNotInTheTicketIsDroppedAndNoted(t *testing.T) {
	f := newReviews(t)
	checked := f.submit(t, `
standards: 0 findings
spec: 1 finding, a made-up requirement
findings:
  - axis: spec
    kind: missing
    file: cache/cache.go
    line: 3
    source: ticket
    quote: The cache expires after a minute.
    text: there is no expiry
`)
	if len(checked.Accepted) != 0 || len(checked.Dropped) != 1 {
		t.Fatalf("checked = %+v, want the finding dropped", checked)
	}
	e := f.review(t)
	if text := journal.Text(e); !strings.Contains(text, "The cache expires after a minute.") || !strings.Contains(text, "not in the ticket") {
		t.Fatalf("review entry = %q, want the dropped finding noted with why", text)
	}
}

func TestAFindingThatPointsOutsideTheDiffIsDropped(t *testing.T) {
	f := newReviews(t)
	finding := func(file string, line int) string {
		return "  - {axis: spec, kind: partial, file: " + file + ", line: " + strconv.Itoa(line) +
			", source: ticket, quote: A second read of the same key does not reach the store., text: every read reaches the store}\n"
	}
	checked := f.submit(t, "standards: 0 findings\nspec: 3 findings\nfindings:\n"+
		finding("cache/cache.go", 4)+finding("README.md", 1)+finding("cache/cache.go", 40))
	if len(checked.Accepted) != 1 || checked.Accepted[0].Line != 4 {
		t.Fatalf("accepted = %+v, want only the finding at cache/cache.go:4", checked.Accepted)
	}
	for _, d := range checked.Dropped {
		if !strings.Contains(d.Reason, "not in the diff") {
			t.Errorf("dropped %s:%d for %q, want: not in the diff", d.Finding.File, d.Finding.Line, d.Reason)
		}
	}
	if len(checked.Dropped) != 2 {
		t.Fatalf("dropped = %+v, want README.md:1 and cache/cache.go:40", checked.Dropped)
	}
}

func TestAStandardsQuoteIsCheckedInItsFileOnMainAcrossLineBreaks(t *testing.T) {
	f := newReviews(t)
	finding := func(source, quote string) string {
		return "  - {axis: standards, kind: standard, file: cache/cache.go, line: 3, source: " + source + ", quote: " + quote + ", text: Get has no doc comment}\n"
	}
	checked := f.submit(t, "standards: 3 findings\nspec: 0 findings\nfindings:\n"+
		finding("CODING_STANDARDS.md", "Every exported function has a doc comment.")+
		finding("CODING_STANDARDS.md", "Every function is short.")+
		finding("STYLE.md", "Every exported function has a doc comment."))
	if len(checked.Accepted) != 1 || checked.Accepted[0].Quote != "Every exported function has a doc comment." || checked.Accepted[0].Source != "CODING_STANDARDS.md" {
		t.Fatalf("accepted = %+v, want the quote found in CODING_STANDARDS.md across its line break", checked.Accepted)
	}
	if len(checked.Dropped) != 2 || !strings.Contains(checked.Dropped[0].Reason, "not in CODING_STANDARDS.md") || !strings.Contains(checked.Dropped[1].Reason, "STYLE.md") {
		t.Fatalf("dropped = %+v, want the missing quote and the missing file named", checked.Dropped)
	}
}

func TestAReportOutsideItsSchemaIsRejected(t *testing.T) {
	const good = "{axis: spec, kind: missing, file: a.go, line: 1, source: ticket, quote: q, text: t}"
	for name, report := range map[string]string{
		"no spec line":       "standards: 0 findings\n",
		"no standards line":  "spec: 0 findings\n",
		"unknown axis":       "standards: s\nspec: s\nfindings: [{axis: style, kind: missing, file: a.go, line: 1, source: ticket, quote: q, text: t}]\n",
		"kind of other axis": "standards: s\nspec: s\nfindings: [{axis: spec, kind: smell, file: a.go, line: 1, source: ticket, quote: q, text: t}]\n",
		"no quote":           "standards: s\nspec: s\nfindings: [{axis: spec, kind: missing, file: a.go, line: 1, source: ticket, text: t}]\n",
		"no line":            "standards: s\nspec: s\nfindings: [{axis: spec, kind: missing, file: a.go, source: ticket, quote: q, text: t}]\n",
		"unknown field":      "standards: s\nspec: s\nverdict: approve\nfindings: [" + good + "]\n",
	} {
		if _, err := runner.ParseReviewResult([]byte(report)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := runner.ParseReviewResult([]byte("standards: s\nspec: s\nfindings: [" + good + "]\n")); err != nil {
		t.Fatalf("a good report: %v", err)
	}
}

// refusesUnquotedColon checks that a parse error names the report's line and
// says to quote the value (S4.9).
func refusesUnquotedColon(t *testing.T, err error, line int) {
	t.Helper()
	if err == nil {
		t.Fatal("accepted")
	}
	for _, want := range []string{"line " + strconv.Itoa(line), "quote the value"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not say %q", err, want)
		}
	}
}

func TestAReportWithAnUnquotedColonIsRefusedWithItsLineAndTheFix(t *testing.T) {
	report := "standards: 1 finding; most serious: duplicated code\nspec: 0 findings\n"
	_, err := runner.ParseReviewResult([]byte(report))
	refusesUnquotedColon(t, err, 1)
}

func TestAnUnquotedColonOnALaterLineIsNamedAfterACommentWithAColon(t *testing.T) {
	report := "standards: \"0 findings\"\n" +
		"spec: \"1 finding\"\n" +
		"findings:\n" +
		"  - axis: spec  # standards; spec: missing\n" +
		"    kind: missing\n" +
		"    text: dup: here\n"
	_, err := runner.ParseReviewResult([]byte(report))
	refusesUnquotedColon(t, err, 6)
}

func TestACodeSmellQuotesTheBaselineOfTheReviewersInstructions(t *testing.T) {
	f := newReviews(t) // main has no .istari/roles/reviewer.md: the built-in one applies
	checked := f.submit(t, `
standards: 1 finding, possibly a mysterious name
spec: 0 findings
findings:
  - axis: standards
    kind: smell
    file: cache/cache.go
    line: 3
    source: instructions
    quote: "mysterious name: a name that does not reveal what it does or holds"
    text: possibly a mysterious name, Get says nothing of the cache
`)
	if len(checked.Accepted) != 1 {
		t.Fatalf("checked = %+v, want the smell accepted with its quote from the baseline", checked)
	}
}

func TestCodeFindsTheDiffBaseTheFilesOutsideTheZoneAndTheStandardsForTheReviewer(t *testing.T) {
	f := newReviews(t)
	write(t, filepath.Join(f.run.Dir, "go.mod"), "module x\n")
	gitIn(t, f.run.Dir, "add", ".")
	gitIn(t, f.run.Dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "mod")
	in, err := f.ws.ReviewInput(ctx, f.run.Ticket, f.run.Dir, []string{"cache/cache.go", "cache/cache_test.go (new)"})
	if err != nil {
		t.Fatal(err)
	}
	if in.Base == "" || strings.Join(in.Files, " ") != "cache/cache.go go.mod" || strings.Join(in.OutsideZone, " ") != "go.mod" {
		t.Fatalf("review input = %+v, want both changed files and go.mod outside the zone", in)
	}
	if strings.Join(in.Standards, " ") != "CODING_STANDARDS.md" {
		t.Fatalf("standards = %v, want the one standards file on main", in.Standards)
	}
}

func TestAReviewerWhoseFirstCompoundCommandIsDeniedStillSubmitsItsReview(t *testing.T) {
	// The scripted session opens as the runs of #159 did: one chain with a
	// command the reviewer may not run, which Claude Code denies whole.
	denied, err := json.Marshal(map[string]string{"command": "cat .istari/skill.md | head -80; istari whoami; git fetch -q origin main; git log --oneline -3"})
	if err != nil {
		t.Fatal(err)
	}
	f := newRunner(t, runner.RunnerOptions{}, harness.Event{Kind: harness.EventToolCall, Tool: "Bash", ToolInput: denied})
	profile := claudecode.DefaultProfiles()["reviewer"]
	profile.Harness = f.h.name
	ws := runner.NewWorkspaces(f.store, runner.WorkspaceOptions{DataDir: f.data, Server: "file://" + filepath.Join(f.root, "remotes")})
	f.runner = runner.New(f.store, f.tasks, f.tickets, ws,
		runner.RunnerOptions{DataDir: f.data, Projects: []string{"web"}, Slots: []runner.Slot{{Profile: "reviewer-1", Role: "reviewer", Harness: profile}}})
	tk := f.ticket(t, 40, "Review me")
	f.fire(t, tk.Ref, "accept", "start")
	task, err := f.tasks.Create(ctx, "system", core.NewTask{Ticket: tk.Ref, Project: "web", Role: "reviewer"})
	if err != nil {
		t.Fatal(err)
	}
	f.start(t)
	s := f.session(t)
	for _, want := range []string{"Run each `istari` command on its own", "A denied command denies only that command", "- `git fetch origin main`\n"} {
		if !strings.Contains(s.spec.Brief, want) {
			t.Errorf("the reviewer's brief lacks %q", want)
		}
	}
	// After the denial the agent runs each istari command on its own.
	run, err := f.runner.Runs().Get(ctx, s.spec.Env["ISTARI_RUN"])
	if err != nil {
		t.Fatal(err)
	}
	res, err := runner.ParseReviewResult([]byte("standards: \"0 findings\"\nspec: \"0 findings\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	reviews := runner.NewReviews(ws, journal.New(nil, f.store, nil))
	checked, err := reviews.Check(ctx, run, res, "")
	if err != nil {
		t.Fatalf("istari review submit: %v", err)
	}
	if err := reviews.Record(ctx, run, checked); err != nil {
		t.Fatalf("istari review submit: %v", err)
	}
	if _, err := f.runner.Runs().Complete(ctx, run.ID, "reviewed"); err != nil {
		t.Fatalf("istari complete: %v", err)
	}
	s.finish <- harness.Result{ExitCode: 0}
	f.waitTask(t, task.ID, core.TaskCompleted)
	ref, err := tickets.ParseRef(tk.Ref)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := journal.New(nil, f.store, nil).Stored(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(entries, func(e journal.Entry) bool { return e.Kind == journal.Review }) {
		t.Fatalf("journal = %+v, want the review recorded", entries)
	}
}

func TestAQuoteMustComeFromTheSourceItsAxisAndKindAllow(t *testing.T) {
	f := newReviews(t)
	finding := func(axis, kind, source, quote string) string {
		return "  - {axis: " + axis + ", kind: " + kind + ", file: cache/cache.go, line: 3, source: " + source + ", quote: \"" + quote + "\", text: t}\n"
	}
	checked := f.submit(t, "standards: s\nspec: s\nfindings:\n"+
		finding("spec", "missing", "CODING_STANDARDS.md", "Every exported function has a doc comment.")+ // Spec quotes the ticket
		finding("standards", "standard", "ticket", "Add a cache in front of the store.")+ // a standard quotes a standard
		finding("standards", "standard", "cache/cache.go", "package cache")+ // code is no standard
		finding("standards", "smell", "CODING_STANDARDS.md", "Every exported function has a doc comment.")+ // a smell quotes the baseline
		finding("standards", "smell", "instructions", "You did not write this code.")) // the baseline, not any line
	if len(checked.Accepted) != 0 || len(checked.Dropped) != 5 {
		t.Fatalf("checked = %+v, want all five dropped", checked)
	}
	for i, want := range []string{"a spec finding quotes the ticket", "a standard finding quotes a standards file", "cache/cache.go is not a standards file", "a smell finding quotes the baseline", "not a code smell of the baseline"} {
		if !strings.Contains(checked.Dropped[i].Reason, want) {
			t.Errorf("finding %d dropped for %q, want %q", i+1, checked.Dropped[i].Reason, want)
		}
	}
}
