package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// The edits below change the config file as text, so that a human's
// comments, order and formatting survive. Each edit is checked by loading
// the result before the file is replaced.

var (
	headerRe   = regexp.MustCompile(`^\s*\[`)
	projectRe  = regexp.MustCompile(`^\s*\[\[project\]\]\s*(#.*)?$`)
	reposRe    = regexp.MustCompile(`^\s*\[\[project\.repos\]\]\s*(#.*)?$`)
	subtableRe = regexp.MustCompile(`^\s*\[\[?project\.`)
	nameRe     = regexp.MustCompile(`^\s*name\s*=\s*"([^"]*)"`)
	repoKeyRe  = regexp.MustCompile(`^\s*repo\s*=\s*"([^"]*)"`)
)

// AppendProject adds a single-repository project to the config file at path,
// creating the file if needed.
func AppendProject(path, name, repo string) error {
	text, err := readText(path)
	if err != nil {
		return err
	}
	block := fmt.Sprintf("\n[[project]]\nname = %s\nrepo = %s\n", strconv.Quote(name), strconv.Quote(repo))
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	return writeChecked(path, text+block)
}

// AddRepo adds a repository to a project. A single-repository project is
// rewritten to the [[project.repos]] form with its repository as the primary.
func AddRepo(path, project string, r ProjectRepo) error {
	text, err := readText(path)
	if err != nil {
		return err
	}
	lines := strings.Split(text, "\n")
	start, end, err := projectBlock(lines, project)
	if err != nil {
		return err
	}
	var add []string
	for i := start + 1; i < end; i++ {
		if subtableRe.MatchString(lines[i]) {
			break
		}
		if m := repoKeyRe.FindStringSubmatch(lines[i]); m != nil {
			// repo = "x" becomes the primary entry of the list.
			lines = append(lines[:i], lines[i+1:]...)
			end--
			add = append(add, "", "[[project.repos]]", "repo = "+strconv.Quote(m[1]), "primary = true")
			break
		}
	}
	add = append(add, "", "[[project.repos]]", "repo = "+strconv.Quote(r.Repo))
	if r.DoneWhen != "" {
		add = append(add, "done_when = "+strconv.Quote(r.DoneWhen))
	}
	if r.Link != "" {
		add = append(add, "link = "+strconv.Quote(r.Link))
	}
	for end > start && strings.TrimSpace(lines[end-1]) == "" {
		end-- // insert before the blank lines that end the block
	}
	lines = append(lines[:end], append(add, lines[end:]...)...)
	return writeChecked(path, strings.Join(lines, "\n"))
}

// RemoveRepo removes a [[project.repos]] entry of a project.
func RemoveRepo(path, project, repo string) error {
	text, err := readText(path)
	if err != nil {
		return err
	}
	lines := strings.Split(text, "\n")
	start, end, err := projectBlock(lines, project)
	if err != nil {
		return err
	}
	for i := start + 1; i < end; i++ {
		if !reposRe.MatchString(lines[i]) {
			continue
		}
		j := i + 1
		for j < end && !headerRe.MatchString(lines[j]) {
			j++
		}
		for k := i + 1; k < j; k++ {
			if m := repoKeyRe.FindStringSubmatch(lines[k]); m != nil && m[1] == repo {
				from := i
				if from > 0 && strings.TrimSpace(lines[from-1]) == "" {
					from--
				}
				lines = append(lines[:from], lines[j:]...)
				return writeChecked(path, strings.Join(lines, "\n"))
			}
		}
	}
	return fmt.Errorf("project %s has no repository %s", project, repo)
}

// projectBlock finds the lines of the project named name: from its
// [[project]] header to the next table that is not one of its sub-tables.
func projectBlock(lines []string, name string) (int, int, error) {
	for i, line := range lines {
		if !projectRe.MatchString(line) {
			continue
		}
		end := i + 1
		found := false
		for end < len(lines) && !(headerRe.MatchString(lines[end]) && !subtableRe.MatchString(lines[end])) {
			if m := nameRe.FindStringSubmatch(lines[end]); m != nil && m[1] == name && !found {
				found = true
			}
			end++
		}
		if found {
			return i, end, nil
		}
	}
	return 0, 0, fmt.Errorf("the config has no project %q", name)
}

func readText(path string) (string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	return string(data), err
}

// writeChecked loads text as a config and, if it is valid, replaces the file.
func writeChecked(path, text string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.toml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(text); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if _, err := Load(tmp.Name()); err != nil {
		return fmt.Errorf("the edited config would be invalid: %w", err)
	}
	mode := os.FileMode(0o600)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
