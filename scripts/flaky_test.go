package scripts_test

import (
	"errors"
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
func (r *flakyRepo) flaky(t *testing.T, files ...string) (string, int) {
	t.Helper()
	script, err := filepath.Abs("flaky.sh")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", append([]string{script}, files...)...)
	cmd.Dir = r.dir
	out, err := cmd.CombinedOutput()
	if exit := (*exec.ExitError)(nil); errors.As(err, &exit) {
		return string(out), exit.ExitCode()
	} else if err != nil {
		t.Fatal(err)
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

	out, code := r.flaky(t, "p/flaky_test.go")

	if code == 0 {
		t.Fatalf("flaky.sh exited 0 on a test that fails 1 time in 5:\n%s", out)
	}
	if !strings.Contains(out, "flaky: TestFlaky failed on run 5 of 20 in ./p") {
		t.Errorf("the output does not name the test and its failing run:\n%s", out)
	}
}

func TestFlakyShRunsOnlyTheTestsAChangedFileChanges(t *testing.T) {
	t.Parallel()
	r := newFlakyRepo(t)
	r.write("p/p_test.go", `package p

import "testing"

func TestAlwaysFails(t *testing.T) {
	t.Fatal("not changed, so not run")
}

func TestOK(t *testing.T) {
	if Answer() != 42 {
		t.Fatal("wrong answer")
	}
}
`)
	r.commit("base tests")
	r.write("p/p_test.go", `package p

import "testing"

func TestAlwaysFails(t *testing.T) {
	t.Fatal("not changed, so not run")
}

func TestOK(t *testing.T) {
	if got := Answer(); got != 42 {
		t.Fatalf("Answer() = %d, want 42", got)
	}
}
`)
	r.commit("TestOK changed")

	out, code := r.flaky(t, "p/p_test.go")

	if code != 0 {
		t.Fatalf("flaky.sh exited %d, though only TestOK changed:\n%s", code, out)
	}
	if !strings.Contains(out, "-run '^(TestOK)$' ./p") {
		t.Errorf("the output does not show TestOK run alone:\n%s", out)
	}
	if strings.Contains(out, "TestAlwaysFails") {
		t.Errorf("the unchanged TestAlwaysFails was run:\n%s", out)
	}
}

func TestFlakyShSaysSoWhenNoTestFunctionChanged(t *testing.T) {
	t.Parallel()
	r := newFlakyRepo(t)
	r.write("p/p_test.go", `package p

import "testing"

func want() int { return 42 }

func TestAlwaysFails(t *testing.T) {
	t.Fatal("not changed, so not run")
}
`)
	r.commit("base tests")
	r.write("p/p_test.go", `package p

import "testing"

// want is the answer the tests expect.
func want() int {
	return 42
}

func TestAlwaysFails(t *testing.T) {
	t.Fatal("not changed, so not run")
}
`)
	r.commit("a helper changed")

	for name, files := range map[string][]string{"a changed helper": {"p/p_test.go"}, "no files": nil} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out, code := r.flaky(t, files...)
			if code != 0 || strings.TrimSpace(out) != "flaky: no new or changed tests" {
				t.Errorf("flaky.sh exited %d with:\n%s\nwant 0 with %q", code, out, "flaky: no new or changed tests")
			}
		})
	}
}
