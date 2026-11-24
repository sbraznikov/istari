package standard

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/sbraznikov/istari/internal/tickets/journal"
)

// body is a ticket body split for checking.
type body struct {
	sections map[string]string // heading → text, code included (for emptiness)
	order    []string          // headings in order
	prose    string            // all text outside code fences and the metadata table
	meta     map[string]string // metadata table: column (lower case) → value
}

var headingRe = regexp.MustCompile(`^##\s+(.+?)\s*#*\s*$`)

// parse splits a markdown body into sections, the prose outside code fences,
// and the metadata table: the last table outside fences whose header names
// Type and Size, with a separator row. The table and a --- rule right before
// it are not part of any section; any other --- is content.
func parse(src string) body {
	// The work state block is istari's summary, not the ticket's text: it
	// changes with every journal entry (spec section 4.6).
	src = journal.StripWorkState(strings.ReplaceAll(src, "\r\n", "\n"))
	lines := strings.Split(src, "\n")
	inFence := make([]bool, len(lines))
	fenced := false
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			inFence[i] = true
			fenced = !fenced
			continue
		}
		inFence[i] = fenced
	}
	b := body{sections: map[string]string{}, meta: map[string]string{}}
	skip := make([]bool, len(lines))
	for i := len(lines) - 3; i >= 0; i-- {
		if inFence[i] {
			continue
		}
		header := lowerCells(lines[i])
		if !slices.Contains(header, "type") || !slices.Contains(header, "size") || !isSeparator(lines[i+1]) {
			continue
		}
		values := cells(lines[i+2])
		for j, h := range header {
			if j < len(values) {
				b.meta[h] = strings.Trim(values[j], "` ")
			}
		}
		for k := i; k < i+3; k++ {
			skip[k] = true
		}
		for k := i - 1; k >= 0 && !inFence[k]; k-- { // the rule and blank lines before it
			t := strings.TrimSpace(lines[k])
			if t != "" && t != "---" {
				break
			}
			skip[k] = true
			if t == "---" {
				break
			}
		}
		break
	}
	current := ""
	var text, prose strings.Builder
	flush := func() {
		if current != "" {
			b.sections[current] = text.String()
		}
		text.Reset()
	}
	for i, l := range lines {
		if skip[i] {
			continue
		}
		if !inFence[i] {
			if m := headingRe.FindStringSubmatch(l); m != nil {
				flush()
				current = m[1]
				if _, seen := b.sections[current]; !seen {
					b.order = append(b.order, current)
				}
				continue
			}
			prose.WriteString(l + "\n")
		}
		if current != "" {
			text.WriteString(l + "\n")
		}
	}
	flush()
	b.prose = prose.String()
	return b
}

func cells(line string) []string {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "|") {
		return nil
	}
	parts := strings.Split(strings.Trim(line, "|"), "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

func lowerCells(line string) []string {
	c := cells(line)
	for i := range c {
		c[i] = strings.ToLower(c[i])
	}
	return c
}

var separatorRe = regexp.MustCompile(`^\|?(\s*:?-{3,}:?\s*\|)+\s*:?-{0,}:?\s*\|?$`)

func isSeparator(line string) bool { return separatorRe.MatchString(strings.TrimSpace(line)) }

// knownExt are the file extensions a backticked token must have to count as
// a file: otherwise `os.Stat` or `standard.Default` would.
var knownExt = []string{
	"go", "mod", "sum", "md", "txt", "toml", "yaml", "yml", "json", "xml", "ini", "cfg", "conf", "env",
	"ts", "tsx", "js", "jsx", "mjs", "cjs", "py", "rb", "rs", "java", "kt", "swift", "c", "h", "cc", "cpp", "hpp",
	"sh", "bash", "zsh", "sql", "proto", "graphql", "html", "css", "scss", "svg", "png", "jpg", "gif",
	"tmpl", "lock", "csv", "tf", "hcl", "dockerfile",
}

// knownNames are files without an extension.
var knownNames = []string{"README", "Makefile", "Dockerfile", "LICENSE", "CHANGELOG", "Procfile", "Gemfile", "Rakefile", "Justfile", "CODEOWNERS"}

var (
	codeRe     = regexp.MustCompile("`([^`\n]+)`(\\s*\\(new\\))?")
	segmentRe  = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	lineSuffix = regexp.MustCompile(`:\d+(:\d+)?$`)
)

// paths returns the backticked file paths in text that are not marked (new).
// A token is a path when it is a known file name, a dotfile, has a known
// extension, or has a slash and starts with a directory that exists in root.
// Hostnames, import paths, ranges, globs, refs and code are not paths.
func paths(text, root string) []string {
	var out []string
	for _, m := range codeRe.FindAllStringSubmatch(text, -1) {
		p := lineSuffix.ReplaceAllString(m[1], "") // go.mod:12
		if m[2] != "" || !isPath(p, root) || slices.Contains(out, p) {
			continue
		}
		out = append(out, p)
	}
	return out
}

func isPath(p, root string) bool {
	if strings.ContainsAny(p, " ~*#@:") || strings.Contains(p, "..") || strings.HasPrefix(p, "-") {
		return false
	}
	segs := strings.Split(strings.TrimSuffix(p, "/"), "/")
	for _, s := range segs {
		if !segmentRe.MatchString(s) {
			return false
		}
	}
	last := segs[len(segs)-1]
	file := slices.Contains(knownNames, last) ||
		strings.HasPrefix(last, ".") && len(last) > 1 ||
		slices.Contains(knownExt, strings.ToLower(strings.TrimPrefix(filepath.Ext(last), ".")))
	if len(segs) == 1 {
		return file
	}
	first := segs[0]
	if strings.Contains(first, ".") && !strings.HasPrefix(first, ".") {
		return false // api.github.com/…, github.com/acme/… : a host
	}
	if file {
		return true
	}
	// A directory path: only when its first directory exists, so that a
	// repository name like acme/other is not taken for one.
	info, err := os.Stat(filepath.Join(root, first))
	return err == nil && info.IsDir()
}

var refRe = regexp.MustCompile(`(?:^|[^A-Za-z0-9_/#:.-])((?:gh:)?(?:([A-Za-z0-9-]+)/)?([A-Za-z0-9._-]+)?#(\d+))`)

// ref is a dependency as written, resolved against repo.
type ref struct {
	written string
	full    string // gh:owner/repo#n, or "" if it cannot be resolved
}

// needsRefs returns the refs on the "- <word>:" lines of prose. #n is this
// repository, repo#n a sibling of the same owner, owner/repo#n any.
func needsRefs(prose, word, repo string) []ref {
	prefix := "- " + word + ":"
	owner, _, _ := strings.Cut(repo, "/")
	var out []ref
	for _, line := range strings.Split(prose, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), prefix)
		if !ok {
			continue
		}
		for _, m := range refRe.FindAllStringSubmatch(rest, -1) {
			written, o, r, n := m[1], m[2], m[3], m[4]
			full := ""
			switch {
			case strings.HasPrefix(written, "gh:"):
				full = written
			case o != "" && r != "":
				full = "gh:" + o + "/" + r + "#" + n
			case r != "" && owner != "":
				full = "gh:" + owner + "/" + r + "#" + n
			case r == "" && repo != "":
				full = "gh:" + repo + "#" + n
			}
			out = append(out, ref{written: written, full: full})
		}
	}
	return out
}
