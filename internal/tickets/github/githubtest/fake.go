// Package githubtest is an in-memory GitHub REST API for the issue endpoints
// the tickets/github adapter uses, for tests. It keeps issue events (who
// labeled, closed or reopened), comments and collaborator permissions, and
// lets a test act as a human on github.com.
//
// It is stricter than GitHub in one way: an issue may only carry labels that
// exist in the repository.
package githubtest

import (
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Repo is the one repository the fake serves.
const Repo = "acme/web"

// Server is the fake.
type Server struct {
	URL       string // the API base URL to give the adapter
	Token     string // the token the adapter must send
	TokenUser string // the login the token acts as

	mu         sync.Mutex
	now        time.Time
	labels     map[string]Label
	issues     map[int]*Issue
	nextID     int64
	subIssues  map[int][]int64
	perms      map[string]string
	pageSize   int
	prefix     string
	moved      map[int]bool
	deleted    map[int]bool
	dropLabels bool
	failNext   int
	files      map[string]string // repository files on the default branch
	commits    int               // commits on the default branch: its head

	appID           int64
	appKey          *rsa.PublicKey
	installation    int64
	issued          map[string]time.Time // installation tokens → expiry
	checks          map[string][]Check   // commit → check runs
	statuses        map[string]string    // commit → combined status
	deletedBranches []string
	counted         []string // "METHOD path" of the requests that count against the quota
}

// Label is a repository label.
type Label struct {
	Name, Color, Description string
}

// Issue is an issue as the fake holds it.
type Issue struct {
	ID          int64
	Number      int
	Title       string
	Body        string
	State       string // open or closed
	StateReason string
	Labels      []string
	Author      string
	PullRequest bool
	Branch      string   // a pull request's head branch
	Base        string   // a pull request's base branch
	HeadSHA     string   // a pull request's head commit
	Reviews     []Review // a pull request's reviews
	Merged      bool
	MergeMethod string
	Draft       bool
	ClosedAt    time.Time
	Commits     []string // a pull request's commit messages
	Updated     time.Time
	Events      []Event
	Comments    []Comment
}

// Event is an issue event: labeled, unlabeled, closed or reopened.
type Event struct {
	Kind, Actor, Label string
}

// Comment is an issue comment.
type Comment struct {
	ID     int64
	Author string
	Body   string
}

// Option configures New.
type Option func(*Server)

// Prefix serves the API under a path prefix, as GitHub Enterprise does (/api/v3).
func Prefix(p string) Option { return func(s *Server) { s.prefix = p } }

// New starts the fake. The token acts as "alex", an admin of the repository.
func New(t testing.TB, opts ...Option) *Server {
	s := &Server{Token: "test-token", TokenUser: "alex", now: time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC),
		labels: map[string]Label{"bug": {Name: "bug", Color: "d73a4a"}}, issues: map[int]*Issue{}, nextID: 1000,
		subIssues: map[int][]int64{}, perms: map[string]string{"alex": "admin"}, pageSize: 100,
		moved: map[int]bool{}, deleted: map[int]bool{}, files: map[string]string{},
		issued: map[string]time.Time{}, checks: map[string][]Check{}, statuses: map[string]string{}}
	for _, o := range opts {
		o(s)
	}
	mux := http.NewServeMux()
	const repo = "/repos/" + Repo
	mux.HandleFunc("GET "+repo+"/issues/{n}", s.getIssue)
	mux.HandleFunc("PATCH "+repo+"/issues/{n}", s.patchIssue)
	mux.HandleFunc("GET "+repo+"/issues", s.listIssues)
	mux.HandleFunc("POST "+repo+"/issues", s.createIssue)
	mux.HandleFunc("PUT "+repo+"/issues/{n}/labels", s.setLabels)
	mux.HandleFunc("GET "+repo+"/issues/{n}/events", s.listEvents)
	mux.HandleFunc("GET "+repo+"/issues/{n}/comments", s.listComments)
	mux.HandleFunc("POST "+repo+"/issues/{n}/comments", s.comment)
	mux.HandleFunc("POST "+repo+"/issues/{n}/sub_issues", s.addSubIssue)
	mux.HandleFunc("GET "+repo+"/labels/{name}", s.getLabel)
	mux.HandleFunc("POST "+repo+"/labels", s.createLabel)
	mux.HandleFunc("GET "+repo+"/collaborators/{user}/permission", s.permission)
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, _ *http.Request) {
		s.write(w, http.StatusOK, map[string]string{"login": s.TokenUser})
	})
	mux.HandleFunc("GET "+repo+"/contents/{path...}", s.contents)
	mux.HandleFunc("GET "+repo+"/commits/{ref}", s.commit)
	mux.HandleFunc("GET "+repo+"/pulls/{n}", s.getPull)
	mux.HandleFunc("POST "+repo+"/pulls", s.createPull)
	mux.HandleFunc("PATCH "+repo+"/pulls/{n}", s.updatePull)
	mux.HandleFunc("GET "+repo+"/pulls/{n}/reviews", s.reviews)
	mux.HandleFunc("POST "+repo+"/pulls/{n}/reviews", s.createReview)
	mux.HandleFunc("PUT "+repo+"/pulls/{n}/merge", s.mergePull)
	mux.HandleFunc("GET "+repo+"/commits/{sha}/check-runs", s.checkRuns)
	mux.HandleFunc("GET "+repo+"/commits/{sha}/status", s.combinedStatus)
	mux.HandleFunc("DELETE "+repo+"/git/refs/{ref...}", s.deleteRef)
	mux.HandleFunc("GET "+repo+"/installation", s.repoInstallation)
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", s.accessToken)
	mux.HandleFunc("GET "+repo+"/pulls/{n}/commits", s.listPullCommits)
	mux.HandleFunc("POST /graphql", s.graphql)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		appOnly := strings.HasSuffix(r.URL.Path, "/installation") || strings.HasPrefix(strings.TrimPrefix(r.URL.Path, s.prefix), "/app/")
		if appOnly && !s.appAuth(r) || !appOnly && !s.tokenOK(r) {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		if s.failNext > 0 {
			s.failNext--
			s.write(w, http.StatusInternalServerError, map[string]string{"message": "Server Error"})
			return
		}
		if gql, ok := strings.CutSuffix(s.prefix, "/v3"); ok && r.URL.Path == gql+"/graphql" {
			r.URL.Path = s.prefix + "/graphql" // GitHub Enterprise: GraphQL at /api/graphql, REST at /api/v3
		}
		if !strings.HasPrefix(r.URL.Path, s.prefix+"/") {
			http.NotFound(w, r)
			return
		}
		r.URL.Path = strings.TrimPrefix(r.URL.Path, s.prefix)
		s.serveConditional(w, r, mux)
	}))
	t.Cleanup(srv.Close)
	s.URL = srv.URL + s.prefix
	return s
}

// serveConditional answers as GitHub does with ETags: a GET's 200 carries the
// ETag of its body, and a GET whose If-None-Match names the ETag of the
// answer it would get is answered 304 and does not count against the quota.
// Every other answer is counted.
func (s *Server) serveConditional(w http.ResponseWriter, r *http.Request, h http.Handler) {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	res := rec.Result()
	body := rec.Body.Bytes()
	if r.Method == http.MethodGet && res.StatusCode == http.StatusOK {
		sum := sha256.Sum256(body)
		etag := `"` + hex.EncodeToString(sum[:8]) + `"`
		res.Header.Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			maps.Copy(w.Header(), res.Header)
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	s.counted = append(s.counted, r.Method+" "+r.URL.Path)
	maps.Copy(w.Header(), res.Header)
	w.WriteHeader(res.StatusCode)
	w.Write(body)
}

// Counted returns the requests that counted against the quota, as "METHOD
// path", oldest first: all but the ones answered 304.
func (s *Server) Counted() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.counted)
}

func (s *Server) tick() time.Time {
	s.now = s.now.Add(time.Second)
	return s.now
}

func (s *Server) write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (s *Server) issueJSON(i *Issue) map[string]any {
	labels := []map[string]string{}
	for _, l := range i.Labels {
		labels = append(labels, map[string]string{"name": l})
	}
	m := map[string]any{"id": i.ID, "number": i.Number, "title": i.Title, "body": i.Body, "state": i.State,
		"state_reason": i.StateReason, "labels": labels, "user": map[string]string{"login": i.Author},
		"html_url": fmt.Sprintf("https://github.com/%s/issues/%d", Repo, i.Number), "updated_at": i.Updated.Format(time.RFC3339),
		"comments": len(i.Comments)}
	if i.PullRequest {
		m["pull_request"] = map[string]string{"url": "x"}
	}
	if !i.ClosedAt.IsZero() {
		m["closed_at"] = i.ClosedAt.Format(time.RFC3339)
	}
	return m
}

func (s *Server) issue(w http.ResponseWriter, r *http.Request) *Issue {
	n, _ := strconv.Atoi(r.PathValue("n"))
	if s.moved[n] {
		// Point at an issue that exists, so that a client following the
		// redirect would get an answer instead of an error.
		w.Header().Set("Location", "http://"+r.Host+s.prefix+"/repos/"+Repo+"/issues/1")
		s.write(w, http.StatusMovedPermanently, map[string]string{"message": "Moved Permanently"})
		return nil
	}
	if s.deleted[n] {
		s.write(w, http.StatusGone, map[string]string{"message": "This issue was deleted"})
		return nil
	}
	i, ok := s.issues[n]
	if !ok {
		s.write(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return nil
	}
	return i
}

func (s *Server) checkLabels(w http.ResponseWriter, labels []string) bool {
	for _, l := range labels {
		if _, ok := s.labels[l]; !ok {
			s.write(w, http.StatusUnprocessableEntity, map[string]string{"message": "label does not exist: " + l})
			return false
		}
	}
	return true
}

// relabel replaces an issue's labels and records labeled and unlabeled events.
func (s *Server) relabel(i *Issue, actor string, labels []string) {
	for _, l := range i.Labels {
		if !slices.Contains(labels, l) {
			i.Events = append(i.Events, Event{Kind: "unlabeled", Actor: actor, Label: l})
		}
	}
	for _, l := range labels {
		if !slices.Contains(i.Labels, l) {
			i.Events = append(i.Events, Event{Kind: "labeled", Actor: actor, Label: l})
		}
	}
	i.Labels = slices.Clone(labels)
	i.Updated = s.tick()
}

func (s *Server) setState(i *Issue, actor, state, reason string) {
	if state == i.State {
		i.StateReason = reason
		return
	}
	i.State, i.StateReason = state, reason
	i.Updated = s.tick()
	if state == "closed" {
		i.ClosedAt = i.Updated
	}
	kind := "closed"
	if state == "open" {
		kind = "reopened"
	}
	i.Events = append(i.Events, Event{Kind: kind, Actor: actor})
	i.Updated = s.tick()
}

func (s *Server) getIssue(w http.ResponseWriter, r *http.Request) {
	if i := s.issue(w, r); i != nil {
		s.write(w, http.StatusOK, s.issueJSON(i))
	}
}

func (s *Server) patchIssue(w http.ResponseWriter, r *http.Request) {
	i := s.issue(w, r)
	if i == nil {
		return
	}
	var req struct {
		State       *string `json:"state"`
		StateReason *string `json:"state_reason"`
		Body        *string `json:"body"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	if req.Body != nil {
		i.Body = *req.Body
		i.Updated = s.tick()
	}
	if req.State != nil {
		reason := ""
		if req.StateReason != nil {
			reason = *req.StateReason
		}
		s.setState(i, s.TokenUser, *req.State, reason)
	}
	s.write(w, http.StatusOK, s.issueJSON(i))
}

func (s *Server) listIssues(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var want []string
	if q.Get("labels") != "" {
		want = strings.Split(q.Get("labels"), ",")
	}
	var all []map[string]any
	for n := 1; n <= len(s.issues); n++ {
		i := s.issues[n]
		if q.Get("state") != "all" && i.State != "open" {
			continue
		}
		if !containsAll(i.Labels, want) {
			continue
		}
		if since, err := time.Parse(time.RFC3339, q.Get("since")); err == nil && i.Updated.Before(since) {
			continue
		}
		all = append(all, s.issueJSON(i))
	}
	s.page(w, r, all)
}

func (s *Server) page(w http.ResponseWriter, r *http.Request, all []map[string]any) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	from := min((page-1)*s.pageSize, len(all))
	to := min(from+s.pageSize, len(all))
	if to < len(all) {
		next := *r.URL
		nq := next.Query()
		nq.Set("page", strconv.Itoa(page+1))
		next.RawQuery = nq.Encode()
		w.Header().Set("Link", fmt.Sprintf(`<http://%s%s%s>; rel="next"`, r.Host, s.prefix, next.RequestURI()))
	}
	if all == nil {
		all = []map[string]any{}
	}
	s.write(w, http.StatusOK, all[from:to])
}

func containsAll(have, want []string) bool {
	for _, w := range want {
		if !slices.Contains(have, w) {
			return false
		}
	}
	return true
}

func (s *Server) createIssue(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title, Body string
		Labels      []string
	}
	json.NewDecoder(r.Body).Decode(&req)
	if !s.checkLabels(w, req.Labels) {
		return
	}
	if s.dropLabels {
		req.Labels = nil
	}
	i := s.add(req.Title, req.Body, s.TokenUser)
	s.relabel(i, s.TokenUser, req.Labels)
	s.write(w, http.StatusCreated, s.issueJSON(i))
}

func (s *Server) add(title, body, author string) *Issue {
	s.nextID++
	i := &Issue{ID: s.nextID, Number: len(s.issues) + 1, Title: title, Body: body, State: "open", Author: author, Updated: s.tick()}
	s.issues[i.Number] = i
	return i
}

func (s *Server) setLabels(w http.ResponseWriter, r *http.Request) {
	i := s.issue(w, r)
	if i == nil {
		return
	}
	var req struct{ Labels []string }
	json.NewDecoder(r.Body).Decode(&req)
	if !s.checkLabels(w, req.Labels) {
		return
	}
	s.relabel(i, s.TokenUser, req.Labels)
	s.write(w, http.StatusOK, s.issueJSON(i)["labels"])
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	i := s.issue(w, r)
	if i == nil {
		return
	}
	var all []map[string]any
	for k, e := range i.Events {
		m := map[string]any{"id": k + 1, "event": e.Kind, "actor": map[string]string{"login": e.Actor}}
		if e.Label != "" {
			m["label"] = map[string]string{"name": e.Label}
		}
		all = append(all, m)
	}
	s.page(w, r, all)
}

func (s *Server) listComments(w http.ResponseWriter, r *http.Request) {
	i := s.issue(w, r)
	if i == nil {
		return
	}
	var all []map[string]any
	for _, c := range i.Comments {
		all = append(all, map[string]any{"id": c.ID, "body": c.Body, "user": map[string]string{"login": c.Author},
			"html_url": fmt.Sprintf("https://github.com/%s/issues/%d#issuecomment-%d", Repo, i.Number, c.ID)})
	}
	s.page(w, r, all)
}

func (s *Server) comment(w http.ResponseWriter, r *http.Request) {
	i := s.issue(w, r)
	if i == nil {
		return
	}
	var req struct{ Body string }
	json.NewDecoder(r.Body).Decode(&req)
	c := s.addComment(i, s.TokenUser, req.Body)
	s.write(w, http.StatusCreated, map[string]any{"id": c.ID, "html_url": fmt.Sprintf("https://github.com/%s/issues/%d#issuecomment-%d", Repo, i.Number, c.ID)})
}

func (s *Server) addComment(i *Issue, author, body string) Comment {
	s.nextID++
	c := Comment{ID: s.nextID, Author: author, Body: body}
	i.Comments = append(i.Comments, c)
	i.Updated = s.tick()
	return c
}

func (s *Server) addSubIssue(w http.ResponseWriter, r *http.Request) {
	i := s.issue(w, r)
	if i == nil {
		return
	}
	var req struct {
		SubIssueID int64 `json:"sub_issue_id"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	s.subIssues[i.Number] = append(s.subIssues[i.Number], req.SubIssueID)
	s.write(w, http.StatusCreated, s.issueJSON(i))
}

func (s *Server) getLabel(w http.ResponseWriter, r *http.Request) {
	l, ok := s.labels[r.PathValue("name")]
	if !ok {
		s.write(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	s.write(w, http.StatusOK, map[string]string{"name": l.Name, "color": l.Color, "description": l.Description})
}

func (s *Server) createLabel(w http.ResponseWriter, r *http.Request) {
	var l Label
	json.NewDecoder(r.Body).Decode(&l)
	if _, exists := s.labels[l.Name]; exists {
		s.write(w, http.StatusUnprocessableEntity, map[string]string{"message": "already_exists"})
		return
	}
	s.labels[l.Name] = l
	s.write(w, http.StatusCreated, l)
}

func (s *Server) permission(w http.ResponseWriter, r *http.Request) {
	user := r.PathValue("user")
	perm, ok := s.perms[user]
	if !ok {
		perm = "read" // anyone may read a public repository
	}
	s.write(w, http.StatusOK, map[string]any{"permission": perm, "user": map[string]string{"login": user}})
}

func (s *Server) contents(w http.ResponseWriter, r *http.Request) {
	body, ok := s.files[r.PathValue("path")]
	if !ok {
		s.write(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	if r.Header.Get("Accept") != "application/vnd.github.raw+json" {
		s.write(w, http.StatusNotAcceptable, map[string]string{"message": "ask for the raw media type"})
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(body))
}

// SetFile puts a file on the repository's default branch, in a new commit.
func (s *Server) SetFile(path, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files[path] = body
	s.commits++
}

// commit answers the SHA of the default branch's head for the ref HEAD, with
// the media type that asks for the bare SHA.
func (s *Server) commit(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("ref") != "HEAD" || r.Header.Get("Accept") != "application/vnd.github.sha" {
		s.write(w, http.StatusUnprocessableEntity, map[string]string{"message": "the fake answers only HEAD as a bare SHA"})
		return
	}
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "%040x", s.commits)
}

// HumanEditBody replaces an issue's body as login would.
func (s *Server) HumanEditBody(n int, login, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.issues[n].Body = body
	s.issues[n].Updated = s.tick()
}

// Seed adds an issue as a human would have created it, creating its labels.
func (s *Server) Seed(title, body, author string, labels ...string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensure(labels)
	i := s.add(title, body, author)
	s.relabel(i, author, labels)
	return i.Number
}

func (s *Server) ensure(labels []string) {
	for _, l := range labels {
		if _, ok := s.labels[l]; !ok {
			s.labels[l] = Label{Name: l, Color: "ededed"}
		}
	}
}

// HumanSetLabels replaces an issue's labels as login would on github.com.
func (s *Server) HumanSetLabels(n int, login string, labels ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensure(labels)
	s.relabel(s.issues[n], login, labels)
}

// HumanAddLabel adds one label as login would, leaving the others.
func (s *Server) HumanAddLabel(n int, login, label string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensure([]string{label})
	s.relabel(s.issues[n], login, append(slices.Clone(s.issues[n].Labels), label))
}

// HumanClose closes an issue as login (reason completed or not_planned).
func (s *Server) HumanClose(n int, login, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setState(s.issues[n], login, "closed", reason)
}

// HumanReopen reopens an issue as login.
func (s *Server) HumanReopen(n int, login string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setState(s.issues[n], login, "open", "reopened")
}

// HumanMerge merges pull request pr as login on github.com. GitHub also
// closes the issue the pull request names with `Closes #n`: a test does that
// with HumanClose(n, login, "completed"), when its order needs it.
func (s *Server) HumanMerge(pr int, login string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.issues[pr]
	p.Merged, p.MergeMethod = true, "merge"
	s.setState(p, login, "closed", "")
}

// HumanComment comments on an issue as login.
func (s *Server) HumanComment(n int, login, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.addComment(s.issues[n], login, body)
}

// SetPermission gives login a permission: admin, maintain, write, triage or read.
func (s *Server) SetPermission(login, perm string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.perms[login] = perm
}

// FailNext makes the next n requests answer 500.
func (s *Server) FailNext(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNext = n
}

// SetPageSize sets how many items a page holds.
func (s *Server) SetPageSize(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pageSize = n
}

// SeedPull adds a pull request from branch with commits of these messages.
func (s *Server) SeedPull(branch string, messages ...string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.add("pull request", "", "bob")
	i.PullRequest, i.Branch, i.Commits = true, branch, slices.Clone(messages)
	return i.Number
}

func (s *Server) pull(w http.ResponseWriter, r *http.Request) *Issue {
	i := s.issue(w, r)
	if i != nil && !i.PullRequest {
		s.write(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return nil
	}
	return i
}

func (s *Server) getPull(w http.ResponseWriter, r *http.Request) {
	if i := s.pull(w, r); i != nil {
		s.write(w, http.StatusOK, s.pullJSON(i))
	}
}

func (s *Server) listPullCommits(w http.ResponseWriter, r *http.Request) {
	i := s.pull(w, r)
	if i == nil {
		return
	}
	all := make([]map[string]any, len(i.Commits))
	for k, m := range i.Commits {
		all[k] = map[string]any{"sha": fmt.Sprintf("%040d", k+1), "commit": map[string]any{"message": m}}
	}
	s.page(w, r, all)
}

// MarkPullRequest makes an issue a pull request.
func (s *Server) MarkPullRequest(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.issues[n].PullRequest = true
}

// Move makes an issue answer 301, as a transferred issue does.
func (s *Server) Move(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.moved[n] = true
}

// Delete makes an issue answer 410, as a deleted issue does.
func (s *Server) Delete(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleted[n] = true
}

// DropLabels makes issue creation ignore labels, as GitHub does for a token
// without push access.
func (s *Server) DropLabels(drop bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropLabels = drop
}

// Issue returns a copy of an issue.
func (s *Server) Issue(n int) Issue {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := *s.issues[n]
	i.Labels = slices.Clone(i.Labels)
	i.Events = slices.Clone(i.Events)
	i.Comments = slices.Clone(i.Comments)
	return i
}

// Label returns a label of the repository.
func (s *Server) Label(name string) (Label, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.labels[name]
	return l, ok
}

// SubIssues returns the ids of an issue's sub-issues.
func (s *Server) SubIssues(n int) []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.subIssues[n])
}
