package api

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/store"
)

// File names inside the daemon's data directory.
const (
	SocketName   = "istari.sock"
	DatabaseName = "istari.db"
	LockName     = "daemon.lock"
)

// ErrAlreadyRunning means another daemon holds the data directory.
var ErrAlreadyRunning = errors.New("another istari daemon is already running on this data directory")

// shutdownTimeout bounds how long in-flight requests may take after SIGTERM.
const shutdownTimeout = 30 * time.Second

// DaemonOptions configures RunDaemon.
type DaemonOptions struct {
	DataDir string
	Limits  core.Limits
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

	socket := filepath.Join(dataDir, SocketName)
	os.Remove(socket) // a stale socket of a daemon that died; the lock proves none runs
	ln, err := listenPrivate(socket)
	if err != nil {
		return err
	}
	defer os.Remove(socket)

	srv := NewServer(st, ServerOptions{Clock: time.Now, Limits: opts.Limits})
	schedCtx, stopScheduler := context.WithCancel(context.Background())
	scheduled := make(chan struct{})
	go func() { defer close(scheduled); srv.Scheduler().Run(schedCtx) }()
	defer func() { stopScheduler(); <-scheduled }()
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
