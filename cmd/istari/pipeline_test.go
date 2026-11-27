package main_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDoctorRejectsAPipelineWithAnUnknownRoleAtItsLine(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	git(t, repo, "init", "-q", "-b", "main")
	if err := os.MkdirAll(filepath.Join(repo, ".istari"), 0o755); err != nil {
		t.Fatal(err)
	}
	bad := "[[stage]]\nname = \"plan\"\nrole = \"planner\"\n\n[[stage]]\nname = \"magic\"\nrole = \"wizard\"\n"
	if err := os.WriteFile(filepath.Join(repo, ".istari", "pipeline.toml"), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	config := writeDaemonConfig(t, newDataDir(t))
	out, _, code := istariEnv(t, repo, config, []string{"PATH=/usr/bin:/bin"}, "doctor")
	if code != 1 || !strings.Contains(out, "line 5") || !strings.Contains(out, "wizard") {
		t.Fatalf("doctor with a bad pipeline: exit %d\n%s", code, out)
	}
	good := "[[stage]]\nname = \"plan\"\nrole = \"planner\"\n"
	if err := os.WriteFile(filepath.Join(repo, ".istari", "pipeline.toml"), []byte(good), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _, _ = istariEnv(t, repo, config, []string{"PATH=/usr/bin:/bin"}, "doctor")
	if !strings.Contains(out, "pipeline") || strings.Contains(out, "wizard") {
		t.Fatalf("doctor with a good pipeline:\n%s", out)
	}
}

// earlyReviewRepo returns a git repository whose pipeline's review stage
// (line 5) needs only implement, with the standards files in place so the
// pipeline row is doctor's only warning.
func earlyReviewRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	git(t, repo, "init", "-q", "-b", "main")
	early := "[[stage]]\nname = \"implement\"\nrole = \"implementer\"\n\n[[stage]]\nname = \"review\"\nrole = \"reviewer\"\nneeds = [\"implement\"]\n"
	writeFiles(t, repo, "CODING_STANDARDS.md", "CONTRIBUTING.md", "GLOSSARY.md", "adr/", ".istari/")
	if err := os.WriteFile(filepath.Join(repo, ".istari", "pipeline.toml"), []byte(early), 0o644); err != nil {
		t.Fatal(err)
	}
	return repo
}

func TestDoctorWarnsOfAReviewThatDoesNotWaitForVerify(t *testing.T) {
	t.Parallel()
	out, _ := doctorInRepo(t, earlyReviewRepo(t))
	var row string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "pipeline ") {
			row = line
		}
	}
	if !strings.Contains(row, "warn") || !strings.Contains(row, "line 5") || !strings.Contains(row, `"verify"`) {
		t.Fatalf("the pipeline row = %q, want a warning naming line 5 and the fix\n%s", row, out)
	}
}

func TestDoctorJSONShowsTheEarlyReviewAsAWarningThatIsNotOK(t *testing.T) {
	t.Parallel()
	out, code := doctorInRepo(t, earlyReviewRepo(t), "--json")
	if code != 0 {
		t.Fatalf("doctor with only a warning: exit %d, want 0\n%s", code, out)
	}
	var rows []struct {
		Name string `json:"name"`
		OK   bool   `json:"ok"`
		Warn bool   `json:"warn"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("doctor --json: %v\n%s", err, out)
	}
	for _, r := range rows {
		if strings.HasPrefix(r.Name, "pipeline ") {
			if r.OK || !r.Warn {
				t.Fatalf("the pipeline row has ok %v, warn %v; want ok false, warn true, as the standards row\n%s", r.OK, r.Warn, out)
			}
			return
		}
	}
	t.Fatalf("doctor --json shows no pipeline row\n%s", out)
}

func TestAHumanApprovesThePlanGate(t *testing.T) {
	t.Parallel()
	d := startDaemon(t, newDataDir(t)) //nolint:forbidigo // no project and no due timer: no loop pass decides the outcome
	d.call(t, "task.create", map[string]any{"actor": "system", "id": "gate_1", "ticket": "gh:acme/web#1", "project": "web", "role": "human"}, nil)
	var gates []struct{ ID string }
	d.mustCLI(t, &gates, "gate", "list")
	if len(gates) != 1 || gates[0].ID != "gate_1" {
		t.Fatalf("gate list = %+v", gates)
	}
	if _, stderr, code := d.cli(t, "gate", "approve", "gate_1", "--actor", "agent:planner-1"); code != 1 || !strings.Contains(stderr, "human_only") {
		t.Fatalf("an agent approving: exit %d %q", code, stderr)
	}
	d.mustCLI(t, nil, "gate", "approve", "gate_1", "--actor", "human:alex")
	var task struct{ State string }
	d.mustCLI(t, &task, "task", "show", "gate_1")
	var done []struct{ Actor string }
	d.mustCLI(t, &done, "tail", "--type", "task.completed")
	if task.State != "completed" || len(done) != 1 || done[0].Actor != "human:alex" {
		t.Fatalf("after approval the gate is %+v, completed by %+v", task, done)
	}
}
