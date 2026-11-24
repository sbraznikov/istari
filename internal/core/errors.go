package core

import "fmt"

// Error codes returned to callers. They are stable: the CLI and agents match on them.
const (
	CodeNotFound     = "not_found"
	CodeInvalidTask  = "invalid_task"
	CodeInvalidNeeds = "invalid_needs"
	CodeInvalidState = "invalid_state"
	CodeTaskClaimed  = "task_claimed"
	CodeClaimLost    = "claim_lost"
	CodeNoReadyTask  = "no_ready_task"

	CodeInvalidTicket     = "invalid_ticket"
	CodeInvalidTransition = "invalid_transition"
	CodeGuardFailed       = "guard_failed"
	CodeTicketHeld        = "ticket_held"
)

// Error is a refusal by the core: the request was understood and is not allowed.
type Error struct {
	Code    string
	Message string
	Err     error // the cause, if any, such as a guard's refusal
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Unwrap returns the cause.
func (e *Error) Unwrap() error { return e.Err }

func errorf(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}
