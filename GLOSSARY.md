# Glossary

The words of istari, as `docs/design.md` and the code use them. Code, tests, messages and tickets use these words and no synonyms. The section in brackets is where the spec defines the term.

## Work and its records

- **Project**: one or more repositories of one service, with a tracker (and later a Slack channel). [2]
- **Ticket**: a unit of work, an issue in the tracker, which is the truth; istari keeps a mirror. Belongs to exactly one repository. [2]
- **Ref**: a ticket's identifier, `gh:owner/repo#123`. [2]
- **Task**: one pipeline stage of one ticket, with a role, a state, a lease and needs. [2, 4.2]
- **Run**: one attempt of one agent at one task: a harness process in the ticket's worktree, with its transcript. [2, 7.2]
- **Event**: an immutable record in the log, written with `store.Append`. Everything else is a projection of the log. [2, 3.1]
- **Projection**: the current state of a ticket, task, run or workspace, kept in the store and changed only together with an event. [3.1]
- **Actor**: who acts: `human:<login>`, `agent:<profile>` or `system`. [2]
- **Gate**: a point where the pipeline waits for a human decision, such as approving a plan. [2, 4.3]
- **Journal**: the ticket's record of decisions, assumptions, questions, answers, plans, checkpoints, reviews, transitions, notes, CI failures and trusted members' comments, kept as comments in the tracker. Work resumes from it. [4.6]
- **Work state**: the summary block at the top of the ticket: stage, branch, pull request, last checkpoint, open questions, next step. [4.6]

## States

- **Ticket state**: one of `new`, `ready`, `planning`, `in_progress`, `in_review`, `awaiting_approval`, `done`, `canceled`. Stored as the label `istari:<state>`. [4.1]
- **Transition**: a change of a ticket's state by an event of the transition table, named T1 to T16 (T7 `changes_requested`, T8 `review_passed`, …). There are no others. [4.1]
- **Hold**: a flag over a ticket's state that stops its tasks from being handed out: `needs_input`, `attempts_exhausted`, `budget_exceeded`, `paused`, `waiting_dependency`, `waiting_zone`. Stored as the label `istari:hold:<reason>`. [4.1]
- **Task state**: one of `waiting`, `ready`, `claimed`, `blocked`, `completed`, `failed`, `canceled`. [4.2]
- **Lease**: a claimed task's time-limited ownership, renewed by its runner; when it runs out the task returns to `ready`. [4.2]
- **Attempt**: a failed try at a task; after `max_attempts` the ticket is held `attempts_exhausted`. [4.2]

## Admission

- **Ticket standard**: the structure (`.istari/ticket-standard.toml`, checked by code) and the quality (`.istari/ticket-standard.md`, judged by the refiner) a ticket must have before work starts. [4.5]
- **Deviation**: one way a ticket falls short of the standard. [4.5]
- **Admission**: a ticket that meets the standard moves to `ready`: on its own when a trusted member wrote it (unless the project's `admission` is `"label"`), when an agent created it for an admitted ticket, or when a trusted member admits it. [4.5, 12]
- **Trusted**: the members whose tickets are admitted without a human; the project's `trusted`, or everyone with write access. [4.5, 12]
- **Needs**: the tickets a ticket depends on (`- Needs: #12`); it starts only when they are done. [4.1, 4.5]
- **Size**: `XS`, `S`, `M`, `L`, `XL`; sets the budget and, above `max_size`, requires a split. [4.7]
- **Priority**: `P0` to `P3`; the order in which ready tickets start. [4.5]

## The pipeline

- **Pipeline**: the stages of a repository, declared in `.istari/pipeline.toml`: refine, plan, implement, verify, review, approve, merge. [4.3]
- **Stage**: one step of the pipeline; it becomes a task per ticket. [4.3]
- **Round**: one pass from implement to review; a rework starts the next round. Limited by `max_review_rounds`. [4.3, 7.6]
- **Conflict round**: a round that T10 starts when GitHub reports the PR in conflict with the main branch; its run merges the main branch first, its review looks only at the conflict resolution, and it does not count against `max_review_rounds`. [4.3, 4.8]
- **Unrelated round**: a round that starts when the implementer answered a red CI `--ci-unrelated` and its rerun was red too, or GitHub refused it; its task IDs carry the marker `u` (`rNu`), and it does not count against `max_review_rounds`. One head is rerun at most twice; the third answer holds the ticket `needs_input`. [4.3]
- **Rework**: a return to implement after red CI, accepted review findings or a conflict with the main branch. [4.3]
- **Plan**: the planner's structured result: summary, slices, seams, change zone, size, and a split if needed. [4.3, 7.6]
- **Slice**: one failing test and the minimum code to pass it; every green slice is a checkpoint. [7.6]
- **Seam**: a public boundary where tests observe behaviour, agreed before the first test, with what it catches and what it misses. [7.6]
- **Change zone**: the files a plan says the change touches; overlapping zones of tickets in progress are serialized. [4.9]
- **Split**: a ticket larger than `max_size` is divided into child tickets; the parent has no tasks of its own and is done when its children are. [4.3]
- **Checkpoint**: an implementer's commit, pushed, with the checks istari ran itself, recorded in the journal. [4.6, 7.7]
- **Checks**: the commands of `[checks]` (`typecheck`, `test`, `full`) that istari runs; an agent's claim that tests pass is never taken. [7.7]
- **Verify**: the stage without an agent that follows CI on the pull request. [4.3]

## Agents

- **Role**: `refiner`, `planner`, `implementer` or `reviewer`; what an agent may do. [7.1]
- **Profile**: a runner slot of a role, such as `implementer-2`; not a personality, no memory between runs. [7.2]
- **Runner**: the part of the daemon that claims tasks, prepares the worktree, starts the harness and ends the run. [3.1, 7.2]
- **Harness**: the program that runs the model, such as Claude Code. [7.3]
- **Brief**: the first message of a run, assembled by code from durable sources only: the ticket, its journal, its branch and its pull request. [4.6, 7.2]
- **Role instruction**: `.istari/roles/<role>.md`, or the built-in one; read from the main branch. [7.2]
- **Run token**: the credential of a run, valid for its ticket and its role's operations only. [7.2, 11]
- **Worktree** (workspace): the ticket's own git worktree on the branch `istari/<n>/<slug>`, shared by all its runs. [7.2]
- **Budget**: the dollars a ticket may spend, by its size; a run is capped at what is left. [7.5]

## Review

- **Axis**: one of the two independent views of a review: **Standards** (the repository's documented standards and the baseline of code smells) and **Spec** (what the ticket asked for). [7.6]
- **Finding**: one item of a review report: axis, kind, file and line of the diff, source, verbatim quote, text. [7.6]
- **Kind**: on Spec `missing`, `partial`, `wrong`, `extra`; on Standards `standard` or `smell`. [7.6]
- **Baseline**: the twelve code smells every review checks (mysterious name, duplicated code, …), quoted from the reviewer's instructions. [7.6]
- **Accepted finding**: one whose place is in the diff and whose quote is in its source; any accepted finding returns the ticket for rework. [7.6, 7.7]
- **Verdict**: the outcome code computes from the accepted findings: rework (T7) or passed (T8). [7.6]

## Running istari

- **Daemon**: the long-running process that holds the store and runs the sync, the pipeline engine, the scheduler, the runner and the API. [3]
- **Store**: istari's SQLite database: the event log and the projections. Expendable: the tracker restores it (`istari recover`). [2, 4.6]
- **Sync**: reading the tracker's issues into tickets and writing labels and comments back. [3.1, 5]
- **Idempotency key**: a key that makes a repeated write return the first result instead of writing again. [9]
