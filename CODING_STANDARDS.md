# Coding standards

The rules istari's Go code follows. A reviewer quotes a rule by its number and text; `AGENTS.md` holds the rules of the process (tickets, branches, pull requests), `GLOSSARY.md` the words to use, `docs/design.md` the behaviour.

Each rule says where it comes from: **[repo]** is a convention of this code base, the other keys are sources from `docs/research/go-coding-standards.md` (Go team documents, the Google Go style guide, the Uber guide as third-party). Where sources disagree, the rule says which side this repository takes. Gaps between a rule and today's code are listed at the end; a change must not widen them.

## 0. What tooling checks

- **S0.1** Code is gofmt-formatted, passes `go vet ./...`, passes `staticcheck ./...` and `errcheck -exclude .errcheck-excludes ./...` (each exclusion there with a reason), and the module is tidy (`go mod tidy -diff` reports nothing). A reviewer does not flag what these tools report. [GSG] [VET]
- **S0.2** There is no line length limit; break lines for meaning. [CRC] [GSG] (Uber's 99 characters is not adopted.)

## 1. Design: the model supplies data, code decides

- **S1.1** Every state change goes through `store.Append`: one event and its projection changes in one transaction. No second write path. [repo] (spec 2.1, 3.1)
- **S1.2** Ticket transitions happen only through the transition table of spec 4.1; there is no other way to change a ticket's state. [repo]
- **S1.3** What a model produces (plan, refine result, review report, decision card) is parsed against a schema and rejected when it does not fit: YAML with `KnownFields(true)`, TOML rejecting undecoded keys, JSON with `DisallowUnknownFields`. [repo] (spec 7.7)
- **S1.4** A model's claim is verified by code before it counts: tests are run by istari, quotes are looked up, paths are checked. [repo] (spec 7.7)
- **S1.5** Writes that a client may repeat take an idempotency key, recorded in the same transaction as the write. [repo]
- **S1.6** Output is deterministic: lists ordered by id or ref, map keys sorted before use, times in UTC. [repo]
- **S1.7** Time comes from an injected `Clock func() time.Time`, never from `time.Now` inside logic. [repo]

## 2. Packages

- **S2.1** Packages live under `internal/`, the command under `cmd/istari`; the layout follows spec section 13. [LAYOUT] [repo]
- **S2.2** Dependencies point inward: `store` imports nothing of istari, `core` only `store`; `api` and `cli` sit on top. A new import against this direction needs a reason in the pull request. [repo]
- **S2.3** No package named `util`, `common`, `misc`, `helper`, `types` or `models`. [CRC] [PKGN] [GSD]
- **S2.4** A package does not keep mutable global state; dependencies are passed in. `init()` only registers (as `harness/claudecode` does) and does no I/O. [GSB] [UBER] (third-party)
- **S2.5** A new direct dependency needs its reason in the pull request (`AGENTS.md`). Prefer the language, then the standard library, then code in the repository. [GSG] [PROV]
- **S2.6** A test double for a package lives in a package named `<pkg>test` (as `githubtest`). [GSB]

## 3. Naming

- **S3.1** MixedCaps, no underscores, no `ALL_CAPS`; initialisms keep one case (`appID`, `URL`). [CRC] [GSD] (Uber's `_` prefix for globals is not adopted.)
- **S3.2** Exported names do not repeat the package: `journal.New`, not `journal.NewJournal`; a package's main type has `New`, others `NewX`. [PKGN] [EG]
- **S3.3** Options are a struct passed by value, named `XOptions` (or `Options` when the package has one main type); the constructor fills defaults. [GSB] [repo] (Uber's functional options are not adopted.)
- **S3.4** A receiver is one or two letters, the same on every method of a type; never `this` or `self`. [CRC] [GSD]
- **S3.5** No `Get` prefix on getters. [EG] [GSD]
- **S3.6** Name length follows scope: short for a few lines, descriptive far from the declaration. [CRC] [GSD]
- **S3.7** Error codes are snake_case strings in `Code…` constants; event types are `"family.past_tense"` strings (`ticket.hold_set`). [repo]
- **S3.8** Sentinel errors are `ErrX`. [SC] [UBER] (third-party)
- **S3.9** Domain words are those of `GLOSSARY.md` and the spec, in code, tests and messages. [repo]

## 4. Errors

- **S4.1** `error` is the last result; failure is never signalled in-band (`-1`, `""`). [GSD] [CRC]
- **S4.2** No error is discarded with `_` without a comment saying why that is safe. [CRC] [GSD]
- **S4.3** Error strings are lowercase, without trailing punctuation, prefixed with the package or operation (`store: open %s: %w`). [CRC] [GSD] [repo]
- **S4.4** Wrap with `%w` only when callers should inspect the cause, otherwise `%v`; `%w` goes at the end. [ERR13] [GSB]
- **S4.5** Callers tell errors apart with `errors.Is`/`errors.As` on sentinels or `core.Error` codes, never by matching strings. [GSB] [ERR13] [TC]
- **S4.6** An error is handled once: returned or logged, not both. Logging (`log.Printf("istari: …")`) is for background loops that have no caller to return to. [GSB] [UBER] (third-party) [repo]
- **S4.7** Several errors are combined with `errors.Join`, not dropped. [GO120] [repo]
- **S4.8** `panic` only for a broken invariant (an unknown transition, a double registration), never for input or I/O. [EG] [GSB]
- **S4.9** A refusal the user or agent reads says what to do next ("submit the review with istari review submit before istari complete"). [repo]

## 5. Context and concurrency

- **S5.1** `ctx context.Context` is the first parameter of every function that needs one; a context is never stored in a struct or an options struct. [CTX] [CTXS] [GSB]
- **S5.2** `context.Background()` only at the entry point (the daemon's root, `main`, tests). [GSD] [repo]
- **S5.3** Every goroutine has an owner that stops it (its context) and waits for it. No fire-and-forget goroutines. [CRC] [GSD] [UBER] (third-party)
- **S5.4** Prefer synchronous functions; let callers add concurrency. [CRC] [GSD]
- **S5.5** A mutex is a named unexported field `mu`, never embedded, with a comment saying what it serializes. [EG] [UBER] (third-party) [repo]
- **S5.6** New code starts goroutines with `sync.WaitGroup.Go` (Go 1.25). [GO125] [FIX]

## 6. Documentation

- **S6.1** Every exported name has a doc comment: a full sentence that begins with the name; booleans "report whether". [CRC] [DOC]
- **S6.2** Every package has one package comment ("Package x …"), usually in `doc.go`. [DOC] [repo]
- **S6.3** A comment says why, and names the spec section a rule comes from ("spec section 4.1", "T7"). [GSG] [repo]
- **S6.4** A doc comment states a zero value's meaning when it is not obvious ("0: a minute", "nil: no refining"). [DOC] [repo]

## 7. Tests

`AGENTS.md` rules apply first: test-first in vertical slices, behaviour through public interfaces, no mocks of internal collaborators, no tests of private functions, no expected values recomputed the way the code computes them.

- **S7.1** Tests are black-box: `package x_test`. [repo] (stricter than [GSD], which allows both.)
- **S7.2** A test name is a sentence of the behaviour (`TestAFindingThatPointsOutsideTheDiffIsDropped`). [repo]
- **S7.3** Only the standard `testing` package, with plain comparisons (`reflect.DeepEqual`, `slices.Equal`, `maps.Equal`); no assertion libraries. [TC] [GSD] (go-cmp is not adopted: no dependency for tests.)
- **S7.4** A failure message names what was checked and shows got before want: `"x = %v, want %v"`. [TC] [CRC] [GSD]
- **S7.5** `t.Fatal` when later checks would be meaningless, `t.Error` otherwise; never `t.Fatal` from another goroutine. [TC] [GSB] [TESTING]
- **S7.6** Setup helpers call `t.Helper()`, fail with `t.Fatal`, and use `t.TempDir`, `t.Cleanup`, `t.Setenv`. [GSD] [TESTING]
- **S7.7** Real implementations over doubles: SQLite in a temp dir, the production client against a fake server (`githubtest`), scripts standing in for external binaries. No mock libraries. [GSB] [repo]
- **S7.8** Tests do not depend on the machine: no user config, no real network, no tools beyond `git`, `sh` and the binary under test, and no `ISTARI_*` variables of the run that started them (an agent's `ISTARI_SOCKET` and `ISTARI_TOKEN` point at the real daemon). [repo]
- **S7.9** A table test only when the cases share the same logic; cases are named, not indexed. [TC] [GSD]
- **S7.10** In-process concurrency is tested without sleeping (`testing/synctest`, or by waiting on the event). A test across processes may poll, with a deadline and a message that shows the state. [SYNC] [GO125] [repo]
- **S7.11** Output that istari does not own (JSON of a library, GitHub's) is parsed and compared, not compared byte for byte. [TC] [GSD] [GO127]
- **S7.12** Every test and subtest calls `t.Parallel()` as its first statement. A test that changes process state (`t.Setenv`, `t.Chdir`) cannot be parallel; it says which state in a one-line comment instead (`// Not parallel: sets the process environment's HOME.`). [TESTING] (`T.Parallel`, `T.Setenv`)

## 8. Modern Go

- **S8.1** Use the standard helpers instead of hand-written loops: `slices`, `maps`, `cmp`, `min`, `max`, `clear`, `strings.Cut`. [GO121] [FIX]
- **S8.2** `for i := range n` instead of a counting loop; `any` instead of `interface{}`. [GO122] [FIX]
- **S8.3** Generics only when the same code would otherwise be written for several types; an interface when only methods are called. [GEN]
- **S8.4** Run `go fix ./...` after a toolchain upgrade and keep its rewrites. [FIX]

## 9. Simplicity

- **S9.1** The normal path is at the left: handle the error and return early, no `else` after a return. [CRC] [GSD]
- **S9.2** Make the zero value useful; a nil slice over `[]T{}`, emptiness by `len`. [EG] [CRC] [GSD]
- **S9.3** No naked returns except in a handful of lines; named results only when they clarify. [CRC] [GSD]
- **S9.4** Durations are `time.Duration`, instants `time.Time`, never bare integers. [UBER] (third-party)
- **S9.5** Nothing the ticket does not need: no parameters, hooks or interfaces for an imagined future. [GEN] [GSB]
- **S9.6** Where the rules are silent, follow the nearby code. [GSG]

## Open gaps

Places where today's code does not meet a rule. New code follows the rule; fixing the old code is a ticket of its own.

- **S1.7** The window of duplicate candidates is computed from `time.Now()` (`internal/tickets/candidates.go`), and the store stamps an event without a time with `time.Now()` (`internal/store/sqlite.go`).
- **S2.1** Spec section 13 does not list `project`, `tickets/standard`, `tickets/journal` and `tickets/github/githubtest`, and names gRPC where `api` serves HTTP + JSON.
- **S3.7** Ticket events are constants (`core.EventX`), task and VCS events inline string literals (`internal/core/tasks.go`, `internal/vcs/watch.go`).
- **S4.6** Background loops log with `log.Printf`; structured logging (`log/slog`) is not used.
- **S5.6** Goroutines start with `wg.Add(1); go func(){ defer wg.Done() … }()` (`internal/api/daemon.go`).
- **S7.10** Some end-to-end tests wait with fixed sleeps rather than a deadline loop (`cmd/istari/daemon_test.go`).
- **S7.12** The tests of some packages do not call `t.Parallel()` yet; the sibling tickets of #149 bring them in line.
- **S8.1** One `sort.Strings` (`internal/harness/claudecode/claudecode.go`) and a few counting loops remain.
