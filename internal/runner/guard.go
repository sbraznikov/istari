package runner

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

// GitGuard decides whether an agent's shell command may run (spec section
// 7.7, prohibitions): in its own worktree the agent uses git freely, and only
// what reaches beyond the ticket is denied: another worktree or the main
// checkout, the remote's address and credentials, a push around istari,
// istari's own hook, and the variables that keep the user's logins away from
// the agent. worktree is the run's worktree, cwd where the command starts.
// The harness calls it through a hook before every shell command, so it
// holds whatever the role instruction says. It is the first barrier: the
// agent's environment has no credentials either.
func GitGuard(command, worktree, cwd string) error {
	g := guard{worktree: real(worktree)}
	return g.check(command, real(cwd), 0)
}

// FileGuard decides whether a file tool may write path: never into a git
// directory, where the remote, the credentials and the hooks are configured.
// result is the run's result file, ISTARI_RESULT, or empty: a run that has
// one writes that file and no other (spec section 7.3), whatever a settings
// file in the repository allows.
func FileGuard(path, result string) error {
	if result != "" {
		if filepath.Clean(path) == filepath.Clean(result) || real(path) == real(result) {
			return nil
		}
		return fmt.Errorf("%s is not your result file: you may write only $ISTARI_RESULT, %s", path, result)
	}
	for _, part := range strings.Split(filepath.ToSlash(filepath.Clean(path)), "/") {
		if part == ".git" || strings.HasSuffix(part, ".git") {
			return fmt.Errorf("%s is inside a git directory, which only istari changes", path)
		}
	}
	return nil
}

// real is path with its symlinks resolved, when it exists.
func real(path string) string {
	if path == "" {
		return ""
	}
	if r, err := filepath.EvalSymlinks(path); err == nil {
		return r
	}
	return filepath.Clean(path)
}

type guard struct{ worktree string }

func (g guard) inside(dir string) bool {
	if g.worktree == "" {
		return false
	}
	dir = filepath.Clean(dir)
	return dir == g.worktree || strings.HasPrefix(dir, g.worktree+string(filepath.Separator))
}

// protected are the variables that keep the user's settings and logins away
// from the agent: none may be set, exported or unset.
func protected(name string) bool {
	for _, p := range []string{"GIT_CONFIG", "GIT_SSH", "GIT_ASKPASS", "SSH_ASKPASS", "GIT_TERMINAL_PROMPT", "GH_", "GITHUB_", "XDG_CONFIG_HOME"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return name == "HOME"
}

func assignment(word string) (name, value string, ok bool) {
	name, value, ok = strings.Cut(word, "=")
	return name, value, ok && name != "" && !strings.ContainsAny(name, "/-.$")
}

func (g guard) check(command, cwd string, depth int) error {
	if depth > 4 {
		return fmt.Errorf("the command nests shells too deeply to check")
	}
	dir := cwd
	env := map[string]string{} // exported for the commands after
	for _, words := range simpleCommands(command) {
		own := map[string]string{}
		for k, v := range env {
			own[k] = v
		}
		// Assignments before the command.
		for len(words) > 0 {
			name, value, ok := assignment(words[0])
			if !ok {
				break
			}
			if protected(name) {
				return fmt.Errorf("%s keeps the user's logins away from you; it cannot be changed", name)
			}
			own[name] = value
			words = words[1:]
		}
		if len(words) == 0 {
			continue
		}
		args := words[1:]
		switch name := filepath.Base(words[0]); name {
		case "export", "declare", "typeset", "readonly", "local", "unset":
			for _, a := range args {
				k, v, isAssign := assignment(a)
				if !isAssign {
					k = a
				}
				if protected(k) {
					return fmt.Errorf("%s keeps the user's logins away from you; it cannot be changed", k)
				}
				if isAssign && name != "unset" {
					env[k] = v
				}
			}
			continue
		case "env":
			for i := 0; i < len(args); i++ {
				switch a := args[i]; a {
				case "-i", "--ignore-environment", "-":
					return fmt.Errorf("env -i drops the variables that keep the user's logins away from you")
				case "-u", "--unset":
					if i+1 < len(args) && protected(args[i+1]) {
						return fmt.Errorf("%s keeps the user's logins away from you; it cannot be changed", args[i+1])
					}
					i++
				default:
					if k, _, ok := assignment(a); ok && protected(k) {
						return fmt.Errorf("%s keeps the user's logins away from you; it cannot be changed", k)
					}
				}
			}
		case "cd", "pushd":
			target := "~"
			for _, a := range args {
				if a != "--" && !strings.HasPrefix(a, "-") || a == "-" {
					target = a
					break
				}
			}
			dir = g.resolve(dir, target)
			continue
		case "sh", "bash", "zsh", "dash", "ksh":
			if script, ok := shellScript(args); ok {
				if err := g.check(script, dir, depth+1); err != nil {
					return err
				}
				continue
			}
		case "eval":
			if err := g.check(strings.Join(args, " "), dir, depth+1); err != nil {
				return err
			}
			continue
		}
		// git anywhere in the command: behind timeout, sudo, xargs, nice…
		if i := slices.IndexFunc(words, func(w string) bool { return filepath.Base(w) == "git" }); i >= 0 {
			if slices.Contains(words[:i], "xargs") {
				return fmt.Errorf("git behind xargs gets arguments that cannot be checked: write the git command out")
			}
			if err := g.git(words[i+1:], dir, own, depth); err != nil {
				return err
			}
		}
	}
	return nil
}

// shellScript returns the script of sh -c and its variants (-lc, -ec, -c --).
func shellScript(args []string) (string, bool) {
	for i, a := range args {
		if a == "--" || !strings.HasPrefix(a, "-") {
			return "", false
		}
		if strings.Contains(a[1:], "c") && !strings.HasPrefix(a, "--") {
			rest := args[i+1:]
			if len(rest) > 0 && rest[0] == "--" {
				rest = rest[1:]
			}
			if len(rest) > 0 {
				return rest[0], true
			}
			return "", false
		}
	}
	return "", false
}

// git checks one git invocation: its global options, where it works and what
// it does.
func (g guard) git(args []string, dir string, env map[string]string, depth int) error {
	outside := func(what string) error {
		return fmt.Errorf("%s is outside your worktree %s: git works only on your ticket's own worktree", what, g.worktree)
	}
	for _, k := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE"} {
		if v, ok := env[k]; ok && !g.inside(g.resolve(dir, v)) {
			return outside(k + "=" + v)
		}
	}
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		opt, val, hasVal := strings.Cut(args[0], "=")
		takes := opt == "-C" || opt == "-c" || opt == "--git-dir" || opt == "--work-tree" || opt == "--config-env"
		if takes && !hasVal {
			if len(args) < 2 {
				return fmt.Errorf("git %s needs a value", opt)
			}
			val, args = args[1], args[1:]
		}
		args = args[1:]
		switch opt {
		case "-C":
			dir = g.resolve(dir, val)
		case "--git-dir", "--work-tree":
			if !g.inside(g.resolve(dir, val)) {
				return outside(opt + " " + val)
			}
		case "-c", "--config-env":
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
	case sub == "push" || sub == "send-pack" || sub == "http-push":
		return fmt.Errorf("git %s is istari's: run `istari checkpoint`, which commits, runs the checks and pushes your branch", sub)
	case sub == "remote" && len(rest) > 0 && !strings.HasPrefix(rest[0], "-") && !slices.Contains([]string{"show", "get-url", "prune", "update"}, rest[0]):
		return fmt.Errorf("git remote %s changes the remote repository, which only istari does", rest[0])
	case sub == "worktree" && len(rest) > 0 && rest[0] != "list":
		return fmt.Errorf("git worktree %s reaches other worktrees: work in your own, %s", rest[0], g.worktree)
	case strings.HasPrefix(sub, "credential"):
		return fmt.Errorf("git %s touches credentials, which only istari has", sub)
	case sub == "config":
		return configCommand(rest)
	case sub == "rebase":
		for i, a := range rest {
			if v, ok := strings.CutPrefix(a, "--exec="); ok {
				if err := g.check(v, dir, depth+1); err != nil {
					return err
				}
			} else if (a == "--exec" || a == "-x") && i+1 < len(rest) {
				if err := g.check(rest[i+1], dir, depth+1); err != nil {
					return err
				}
			}
		}
	case sub == "submodule" && len(rest) > 0 && rest[0] == "foreach", sub == "bisect" && len(rest) > 0 && rest[0] == "run":
		return g.check(strings.Join(rest[1:], " "), dir, depth+1)
	}
	return nil
}

// configCommand checks git config: reading is free, writing a setting that
// reaches beyond the worktree is not.
func configCommand(args []string) error {
	var words []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case slices.Contains([]string{"--get", "--get-all", "--get-regexp", "--get-urlmatch", "-l", "--list"}, a):
			return nil
		case a == "-f" || a == "--file" || a == "--blob" || a == "--type" || a == "--default":
			i++ // an option with a value
		case !strings.HasPrefix(a, "-"):
			words = append(words, a)
		}
	}
	if len(words) == 0 {
		return nil
	}
	switch words[0] {
	case "get", "list":
		return nil
	case "set", "unset", "rename-section", "remove-section", "edit":
		if len(words) < 2 {
			return nil
		}
		return configKey(words[1])
	}
	return configKey(words[0])
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
	case strings.HasPrefix(k, "alias."):
		return fmt.Errorf("git config %s: an alias could run what is denied; write the command out", key)
	case strings.HasPrefix(k, "include"), k == "core.sshcommand", k == "core.worktree", k == "core.gitproxy":
		return fmt.Errorf("git config %s reaches beyond your worktree", key)
	}
	return nil
}

// resolve returns where target leads from dir. $PWD and the worktree's top
// level are known; ~, cd - and any other variable lead outside.
func (g guard) resolve(dir, target string) string {
	switch target {
	case "$PWD", "${PWD}", "$(pwd)", "`pwd`", ".":
		return dir
	case "$(git rev-parse --show-toplevel)", "`git rev-parse --show-toplevel`":
		return g.worktree
	}
	if target == "-" || strings.HasPrefix(target, "~") || strings.HasPrefix(target, "$") || strings.HasPrefix(target, "`") {
		return "/"
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
// is not a git command; a substitution inside double quotes is checked as
// well, since it runs.
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
