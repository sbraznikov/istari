package core

// RerunOutcome is what a rerun of a pull request's red head did (spec 4.3).
// vcs.Watcher reports it and pipeline's verify acts on it, so both read the
// one type and no translation between them can drift.
type RerunOutcome int

const (
	// RerunNone: no rerun started, because the head has no fresh unrelated
	// answer or GitHub refused the rerun; the rework round starts.
	RerunNone RerunOutcome = iota
	// RerunStarted: the failed jobs run again; verify waits for their result
	// and starts no round.
	RerunStarted
	// RerunSpent: the head was answered unrelated again after it was rerun
	// or its rerun was refused; no second round starts on it, the ticket
	// is held needs_input for a human.
	RerunSpent
)
