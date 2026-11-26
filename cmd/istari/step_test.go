package main_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sbraznikov/istari/internal/tickets/github/githubtest"
)

// startSteppedDaemon starts `istari daemon --stepped`: its scheduler,
// pipeline, ticket sync and PR watch run one pass per d.step (spec 3.1).
func startSteppedDaemon(t *testing.T, dataDir, config string, env ...string) *daemon {
	t.Helper()
	return startDaemonWithArgs(t, dataDir, config, []string{"--stepped"}, env...)
}

// step runs one pass of loop and returns once it has finished.
func (d *daemon) step(t *testing.T, loop string) {
	t.Helper()
	d.call(t, "step", map[string]string{"loop": loop}, nil)
}

// stepStatus posts a step and returns the status and body of the answer.
func (d *daemon) stepStatus(t *testing.T, loop string) (int, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"loop": loop})
	r, err := d.client.Post("http://istari/v1/step", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	data, _ := io.ReadAll(r.Body)
	return r.StatusCode, string(data)
}

// trackedProject writes a config with one project on the fake GitHub's
// repository and returns it with the daemon's environment.
func trackedProject(t *testing.T, gh *githubtest.Server, dataDir string) (config string, env []string) {
	t.Helper()
	config = filepath.Join(t.TempDir(), "config.toml")
	body := fmt.Sprintf("[daemon]\ndata_dir = %q\n\n[[project]]\nname = \"web\"\nrepo = %q\n", dataDir, githubtest.Repo)
	if err := os.WriteFile(config, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return config, []string{"PATH=" + fakeGH(t, gh.Token), "GITHUB_API_URL=" + gh.URL}
}

func TestASteppedDaemonSyncsOnlyWhenAStepAsks(t *testing.T) {
	t.Parallel()
	gh := githubtest.New(t)
	n := gh.Seed("Add rate limits", "## Context\n…", "bob")
	ref := fmt.Sprintf("gh:%s#%d", githubtest.Repo, n)
	dataDir := newDataDir(t)
	config, env := trackedProject(t, gh, dataDir)
	d := startSteppedDaemon(t, dataDir, config, env...)

	// An unstepped daemon syncs at once on start; this one must not.
	for until := time.Now().Add(time.Second); time.Now().Before(until); time.Sleep(100 * time.Millisecond) {
		if events := d.eventTypes(t, ref, "ticket"); len(events) != 0 {
			t.Fatalf("a stepped daemon synced on its own: %v", events)
		}
	}
	d.step(t, "sync")
	// The step answers after the pass: the ticket is there at once.
	var tk struct{ State string }
	d.mustCLI(t, &tk, "ticket", "show", ref)
	if tk.State != "new" {
		t.Fatalf("after the sync step the ticket is %q, want new", tk.State)
	}
}

func TestAStepOfAnUnknownLoopIsABadRequest(t *testing.T) {
	t.Parallel()
	dataDir := newDataDir(t)
	d := startSteppedDaemon(t, dataDir, writeDaemonConfig(t, dataDir))
	if status, body := d.stepStatus(t, "everything"); status != http.StatusBadRequest {
		t.Fatalf("step everything: %d %s, want 400", status, body)
	}
}

func TestADaemonNotSteppedHasNoStep(t *testing.T) {
	t.Parallel()
	d := startDaemon(t, newDataDir(t))
	if status, body := d.stepStatus(t, "sync"); status != http.StatusNotFound {
		t.Fatalf("step on a daemon without --stepped: %d %s, want 404", status, body)
	}
}
