package main_test

import (
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

func TestDoctorWarnsOfAReviewThatDoesNotWaitForVerify(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	git(t, repo, "init", "-q", "-b", "main")
	if err := os.MkdirAll(filepath.Join(repo, ".istari"), 0o755); err != nil {
		t.Fatal(err)
	}
	early := "[[stage]]\nname = \"implement\"\nrole = \"implementer\"\n\n[[stage]]\nname = \"review\"\nrole = \"reviewer\"\nneeds = [\"implement\"]\n"
	if err := os.WriteFile(filepath.Join(repo, ".istari", "pipeline.toml"), []byte(early), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _, _ := istariEnv(t, repo, writeDaemonConfig(t, newDataDir(t)), []string{"PATH=/usr/bin:/bin"}, "doctor")
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

func TestAHumanApprovesThePlanGate(t *testing.T) {
	t.Parallel()
	d := startDaemon(t, newDataDir(t))
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
