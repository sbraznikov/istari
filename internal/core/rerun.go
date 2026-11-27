package core

// RerunOutcome is what a rerun of a pull request's red head did (spec 4.3).
// vcs.Watcher reports it and pipeline's verify acts on it, so both read the
// one type and no translation between them can drift.
type RerunOutcome int

const (
	// RerunNone: no rerun started, because the head has no fresh unrelated
	// answer; the rework round starts and counts toward max_review_rounds.
	RerunNone RerunOutcome = iota
	// RerunStarted: the failed jobs run again; verify waits for their result
	// and starts no round.
	RerunStarted
	// RerunSpent: the head was answered unrelated again after it was rerun
	// or its rerun was refused; no second round starts on it, the ticket
	// is held needs_input for a human.
	RerunSpent
	// RerunRed: the head answered unrelated is red after its rerun, or
	// GitHub refused the rerun. It is reported once per attempt; the rework
	// round it starts is an unrelated round, which does not count toward
	// max_review_rounds.
	RerunRed
)
