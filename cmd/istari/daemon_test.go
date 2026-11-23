package main_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

type daemon struct {
	dataDir string
	socket  string
	config  string
	cmd     *exec.Cmd
	stderr  *bytes.Buffer
	client  *http.Client
}

// newDataDir returns a short directory under /tmp: unix socket paths are
// limited to about 104 bytes on macOS, and t.TempDir() is longer.
func newDataDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ist")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func writeDaemonConfig(t *testing.T, dataDir string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	body := fmt.Sprintf("[daemon]\ndata_dir = %q\n", dataDir)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func socketClient(socket string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}
}

// startDaemon starts `istari daemon` on dataDir and waits until it answers.
func startDaemon(t *testing.T, dataDir string) *daemon {
	t.Helper()
	d := &daemon{dataDir: dataDir, socket: filepath.Join(dataDir, "istari.sock"), config: writeDaemonConfig(t, dataDir), stderr: &bytes.Buffer{}}
	d.cmd = exec.Command(binary, "daemon", "--config", d.config)
	d.cmd.Stderr = d.stderr
	if err := d.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if d.cmd.ProcessState == nil {
			d.cmd.Process.Kill()
			d.cmd.Wait()
		}
	})
	d.client = socketClient(d.socket)
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := d.client.Get("http://istari/v1/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return d
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon did not answer: %v\nstderr: %s", err, d.stderr)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// call posts a JSON request to an operation and decodes the JSON answer.
func (d *daemon) call(t *testing.T, op string, req, resp any) {
	t.Helper()
	body, _ := json.Marshal(req)
	r, err := d.client.Post("http://istari/v1/"+op, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("%s: %v", op, err)
	}
	defer r.Body.Close()
	data, _ := io.ReadAll(r.Body)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("%s: %s %s", op, r.Status, data)
	}
	if resp != nil {
		if err := json.Unmarshal(data, resp); err != nil {
			t.Fatalf("%s: %v in %s", op, err, data)
		}
	}
}

func TestDaemonSocketIsPrivate(t *testing.T) {
	d := startDaemon(t, newDataDir(t))
	info, err := os.Stat(d.socket)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("socket mode = %o, want 600", mode)
	}
}

func TestASecondDaemonOnTheSameDataDirRefusesToStart(t *testing.T) {
	d := startDaemon(t, newDataDir(t))
	_, stderr, code := run(t, "daemon", "--config", d.config)
	if code != 1 {
		t.Fatalf("second daemon exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "already running") {
		t.Fatalf("stderr = %q, want it to say a daemon is already running", stderr)
	}
	// The first daemon still answers.
	d.call(t, "ticket.list", map[string]any{}, nil)
}

type sseEvent struct {
	ID, Type string
	Data     json.RawMessage
}

// readSSE reads n events from an SSE stream opened at url.
func readSSE(t *testing.T, client *http.Client, url, lastEventID string, n int) []sseEvent {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q", ct)
	}
	var events []sseEvent
	var cur sseEvent
	sc := bufio.NewScanner(resp.Body)
	for len(events) < n && sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "id: "):
			cur.ID = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "event: "):
			cur.Type = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			cur.Data = json.RawMessage(strings.TrimPrefix(line, "data: "))
		case line == "" && cur.ID != "":
			events = append(events, cur)
			cur = sseEvent{}
		}
	}
	if len(events) < n {
		t.Fatalf("stream ended after %d of %d events: %v", len(events), n, sc.Err())
	}
	return events
}

func createTicket(t *testing.T, d *daemon, ref string) {
	t.Helper()
	d.call(t, "ticket.create", map[string]any{"actor": "human:alex", "ref": ref, "project": "web"}, nil)
}

func TestSSEDeliversEveryLaterEventInOrderAcrossAReconnect(t *testing.T) {
	d := startDaemon(t, newDataDir(t))
	createTicket(t, d, "gh:acme/web#1")
	first := readSSE(t, d.client, "http://istari/v1/events/stream", "", 1)
	cursor := first[0].ID

	// Events appended while the client is connected arrive live.
	done := make(chan []sseEvent)
	go func() { done <- readSSE(t, d.client, "http://istari/v1/events/stream?after="+cursor, "", 2) }()
	time.Sleep(100 * time.Millisecond)
	createTicket(t, d, "gh:acme/web#2")
	createTicket(t, d, "gh:acme/web#3")
	live := <-done
	if live[0].ID <= cursor || live[1].ID <= live[0].ID {
		t.Fatalf("live cursors %s, %s do not follow %s in order", live[0].ID, live[1].ID, cursor)
	}

	// After a disconnect, Last-Event-ID resumes exactly after the last event seen.
	createTicket(t, d, "gh:acme/web#4")
	resumed := readSSE(t, d.client, "http://istari/v1/events/stream", live[0].ID, 2)
	if resumed[0].ID != live[1].ID {
		t.Fatalf("resumed at %s, want %s", resumed[0].ID, live[1].ID)
	}
	var e struct{ Ticket string }
	json.Unmarshal(resumed[1].Data, &e)
	if e.Ticket != "gh:acme/web#4" || resumed[1].Type != "ticket.transitioned" {
		t.Fatalf("resumed event = %s %s, want the event of #4", resumed[1].Type, resumed[1].Data)
	}

	// Filters apply to the stream.
	filtered := readSSE(t, d.client, "http://istari/v1/events/stream?ticket="+strings.ReplaceAll("gh:acme/web#3", "#", "%23"), "", 1)
	json.Unmarshal(filtered[0].Data, &e)
	if e.Ticket != "gh:acme/web#3" {
		t.Fatalf("filtered stream gave %s", filtered[0].Data)
	}
}

func TestSIGTERMCompletesInFlightRequestsAndExitsZero(t *testing.T) {
	d := startDaemon(t, newDataDir(t))
	// Start a request whose body is still being sent when SIGTERM arrives.
	body, writer := io.Pipe()
	result := make(chan error, 1)
	go func() {
		resp, err := d.client.Post("http://istari/v1/ticket.create", "application/json", body)
		if err == nil {
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				data, _ := io.ReadAll(resp.Body)
				err = fmt.Errorf("%s %s", resp.Status, data)
			}
		}
		result <- err
	}()
	io.WriteString(writer, `{"actor":"human:alex",`)
	time.Sleep(200 * time.Millisecond)
	if err := d.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	io.WriteString(writer, `"ref":"gh:acme/web#1","project":"web"}`)
	writer.Close()
	if err := <-result; err != nil {
		t.Fatalf("in-flight request: %v", err)
	}
	if err := d.cmd.Wait(); err != nil {
		t.Fatalf("daemon exit: %v\nstderr: %s", err, d.stderr)
	}

	// The request's effect is durable.
	again := startDaemon(t, d.dataDir)
	var tickets []struct{ Ref string }
	again.call(t, "ticket.list", map[string]any{}, &tickets)
	if len(tickets) != 1 || tickets[0].Ref != "gh:acme/web#1" {
		t.Fatalf("tickets after restart = %v", tickets)
	}
}

func TestDaemonInstallWritesTheUnitAndOnlyPrintsHowToEnableIt(t *testing.T) {
	for _, tc := range []struct{ kind, path, enable, contains string }{
		{"launchd", "Library/LaunchAgents/dev.istari.daemon.plist", "launchctl", "<string>daemon</string>"},
		{"systemd", ".config/systemd/user/istari.service", "systemctl --user enable", "ExecStart="},
	} {
		home := t.TempDir()
		cmd := exec.Command(binary, "daemon", "install", "--kind", tc.kind)
		cmd.Env = append(os.Environ(), "HOME="+home)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", tc.kind, err, out)
		}
		unit, err := os.ReadFile(filepath.Join(home, tc.path))
		if err != nil {
			t.Fatalf("%s: unit not written: %v", tc.kind, err)
		}
		if !strings.Contains(string(unit), tc.contains) {
			t.Errorf("%s unit lacks %q:\n%s", tc.kind, tc.contains, unit)
		}
		if !strings.Contains(string(out), tc.enable) {
			t.Errorf("%s output does not say how to enable it:\n%s", tc.kind, out)
		}
	}
}

func TestTheAPIAnswersCoreRefusalsWithTheirCodes(t *testing.T) {
	d := startDaemon(t, newDataDir(t))
	post := func(op, body string) (int, string) {
		resp, err := d.client.Post("http://istari/v1/"+op, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var e struct{ Code string }
		json.NewDecoder(resp.Body).Decode(&e)
		return resp.StatusCode, e.Code
	}
	d.call(t, "task.create", map[string]any{"actor": "system", "id": "t_1", "ticket": "gh:acme/web#1", "role": "implementer"}, nil)
	d.call(t, "task.claim", map[string]any{"actor": "agent:a", "id": "t_1"}, nil)
	for _, tc := range []struct {
		op, body string
		status   int
		code     string
	}{
		{"task.claim", `{"actor":"agent:b","id":"t_1"}`, 409, "task_claimed"},
		{"task.show", `{"id":"t_404"}`, 404, "not_found"},
		{"task.claim", `{"actor":"agent:b","id":"t_1","colour":"red"}`, 400, "bad_request"},
	} {
		if status, code := post(tc.op, tc.body); status != tc.status || code != tc.code {
			t.Errorf("%s %s = %d %s, want %d %s", tc.op, tc.body, status, code, tc.status, tc.code)
		}
	}
}
