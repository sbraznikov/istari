package githubtest

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Check is one check run of a commit.
type Check struct {
	Name, Status, Conclusion string // status: queued, in_progress, completed
}

// Review is one review of a pull request.
type Review struct {
	User, State string // APPROVED, CHANGES_REQUESTED, COMMENTED
	Body        string
}

// EnableApp makes the fake a GitHub App host: requests signed with the App's
// key get installation tokens for the repository.
func (s *Server) EnableApp(appID int64, key *rsa.PublicKey, installation int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.appID, s.appKey, s.installation = appID, key, installation
}

// InstallationTokens returns the installation tokens the fake issued.
func (s *Server) InstallationTokens() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for t := range s.issued {
		out = append(out, t)
	}
	return out
}

// SetHeadSHA sets the head commit of a pull request.
func (s *Server) SetHeadSHA(n int, sha string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.issues[n].HeadSHA = sha
}

// SetBaseSHA sets the base commit of a pull request.
func (s *Server) SetBaseSHA(n int, sha string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.issues[n].BaseSHA = sha
}

// SetMergeable sets what GitHub says about merging a pull request into its
// base: mergeable nil is null (not computed yet), state its mergeable_state.
func (s *Server) SetMergeable(n int, mergeable *bool, state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.issues[n].Mergeable, s.issues[n].MergeState = mergeable, state
}

// SetChecks sets the check runs of a commit.
func (s *Server) SetChecks(sha string, checks ...Check) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checks[sha] = checks
}

// AddReview adds a review to a pull request.
func (s *Server) AddReview(n int, user, state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.issues[n].Reviews = append(s.issues[n].Reviews, Review{User: user, State: state})
}

// DeletedBranches returns the branches deleted through the API.
func (s *Server) DeletedBranches() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.deletedBranches...)
}

// appAuth reports whether r carries a valid JWT of the App.
func (s *Server) appAuth(r *http.Request) bool {
	jwt, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || s.appKey == nil {
		return false
	}
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(s.appKey, crypto.SHA256, sum[:], sig) != nil {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	var claims struct {
		Iss json.RawMessage `json:"iss"`
		Exp int64           `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || strings.Trim(string(claims.Iss), `"`) != strconv.FormatInt(s.appID, 10) {
		return false
	}
	return time.Now().Unix() < claims.Exp
}

// tokenOK reports whether a request's token may use the REST API.
func (s *Server) tokenOK(r *http.Request) bool {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return false
	}
	if token == s.Token {
		return true
	}
	until, issued := s.issued[token]
	return issued && time.Now().Before(until)
}

func (s *Server) repoInstallation(w http.ResponseWriter, r *http.Request) {
	s.write(w, http.StatusOK, map[string]any{"id": s.installation})
}

func (s *Server) accessToken(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("id") != strconv.FormatInt(s.installation, 10) {
		s.write(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	s.nextID++
	token := fmt.Sprintf("ghs_fake%d", s.nextID)
	expires := time.Now().Add(time.Hour)
	s.issued[token] = expires
	s.write(w, http.StatusCreated, map[string]any{"token": token, "expires_at": expires.UTC().Format(time.RFC3339)})
}

func (s *Server) createPull(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title, Body, Head, Base string
		Draft                   bool
	}
	json.NewDecoder(r.Body).Decode(&req)
	for _, i := range s.issues {
		if i.PullRequest && i.Branch == req.Head && i.State == "open" {
			s.write(w, http.StatusUnprocessableEntity, map[string]string{"message": "A pull request already exists for " + req.Head})
			return
		}
	}
	i := s.add(req.Title, req.Body, s.TokenUser)
	i.PullRequest, i.Branch, i.Base, i.Draft = true, req.Head, req.Base, req.Draft
	s.write(w, http.StatusCreated, s.pullJSON(i))
}

func (s *Server) pullJSON(i *Issue) map[string]any {
	return map[string]any{
		"number": i.Number, "node_id": pullNodeID(i.Number), "draft": i.Draft,
		"state": i.State, "merged": i.Merged, "title": i.Title, "body": i.Body,
		"mergeable": i.Mergeable, "mergeable_state": mergeableState(i), // GitHub computes them lazily: null until then
		"html_url": fmt.Sprintf("https://github.com/%s/pull/%d", Repo, i.Number),
		"head":     map[string]any{"ref": i.Branch, "sha": i.HeadSHA, "repo": map[string]any{"full_name": Repo}},
		"base":     map[string]any{"ref": i.Base, "sha": i.BaseSHA},
	}
}

func mergeableState(i *Issue) string {
	if i.MergeState == "" {
		return "unknown"
	}
	return i.MergeState
}

// pullNodeID is a pull request's GraphQL node id.
func pullNodeID(n int) string { return fmt.Sprintf("PR_fake%d", n) }

// graphql serves the one mutation istari uses: markPullRequestReadyForReview.
// Like GitHub, it answers 200 with an "errors" list when the request fails.
func (s *Server) graphql(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Query     string
		Variables struct{ ID string }
	}
	json.NewDecoder(r.Body).Decode(&req)
	fail := func(msg string) {
		s.write(w, http.StatusOK, map[string]any{"errors": []map[string]string{{"message": msg}}})
	}
	if !strings.Contains(req.Query, "markPullRequestReadyForReview") {
		fail("the fake knows only markPullRequestReadyForReview")
		return
	}
	for _, i := range s.issues {
		if i.PullRequest && pullNodeID(i.Number) == req.Variables.ID {
			if !i.Draft {
				fail("Pull request is not in draft")
				return
			}
			i.Draft = false
			s.write(w, http.StatusOK, map[string]any{"data": map[string]any{"markPullRequestReadyForReview": map[string]any{"pullRequest": map[string]any{"isDraft": false}}}})
			return
		}
	}
	fail("Could not resolve to a node with the global id of '" + req.Variables.ID + "'")
}

func (s *Server) updatePull(w http.ResponseWriter, r *http.Request) {
	i := s.pull(w, r)
	if i == nil {
		return
	}
	var req struct{ Title, Body *string }
	json.NewDecoder(r.Body).Decode(&req)
	if req.Title != nil {
		i.Title = *req.Title
	}
	if req.Body != nil {
		i.Body = *req.Body
	}
	s.write(w, http.StatusOK, s.pullJSON(i))
}

func (s *Server) checkRuns(w http.ResponseWriter, r *http.Request) {
	var runs []map[string]any
	for _, c := range s.checks[r.PathValue("sha")] {
		runs = append(runs, map[string]any{"name": c.Name, "status": c.Status, "conclusion": c.Conclusion})
	}
	if runs == nil {
		runs = []map[string]any{}
	}
	s.write(w, http.StatusOK, map[string]any{"total_count": len(runs), "check_runs": runs})
}

func (s *Server) reviews(w http.ResponseWriter, r *http.Request) {
	i := s.pull(w, r)
	if i == nil {
		return
	}
	var all []map[string]any
	for k, rv := range i.Reviews {
		all = append(all, map[string]any{"id": k + 1, "user": map[string]any{"login": rv.User}, "state": rv.State})
	}
	s.page(w, r, all)
}

func (s *Server) createReview(w http.ResponseWriter, r *http.Request) {
	i := s.pull(w, r)
	if i == nil {
		return
	}
	var req struct{ Body, Event string }
	json.NewDecoder(r.Body).Decode(&req)
	state, ok := map[string]string{"COMMENT": "COMMENTED", "APPROVE": "APPROVED", "REQUEST_CHANGES": "CHANGES_REQUESTED"}[req.Event]
	if !ok {
		s.write(w, http.StatusUnprocessableEntity, map[string]string{"message": "Unprocessable event " + req.Event})
		return
	}
	i.Reviews = append(i.Reviews, Review{User: s.TokenUser, State: state, Body: req.Body})
	s.write(w, http.StatusOK, map[string]any{"id": len(i.Reviews), "state": state})
}

func (s *Server) mergePull(w http.ResponseWriter, r *http.Request) {
	i := s.pull(w, r)
	if i == nil {
		return
	}
	var req struct {
		Method string `json:"merge_method"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	i.Merged, i.MergeMethod, i.State = true, req.Method, "closed"
	s.write(w, http.StatusOK, map[string]any{"merged": true, "sha": "merge-" + i.HeadSHA})
}

func (s *Server) deleteRef(w http.ResponseWriter, r *http.Request) {
	s.deletedBranches = append(s.deletedBranches, strings.TrimPrefix(r.PathValue("ref"), "heads/"))
	w.WriteHeader(http.StatusNoContent)
}

// SetStatus sets the combined commit status of a commit (pending, success,
// failure or error), as a repository without check runs reports CI.
func (s *Server) SetStatus(sha, state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statuses[sha] = state
}

// DismissReviews dismisses every review of a pull request.
func (s *Server) DismissReviews(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.issues[n].Reviews {
		s.issues[n].Reviews[k].State = "DISMISSED"
	}
}

func (s *Server) combinedStatus(w http.ResponseWriter, r *http.Request) {
	state, ok := s.statuses[r.PathValue("sha")]
	count := 1
	if !ok {
		state, count = "pending", 0
	}
	s.write(w, http.StatusOK, map[string]any{"state": state, "total_count": count})
}

// IssueCount returns how many issues and pull requests exist.
func (s *Server) IssueCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.issues)
}
