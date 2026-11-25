package standard

import (
	"context"
	"errors"

	"github.com/sbraznikov/istari/internal/tickets"
)

// StandardPath is where a repository keeps its ticket standard.
const StandardPath = ".istari/ticket-standard.toml"

// ForTracker checks tickets for the ticket sync: the standard comes from the
// repository's default branch through the tracker, or is the default; file
// paths are not checked, since the daemon has no checkout.
type ForTracker struct {
	Tracker interface {
		tickets.Provider
		tickets.Files
	}
	DefaultPriority string
}

var _ tickets.Standards = ForTracker{}

// Check implements tickets.Standards.
func (f ForTracker) Check(ctx context.Context, repo string, t tickets.Ticket) (tickets.Conformance, error) {
	std := Default()
	data, err := f.Tracker.ReadFile(ctx, repo, StandardPath)
	switch {
	case err == nil:
		if std, err = Parse(data); err != nil {
			return tickets.Conformance{Deviations: []string{"the repository's " + StandardPath + " is invalid: " + err.Error()}}, nil
		}
	case !errors.Is(err, tickets.ErrNotFound):
		return tickets.Conformance{}, err
	}
	ref := t.Ref
	res, err := Checker{Standard: std, Tracker: f.Tracker, Repo: repo, DefaultPriority: f.DefaultPriority}.Check(ctx, Draft{Ref: &ref, Title: t.Title, Body: t.Body, Labels: t.Labels})
	if err != nil {
		return tickets.Conformance{}, err
	}
	conf := tickets.Conformance{Hash: Hash(std, t.Body), Priority: res.TablePriority}
	for _, d := range res.Deviations {
		conf.Deviations = append(conf.Deviations, d.Rule+": "+d.Message)
	}
	return conf, nil
}
