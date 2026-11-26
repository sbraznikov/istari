package exectest

import (
	"os"
	"syscall"
	"testing"
)

// Write puts script at path as an executable file that a test can start at
// once.
//
// It holds syscall.ForkLock for reading while the file is open for writing.
// os/exec takes that lock for writing around every fork, so no process
// started by another goroutine of the test binary can inherit the write
// descriptor; on Linux such a descriptor makes the start fail with "text file
// busy" (golang/go#22315).
func Write(t testing.TB, path, script string) {
	t.Helper()
	syscall.ForkLock.RLock()
	err := write(path, script)
	syscall.ForkLock.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func write(path, script string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	_, err = f.WriteString(script)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}
