package runner

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// quoteHint is the fix for the most common mistake in a submitted result: a
// value with ": " in it that is not quoted.
const quoteHint = `a value with ": " in it must be quoted: quote the value, as in key: "a: b"`

// yamlError turns the decode error of a submitted result into a refusal that
// names the line of the result and, for an unquoted value with ": " in it,
// says how to fix it (S4.9).
//
// Such a value fails in one of two ways: as the value of a key, the scanner
// refuses it ("mapping values are not allowed"); yaml.v3 names the line but
// leaves it out when it is the first one, and only then is the line found in
// data; as a list item
// where a string is expected, it decodes as a map, and yaml.v3 names the line.
func yamlError(what string, data []byte, err error) error {
	switch msg := err.Error(); {
	case strings.Contains(msg, "mapping values are not allowed"):
		if line := yamlLine(msg); line > 0 {
			return fmt.Errorf("%s: line %d: %s", what, line, quoteHint)
		}
		if line := unquotedColonLine(data); line > 0 {
			return fmt.Errorf("%s: line %d: %s", what, line, quoteHint)
		}
		return fmt.Errorf("%s: %w; %s", what, err, quoteHint)
	case strings.Contains(msg, "cannot unmarshal !!map into string"):
		return fmt.Errorf("%s: %w; %s", what, err, quoteHint)
	default:
		return fmt.Errorf("%s: %w", what, err)
	}
}

// yamlLineRE finds the line yaml.v3 names in its error.
var yamlLineRE = regexp.MustCompile(`line (\d+):`)

// yamlLine returns the line yaml.v3 names in its error message, or 0 when it
// names none.
func yamlLine(msg string) int {
	m := yamlLineRE.FindStringSubmatch(msg)
	if m == nil {
		return 0
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0
	}
	return n
}

// unquotedColonLine returns the first line, counted from 1, whose value
// contains ": " without being quoted, or 0 when there is none.
func unquotedColonLine(data []byte) int {
	for i, l := range strings.Split(string(data), "\n") {
		l = strings.TrimPrefix(strings.TrimLeft(l, " "), "- ")
		_, value, ok := strings.Cut(l, ": ")
		value = strings.TrimSpace(value)
		if !ok || value == "" || strings.ContainsAny(value[:1], `'"|>{[`) {
			continue
		}
		if strings.Contains(value, ": ") {
			return i + 1
		}
	}
	return 0
}
