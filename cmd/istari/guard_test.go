package main_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestTheGitGuardHookDeniesWithExitTwoAndTellsTheAgentWhy(t *testing.T) {
	t.Parallel()
	wt := t.TempDir()
	hook := func(input string, env ...string) (string, int) {
		t.Helper()
		cmd := exec.Command(binary, "hook", "git-guard", "--harness", "claude-code")
		cmd.Env = append(os.Environ(), env...)
		cmd.Stdin = strings.NewReader(input)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		err := cmd.Run()
		code := 0
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return stderr.String(), code
	}
	call := func(command string) string {
		return `{"hook_event_name":"PreToolUse","tool_name":"Bash","cwd":"` + wt + `","tool_input":{"command":"` + command + `"}}`
	}
	if out, code := hook(call("git commit --amend --no-edit"), "ISTARI_WORKTREE="+wt); code != 0 {
		t.Fatalf("amend in the own worktree: exit %d %q", code, out)
	}
	if out, code := hook(call("git push origin HEAD"), "ISTARI_WORKTREE="+wt); code != 2 || !strings.Contains(out, "istari checkpoint") {
		t.Fatalf("git push: exit %d %q, want 2 pointing to istari checkpoint", code, out)
	}
	if out, code := hook(`{"tool_name":"Read","tool_input":{"file_path":"/etc/hosts"}}`, "ISTARI_WORKTREE="+wt); code != 0 {
		t.Fatalf("a tool that is not the shell: exit %d %q", code, out)
	}
	if _, code := hook("not json", "ISTARI_WORKTREE="+wt); code != 2 {
		t.Fatalf("unreadable input: exit %d, want 2", code)
	}
	if _, code := hook(call("git status"), "ISTARI_WORKTREE="); code != 2 {
		t.Fatalf("git without a known worktree: exit %d, want 2", code)
	}
}

func TestTheGitGuardHookKeepsFileToolsOutOfGitDirectories(t *testing.T) {
	t.Parallel()
	wt := t.TempDir()
	for path, want := range map[string]int{wt + "/src/a.go": 0, wt + "/.git": 2, "/data/repos/acme/web.git/config": 2} {
		cmd := exec.Command(binary, "hook", "git-guard", "--harness", "claude-code")
		cmd.Env = append(os.Environ(), "ISTARI_WORKTREE="+wt)
		cmd.Stdin = strings.NewReader(`{"tool_name":"Write","cwd":"` + wt + `","tool_input":{"file_path":"` + path + `","content":"x"}}`)
		code := 0
		if err := cmd.Run(); err != nil {
			code = err.(*exec.ExitError).ExitCode()
		}
		if code != want {
			t.Errorf("writing %s: exit %d, want %d", path, code, want)
		}
	}
}

func TestARunWithAResultFileMayWriteThatFileAndNoOther(t *testing.T) {
	t.Parallel()
	wt := t.TempDir()
	result := t.TempDir() + "/runs/r_1/result.yaml"
	for path, want := range map[string]int{
		result:                   0,
		wt + "/src/a.go":         2,
		wt + "/plan.yaml":        2,
		result + "/../plan.yaml": 2,
		"/tmp/notes.txt":         2,
	} {
		cmd := exec.Command(binary, "hook", "git-guard", "--harness", "claude-code")
		cmd.Env = append(os.Environ(), "ISTARI_WORKTREE="+wt, "ISTARI_RESULT="+result)
		cmd.Stdin = strings.NewReader(`{"tool_name":"Write","cwd":"` + wt + `","tool_input":{"file_path":"` + path + `","content":"x"}}`)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		code := 0
		if err := cmd.Run(); err != nil {
			code = err.(*exec.ExitError).ExitCode()
		}
		if code != want || want == 2 && !strings.Contains(stderr.String(), "ISTARI_RESULT") {
			t.Errorf("writing %s with a result file: exit %d %q, want %d and a denial naming ISTARI_RESULT", path, code, stderr.String(), want)
		}
	}
}
