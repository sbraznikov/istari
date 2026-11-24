// Package skill holds istari's instructions for agents (spec section 13):
// the built-in role instructions and the agent skill.
package skill

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
)

//go:embed roles/*.md skill.md
var files embed.FS

// Skill returns the agent skill: how to work with istari (spec section 7.4).
func Skill() string {
	data, err := files.ReadFile("skill.md")
	if err != nil {
		panic(err) // embedded at build time
	}
	return string(data)
}

// ReadFile reads a file of the repository, e.g. from its main branch; it
// returns an error wrapping fs.ErrNotExist when there is none.
type ReadFile func(path string) ([]byte, error)

// Instruction returns the instruction of an agent role (spec section 7.2):
// the repository's .istari/roles/<role>.md as read, or the built-in one. Read
// it from the main branch: a ticket's branch must not rewrite the
// instructions of the roles that check it.
func Instruction(role string, read ReadFile) (string, error) {
	if read != nil {
		data, err := read(".istari/roles/" + role + ".md")
		if err == nil {
			return string(data), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
	}
	data, err := files.ReadFile("roles/" + role + ".md")
	if err != nil {
		return "", fmt.Errorf("no instruction for role %q", role)
	}
	return string(data), nil
}
