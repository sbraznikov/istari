package tickets

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Candidate is a ticket that may duplicate or contradict another: code picks
// them, the refiner reads only these (spec section 4.5, step 2).
type Candidate struct {
	Ref    Ref      `json:"ref"`
	Title  string   `json:"title"`
	Closed bool     `json:"closed"`
	Shared []string `json:"shared"` // what it has in common: paths, identifiers, words
}

// Lister lists a repository's tickets.
type Lister interface {
	List(ctx context.Context, q Query) ([]Ticket, error)
}

// candidateWindow is how long a completed ticket stays a candidate.
const candidateWindow = 30 * 24 * time.Hour

const maxCandidates = 5

var (
	backticked = regexp.MustCompile("`([^`\\s]+)`")
	word       = regexp.MustCompile(`[A-Za-z][A-Za-z0-9]{3,}`)
	stopwords  = map[string]bool{"with": true, "from": true, "that": true, "this": true, "into": true, "when": true, "make": true,
		"have": true, "will": true, "should": true, "their": true, "them": true, "they": true, "about": true, "after": true}
)

// signature is what a ticket is about: paths, identifiers and title words.
type signature struct {
	paths, idents, words []string
}

func signatureOf(t Ticket) signature {
	var s signature
	for _, m := range backticked.FindAllStringSubmatch(t.Title+"\n"+t.Body, -1) {
		tok := strings.Trim(m[1], ".,;:")
		if strings.Contains(tok, "/") || regexp.MustCompile(`\.[a-z]{1,5}$`).MatchString(tok) && !strings.Contains(tok, "(") {
			s.paths = append(s.paths, tok)
		} else {
			s.idents = append(s.idents, tok)
		}
	}
	for _, w := range word.FindAllString(t.Title, -1) {
		w = strings.ToLower(w)
		if !stopwords[w] {
			s.words = append(s.words, strings.TrimSuffix(w, "s"))
		}
	}
	for _, list := range []*[]string{&s.paths, &s.idents, &s.words} {
		slices.Sort(*list)
		*list = slices.Compact(*list)
	}
	return s
}

// Candidates returns the open and recently completed tickets of the
// ticket's repository that share paths (weight 3), identifiers (2) or title
// words (1) with it, the most alike first: at most five, each with a score
// of at least two (one identifier, or two words).
func Candidates(ctx context.Context, p Lister, t Ticket) ([]Candidate, error) {
	all, err := p.List(ctx, Query{Repo: t.Ref.Repo, Closed: true})
	if err != nil {
		return nil, err
	}
	mine := signatureOf(t)
	type scored struct {
		c     Candidate
		score int
	}
	var out []scored
	for _, o := range all {
		if o.Ref == t.Ref || o.Closed && (o.ClosedAs != "completed" || time.Since(o.Updated) > candidateWindow) {
			continue
		}
		theirs := signatureOf(o)
		c := Candidate{Ref: o.Ref, Title: o.Title, Closed: o.Closed}
		score := 0
		for _, part := range []struct {
			mine, theirs []string
			weight       int
		}{{mine.paths, theirs.paths, 3}, {mine.idents, theirs.idents, 2}, {mine.words, theirs.words, 1}} {
			for _, x := range part.mine {
				if slices.Contains(part.theirs, x) {
					score += part.weight
					c.Shared = append(c.Shared, x)
				}
			}
		}
		if score >= 2 {
			out = append(out, scored{c, score})
		}
	}
	slices.SortStableFunc(out, func(a, b scored) int {
		if a.score != b.score {
			return b.score - a.score
		}
		return a.c.Ref.Number - b.c.Ref.Number
	})
	var result []Candidate
	for i, s := range out {
		if i == maxCandidates {
			break
		}
		result = append(result, s.c)
	}
	return result, nil
}
