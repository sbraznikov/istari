package github_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeGitHub is an in-memory GitHub REST API for the issue endpoints the
// adapter uses. It is stricter than GitHub in one way: an issue may only
// carry labels that exist in the repository.
type fakeGitHub struct {
	mu        sync.Mutex
	t         *testing.T
	token     string
	labels    map[string]fakeLabel // by name
	issues    map[int]*fakeIssue
	nextID    int64
	comments  map[int][]string
	subIssues map[int][]int64
	pageSize  int
}

type fakeLabel struct {
	Name, Color, Description string
}

type fakeIssue struct {
	ID          int64    `json:"id"`
	Number      int      `json:"number"`
	Title       string   `json:"title"`
	Body        string   `json:"body"`
	State       string   `json:"state"`
	StateReason string   `json:"state_reason,omitempty"`
	Labels      []string `json:"-"`
	Author      string   `json:"-"`
	PullRequest bool     `json:"-"`
}

func newFakeGitHub(t *testing.T) (*fakeGitHub, *httptest.Server) {
	f := &fakeGitHub{t: t, token: "test-token", labels: map[string]fakeLabel{"bug": {Name: "bug", Color: "d73a4a"}},
		issues: map[int]*fakeIssue{}, comments: map[int][]string{}, subIssues: map[int][]int64{}, nextID: 1000, pageSize: 100}
	mux := http.NewServeMux()
	const repo = "/repos/acme/web"
	mux.HandleFunc("GET "+repo+"/issues/{n}", f.getIssue)
	mux.HandleFunc("PATCH "+repo+"/issues/{n}", f.patchIssue)
	mux.HandleFunc("GET "+repo+"/issues", f.listIssues)
	mux.HandleFunc("POST "+repo+"/issues", f.createIssue)
	mux.HandleFunc("PUT "+repo+"/issues/{n}/labels", f.setLabels)
	mux.HandleFunc("GET "+repo+"/labels/{name}", f.getLabel)
	mux.HandleFunc("POST "+repo+"/labels", f.createLabel)
	mux.HandleFunc("POST "+repo+"/issues/{n}/comments", f.comment)
	mux.HandleFunc("POST "+repo+"/issues/{n}/sub_issues", f.addSubIssue)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+f.token {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeGitHub) write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (f *fakeGitHub) issueJSON(i *fakeIssue) map[string]any {
	labels := []map[string]string{}
	for _, l := range i.Labels {
		labels = append(labels, map[string]string{"name": l})
	}
	m := map[string]any{"id": i.ID, "number": i.Number, "title": i.Title, "body": i.Body, "state": i.State,
		"state_reason": i.StateReason, "labels": labels, "user": map[string]string{"login": i.Author},
		"html_url": fmt.Sprintf("https://github.com/acme/web/issues/%d", i.Number)}
	if i.PullRequest {
		m["pull_request"] = map[string]string{"url": "x"}
	}
	return m
}

func (f *fakeGitHub) issue(w http.ResponseWriter, r *http.Request) *fakeIssue {
	n, _ := strconv.Atoi(r.PathValue("n"))
	i, ok := f.issues[n]
	if !ok {
		f.write(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return nil
	}
	return i
}

func (f *fakeGitHub) checkLabels(w http.ResponseWriter, labels []string) bool {
	for _, l := range labels {
		if _, ok := f.labels[l]; !ok {
			f.write(w, http.StatusUnprocessableEntity, map[string]string{"message": "label does not exist: " + l})
			return false
		}
	}
	return true
}

func (f *fakeGitHub) getIssue(w http.ResponseWriter, r *http.Request) {
	if i := f.issue(w, r); i != nil {
		f.write(w, http.StatusOK, f.issueJSON(i))
	}
}

func (f *fakeGitHub) patchIssue(w http.ResponseWriter, r *http.Request) {
	i := f.issue(w, r)
	if i == nil {
		return
	}
	var req struct {
		State       *string `json:"state"`
		StateReason *string `json:"state_reason"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	if req.State != nil {
		i.State = *req.State
		i.StateReason = ""
	}
	if req.StateReason != nil {
		i.StateReason = *req.StateReason
	}
	f.write(w, http.StatusOK, f.issueJSON(i))
}

func (f *fakeGitHub) listIssues(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var want []string
	if q.Get("labels") != "" {
		want = strings.Split(q.Get("labels"), ",")
	}
	var all []map[string]any
	for n := 1; n <= len(f.issues); n++ {
		i := f.issues[n]
		if q.Get("state") != "all" && i.State != "open" {
			continue
		}
		if !containsAll(i.Labels, want) {
			continue
		}
		all = append(all, f.issueJSON(i))
	}
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}
	from := (page - 1) * f.pageSize
	to := min(from+f.pageSize, len(all))
	if from > len(all) {
		from = len(all)
	}
	if to < len(all) {
		next := *r.URL
		nq := next.Query()
		nq.Set("page", strconv.Itoa(page+1))
		next.RawQuery = nq.Encode()
		w.Header().Set("Link", fmt.Sprintf(`<http://%s%s>; rel="next"`, r.Host, next.RequestURI()))
	}
	f.write(w, http.StatusOK, all[from:to])
}

func containsAll(have, want []string) bool {
	for _, w := range want {
		if !slices.Contains(have, w) {
			return false
		}
	}
	return true
}

func (f *fakeGitHub) createIssue(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title, Body string
		Labels      []string
	}
	json.NewDecoder(r.Body).Decode(&req)
	if !f.checkLabels(w, req.Labels) {
		return
	}
	f.nextID++
	i := &fakeIssue{ID: f.nextID, Number: len(f.issues) + 1, Title: req.Title, Body: req.Body, State: "open", Labels: req.Labels, Author: "istari-bot"}
	f.issues[i.Number] = i
	f.write(w, http.StatusCreated, f.issueJSON(i))
}

func (f *fakeGitHub) setLabels(w http.ResponseWriter, r *http.Request) {
	i := f.issue(w, r)
	if i == nil {
		return
	}
	var req struct{ Labels []string }
	json.NewDecoder(r.Body).Decode(&req)
	if !f.checkLabels(w, req.Labels) {
		return
	}
	i.Labels = req.Labels
	f.write(w, http.StatusOK, f.issueJSON(i)["labels"])
}

func (f *fakeGitHub) getLabel(w http.ResponseWriter, r *http.Request) {
	l, ok := f.labels[r.PathValue("name")]
	if !ok {
		f.write(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	f.write(w, http.StatusOK, map[string]string{"name": l.Name, "color": l.Color, "description": l.Description})
}

func (f *fakeGitHub) createLabel(w http.ResponseWriter, r *http.Request) {
	var l fakeLabel
	json.NewDecoder(r.Body).Decode(&l)
	if _, exists := f.labels[l.Name]; exists {
		f.write(w, http.StatusUnprocessableEntity, map[string]string{"message": "already_exists"})
		return
	}
	f.labels[l.Name] = l
	f.write(w, http.StatusCreated, l)
}

func (f *fakeGitHub) comment(w http.ResponseWriter, r *http.Request) {
	i := f.issue(w, r)
	if i == nil {
		return
	}
	var req struct{ Body string }
	json.NewDecoder(r.Body).Decode(&req)
	f.comments[i.Number] = append(f.comments[i.Number], req.Body)
	f.nextID++
	f.write(w, http.StatusCreated, map[string]any{"id": f.nextID, "html_url": fmt.Sprintf("https://github.com/acme/web/issues/%d#issuecomment-%d", i.Number, f.nextID)})
}

func (f *fakeGitHub) addSubIssue(w http.ResponseWriter, r *http.Request) {
	i := f.issue(w, r)
	if i == nil {
		return
	}
	var req struct {
		SubIssueID int64 `json:"sub_issue_id"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	f.subIssues[i.Number] = append(f.subIssues[i.Number], req.SubIssueID)
	f.write(w, http.StatusCreated, f.issueJSON(i))
}

// seed adds an issue as a human would have created it.
func (f *fakeGitHub) seed(title, body, author string, labels ...string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, l := range labels {
		if _, ok := f.labels[l]; !ok {
			f.labels[l] = fakeLabel{Name: l, Color: "ededed"}
		}
	}
	f.nextID++
	n := len(f.issues) + 1
	f.issues[n] = &fakeIssue{ID: f.nextID, Number: n, Title: title, Body: body, State: "open", Labels: labels, Author: author}
	return n
}

func (f *fakeGitHub) snapshot(n int) fakeIssue {
	f.mu.Lock()
	defer f.mu.Unlock()
	return *f.issues[n]
}
