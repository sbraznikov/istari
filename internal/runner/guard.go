package runner

import (
	"fmt"
	"path/filepath"
	"strings"
)

// GitGuard decides whether an agent's shell command may run (spec section
// 7.7, prohibitions): in its own worktree the agent uses git freely, and only
// what reaches beyond the ticket is denied: another worktree or the main
// checkout, the remote's address and credentials, a push around istari, and
// istari's own hook. worktree is the run's worktree, cwd where the command
// starts. The harness calls it through a hook before every shell command, so
// it holds whatever the role instruction says.
func GitGuard(command, worktree, cwd string) error {
	g := guard{worktree: filepath.Clean(worktree)}
	return g.check(command, cwd, 0)
}

type guard struct{ worktree string }

func (g guard) inside(dir string) bool {
	dir = filepath.Clean(dir)
	return dir == g.worktree || strings.HasPrefix(dir, g.worktree+string(filepath.Separator))
}

func (g guard) check(command, cwd string, depth int) error {
	if depth > 4 {
		return fmt.Errorf("the command nests shells too deeply to check")
	}
	dir := cwd
	for _, words := range simpleCommands(command) {
		// Variables, and the wrappers that run the next word as the command.
		env := map[string]string{}
		for len(words) > 0 {
			if k, v, ok := strings.Cut(words[0], "="); ok && k != "" && !strings.ContainsAny(k, "/-.") {
				env[k] = v
			} else if !wrapper(words[0]) {
				break
			}
			words = words[1:]
		}
		if len(words) == 0 {
			continue
		}
		switch name := filepath.Base(words[0]); {
		case name == "cd":
			target := "~"
			if len(words) > 1 {
				target = words[1]
			}
			dir = resolve(dir, target)
		case (name == "sh" || name == "bash" || name == "zsh" || name == "dash") && len(words) > 2 && words[1] == "-c":
			if err := g.check(words[2], dir, depth+1); err != nil {
				return err
			}
		case name == "git":
			if err := g.git(words[1:], dir, env); err != nil {
				return err
			}
		}
	}
	return nil
}

func wrapper(word string) bool {
	switch word {
	case "env", "command", "exec", "nohup", "time", "nice":
		return true
	}
	return false
}

// git checks one git invocation: its global options, where it works and what
// it does.
func (g guard) git(args []string, dir string, env map[string]string) error {
	outside := func(what string) error {
		return fmt.Errorf("%s is outside your worktree %s: git works only on your ticket's own worktree", what, g.worktree)
	}
	for _, k := range []string{"GIT_DIR", "GIT_WORK_TREE"} {
		if v, ok := env[k]; ok && !g.inside(resolve(dir, v)) {
			return outside(k + "=" + v)
		}
	}
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		opt, val, hasVal := strings.Cut(args[0], "=")
		takes := opt == "-C" || opt == "-c" || opt == "--git-dir" || opt == "--work-tree"
		if takes && !hasVal {
			if len(args) < 2 {
				return fmt.Errorf("git %s needs a value", opt)
			}
			val, args = args[1], args[1:]
		}
		args = args[1:]
		switch opt {
		case "-C":
			dir = resolve(dir, val)
		case "--git-dir", "--work-tree":
			if !g.inside(resolve(dir, val)) {
				return outside(opt + " " + val)
			}
		case "-c":
			if err := configKey(strings.SplitN(val, "=", 2)[0]); err != nil {
				return err
			}
		}
	}
	if !g.inside(dir) {
		return outside(dir)
	}
	if len(args) == 0 {
		return nil
	}
	sub, rest := args[0], args[1:]
	switch {
	case sub == "push":
		return fmt.Errorf("git push is istari's: run `istari checkpoint`, which commits, runs the checks and pushes your branch")
	case sub == "remote" && len(rest) > 0 && !strings.HasPrefix(rest[0], "-") && rest[0] != "show" && rest[0] != "get-url":
		return fmt.Errorf("git remote %s changes the remote repository, which only istari does", rest[0])
	case sub == "worktree" && len(rest) > 0 && rest[0] != "list":
		return fmt.Errorf("git worktree %s reaches other worktrees: work in your own, %s", rest[0], g.worktree)
	case strings.HasPrefix(sub, "credential"):
		return fmt.Errorf("git %s touches credentials, which only istari has", sub)
	case sub == "config":
		for _, a := range rest {
			if !strings.HasPrefix(a, "-") {
				return configKey(a) // the first word that is not an option is the key
			}
		}
	}
	return nil
}

// configKey refuses the settings that reach beyond the worktree.
func configKey(key string) error {
	k := strings.ToLower(key)
	switch {
	case strings.HasPrefix(k, "remote.") && (strings.HasSuffix(k, ".url") || strings.HasSuffix(k, ".pushurl")), strings.HasPrefix(k, "url."):
		return fmt.Errorf("git config %s changes the remote repository address, which only istari does", key)
	case strings.HasPrefix(k, "credential"):
		return fmt.Errorf("git config %s touches credentials, which only istari has", key)
	case k == "core.hookspath":
		return fmt.Errorf("git config %s would remove istari's hook, which adds the ticket trailer", key)
	}
	return nil
}

// resolve returns where target leads from dir; ~ and $HOME lead outside any
// worktree.
func resolve(dir, target string) string {
	if strings.HasPrefix(target, "~") || strings.HasPrefix(target, "$") {
		return "/home"
	}
	if filepath.IsAbs(target) {
		return filepath.Clean(target)
	}
	return filepath.Join(dir, target)
}

// simpleCommands splits a shell command line into its simple commands, as
// words with their quotes removed: at ; & | newlines, and at the edges of
// subshells and command substitutions, so a command inside $( ) or ( ) is
// checked too. A quoted string is one word, so text that only mentions git
// is not a git command.
func simpleCommands(line string) [][]string {
	var out, subs [][]string
	var words []string
	var word strings.Builder
	inWord := false
	endWord := func() {
		if inWord {
			words = append(words, word.String())
			word.Reset()
			inWord = false
		}
	}
	endCommand := func() {
		endWord()
		if len(words) > 0 {
			out = append(out, words)
			words = nil
		}
	}
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == '\\' && i+1 < len(line):
			i++
			word.WriteByte(line[i])
			inWord = true
		case c == '\'' || c == '"':
			end := strings.IndexByte(line[i+1:], c)
			if end < 0 {
				end = len(line) - i - 1
			}
			quoted := line[i+1 : i+1+end]
			word.WriteString(quoted)
			inWord = true
			i += end + 1
			// Inside double quotes a substitution still runs.
			if c == '"' && (strings.Contains(quoted, "$(") || strings.Contains(quoted, "`")) {
				subs = append(subs, simpleCommands(quoted)...)
			}
		case c == '`':
			endCommand() // a command substitution is a command of its own
		case c == '$' && i+1 < len(line) && line[i+1] == '(':
			endCommand()
			i++
		case strings.IndexByte(";&|()\n", c) >= 0:
			endCommand()
		case c == ' ' || c == '\t':
			endWord()
		default:
			word.WriteByte(c)
			inWord = true
		}
	}
	endCommand()
	return append(out, subs...)
}
