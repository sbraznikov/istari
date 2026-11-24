// Package tickets defines how istari talks to ticket trackers (spec section
// 5): the TicketProvider interface, ticket references and the data that
// crosses it. Adapters live in subpackages, e.g. tickets/github.
package tickets

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sbraznikov/istari/internal/core"
)

// Ref is a ticket reference with its provider prefix (spec section 5), e.g.
// gh:acme/web#123, lin:ENG-42 or jira:PROJ-7.
type Ref struct {
	Provider string // gh, lin or jira
	Repo     string // gh: owner/repo
	Number   int    // gh: the issue number
	Key      string // lin and jira: the issue key
}

var (
	ghRef  = regexp.MustCompile(`^([A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?/[A-Za-z0-9._-]+)#([1-9][0-9]*)$`)
	keyRef = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*-[1-9][0-9]*$`)
)

// ParseRef parses a ticket reference.
func ParseRef(s string) (Ref, error) {
	provider, rest, ok := strings.Cut(s, ":")
	if !ok {
		return Ref{}, fmt.Errorf("ticket ref %q has no provider prefix (gh:, lin:, jira:)", s)
	}
	switch provider {
	case "gh":
		m := ghRef.FindStringSubmatch(rest)
		if m == nil {
			return Ref{}, fmt.Errorf("ticket ref %q is not gh:owner/repo#number", s)
		}
		n, err := strconv.Atoi(m[2])
		if err != nil {
			return Ref{}, fmt.Errorf("ticket ref %q: %w", s, err)
		}
		return Ref{Provider: provider, Repo: m[1], Number: n}, nil
	case "lin", "jira":
		if !keyRef.MatchString(rest) {
			return Ref{}, fmt.Errorf("ticket ref %q is not %s:KEY-number", s, provider)
		}
		return Ref{Provider: provider, Key: rest}, nil
	default:
		return Ref{}, fmt.Errorf("ticket ref %q has an unknown provider %q", s, provider)
	}
}

// String formats the reference; ParseRef(r.String()) == r.
func (r Ref) String() string {
	if r.Provider == "gh" {
		return fmt.Sprintf("gh:%s#%d", r.Repo, r.Number)
	}
	return r.Provider + ":" + r.Key
}

// State is what istari records on a ticket in its tracker: the state of the
// machine (spec section 4.1) and the hold reason, if any.
type State struct {
	State core.TicketState
	Hold  string
}

// Ticket is a ticket as the tracker has it.
type Ticket struct {
	Ref    Ref
	Title  string
	Body   string
	Labels []string // every label, including istari's own
	Author string
	State  State // as read from the tracker
	// StateLabels are all state labels the ticket carries. Normally one; a
	// human who adds a label without removing the old one leaves two.
	StateLabels []core.TicketState
	Closed      bool
	URL         string
	Updated     time.Time // last change of anything on the ticket
	Comments    int       // number of comments
}

// Comment is a comment on a ticket.
type Comment struct {
	ID     int64
	Author string
	Body   string
	URL    string
	Own    bool // written by istari
}

// History tells who did what on a ticket.
type History interface {
	// LastActor returns the login of who last did kind on the ticket: labeled
	// (with label), closed or reopened. It is "" if nobody did.
	LastActor(ctx context.Context, ref Ref, kind, label string) (string, error)
	// Comments returns the ticket's comments in order.
	Comments(ctx context.Context, ref Ref) ([]Comment, error)
}

// Access tells who may change a repository's tickets.
type Access interface {
	// CanWrite tells whether login has write access to repo.
	CanWrite(ctx context.Context, repo, login string) (bool, error)
}

// Query selects tickets of one repository.
type Query struct {
	Repo   string   // gh: owner/repo
	Labels []string // all of them must be set
	Closed bool     // include closed tickets
}

// Draft is a ticket to create. It is created in state new (T1).
type Draft struct {
	Repo   string
	Title  string
	Body   string
	Labels []string
}

// CommentRef identifies a comment.
type CommentRef struct {
	ID  int64
	URL string
}

// LinkKind is what a link points to.
type LinkKind string

// Kinds of links (spec section 5).
const (
	LinkPullRequest LinkKind = "pull_request"
	LinkSlackThread LinkKind = "slack_thread"
	LinkChild       LinkKind = "child"
)

// Link connects a ticket to a pull request, a Slack thread or a child ticket.
type Link struct {
	Kind  LinkKind
	URL   string // pull request or Slack thread
	Child Ref    // child ticket
}

// Event is a change in the tracker, delivered by Subscribe.
type Event struct {
	Ref  Ref
	Kind string
}

// Caps tells what a tracker supports.
type Caps struct {
	SubIssues    bool
	CustomStates bool
	Webhooks     bool
}

// Errors of providers.
var (
	ErrNotFound       = errors.New("tickets: not found")
	ErrNotImplemented = errors.New("tickets: not implemented by this provider yet")
)

// Provider is a ticket tracker (spec section 5: TicketProvider).
type Provider interface {
	Get(ctx context.Context, ref Ref) (Ticket, error)
	List(ctx context.Context, q Query) ([]Ticket, error)
	Create(ctx context.Context, d Draft) (Ticket, error)
	SetState(ctx context.Context, ref Ref, s State) error
	Comment(ctx context.Context, ref Ref, body string) (CommentRef, error)
	Link(ctx context.Context, ref Ref, l Link) error
	Subscribe(ctx context.Context) (<-chan Event, error)
	Capabilities() Caps
}
