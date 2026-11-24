package runner

import (
	"strconv"
	"strings"

	"github.com/sbraznikov/istari/internal/tickets"
)

// BranchPrefix starts every ticket branch (spec section 2.1).
const BranchPrefix = "istari/"

// maxSlug bounds the title part of a branch name.
const maxSlug = 45

// Branch returns the ticket's branch, istari/<ticket-key>/<slug> (spec
// section 2.1): the key is the issue number for GitHub and the issue key
// otherwise; the slug is the title in lower-case ASCII words.
func Branch(ref tickets.Ref, title string) string {
	key := ref.Key
	if ref.Provider == "gh" {
		key = strconv.Itoa(ref.Number)
	}
	return BranchPrefix + key + "/" + slug(title)
}

func slug(title string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(title) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else if s := b.String(); s != "" && !strings.HasSuffix(s, "-") {
			b.WriteByte('-')
		}
	}
	s := strings.TrimSuffix(b.String(), "-")
	if len(s) > maxSlug {
		s = s[:maxSlug]
		if i := strings.LastIndexByte(s, '-'); i > 0 {
			s = s[:i]
		}
	}
	if s == "" {
		return "ticket"
	}
	return s
}
