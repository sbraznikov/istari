package tickets_test

import (
	"testing"

	"github.com/sbraznikov/istari/internal/tickets"
)

func TestRefsParseAndFormatLosslessly(t *testing.T) {
	t.Parallel()
	for _, s := range []string{
		"gh:acme/web#123",
		"gh:Acme-Corp/billing.core#1",
		"gh:a/b_c-d.e#999999",
		"lin:ENG-42",
		"jira:PROJ-7",
	} {
		ref, err := tickets.ParseRef(s)
		if err != nil {
			t.Fatalf("ParseRef(%q): %v", s, err)
		}
		if got := ref.String(); got != s {
			t.Errorf("ParseRef(%q).String() = %q", s, got)
		}
	}
	gh, _ := tickets.ParseRef("gh:acme/web#123")
	if gh.Provider != "gh" || gh.Repo != "acme/web" || gh.Number != 123 {
		t.Errorf("gh ref fields = %+v", gh)
	}
	for _, bad := range []string{"", "acme/web#1", "gh:acme/web", "gh:acme#1", "gh:acme/web#0", "gh:acme/web#x", "gh:/web#1", "svn:x", "gh:a/b/c#1", "lin:"} {
		if _, err := tickets.ParseRef(bad); err == nil {
			t.Errorf("ParseRef(%q) succeeded", bad)
		}
	}
}
