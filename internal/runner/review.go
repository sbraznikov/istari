package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/sbraznikov/istari/internal/pipeline"
	"github.com/sbraznikov/istari/internal/tickets"
	"github.com/sbraznikov/istari/internal/tickets/journal"
	"github.com/sbraznikov/istari/internal/vcs"
	"github.com/sbraznikov/istari/skill"
)

// Finding is one finding of the reviewer's report (spec section 7.6): an
// axis, a place in the diff, a kind, and a quote from the ticket, the
// document or the standard it rests on.
type Finding struct {
	Axis   string `yaml:"axis" json:"axis"` // standards | spec
	Kind   string `yaml:"kind" json:"kind"`
	File   string `yaml:"file" json:"file"`
	Line   int    `yaml:"line" json:"line"`
	Source string `yaml:"source" json:"source"` // ticket, instructions, or the repository path of the document or standard
	Quote  string `yaml:"quote" json:"quote"`
	Text   string `yaml:"text" json:"text"`
}

// ReviewResult is the reviewer's structured report: one summary line per
// axis and the findings.
type ReviewResult struct {
	Standards string    `yaml:"standards" json:"standards"`
	Spec      string    `yaml:"spec" json:"spec"`
	Findings  []Finding `yaml:"findings" json:"findings"`
}

// ParseReviewResult reads and validates a report against its schema.
func ParseReviewResult(data []byte) (ReviewResult, error) {
	var r ReviewResult
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&r); err != nil {
		return ReviewResult{}, yamlError("review", data, err)
	}
	bad := func(format string, args ...any) error { return fmt.Errorf("review: "+format, args...) }
	if strings.TrimSpace(r.Standards) == "" || strings.TrimSpace(r.Spec) == "" {
		return ReviewResult{}, bad("one line per axis is needed: standards and spec, the number of findings and the most serious one")
	}
	for i, f := range r.Findings {
		kinds, ok := FindingKinds[f.Axis]
		switch {
		case !ok:
			return ReviewResult{}, bad("finding %d: axis %q is not standards or spec", i+1, f.Axis)
		case !slices.Contains(kinds, f.Kind):
			return ReviewResult{}, bad("finding %d: kind %q is not one of the %s axis: %s", i+1, f.Kind, f.Axis, strings.Join(kinds, ", "))
		case f.File == "" || f.Line < 1:
			return ReviewResult{}, bad("finding %d: a file and a line of the diff are needed", i+1)
		case f.Source == "" || strings.TrimSpace(f.Quote) == "":
			return ReviewResult{}, bad("finding %d: a source (ticket, instructions or the standard's path) and a quote from it are needed", i+1)
		case strings.TrimSpace(f.Text) == "":
			return ReviewResult{}, bad("finding %d: the text of the finding is needed", i+1)
		}
	}
	return r, nil
}

// FindingKinds are the kinds of findings of each axis (spec section 7.6,
// verdict table): on Spec a requirement not met, met partially or wrongly,
// or behaviour the ticket did not ask for; on Standards a documented
// standard violated, or a code smell of the baseline.
var FindingKinds = map[string][]string{
	AxisSpec:      {"missing", "partial", "wrong", "extra"},
	AxisStandards: {KindStandard, KindSmell},
}

// DroppedFinding is a finding code did not accept, and why.
type DroppedFinding struct {
	Finding Finding
	Reason  string
}

// ReviewCheck is a report as code checked it.
type ReviewCheck struct {
	Result   ReviewResult
	Accepted []Finding
	Dropped  []DroppedFinding
}

// Reviews checks and records the reviewer's reports; code, not the reviewer,
// decides which findings stand (spec section 7.7).
type Reviews struct {
	ws      *Workspaces
	journal *journal.Journal
}

// NewReviews returns the report checker.
func NewReviews(ws *Workspaces, j *journal.Journal) *Reviews {
	return &Reviews{ws: ws, journal: j}
}

// Check verifies each finding of a report against the run's diff and the
// sources it quotes; ticket is the ticket's text.
func (r *Reviews) Check(ctx context.Context, run Run, res ReviewResult, ticket string) (ReviewCheck, error) {
	ref, err := tickets.ParseRef(run.Ticket)
	if err != nil {
		return ReviewCheck{}, err
	}
	diff, err := r.ws.reviewDiff(ctx, run.Ticket, run.Dir)
	if err != nil {
		return ReviewCheck{}, err
	}
	src := sources{ticket: ticket}
	if src.standards, err = r.ws.mainStandards(ctx, ref.Repo); err != nil {
		return ReviewCheck{}, err
	}
	mainFile := func(path string) ([]byte, error) { return r.ws.MainFile(ctx, ref.Repo, path) }
	if src.instructions, err = skill.Instruction("reviewer", mainFile); err != nil {
		return ReviewCheck{}, err
	}
	out := ReviewCheck{Result: res}
	for _, f := range res.Findings {
		reason := ""
		if !diff.has(f.File, f.Line) {
			reason = fmt.Sprintf("%s:%d is not in the diff", f.File, f.Line)
		} else if reason, err = src.check(f, mainFile); err != nil {
			return ReviewCheck{}, err
		}
		if reason != "" {
			out.Dropped = append(out.Dropped, DroppedFinding{f, reason})
			continue
		}
		out.Accepted = append(out.Accepted, f)
	}
	return out, nil
}

// sources are what a quote may come from (spec sections 7.6 and 7.7): the
// ticket or any file of the main branch for the Spec axis; a standards file of the main branch for a
// documented standard; the baseline of code smells in the reviewer's
// instructions for a smell.
type sources struct {
	ticket, instructions string
	standards            []string
}

// check tells why a finding's quote is not verbatim in a source its axis and
// kind allow, or "". Runs of white space count as one space, as Markdown
// wraps lines.
func (s sources) check(f Finding, mainFile skill.ReadFile) (string, error) {
	var text, name string
	switch {
	case f.Axis == AxisSpec && f.Source == SourceTicket:
		text, name = s.ticket, "the ticket"
	case f.Axis == AxisSpec:
		if f.Source == SourceInstructions {
			return "a spec finding quotes the ticket or a repository file, not " + f.Source, nil
		}
		data, err := mainFile(f.Source)
		if errors.Is(err, fs.ErrNotExist) {
			return f.Source + " is not a file on the main branch", nil
		}
		if err != nil {
			return "", err
		}
		text, name = string(data), f.Source
	case f.Kind == KindSmell:
		if f.Source != SourceInstructions {
			return "a smell finding quotes the baseline of code smells (source " + SourceInstructions + "), not " + f.Source, nil
		}
		if !isBaselineSmell(f.Quote) {
			return "the quote is not a code smell of the baseline: it starts with the smell's name", nil
		}
		text, name = s.instructions, "the reviewer's instructions"
	default: // a documented standard
		if f.Source == SourceTicket || f.Source == SourceInstructions {
			return "a standard finding quotes a standards file, not " + f.Source, nil
		}
		if !slices.Contains(s.standards, f.Source) {
			return f.Source + " is not a standards file on the main branch", nil
		}
		data, err := mainFile(f.Source)
		if err != nil {
			return "", err
		}
		text, name = string(data), f.Source
	}
	if !strings.Contains(squeeze(text), squeeze(f.Quote)) {
		return "the quote is not in " + name, nil
	}
	return "", nil
}

// baselineSmells are the code smells every review checks (spec section 7.6).
var baselineSmells = []string{"mysterious name", "duplicated code", "feature envy", "data clumps", "primitive obsession", "repeated switches",
	"shotgun surgery", "divergent change", "speculative generality", "message chains", "middle man", "refused bequest"}

// isBaselineSmell tells whether a quote is one of the baseline's smells: it
// starts with the smell's name.
func isBaselineSmell(quote string) bool {
	q := strings.ToLower(squeeze(quote))
	return slices.ContainsFunc(baselineSmells, func(name string) bool { return strings.HasPrefix(q, name) })
}

// The axes, the kinds the verdict table names and the sources of quotes
// that are not a file's path (spec section 7.6).
const (
	AxisStandards = "standards"
	AxisSpec      = "spec"
	KindStandard  = "standard"
	KindSmell     = "smell"
	// SourceTicket is the ticket's text.
	SourceTicket = "ticket"
	// SourceInstructions is the reviewer's instructions, where the baseline
	// of code smells is.
	SourceInstructions = "instructions"
	// The verdicts a review entry records.
	VerdictPassed           = "passed"
	VerdictChangesRequested = "changes_requested"
)

// squeeze turns every run of white space into one space.
func squeeze(s string) string { return strings.Join(strings.Fields(s), " ") }

// Record writes a checked report into the ticket's journal, with the
// findings code dropped and why.
func (r *Reviews) Record(ctx context.Context, run Run, c ReviewCheck) error {
	ref, err := tickets.ParseRef(run.Ticket)
	if err != nil {
		return err
	}
	verdict := VerdictPassed
	if len(c.Accepted) > 0 {
		verdict = VerdictChangesRequested
	}
	e := journal.ReviewEntry{Verdict: verdict, Task: run.Task}
	for _, f := range c.Accepted {
		e.Findings = append(e.Findings, f.String())
	}
	for _, d := range c.Dropped {
		e.Dropped = append(e.Dropped, d.Finding.String()+" — dropped: "+d.Reason)
	}
	return r.journal.Append(ctx, ref, journal.Entry{Kind: journal.Review, Actor: run.Agent(), Review: &e})
}

// String renders a finding for the journal and the pull request.
func (f Finding) String() string {
	return fmt.Sprintf("`%s:%d` %s %s: %s (“%s”, %s)", f.File, f.Line, f.Axis, f.Kind, f.Text, f.Quote, f.Source)
}

// Published is the review istari publishes on the pull request: the
// accepted findings under their axis, the two apart (spec section 7.6).
func (c ReviewCheck) Published() vcs.Review {
	review := vcs.Review{Standards: vcs.Axis{Summary: c.Result.Standards}, Spec: vcs.Axis{Summary: c.Result.Spec}}
	for _, f := range c.Accepted {
		if f.Axis == AxisStandards {
			review.Standards.Findings = append(review.Standards.Findings, f.String())
		} else {
			review.Spec.Findings = append(review.Spec.Findings, f.String())
		}
	}
	return review
}

// reviewDiff is the diff of a ticket's branch against its merge base with
// the main branch: for each file, the line ranges of the new version that
// the diff shows, context included, as GitHub shows it.
type reviewDiff struct {
	base  string
	files map[string][][2]int
	order []string // the changed files, in the diff's order
}

func (d reviewDiff) has(file string, line int) bool {
	for _, h := range d.files[file] {
		if line >= h[0] && line <= h[1] {
			return true
		}
	}
	return false
}

// reviewDiff computes the diff of a ticket's worktree dir against the merge
// base of its HEAD and the main branch as last fetched.
func (w *Workspaces) reviewDiff(ctx context.Context, ticket, dir string) (reviewDiff, error) {
	ref, err := tickets.ParseRef(ticket)
	if err != nil {
		return reviewDiff{}, err
	}
	main, err := mainBranch(ctx, w.clonePath(ref.Repo))
	if err != nil {
		return reviewDiff{}, err
	}
	base, err := gitOut(ctx, dir, "merge-base", "HEAD", "refs/remotes/origin/"+main)
	if err != nil {
		return reviewDiff{}, err
	}
	out, err := gitOut(ctx, dir, "diff", "--no-color", "--no-ext-diff", base, "HEAD")
	if err != nil {
		return reviewDiff{}, err
	}
	d := reviewDiff{base: base, files: map[string][][2]int{}}
	var file string
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "+++ "):
			file = strings.TrimPrefix(strings.TrimPrefix(line, "+++ "), "b/")
			if file == "/dev/null" {
				file = ""
			} else {
				d.order = append(d.order, file)
			}
		case strings.HasPrefix(line, "@@ ") && file != "":
			// @@ -a,b +c,d @@: the new version's lines c … c+d-1.
			fields := strings.Fields(line)
			if len(fields) < 3 {
				continue
			}
			start, count := hunkRange(strings.TrimPrefix(fields[2], "+"))
			if count > 0 {
				d.files[file] = append(d.files[file], [2]int{start, start + count - 1})
			}
		}
	}
	return d, nil
}

// ReviewInput is what code prepares for the reviewer (spec section 7.7):
// the base of the diff, the changed files and those of them outside the
// plan's change zone, the standards sources and the checks on the main
// branch: what the tooling already enforces (spec section 7.6).
type ReviewInput struct {
	Base        string
	Files       []string
	OutsideZone []string
	Standards   []string
	Checks      []pipeline.Check
}

// standardsFiles are the files about how to write code that the Standards
// axis reads (spec section 7.6), besides the ADRs under adr/ or docs/adr/.
var standardsFiles = []string{"CODING_STANDARDS.md", "CONTRIBUTING.md", "GLOSSARY.md", "AGENTS.md"}

// ReviewInput prepares the review of a ticket's worktree dir; zone is the
// plan's change zone ("path (new)" for a file the change creates).
func (w *Workspaces) ReviewInput(ctx context.Context, ticket, dir string, zone []string) (ReviewInput, error) {
	d, err := w.reviewDiff(ctx, ticket, dir)
	if err != nil {
		return ReviewInput{}, err
	}
	in := ReviewInput{Base: d.base, Files: d.order}
	inZone := map[string]bool{}
	for _, z := range zone {
		path, _ := strings.CutSuffix(strings.TrimSpace(z), "(new)")
		inZone[strings.TrimSpace(path)] = true
	}
	for _, f := range d.order {
		if !inZone[f] {
			in.OutsideZone = append(in.OutsideZone, f)
		}
	}
	ref, err := tickets.ParseRef(ticket)
	if err != nil {
		return ReviewInput{}, err
	}
	if in.Standards, err = w.mainStandards(ctx, ref.Repo); err != nil {
		return ReviewInput{}, err
	}
	checks, err := w.checks(ctx, ref.Repo)
	if err != nil {
		return ReviewInput{}, err
	}
	in.Checks = checks.List
	return in, nil
}

// mainStandards lists the standards sources on the main branch.
func (w *Workspaces) mainStandards(ctx context.Context, repo string) ([]string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	clone := w.clonePath(repo)
	main, err := mainBranch(ctx, clone)
	if err != nil {
		return nil, err
	}
	out, err := gitOut(ctx, clone, "ls-tree", "-r", "--name-only", "origin/"+main)
	if err != nil {
		return nil, err
	}
	var found []string
	for _, path := range strings.Split(out, "\n") {
		if slices.Contains(standardsFiles, path) || strings.HasPrefix(path, "adr/") || strings.HasPrefix(path, "docs/adr/") {
			found = append(found, path)
		}
	}
	return found, nil
}

// hunkRange reads "c,d" or "c" (d = 1) of a hunk header.
func hunkRange(s string) (start, count int) {
	a, b, found := strings.Cut(s, ",")
	start, _ = strconv.Atoi(a)
	count = 1
	if found {
		count, _ = strconv.Atoi(b)
	}
	return start, count
}
