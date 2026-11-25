package runner

import (
	"fmt"
	"strings"
)

// yamlError turns the decode error of a submitted result into a refusal that
// names the line of the result and, for the common mistake of an unquoted
// value with ": " in it, says how to fix it (S4.9). yaml.v3 leaves the line
// out when the mistake is on the first line, so the line is found in data.
func yamlError(what string, data []byte, err error) error {
	if !strings.Contains(err.Error(), "mapping values are not allowed") {
		return fmt.Errorf("%s: %w", what, err)
	}
	line := unquotedColonLine(data)
	if line == 0 {
		return fmt.Errorf("%s: %w; a value with \": \" in it must be quoted: quote the value", what, err)
	}
	return fmt.Errorf("%s: line %d: a value with \": \" in it must be quoted: quote the value, as in key: \"a: b\"", what, line)
}

// unquotedColonLine returns the first line, counted from 1, whose value
// contains ": " without being quoted, or 0 when there is none.
func unquotedColonLine(data []byte) int {
	for i, l := range strings.Split(string(data), "\n") {
		l = strings.TrimLeft(l, " ")
		l = strings.TrimPrefix(l, "- ")
		_, value, ok := strings.Cut(l, ": ")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		if value == "" || strings.ContainsAny(value[:1], "'\"|>{[") {
			continue
		}
		if strings.Contains(value, ": ") {
			return i + 1
		}
	}
	return 0
}
