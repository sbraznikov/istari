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
	"sync"
	"syscall"
	"testing"
	"time"
)

type daemon struct {
	dataDir string
	socket  string
	config  string
	cmd     *exec.Cmd
	stderr  *lockedBuffer
	client  *http.Client
}

// lockedBuffer is the daemon's stderr: the daemon process writes it while a
// test reads it.
type lockedBuffer struct {
	mu  sync.Mutex // serializes the daemon's writes and the test's reads of buf
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
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

func writeDaemonConfig(t *testing.T, dataDir string, daemonKeys ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	body := fmt.Sprintf("[daemon]\ndata_dir = %q\n%s\n", dataDir, strings.Join(daemonKeys, "\n"))
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
// daemonKeys are extra lines of the [daemon] table of its config.
func startDaemon(t *testing.T, dataDir string, daemonKeys ...string) *daemon {
	t.Helper()
	return startDaemonWithConfig(t, dataDir, writeDaemonConfig(t, dataDir, daemonKeys...))
}

// startDaemonWithConfig starts `istari daemon` with a config file whose
// data_dir is dataDir; env are extra environment variables of the daemon.
func startDaemonWithConfig(t *testing.T, dataDir, config string, env ...string) *daemon {
	t.Helper()
	d := &daemon{dataDir: dataDir, socket: filepath.Join(dataDir, "istari.sock"), config: config, stderr: &lockedBuffer{}}
	d.cmd = exec.Command(binary, "daemon", "--config", d.config)
	d.cmd.Env = append(os.Environ(), env...)
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
	t.Parallel()
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
	t.Parallel()
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

// openSSE connects to an SSE stream; next reads its events one by one.
type sseStream struct {
	resp   *http.Response
	sc     *bufio.Scanner
	cancel context.CancelFunc
}

func openSSE(client *http.Client, url, lastEventID string) (*sseStream, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := client.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("stream answered %s %s", resp.Status, resp.Header.Get("Content-Type"))
	}
	return &sseStream{resp: resp, sc: bufio.NewScanner(resp.Body), cancel: cancel}, nil
}

func (s *sseStream) close() { s.resp.Body.Close(); s.cancel() }

func (s *sseStream) next(n int) ([]sseEvent, error) {
	var events []sseEvent
	var cur sseEvent
	for len(events) < n && s.sc.Scan() {
		line := s.sc.Text()
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
		return events, fmt.Errorf("stream ended after %d of %d events: %v", len(events), n, s.sc.Err())
	}
	return events, nil
}

// readSSE reads n events from a new connection to url.
func readSSE(t *testing.T, client *http.Client, url, lastEventID string, n int) []sseEvent {
	t.Helper()
	stream, err := openSSE(client, url, lastEventID)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.close()
	events, err := stream.next(n)
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func createTicket(t *testing.T, d *daemon, ref string) {
	t.Helper()
	d.call(t, "ticket.create", map[string]any{"actor": "human:alex", "ref": ref, "project": "web"}, nil)
}

func TestSSEDeliversEveryLaterEventInOrderAcrossAReconnect(t *testing.T) {
	t.Parallel()
	d := startDaemon(t, newDataDir(t))
	createTicket(t, d, "gh:acme/web#1")
	first := readSSE(t, d.client, "http://istari/v1/events/stream", "", 1)
	cursor := first[0].ID

	// Events appended after the client connected arrive on the open stream.
	stream, err := openSSE(d.client, "http://istari/v1/events/stream?after="+cursor, "")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.close()
	createTicket(t, d, "gh:acme/web#2")
	createTicket(t, d, "gh:acme/web#3")
	live, err := stream.next(2)
	if err != nil {
		t.Fatal(err)
	}
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
	t.Parallel()
	d := startDaemon(t, newDataDir(t))
	// One connection: a first request proves the daemon accepted it, then a
	// second request is still sending its body when SIGTERM arrives.
	conn, err := net.Dial("unix", d.socket)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	reader := bufio.NewReader(conn)
	fmt.Fprint(conn, "GET /v1/health HTTP/1.1\r\nHost: istari\r\n\r\n")
	if resp, err := http.ReadResponse(reader, nil); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("first request: %v", err)
	} else {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	body := `{"actor":"human:alex","ref":"gh:acme/web#1","project":"web"}`
	fmt.Fprintf(conn, "POST /v1/ticket.create HTTP/1.1\r\nHost: istari\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body[:20])
	time.Sleep(200 * time.Millisecond) // let the daemon start reading the body
	if err := d.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	fmt.Fprint(conn, body[20:])
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatalf("in-flight request: %v", err)
	}
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("in-flight request: %s %s", resp.Status, data)
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
	t.Parallel()
	for _, tc := range []struct{ kind, path, enable, contains string }{
		{"launchd", "Library/LaunchAgents/dev.istari.daemon.plist", "launchctl", "<string>daemon</string>"},
		{"systemd", ".config/systemd/user/istari.service", "systemctl --user enable", "ExecStart="},
	} {
		home := filepath.Join(t.TempDir(), "my home")
		cfg := filepath.Join(home, "100% $config.toml")
		cmd := exec.Command(binary, "daemon", "install", "--kind", tc.kind, "--config", cfg)
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
	t.Parallel()
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
		{"task.cancel", `{"id":"t_1"}`, 400, "bad_request"}, // no actor
		{"task.list", ``, 200, ""},                          // an empty body is an empty request
	} {
		if status, code := post(tc.op, tc.body); status != tc.status || code != tc.code {
			t.Errorf("%s %s = %d %s, want %d %s", tc.op, tc.body, status, code, tc.status, tc.code)
		}
	}
}

func TestABadCursorIsABadRequest(t *testing.T) {
	t.Parallel()
	d := startDaemon(t, newDataDir(t))
	for _, url := range []string{"http://istari/v1/events?after=garbage", "http://istari/v1/events/stream?after=garbage"} {
		resp, err := d.client.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s = %s, want 400", url, resp.Status)
		}
	}
}

func TestWithoutAConfigFileTheDaemonUsesTheDefaultDataDir(t *testing.T) {
	t.Parallel()
	home := newDataDir(t)
	cmd := exec.Command(binary, "daemon")
	cmd.Env = append(os.Environ(), "HOME="+home)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Signal(syscall.SIGTERM); cmd.Wait() }()
	socket := filepath.Join(home, ".local", "share", "istari", "istari.sock")
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(socket); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no socket at %s", socket)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestTheSystemdUnitEscapesSpecifiersAndVariables(t *testing.T) {
	t.Parallel()
	home := filepath.Join(t.TempDir(), "home")
	cmd := exec.Command(binary, "daemon", "install", "--kind", "systemd", "--config", "/etc/100% $X.toml")
	cmd.Env = append(os.Environ(), "HOME="+home)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	unit, _ := os.ReadFile(filepath.Join(home, ".config/systemd/user/istari.service"))
	if !strings.Contains(string(unit), `"/etc/100%% $$X.toml"`) {
		t.Fatalf("ExecStart does not escape %% and $:\n%s", unit)
	}
}

func TestTicketFireDoesNotAdmitATicket(t *testing.T) {
	t.Parallel()
	d := startDaemon(t, newDataDir(t))
	ref := "gh:acme/web#1"
	createTicket(t, d, ref)
	resp, err := d.client.Post("http://istari/v1/ticket.fire", "application/json",
		strings.NewReader(`{"actor":"human:alex","ref":"gh:acme/web#1","trigger":"accept"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var e struct{ Code, Message string }
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		t.Fatalf("ticket.fire accept: %s, body is not an error: %v", resp.Status, err)
	}
	if resp.StatusCode != http.StatusConflict || e.Code != "invalid_transition" || !strings.Contains(e.Message, "istari ticket ready") {
		t.Fatalf("ticket.fire accept = %d %+v, want 409 invalid_transition naming istari ticket ready", resp.StatusCode, e)
	}
	var tk struct{ State string }
	d.call(t, "ticket.show", map[string]any{"ref": ref}, &tk)
	if tk.State != "new" {
		t.Fatalf("after a refused accept the ticket is %s, want new", tk.State)
	}
}

func TestTheUnitCarriesThePathOfTheShellThatInstalledIt(t *testing.T) {
	t.Parallel()
	path := "/opt/my tools/bin:/usr/bin:/bin" // where gh and claude are found
	for _, tc := range []struct{ kind, file, want string }{
		{"launchd", "Library/LaunchAgents/dev.istari.daemon.plist", "<key>PATH</key>\n      <string>" + path + "</string>"},
		{"systemd", ".config/systemd/user/istari.service", `Environment="PATH=` + path + `"`},
	} {
		home := t.TempDir()
		cmd := exec.Command(binary, "daemon", "install", "--kind", tc.kind)
		cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+path+string(os.PathListSeparator)+filepath.Dir(binary))
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", tc.kind, err, out)
		}
		unit, _ := os.ReadFile(filepath.Join(home, tc.file))
		want := strings.Replace(tc.want, path, path+string(os.PathListSeparator)+filepath.Dir(binary), 1)
		if !strings.Contains(string(unit), want) {
			t.Errorf("%s unit lacks %q:\n%s", tc.kind, want, unit)
		}
		if !strings.Contains(string(out), "PATH") {
			t.Errorf("%s output does not say the PATH was written:\n%s", tc.kind, out)
		}
	}
}
