package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/sbraznikov/istari/internal/api"
	"github.com/sbraznikov/istari/internal/store"
)

// client talks to the daemon over its unix socket.
type client struct {
	http   *http.Client
	socket string
	token  string
}

// newClient finds the daemon: ISTARI_SOCKET, which the runner gives an agent,
// or the socket in the config's data directory. ISTARI_TOKEN, an agent's run
// token, goes with every request.
func newClient(opts *Options) (*client, error) {
	socket := os.Getenv("ISTARI_SOCKET")
	if socket == "" {
		cfg, err := loadConfig(opts.Config)
		if err != nil {
			return nil, err
		}
		dataDir, err := cfg.Daemon.DataDirPath()
		if err != nil {
			return nil, err
		}
		socket = filepath.Join(dataDir, api.SocketName)
	}
	return &client{socket: socket, token: os.Getenv("ISTARI_TOKEN"), http: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}}, nil
}

// refusal is an answer of the daemon that declines the request; it exits 1.
type refusal struct{ api.ErrorBody }

func (r refusal) Error() string { return r.Code + ": " + r.Message }

func (c *client) unreachable(err error) error {
	return fmt.Errorf("cannot reach the istari daemon at %s (is `istari daemon` running?): %w", c.socket, err)
}

// call performs one operation. A non-empty key makes a write idempotent.
func (c *client) call(ctx context.Context, op, key string, req, resp any) error {
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://istari/v1/"+op, bytes.NewReader(body))
	if err != nil {
		return err
	}
	r.Header.Set("Content-Type", "application/json")
	if key != "" {
		r.Header.Set(api.IdempotencyHeader, key)
	}
	if c.token != "" {
		r.Header.Set("Authorization", "Bearer "+c.token)
	}
	res, err := c.http.Do(r)
	if err != nil {
		return c.unreachable(err)
	}
	defer res.Body.Close()
	return decodeAnswer(res, resp)
}

func decodeAnswer(res *http.Response, resp any) error {
	data, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK {
		var e api.ErrorBody
		if json.Unmarshal(data, &e) != nil || e.Code == "" {
			return fmt.Errorf("daemon answered %s: %s", res.Status, data)
		}
		return refusal{e}
	}
	if resp == nil {
		return nil
	}
	return json.Unmarshal(data, resp)
}

func eventsQuery(after store.Cursor, f store.Filter) url.Values {
	q := url.Values{}
	for k, v := range map[string]string{"after": string(after), "project": f.Project, "ticket": f.Ticket, "type": f.TypePrefix} {
		if v != "" {
			q.Set(k, v)
		}
	}
	return q
}

// events reads one page of the log.
func (c *client) events(ctx context.Context, after store.Cursor, f store.Filter) ([]store.Event, error) {
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://istari/v1/events?"+eventsQuery(after, f).Encode(), nil)
	if err != nil {
		return nil, err
	}
	res, err := c.http.Do(r)
	if err != nil {
		return nil, c.unreachable(err)
	}
	defer res.Body.Close()
	var events []store.Event
	return events, decodeAnswer(res, &events)
}

// stream calls each with every event after the cursor, live, until each
// returns false, ctx ends or the stream fails.
func (c *client) stream(ctx context.Context, after store.Cursor, f store.Filter, each func(store.Event) bool) error {
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://istari/v1/events/stream?"+eventsQuery(after, f).Encode(), nil)
	if err != nil {
		return err
	}
	res, err := c.http.Do(r)
	if err != nil {
		return c.unreachable(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return decodeAnswer(res, nil)
	}
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		var e store.Event
		if err := json.Unmarshal([]byte(data), &e); err != nil {
			return err
		}
		if !each(e) {
			return nil
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return fmt.Errorf("the event stream ended")
}

// defaultActor is ISTARI_ACTOR, or human:<login>.
func defaultActor() string {
	if a := os.Getenv("ISTARI_ACTOR"); a != "" {
		return a
	}
	if u, err := user.Current(); err == nil {
		return "human:" + u.Username
	}
	return "human:unknown"
}
