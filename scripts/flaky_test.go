package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// flakyRepo is a throwaway git repository with a Go module whose package p
// has a base commit, for running flaky.sh in.
type flakyRepo struct {
	t   *testing.T
	dir string
}

func newFlakyRepo(t *testing.T) *flakyRepo {
	t.Helper()
	r := &flakyRepo{t: t, dir: t.TempDir()}
	r.git("init", "-q")
	r.write("go.mod", "module example.com/m\n\ngo 1.22\n")
	r.write("p/p.go", "package p\n\nfunc Answer() int { return 42 }\n")
	r.commit("base")
	return r
}

func (r *flakyRepo) write(name, content string) {
	r.t.Helper()
	path := filepath.Join(r.dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *flakyRepo) git(args ...string) {
	r.t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = r.dir
	if out, err := cmd.CombinedOutput(); err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func (r *flakyRepo) commit(message string) {
	r.t.Helper()
	r.git("add", "-A")
	r.git("commit", "-q", "--no-verify", "-m", message)
}

// flaky runs scripts/flaky.sh in the repository on files and returns its
// combined output and exit code.
func (r *flakyRepo) flaky(files ...string) (string, int) {
	r.t.Helper()
	script, err := filepath.Abs("flaky.sh")
	if err != nil {
		r.t.Fatal(err)
	}
	cmd := exec.Command("sh", append([]string{script}, files...)...)
	cmd.Dir = r.dir
	out, err := cmd.CombinedOutput()
	if exit, ok := err.(*exec.ExitError); ok {
		return string(out), exit.ExitCode()
	} else if err != nil {
		r.t.Fatal(err)
	}
	return string(out), 0
}

const flakyTest = `package p

import "testing"

var runs int

func TestFlaky(t *testing.T) {
	runs++
	if runs%5 == 0 {
		t.Fatalf("run %d failed", runs)
	}
}
`

func TestFlakyShNamesATestOfANewFileThatFailsOnItsFifthRun(t *testing.T) {
	t.Parallel()
	r := newFlakyRepo(t)
	r.write("p/flaky_test.go", flakyTest)
	r.commit("a flaky test")

	out, code := r.flaky("p/flaky_test.go")

	if code == 0 {
		t.Fatalf("flaky.sh exited 0 on a test that fails 1 time in 5:\n%s", out)
	}
	if !strings.Contains(out, "flaky: TestFlaky failed on run 5 of 20 in ./p") {
		t.Errorf("the output does not name the test and its failing run:\n%s", out)
	}
}
