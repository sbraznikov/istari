# Go coding standards: primary-source research

Researched: 2026-11-24 (local toolchain: go1.27.3; repository `go.mod` declares `go 1.27.3`)

## How to use this

This file is the input for `CODING_STANDARDS.md`. Each finding is one rule-like sentence with its source in brackets, so a rule can quote its source. Section 1 lists what gofmt, `go vet`, staticcheck and golangci-lint already enforce. A reviewer should not flag those items, because CI tooling reports them. Third-party sources are marked "(third-party)". Where sources disagree, the conflict is stated and no side is chosen. "Unverified" means no primary source was found for the claim during this research.

Source keys (full list at the end): [CRC] Go Code Review Comments, [TC] Go Test Comments, [EG] Effective Go, [DOC] Go Doc Comments, [GSG] Google guide, [GSD] Google decisions, [GSB] Google best practices, [UBER] Uber guide (third-party), [PROV] Go Proverbs.

Note on [EG]: Effective Go says of itself that it "was written for Go's release in 2009 and is not actively updated" and "does not cover significant changes to the language (generics), ecosystem (modules), or libraries added since." Where it conflicts with newer sources, cite the newer source. [EG]

## 1. Formatting and what tooling enforces (reviewer skips these)

- All Go source must be in gofmt output format. gofmt reformats doc comments too (since Go 1.19). [GSG] [DOC]
- goimports does what gofmt does and also adds and removes imports. [CRC]
- Go has no fixed line length. Break lines for meaning, not length. [CRC] [GSG] Conflict: Uber sets a soft limit of 99 characters. [UBER] (third-party)
- `go vet` runs all of its analyzers by default. That includes appends, assign, atomic, bools, buildtag, composites (unkeyed literals of types from other packages), copylocks, defers, errorsas, hostport, httpresponse, ifaceassert, loopclosure, lostcancel, nilfunc, printf, shift, sigchanyzer, slog, stdmethods, stdversion, stringintconv, structtag, testinggoroutine, tests, timeformat, unmarshal, unreachable, unsafeptr, unusedresult and waitgroup. [VET]
- `go test` runs a subset of vet automatically. Since Go 1.27 that subset includes `stdversion`, which reports stdlib symbols newer than the file's Go version. [GO127]
- The vet analyzers added by release:
  - Go 1.22: `append` with no values, deferred `time.Since`, and malformed `slog` key-value pairs. [GO122]
  - Go 1.23: `stdversion`. [GO123]
  - Go 1.24: `tests`, non-constant `Printf` format strings, invalid `//go:build go1.x.y`, and `copylock` in 3-clause loops. [GO124]
  - Go 1.25: `waitgroup` (misplaced `Add`) and `hostport`. [GO125]
  - Go 1.27: the printf check reports `%w` with a `*E` argument when `E` implements `error`. [GO127]
- Reviewers can skip vet findings such as lost `cancel` funcs (`lostcancel`), `t.Fatal` from a goroutine (`testinggoroutine`) and loop-variable capture. [VET]
- staticcheck groups its checks as SA (correctness), S (simplifications), ST (style) and QF (quick fixes). [SC]
  - Default-on ST checks: ST1005 (error strings), ST1012 (error variable naming `ErrFoo`/`errFoo`) and ST1019 (duplicate imports). [SC]
  - Off by default: ST1000 (package comment), ST1003 (naming), ST1016 (consistent receiver names), ST1020–ST1022 (doc comment starts with the name) and ST1023. Those stay reviewer territory unless enabled in config. [SC]
  - Other notable checks: SA1012 (nil context), SA1029 (built-in type as `WithValue` key), SA5001 (defer `Close` before the error check), SA9001 (defer in a range loop), SA1019 (deprecated API use) and SA4006 (value overwritten before read). [SC]
- golangci-lint v2's default ("standard") set is errcheck, govet, ineffassign, staticcheck and unused. errcheck reports unchecked errors, so a reviewer need not flag a discarded `err` if CI runs it. [GCL-QS] [GCL-CFG]
- Uber recommends at least errcheck, goimports, revive, govet and staticcheck, run through golangci-lint. [UBER] (third-party)
- `go fix` was rewritten in Go 1.26 to host "modernizers" that rewrite code to current idioms without changing behaviour. [FIX] [GO126]
  - Go 1.27.3 registers: any, atomictypes, buildtag, embedlit, errorsastype, forvar, hostport, inline, mapsloop, minmax, newexpr, omitzero, plusbuild, rangeint, reflecttypefor, slicesbackward, slicescontains, slicessort, stditerators, stringsbuilder, stringscut, stringscutprefix, stringsseq, testingcontext, unsafefuncs and waitgroupgo. Source: the output of `go tool fix help`.
  - If CI runs `go fix -diff ./...`, a reviewer can skip these idioms. The blog suggests running `go fix` after every toolchain upgrade, usually until it stops changing code (about two passes). [FIX]
- The race detector finds races only on executed paths. Run `go test -race`. [RACE]

## 2. Naming

- Use MixedCaps or mixedCaps, not underscores. An unexported constant is `maxLength`, not `MAX_LENGTH`. [CRC] [GSG]
  - Underscores are allowed in `Test`/`Benchmark`/`Example` function names, in generated code and in file names. [GSD]
- Package names are short, lowercase single words, with no underscores or mixedCaps. The name matches the last element of the import path. [EG] [PKGN]
- Don't name packages `util`, `common`, `misc`, `api`, `types` or `interfaces`. [CRC] [PKGN]
  - Google adds `utility`, `helper`, `model` and `testhelper`. [GSD]
  - Uber adds `shared` and `lib`. [UBER] (third-party)
  - If you can't find a good name, the package boundary may be wrong. [PKGN]
- Don't give a package a name that callers commonly use for local variables (e.g. `buf`). Don't reuse the names of popular stdlib packages (`io`, `http`). [PKGN] [GSD]
- Don't repeat the package name in exported identifiers: `http.Server`, not `http.HTTPServer`; `widget.New`, not `widget.NewWidget`. [PKGN] [GSD]
- `New` in package `pkg` returns `pkg.Pkg`. With several constructors, include the type name (`time.NewTicker`). [PKGN] [EG]
- Keep initialisms in one case: `ServeHTTP` not `ServeHttp`, `appID` not `appId`, `URL` or `url`, never `Url`. [CRC] [GSD]
- Receiver names are one or two letters that abbreviate the type, used the same way on every method. Never `this`, `self` or `me`. Omit the receiver name if it is unused. [CRC] [GSD]
- Getters have no `Get` prefix: `Owner()` / `SetOwner()`. [EG] [GSD]
  - Exception: "get" is part of the concept (HTTP GET). [GSD]
  - Use `Compute` or `Fetch` to signal an expensive or remote call. [GSD]
- A one-method interface is named after its method plus `-er` (`Reader`, `Formatter`). [EG]
- Don't reuse canonical method names (`Read`, `Write`, `Close`, `String`) unless the signature and meaning match. Use `String`, not `ToString`. [EG]
- Name length grows with scope and shrinks with frequency of use. Short-lived locals get short names. Names used far from their declaration get descriptive ones. [CRC] [GSD]
- Don't put the type in a variable name (`users`, not `userSlice` or `numUsers`) unless two forms coexist (`ageString`, `age`). [GSD]
- Name constants by role, not value. No `ALL_CAPS`, no `k` prefix. [GSD]
- Function names: noun-like for functions that return values, verb-like for functions that act. Add a type suffix only to disambiguate (`ParseInt64`). [GSB]
- Error variables are `ErrFoo` (exported) or `errFoo` (unexported). Custom error types end in `Error`. [UBER] (third-party) [SC]
- Don't shadow predeclared identifiers (`error`, `string`, `len`). [UBER] (third-party)
- Don't name variables after imported stdlib packages (e.g. `url`). Inner-scope shadowing with `:=` can cause bugs, so use `=` or a new name. [GSB]
- Conflict: Uber prefixes unexported top-level vars and consts with `_` (`_defaultPort`). Google says names "should generally not contain underscores" outside the listed exceptions, and the Go wiki asks for mixedCaps. [UBER] (third-party) [GSD] [CRC]

## 3. Package design and layout

- A command plus private supporting code puts the supporting packages under `internal/`. Other modules cannot import them, so their API can be refactored freely. [LAYOUT]
- Put commands under `cmd/<name>/main.go` when a repository has several commands or mixes commands with importable packages. [LAYOUT]
- A server project keeps its logic in `internal/` and its binaries in `cmd/`. Code that becomes shareable is split into a separate module. [LAYOUT]
- Group tightly coupled types in one package and put conceptually distinct ideas in separate packages. There is no "one type, one file" rule. Avoid both huge files and many tiny files. [GSB]
- Don't use a single package for all your APIs. Catch-all packages grow without bound. [PKGN]
- Libraries don't force clients onto package-level mutable state. Provide instance values that callers pass as explicit dependencies. Treat global state with "extreme scrutiny". [GSB] Uber: "Avoid Mutable Globals", use dependency injection. [UBER] (third-party)
- Avoid `init()`. If you use it, keep it deterministic, with no I/O and no global-state dependence. Start no goroutines in `init()`. [UBER] (third-party)
- Call `os.Exit` or `log.Fatal` only in `main`, and at most once. Put the logic in a `run() error` that `main` calls. [UBER] (third-party)
- Propagate initialization errors to `main` and exit with an actionable message. [GSB]
- Blank imports (`import _ "pkg"`) belong only in `main` or tests. Don't use dot imports. [CRC] [GSD]
- Rename imports only to resolve a collision. Rename the most local or project-specific import. [CRC] [GSD]
- Import groups:
  - Uber: two groups, stdlib then everything else. [UBER] (third-party)
  - Google: stdlib, then other packages, then protos, then side-effect imports. [GSD]
  - The difference only matters for proto and side-effect imports.
- Keep CLI code separate from library code when code serves both. Define flags only in `main`. Libraries take configuration through Go APIs. [GSB] [GSD]
- For `go.mod`: since Go 1.21 the `go` line is a mandatory minimum and sets the language version. The `tool` directive (Go 1.24) replaces the `tools.go` blank-import workaround. [MOD] [GO121] [GO124]

## 4. Interfaces

- Define interfaces in the package that uses them, not in the package that implements them. [CRC] [GSB]
- "Accept interfaces, return concrete types." [GSB] [GSD]
  - Exceptions: returning an interface for encapsulation, for runtime-selected implementations, or to break an import cycle. [GSB]
- Don't define an interface before a real need exists: several implementations, packages that must be decoupled, or a narrow consumer-facing view of a large type. [CRC] [GSB]
- Keep interfaces small and unexported unless they are needed outside the package. "The bigger the interface, the weaker the abstraction." [GSB] [PROV]
- Don't export test doubles alongside the real implementation, and don't wrap generated clients in new interfaces just for tests. Test through the public API. [GSB]
- If a method only needs to call methods on a value, use an interface, not a type parameter. [GEN]
- Pass interfaces by value, never as a pointer to an interface. [UBER] (third-party) [GSD]
- Compile-time assertion `var _ I = (*T)(nil)`:
  - Uber: add it wherever a type must satisfy an interface. [UBER] (third-party)
  - Effective Go: use it only when no static conversion already exists, "a rare case". [EG]
  - Conflict on how often to use it.
- Don't embed types in exported structs where that would leak the embedded type's methods. Never embed a mutex. [UBER] (third-party)
- Use `any`, not `interface{}`, in new code (Go 1.18+). `go fix -any` rewrites it. [GSD] [FIX]

## 5. Errors

- `error` is the last return value. Return `nil` on success. When an error is returned, the other results are unspecified. [GSD] [CRC]
- Don't signal failure in-band (`-1`, `""`, `nil` pointer). Return an extra `error` or `ok bool`. [CRC] [GSD]
- Exported functions return the `error` interface, not a concrete error pointer type, to avoid typed-nil bugs. [GSD]
- Don't discard errors with `_`. Handle them, return them, or (truly exceptional) panic. If a discard is safe, a comment explains why. [CRC] [GSD]
- Error strings are lowercase and have no trailing punctuation (they get embedded in other messages). Exception: they start with a proper noun or an exported identifier. [CRC] [GSD] (staticcheck ST1005 enforces this)
- Handle each error once. Don't log an error and also return it. The caller decides. [GSB] [UBER] (third-party)
- Callers must be able to tell errors apart without matching strings. Give errors structure with sentinel values or typed errors. [GSB] [TC]
- Compare with `errors.Is` (walks the wrapped chain), not `==`. Use `errors.As`, not a type assertion. [ERR13]
- Since Go 1.26, `errors.AsType[T]` is the generic, type-safe and faster form of `errors.As`. `go fix -errorsastype` rewrites to it. [GO126] [FIX]
- Wrap with `%w` only when callers should inspect the cause. "Wrapping an error makes that error part of your API." [ERR13]
  - Use `%v` (or translate the error) to hide implementation details, e.g. at system boundaries or for database or `*os.PathError` errors. [ERR13] [GSB]
- Put `%w` at the end (`"...: %w"`) so the text reads like the chain. Exception: a sentinel may lead. [GSB]
- Add context the error does not already carry. Avoid "failed to" stop words. [GSB]
- If a function documents that it returns a wrapped sentinel, it must not return the raw underlying error. [ERR13]
- Use `errors.Join`, multiple `%w` in one `fmt.Errorf`, or `Unwrap() []error` to combine errors (Go 1.20). `errors.Is`/`As` inspect all branches. [GO120]
- Since Go 1.21, `errors.ErrUnsupported` is the standard "operation not supported" error. [GO121]
- Static message: `errors.New`. Dynamic message: `fmt.Errorf`. [UBER] (third-party)
- Don't use `panic` for normal error handling. [CRC] [GSD] [PROV]
  - Panic is acceptable for "impossible" states or an unrecoverable init failure. Internal panics must never cross a package boundary. [EG] [GSB]
  - Don't `recover` just to avoid crashes. [GSB]
- Use the comma-ok form for type assertions that can fail. [UBER] (third-party)
- Document significant sentinel errors and error types, and say whether error types are pointers. [GSB]

## 6. Context

- `ctx context.Context` is the first parameter of every function that needs it. [CTX] [CRC] [GSD]
  - Exceptions: HTTP handlers (`r.Context()`), streaming RPCs, and tests (`t.Context()`). [GSD]
- Don't store a Context in a struct. Pass it to each method. [CTX] [CTXS]
  - Backward-compatibility exception: add a `FooContext` variant rather than a context field. [CTXS]
- Never pass a nil Context. Use `context.TODO()` if unsure. [CTX] (staticcheck SA1012)
- Use `context.Background()` only at entry points (`main`, `init`, tests). Library code takes the caller's context. [GSD]
- Use context values only for request-scoped data that transits processes and APIs, never for optional parameters. [CTX] Keys are unexported custom types, not built-ins. (staticcheck SA1029) [SC]
- Always call the `CancelFunc` from `WithCancel`/`WithTimeout`/`WithDeadline`. Failing to call it "leaks the child and its children until the parent is canceled". [CTX] (vet `lostcancel`)
- Don't create custom context types or substitute other interfaces for `context.Context`. [GSD]
- Never put a context in an options struct. [GSB]
- Don't document that cancellation interrupts the function. Document only behaviour that differs, e.g. returning a nil error on cancellation. [GSB]
- Newer APIs:
  - Go 1.20: `WithCancelCause` and `Cause`. [CTX] [GO120]
  - Go 1.21: `AfterFunc`, `WithoutCancel` (keeps values but not cancellation, for work that must outlive a request), `WithDeadlineCause` and `WithTimeoutCause`. [CTX] [GO121]

## 7. Concurrency

- Every goroutine has an obvious exit. Document when and why it exits if that is not obvious. "Goroutines are not garbage collected; they must exit on their own." [CRC] [GSD] [PIPE]
- No fire-and-forget goroutines. Each goroutine needs a way to be stopped and a way to be waited on. [UBER] (third-party)
- Prefer synchronous functions. Callers can add concurrency more easily than they can remove it. [CRC] [GSD]
- Use `sync.WaitGroup.Go` (Go 1.25) instead of `wg.Add(1); go func(){ defer wg.Done() ... }()`. `go fix -waitgroupgo` rewrites it, and vet `waitgroup` flags a misplaced `Add`. [GO125] [FIX]
- For a group of tasks that return errors, use `golang.org/x/sync/errgroup`:
  - `WithContext` cancels the derived ctx on the first error, and `Wait` returns that error. [ERRG]
  - `SetLimit` bounds concurrency.
  - Don't reuse a Group for different tasks.
- Pipeline stages close their outbound channels when they finish sending. Broadcast cancellation by closing a `done` channel (today usually `ctx.Done()`). [PIPE]
- "Share memory by communicating", but a mutex is often best for simple state such as a reference count. "Channels orchestrate; mutexes serialize." [EG] [PROV]
- A zero `sync.Mutex` is ready to use. Hold it as a named unexported field, not a pointer and not embedded. [EG] [UBER] (third-party)
- Don't copy values whose methods use pointer receivers or that contain locks (`sync.Mutex`, `bytes.Buffer`). [CRC] [GSD] (vet `copylocks`)
- Uber: channel buffer size is one or none, and any other size needs justification. [UBER] (third-party)
- Use typed atomics (`atomic.Bool`, `atomic.Int64`) instead of atomic functions on plain integers. [UBER] (third-party)
  - Uber names `go.uber.org/atomic`. The stdlib now has typed atomics, and `go fix -atomictypes` (Go 1.27) rewrites calls to them. [GO127] [FIX]
- Since Go 1.23, timer channels are unbuffered and unstopped timers are garbage collected. Don't poll `len(timer.C)`. [GO123]
- Use `sync.OnceFunc`/`OnceValue`/`OnceValues` (Go 1.21) for lazy initialization instead of a hand-written `sync.Once`. [GO121]
- Run tests with `-race`. The detector only sees executed paths, so concurrency code needs tests that exercise it. Overhead is about 5–10x memory and 2–20x time. [RACE]
- Go 1.26 added the experimental `goroutineleak` pprof profile. Go 1.27 made it generally available. [GO126] [GO127]

## 8. Documentation

- Every exported top-level name has a doc comment. Non-trivial unexported declarations should have one too. [CRC] [DOC] [GSD]
- Doc comments are full sentences that begin with the name being described and end with a period:
  - "Quote returns …"
  - "A Reader serves …"
  - For a boolean function, use "reports whether" (not "or not"). [DOC] [CRC]
- Every package has exactly one package comment, in one file, directly above `package` with no blank line, beginning "Package x …". [DOC] [CRC] [GSD]
- A command's comment begins with the program name ("Istari runs …"). [DOC]
- A function's comment says what it returns, or for side-effect functions what it does. It documents special cases and leaves out algorithm internals. [DOC]
- Types are assumed safe for one goroutine at a time and functions safe for concurrent calls. Document only stronger or different guarantees. [DOC] [GSB]
- If the zero value's meaning is not obvious, document it. [DOC]
- Document any cleanup the caller must perform (e.g. `Close`). [GSB]
- Mark deprecation with a paragraph starting `Deprecated:` that names the replacement. [DOC]
- Syntax:
  - `# Heading` (Go 1.19+)
  - `[Name]` and `[pkg.Name]` doc links
  - `-` lists (no nesting)
  - Code blocks are indented. Accidental indentation becomes a code block. [DOC]
- Comments explain why, not what. [GSG]
- New packages include runnable `Example` functions in test files. [CRC] [GSD]

## 9. Testing

- Use the standard `testing` package with plain Go comparisons. Don't use assertion libraries, and don't use `t.Helper` to build one. [TC] [GSD]
  - Google: `testing` "is the only testing framework permitted" internally. [GSD]
- Failure messages name the function and the input, and print got before want: `Foo(%q) = %v, want %v`. [TC] [CRC] [GSD]
- Compare whole structs in one check:
  - Use `cmp.Diff`/`cmp.Equal` (github.com/google/go-cmp) for new code, not `reflect.DeepEqual`. [TC] [GSD]
  - Label the diff direction, e.g. `(-want +got)`. [TC] [GSD]
  - Note: go-cmp is a dependency. The standard library alone offers `reflect.DeepEqual`, `slices.Equal` and `maps.Equal`.
- Prefer `t.Error` so one run reports every failure. Use `t.Fatal` when later checks would be meaningless (failed setup), or inside a subtest. [TC] [GSD] [GSB]
- Never call `t.Fatal`/`FailNow` from a goroutine other than the test's own. Use `t.Error` and return. [TESTING] [GSB] (vet `testinggoroutine`)
- Don't check error kinds by string comparison. Use `errors.Is` (or `cmpopts.EquateErrors`). If only presence matters, check `err != nil`. [TC] [GSD]
- Don't compare output you don't own byte for byte (e.g. JSON from a library). Parse it and compare semantically. [TC] [GSD]
  - Go 1.27 backs `encoding/json` with v2 and some error text changed, which makes this rule concrete. [GO127]
- Table-driven tests:
  - Use them only when the cases share the same logic. [TC] [GSD]
  - Cases that need different setup or branching go in separate test functions. Don't add a row-level switch or `shouldCallX` flags. [GSD] [UBER] (third-party)
- Name table cases. Don't identify a case by its index. Subtest names are short, readable after escaping, and have no spaces or slashes. [TC] [GSD]
- Subtests must not depend on each other. Each must run alone under `go test -run`. [GSD]
- Test helpers do setup and cleanup:
  - Call `t.Helper()`.
  - Fail with `t.Fatal` on setup errors.
  - Take `*testing.T` after any ctx.
  - Register teardown with `t.Cleanup`. [GSD] [GSB] [TESTING]
- Keep failure decisions in the `Test` function. Validation helpers return an `error` rather than failing the test. [GSB]
- Use these instead of hand-rolled setup:
  - `t.TempDir` (Go 1.15)
  - `t.Setenv` (Go 1.17)
  - `t.Chdir` (Go 1.24). `Setenv` and `Chdir` can't be used in parallel tests. [TESTING] [GO124]
  - `t.Context()` (Go 1.24), which is cancelled before Cleanup functions run. `go fix -testingcontext` rewrites `context.WithCancel` in tests. [TESTING] [GO124] [FIX]
  - `t.ArtifactDir` (Go 1.26) for output files. [TESTING] [GO126]
- Scope setup to the tests that need it (no `init()` loading of fixtures). Use `TestMain` sparingly. Each case resets any global state it changes. [GSB]
- Same-package tests (`package foo`) can reach unexported identifiers. Black-box `package foo_test` is for integration-style tests or for breaking import cycles. [GSD]
  - Note: the Google guide allows both styles. The repo's AGENTS.md rule "test behaviour through public interfaces" is stricter than any source here.
- Prefer real implementations, including the production client against a test server (`httptest`), over hand-built fakes. Test-double packages are named `<pkg>test` (e.g. `creditcardtest`). [GSB]
- Don't sleep to wait for concurrent code. Use `testing/synctest`:
  - It was experimental in 1.24 and became stable in Go 1.25 as `synctest.Test` plus `synctest.Wait`.
  - It provides a fake clock that advances only when every goroutine in the bubble is durably blocked.
  - Go 1.27 adds `synctest.Sleep` and `httptest.NewTestServer` (in-memory network).
  - Inside the bubble, `T.Run`, `T.Parallel` and `T.Deadline` are forbidden. Mutexes, I/O and syscalls are not durably blocking. [SYNC] [SYNCB] [GO125] [GO127]
- Benchmarks use `for b.Loop() { … }` (Go 1.24) instead of `for range b.N`. [GO124] [GO126]
- Name fields in multi-line test struct literals. [GSB]
- Golden files: not covered by the Go wiki or the Google pages read here. Unverified as a primary-source rule.

## 10. Modern idioms (Go 1.21–1.27)

- Use builtins and stdlib packages instead of hand-written loops (Go 1.21). `go fix` modernizers `minmax`, `slicescontains`, `slicessort` and `mapsloop` rewrite them. [GO121] [FIX]
  - Builtins: `min`, `max` and `clear`.
  - Packages: `slices` (`Sort`, `Contains`, `Index`), `maps` (`Clone`, `Copy`, `Equal`) and `cmp` (`Compare`, `Ordered`).
- Use `cmp.Or` for "first non-zero value" defaults (Go 1.22). [GO122]
- Use `slices.Concat` (Go 1.22). [GO122]
- Write `for i := range n` (Go 1.22) instead of a 3-clause counting loop (`go fix -rangeint`). [GO122] [FIX]
- Since Go 1.22 loop variables are per iteration. Remove `x := x` copies (`go fix -forvar`). This applies only to modules whose go line is at least 1.22. [LOOPVAR] [GO122]
- Use `math/rand/v2` for non-crypto randomness and `crypto/rand` for anything secret (keys, tokens). [GO122] [CRC] [GSD]
- `net/http.ServeMux` supports method and wildcard patterns (`"GET /items/{id}"`, `r.PathValue`) since Go 1.22. [GO122]
- Use `database/sql.Null[T]` (Go 1.22) for nullable columns. [GO122]
- Iterators (Go 1.23):
  - Range over `iter.Seq`/`iter.Seq2` functions.
  - Container types offer an `All` method.
  - Use `slices.Collect`/`Sorted` and `maps.Keys`/`Values`.
  - With `iter.Pull`, always call `stop`.
  - Don't overuse iterators: "in many cases a loop will be clearer." [RANGEF] [GO123]
- Use `strings`/`bytes` `Lines`, `SplitSeq` and `FieldsSeq` (Go 1.24) instead of `Split` when only iterating (`go fix -stringsseq`). [GO124] [FIX]
- Use `strings.Cut`/`CutPrefix` instead of `Index` plus slicing (`go fix -stringscut`, `-stringscutprefix`). Go 1.27 adds `strings.CutLast`/`bytes.CutLast`. [FIX] [GO127]
- Use JSON tag `omitzero` (Go 1.24) for zero structs such as `time.Time`. `omitempty` doesn't omit those (`go fix -omitzero`). [GO124] [FIX]
- `encoding/json` is backed by json/v2 in Go 1.27:
  - Defaults are stricter: invalid UTF-8 and duplicate object names are rejected.
  - Error text differs.
  - `encoding/json/v2` and `jsontext` are new packages.
  - Opt out with `GOEXPERIMENT=nojsonv2`. [GO127] [GO125]
- Use `os.Root` (Go 1.24, extended in 1.25) to confine file operations to a directory tree when paths come from untrusted input. [GO124] [GO125]
- Use `new(expr)` (Go 1.26) instead of `ptrTo` helpers (`go fix -newexpr`). [GO126] [FIX]
- Go 1.27 allows generic methods, but interface methods can't have type parameters. [GO127]
- Generics:
  - Write functions first. Avoid type parameters until you are about to write the exact same code several times. [GEN]
  - Use them for container-agnostic functions and data structures.
  - Don't use them when each type needs a different implementation or when you only call methods (use an interface).
  - Don't use them for speed alone. [GEN]
  - Google: don't use them to build DSLs or for single-type code. [GSD]
- Structured logging uses `log/slog` (Go 1.21):
  - Use key-value pairs or `slog.Attr`, and `LogAttrs` on hot paths.
  - `Logger.With` adds common attributes.
  - Use the `…Context` methods so handlers can read trace data. [SLOG] [GO121]
  - vet `slog` checks key-value pairing. [GO122]
  - `slog.NewMultiHandler` (Go 1.26) fans out to several handlers. [GO126]
- Use the `tool` directive (Go 1.24) and `go tool x` for build tools instead of `tools.go`. [GO124] [MOD]

## 11. Dependencies and security

- Prefer core language, then stdlib, then existing in-repo code, before adding a dependency ("least mechanism"). [GSG] "A little copying is better than a little dependency." [PROV]
- `go mod tidy` adds missing and removes unused requirements and syncs `go.sum`. `go mod tidy -diff` exits non-zero if changes are needed, so CI can use it as a check. Source: `go help mod tidy` (go1.27.3).
- Go 1.27: `go mod tidy` collapses require blocks into at most two (direct and indirect). [GO127]
- The `go` directive is a mandatory minimum since Go 1.21. The compiler rejects language features newer than it. [MOD] [GO121]
- Run `govulncheck ./...` (install `golang.org/x/vuln/cmd/govulncheck@latest`). It reports only vulnerabilities your code actually reaches through call paths. [VULN]
- Never use `math/rand` or `math/rand/v2` for keys or secrets, even throwaway ones. [CRC] [GSD]
- Since Go 1.24, `crypto/rand.Read` never returns an error and `crypto/rand.Text` generates random text. [GO124]
- Use `net.JoinHostPort`, not `Sprintf("%s:%d")` (vet `hostport`, Go 1.25). [GO125]
- Handle PII in logs with care. [GSB]

## 12. Simplicity principles

- Google's principles, in priority order: clarity, simplicity, concision, maintainability, consistency. Consistency breaks ties but never overrides the others. [GSG]
  - "Clear is better than clever." [PROV]
  - "Reflection is never clear." [PROV]
- Normal path at minimum indentation:
  - Handle errors first and return early.
  - No `else` after a branch that returns. [CRC] [GSD] [UBER] (third-party)
- Make the zero value useful. Design types so `var v T` works without a constructor. [EG] [PROV] [DOC]
- Prefer a nil slice (`var s []T`) to `[]T{}`. Test emptiness with `len(s) == 0`, not `s == nil`. [CRC] [GSD] [UBER] (third-party)
- Use named results only when they clarify, e.g. two results of the same type. Don't name them just to enable naked returns. [CRC] [GSD]
- Naked returns are fine only in functions "a handful of lines" long. [CRC] [GSD]
- Don't pass pointers just to save bytes (`*string`, `*io.Reader`). [CRC] [GSD]
- Receiver kind:
  - Pointer receivers for mutation, uncopyable types and large structs, or "if in doubt". [CRC] [GSD]
  - Value receivers for maps, funcs, chans and small immutable data. [CRC] [GSD]
  - Don't mix receiver kinds on one type. [CRC] [GSD]
- Declare variables in the narrowest scope (e.g. `if err := f(); err != nil`). [UBER] (third-party)
- Use `:=` for non-zero initialization and `var` for intentional zero values. [GSB]
- Replace a bare `true`/`false` argument with a named type or a `/* name */` comment. [UBER] (third-party)
- Copy slices and maps received or returned at API boundaries when the caller could mutate internal state. [UBER] (third-party)
- Use `time.Time` for instants and `time.Duration` for periods, never bare ints. Put units in field names where a format can't hold a Duration. [UBER] (third-party)
- Start `iota` enums at 1 unless the zero value is a meaningful default. [UBER] (third-party)
- Use `defer` to release locks and files. [UBER] (third-party)
- Use `+` for a few strings, `fmt.Sprintf` for formatting, and `strings.Builder` for piecewise building (`go fix -stringsbuilder`). [GSB] [FIX]
- Many arguments:
  - Google prefers an option struct passed last. Variadic functional options are for when most callers pass none. [GSB]
  - Uber recommends functional options once a constructor has "three or more arguments". [UBER] (third-party)
  - Conflict on the default choice.
- Size hints:
  - Uber: "Prefer Specifying Container Capacity" in `make`. [UBER] (third-party)
  - Google: use size hints "only when justified by empirical analysis". [GSB]
  - Conflict.
- Keep consistent with nearby code where the guides are silent. A change must not make an existing deviation worse. [GSG]
- Don't write a redundant `break` in `switch`. Use a labeled `break` to leave an enclosing loop. [GSD]
- Documentation is for users. Errors are values: handle them gracefully, don't just check them. [PROV]

## Sources

Read for this research (all fetched 2026-11-24 unless stated):

- [CRC] https://go.dev/wiki/CodeReviewComments: Go wiki, Code Review Comments (Go team)
- [TC] https://go.dev/wiki/TestComments: Go wiki, Go Test Comments (Go team)
- [EG] https://go.dev/doc/effective_go: Effective Go (dated 2009, see note above)
- [DOC] https://go.dev/doc/comment: Go Doc Comments
- [PKGN] https://go.dev/blog/package-names: Go blog, "Package names"
- [LAYOUT] https://go.dev/doc/modules/layout: Organizing a Go module
- [MOD] https://go.dev/ref/mod: Go Modules Reference (`go` and `tool` directives; first 100k chars read)
- [ERR13] https://go.dev/blog/go1.13-errors: Go blog, "Working with Errors in Go 1.13"
- [CTX] https://pkg.go.dev/context: package context documentation
- [CTXS] https://go.dev/blog/context-and-structs: Go blog, "Contexts and structs"
- [SLOG] https://go.dev/blog/slog: Go blog, "Structured Logging with slog"
- [GEN] https://go.dev/blog/when-generics: Go blog, "When To Use Generics"
- [RANGEF] https://go.dev/blog/range-functions: Go blog, "Range Over Function Types"
- [LOOPVAR] https://go.dev/blog/loopvar-preview: Go blog, "Fixing For Loops in Go 1.22"
- [PIPE] https://go.dev/blog/pipelines: Go blog, "Go Concurrency Patterns: Pipelines and cancellation"
- [SYNCB] https://go.dev/blog/synctest: Go blog, "Testing concurrent code with testing/synctest" (written for the 1.24 experiment)
- [FIX] https://go.dev/blog/gofix: Go blog, "Using go fix to modernize Go code" (Go 1.26)
- [GO120] https://go.dev/doc/go1.20, [GO121] https://go.dev/doc/go1.21, [GO122] https://go.dev/doc/go1.22, [GO123] https://go.dev/doc/go1.23, [GO124] https://go.dev/doc/go1.24, [GO125] https://go.dev/doc/go1.25, [GO126] https://go.dev/doc/go1.26, [GO127] https://go.dev/doc/go1.27: Go release notes
- [TESTING] https://pkg.go.dev/testing: package testing documentation (method versions, FailNow restriction)
- [SYNC] https://pkg.go.dev/testing/synctest: package testing/synctest documentation
- [RACE] https://go.dev/doc/articles/race_detector: Data Race Detector
- [VULN] https://go.dev/doc/security/vuln/: Go vulnerability management / govulncheck
- [VET] https://pkg.go.dev/cmd/vet: cmd/vet documentation (analyzer list)
- [GSG] https://google.github.io/styleguide/go/guide: Google Go Style Guide, guide (normative and canonical at Google)
- [GSD] https://google.github.io/styleguide/go/decisions: Google Go Style Guide, decisions
- [GSB] https://google.github.io/styleguide/go/best-practices: Google Go Style Guide, best practices
- [UBER] https://github.com/uber-go/guide/blob/master/style.md and https://raw.githubusercontent.com/uber-go/guide/master/style.md: Uber Go Style Guide (third-party)
- [SC] https://staticcheck.dev/docs/checks/: staticcheck check list
- [GCL-QS] https://golangci-lint.run/docs/welcome/quick-start/: golangci-lint quick start (default linters)
- [GCL-CFG] https://golangci-lint.run/docs/configuration/file/: golangci-lint configuration (`linters.default: standard`)
- [PROV] https://go-proverbs.github.io/: Go Proverbs (Rob Pike's 2015 talk; a community-hosted page, not go.dev)
- Local toolchain output (go1.27.3): `go help mod tidy` and `go tool fix help` (modernizer list)

Fetched but not used as a source: https://golangci-lint.run/docs/linters/ (its default markers did not survive extraction) and https://raw.githubusercontent.com/golangci/golangci-lint/main/.golangci.reference.yml (does not list the standard set).

### Not verified or not covered

- Golden-file testing conventions: no primary Go-team or Google page read here covers them.
- The Go wiki Test Comments page has no section on `t.Fatal` from goroutines. That rule comes from [TESTING], [GSB] and vet.
- Whether Uber's guide uses assertion libraries in its examples: not checked, so it is not listed as a conflict.
- The phrase "write code, don't design types" is not quoted from [GEN]. Only the paraphrase "start by writing functions" was confirmed.
- [GO127] does not label itself final. The installed toolchain is go1.27.3, so the release has shipped.
- Effective Go pages beyond the first 100k characters were not read (the final ~7.5k characters, which cover the end of the errors/panic section).
