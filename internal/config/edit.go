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
	"syscall"
)

// The edits below change the config file as text, so that a human's
// comments, order and formatting survive. Each edit is checked by loading
// the result before the file is replaced.

var (
	// headerRe matches a table header line, not an array continuation.
	headerRe   = regexp.MustCompile(`^\s*\[\[?\s*[A-Za-z0-9_."'-]+(\s*\.\s*[A-Za-z0-9_."'-]+)*\s*\]\]?\s*(#.*)?$`)
	projectRe  = regexp.MustCompile(`^\s*\[\[\s*project\s*\]\]\s*(#.*)?$`)
	reposRe    = regexp.MustCompile(`^\s*\[\[\s*project\s*\.\s*repos\s*\]\]\s*(#.*)?$`)
	subtableRe = regexp.MustCompile(`^\s*\[\[?\s*project\s*\.`)
	nameRe     = regexp.MustCompile(`^\s*name\s*=\s*(?:"([^"]*)"|'([^']*)')`)
	repoKeyRe  = regexp.MustCompile(`^\s*repo\s*=\s*(?:"([^"]*)"|'([^']*)')`)
)

// value returns the string a nameRe or repoKeyRe match captured, quoted
// either way.
func value(m []string) string { return m[1] + m[2] }

// document is a config file as lines, with its line ending.
type document struct {
	lines []string
	eol   string
}

func parse(text string) document {
	eol := "\n"
	if strings.Contains(text, "\r\n") {
		eol = "\r\n"
	}
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	return document{lines: lines, eol: eol}
}

func (d document) String() string { return strings.Join(d.lines, d.eol) }

// AppendProject adds a single-repository project to the config file at path,
// creating the file if needed.
func AppendProject(path, name, repo string) error {
	return update(path, func(d *document) error {
		if n := len(d.lines); n > 0 && d.lines[n-1] != "" {
			d.lines = append(d.lines, "")
		}
		d.lines = append(d.lines[:len(d.lines)-1], "", "[[project]]", "name = "+strconv.Quote(name), "repo = "+strconv.Quote(repo), "")
		return nil
	})
}

// AddRepo adds a repository to a project. A single-repository project is
// rewritten to the [[project.repos]] form with its repository as the primary.
func AddRepo(path, project string, r ProjectRepo) error {
	return update(path, func(d *document) error {
		lines := d.lines
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
				add = append(add, "", "[[project.repos]]", "repo = "+strconv.Quote(value(m)), "primary = true")
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
		// Insert before the blank lines and comments that end the block:
		// they belong to whatever follows.
		for end > start+1 && (strings.TrimSpace(lines[end-1]) == "" || strings.HasPrefix(strings.TrimSpace(lines[end-1]), "#")) {
			end--
		}
		d.lines = append(lines[:end], append(add, lines[end:]...)...)
		return nil
	})
}

// RemoveRepo removes a [[project.repos]] entry of a project; the repository
// name is matched ignoring case, as GitHub does.
func RemoveRepo(path, project, repo string) error {
	return update(path, func(d *document) error {
		lines := d.lines
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
				if m := repoKeyRe.FindStringSubmatch(lines[k]); m != nil && strings.EqualFold(value(m), repo) {
					from := i
					if from > 0 && strings.TrimSpace(lines[from-1]) == "" {
						from--
					}
					d.lines = append(lines[:from], lines[j:]...)
					return nil
				}
			}
		}
		return fmt.Errorf("project %s has no repository %s", project, repo)
	})
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
			if m := nameRe.FindStringSubmatch(lines[end]); m != nil && value(m) == name {
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

// update edits the config file at path under an exclusive lock, so that two
// edits at once do not lose one; a symlinked config is edited where it
// points. The result is loaded before it replaces the file.
func update(path string, change func(*document) error) error {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	d := parse(string(data))
	if err := change(&d); err != nil {
		return err
	}
	return writeChecked(path, d.String())
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
