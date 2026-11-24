package pipeline

import (
	"embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

//go:embed roles/*.md
var roles embed.FS

// Instruction returns the instruction of an agent role (spec section 7.2):
// the repository's .istari/roles/<role>.md under dir, or the built-in one.
func Instruction(role, dir string) (string, error) {
	if dir != "" {
		data, err := os.ReadFile(filepath.Join(dir, ".istari", "roles", role+".md"))
		if err == nil {
			return string(data), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	data, err := roles.ReadFile("roles/" + role + ".md")
	if err != nil {
		return "", fmt.Errorf("no instruction for role %q", role)
	}
	return string(data), nil
}

// BuiltInInstruction returns the built-in instruction of a role; istari init
// writes them into a repository as its starting point.
func BuiltInInstruction(role string) (string, error) { return Instruction(role, "") }
