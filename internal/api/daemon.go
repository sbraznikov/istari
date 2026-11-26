package api

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/sbraznikov/istari/internal/config"
	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/github/ratelimit"
	"github.com/sbraznikov/istari/internal/runner"
	"github.com/sbraznikov/istari/internal/store"
	"github.com/sbraznikov/istari/internal/tickets"
	"github.com/sbraznikov/istari/internal/vcs"
)

// File names inside the daemon's data directory.
const (
	SocketName   = "istari.sock"
	DatabaseName = "istari.db"
	LockName     = "daemon.lock"
)

// SocketPath returns the daemon's socket in dataDir, or an error when the
// path is longer than a unix socket may be here: the bind would fail with a
// bare "invalid argument".
func SocketPath(dataDir string) (string, error) {
	socket := filepath.Join(dataDir, SocketName)
	limit := 103 // sun_path of macOS and the BSDs, less the closing NUL
	if runtime.GOOS == "linux" {
		limit = 107
	}
	if len(socket) > limit {
		return socket, fmt.Errorf("the daemon's socket %s is %d bytes, more than the %d a unix socket may have here: choose a shorter data_dir", socket, len(socket), limit)
	}
	return socket, nil
}

// ErrAlreadyRunning means another daemon holds the data directory.
var ErrAlreadyRunning = errors.New("another istari daemon is already running on this data directory")

// shutdownTimeout bounds how long in-flight requests may take after SIGTERM.
const shutdownTimeout = 30 * time.Second

// DaemonOptions configures RunDaemon.
type DaemonOptions struct {
	DataDir      string
	Limits       core.Limits
	Priority     string               // the default ticket priority; "" means core.DefaultPriority
	Tracker      tickets.SyncProvider // nil: no ticket sync
	Repos        []tickets.SyncRepo
	Standards    tickets.Standards // nil: no admission
	SyncInterval time.Duration     // 0: tickets.DefaultSyncInterval
	GitServer    string            // where repositories are cloned from; "": https://github.com
	// Runner's slots turn the runner on; its data dir and socket are the
	// daemon's.
	Runner runner.RunnerOptions
	// Defaults are the installation's pipeline defaults.
	Defaults config.Defaults
	// VCS is the GitHub side; nil turns pushes and pull requests off.
	VCS *vcs.Client
	// GitHub is the transport under Tracker and VCS (ServerOptions.GitHub).
	GitHub *ratelimit.Transport
	// PausedBy, when set, pauses the hand-out of tasks for all projects in
	// this actor's name before the first sync and claim, as
	// istari pause --all does.
	PausedBy string
	// Stepped runs the scheduler, the pipeline, the ticket sync and the PR
	// watch one pass per POST /v1/step instead of on their timers; the runner
	// and the leases keep the real clock. Only a test build sets it (spec 3.1).
	Stepped bool
}

// RunDaemon owns the data directory until ctx is canceled: it takes an
// exclusive lock, opens the store, serves the API on a unix socket with mode
// 0600 and runs the scheduler. On cancellation it stops accepting, ends the
// event streams, stops the scheduler and waits for in-flight requests.
func RunDaemon(ctx context.Context, opts DaemonOptions) error {
	dataDir := opts.DataDir
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf("data directory: %w", err)
	}
	unlock, err := lock(filepath.Join(dataDir, LockName))
	if err != nil {
		return err
	}
	defer unlock()

	st, err := store.Open(filepath.Join(dataDir, DatabaseName))
	if err != nil {
		return err
	}
	defer st.Close()

	socket, err := SocketPath(dataDir)
	if err != nil {
		return err
	}
	os.Remove(socket) // a stale socket of a daemon that died; the lock proves none runs
	ln, err := listenPrivate(socket)
	if err != nil {
		return err
	}
	defer os.Remove(socket)

	srv := NewServer(st, ServerOptions{Clock: time.Now, Limits: opts.Limits, Priority: opts.Priority, Tracker: opts.Tracker, Repos: opts.Repos, Standards: opts.Standards,
		Workspaces: runner.WorkspaceOptions{DataDir: dataDir, Server: opts.GitServer}, Runner: runnerOptions(opts.Runner, dataDir, socket), Defaults: opts.Defaults, VCS: opts.VCS, GitHub: opts.GitHub, Stepped: opts.Stepped})
	// The lock proves no other daemon owns a run still recorded running.
	if err := srv.EndRunsLeftRunning(ctx); err != nil {
		return fmt.Errorf("ending runs left running: %w", err)
	}
	// A store from before a finished ticket canceled its tasks is repaired
	// once, before anything hands them out (spec 4.2).
	if _, err := srv.Tasks().CancelOfFinished(ctx, "system"); err != nil {
		return fmt.Errorf("canceling the open tasks of finished tickets: %w", err)
	}
	if opts.PausedBy != "" {
		// A hand-out still paused from before the restart stays paused.
		var already *core.Error
		if err := srv.Tasks().Pause(ctx, opts.PausedBy, ""); err != nil && !(errors.As(err, &already) && already.Code == core.CodeInvalidState) {
			return fmt.Errorf("pausing the hand-out: %w", err)
		}
	}
	bgCtx, stopBackground := context.WithCancel(context.Background())
	var background sync.WaitGroup
	if !opts.Stepped {
		background.Add(1)
		go func() { defer background.Done(); srv.Scheduler().Run(bgCtx) }()
		background.Add(1)
		go func() { defer background.Done(); srv.RunWorkspaces(bgCtx) }()
		background.Add(1)
		go func() { defer background.Done(); srv.RunVCS(bgCtx, time.Minute) }()
		if srv.Syncer() != nil {
			background.Add(1)
			go func() { defer background.Done(); srv.Syncer().Run(bgCtx, opts.SyncInterval) }()
		}
	}
	if srv.Runner() != nil {
		background.Add(1)
		go func() { defer background.Done(); srv.Runner().Run(bgCtx) }()
	}
	defer func() { stopBackground(); background.Wait() }()
	httpSrv := &http.Server{Handler: srv, ReadHeaderTimeout: 10 * time.Second}
	httpSrv.RegisterOnShutdown(srv.Close)
	served := make(chan error, 1)
	go func() { served <- httpSrv.Serve(ln) }()

	select {
	case err := <-served:
		return fmt.Errorf("serving: %w", err)
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

func runnerOptions(o runner.RunnerOptions, dataDir, socket string) runner.RunnerOptions {
	o.DataDir, o.Socket = dataDir, socket
	return o
}

// lock takes an exclusive, non-blocking flock on path. The kernel releases it
// when the process dies, so a crashed daemon never blocks the next one.
func lock(path string) (unlock func(), err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrAlreadyRunning
		}
		return nil, fmt.Errorf("lock: %w", err)
	}
	return func() { f.Close() }, nil
}

// listenPrivate listens on a unix socket that only the owner can use. The
// umask makes the socket 0600 from the start; the chmod states it explicitly.
func listenPrivate(socket string) (net.Listener, error) {
	old := syscall.Umask(0o077)
	ln, err := net.Listen("unix", socket)
	syscall.Umask(old)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", socket, err)
	}
	if err := os.Chmod(socket, 0o600); err != nil {
		ln.Close()
		return nil, fmt.Errorf("chmod %s: %w", socket, err)
	}
	return ln, nil
}
