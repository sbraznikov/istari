package vcs

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Tokens gives the token for a repository's API and git calls. It stays in
// the daemon: no agent ever sees it (spec section 11).
type Tokens interface {
	Token(ctx context.Context, repo string) (string, error)
}

// StaticToken is one token for every repository: the gh CLI's, when the
// installation has no GitHub App (spec section 11).
type StaticToken string

// Token implements Tokens.
func (t StaticToken) Token(context.Context, string) (string, error) { return string(t), nil }

// AppTokens are a GitHub App's installation tokens, one installation per
// repository, kept until shortly before they expire.
type AppTokens struct {
	appID int64
	key   *rsa.PrivateKey
	base  string
	http  *http.Client
	clock func() time.Time

	mu            sync.Mutex
	installations map[string]int64   // repository → installation
	tokens        map[int64]appToken // installation → token
}

type appToken struct {
	value   string
	expires time.Time
}

// NewAppTokens reads the App's private key (PEM, PKCS#1 or PKCS#8).
func NewAppTokens(appID int64, keyPath, baseURL string) (*AppTokens, error) {
	if strings.HasPrefix(keyPath, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			keyPath = home + keyPath[1:]
		}
	}
	data, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("github app: the private key: %w", err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("github app: %s is not a PEM private key", keyPath)
	}
	var key *rsa.PrivateKey
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		key = k
	} else if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("github app: %s is not an RSA key", keyPath)
		}
		key = rk
	} else {
		return nil, fmt.Errorf("github app: %s: %w", keyPath, err)
	}
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	return &AppTokens{appID: appID, key: key, base: strings.TrimSuffix(baseURL, "/"), http: &http.Client{Timeout: 30 * time.Second},
		clock: time.Now, installations: map[string]int64{}, tokens: map[int64]appToken{}}, nil
}

// Token implements Tokens: the installation token of the repository's
// installation, renewed a few minutes before it expires.
func (a *AppTokens) Token(ctx context.Context, repo string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	id, ok := a.installations[repo]
	if !ok {
		var inst struct {
			ID int64 `json:"id"`
		}
		if err := a.call(ctx, http.MethodGet, "/repos/"+repo+"/installation", &inst); err != nil {
			return "", fmt.Errorf("github app: the installation on %s: %w", repo, err)
		}
		id = inst.ID
		a.installations[repo] = id
	}
	if t, ok := a.tokens[id]; ok && a.clock().Add(5*time.Minute).Before(t.expires) {
		return t.value, nil
	}
	var tok struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := a.call(ctx, http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", id), &tok); err != nil {
		// Uninstalled or reinstalled: look the installation up again next time.
		delete(a.installations, repo)
		delete(a.tokens, id)
		return "", fmt.Errorf("github app: an installation token: %w", err)
	}
	a.tokens[id] = appToken{value: tok.Token, expires: tok.ExpiresAt}
	return tok.Token, nil
}

// call makes a request as the App itself, with a short-lived JWT.
func (a *AppTokens) call(ctx context.Context, method, path string, out any) error {
	jwt, err := a.jwt()
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	res, err := a.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		var e struct{ Message string }
		json.NewDecoder(res.Body).Decode(&e) //nolint:errcheck // a body that is not GitHub's JSON error leaves the message empty; the status still says what failed
		return fmt.Errorf("%s %s: %d %s", method, path, res.StatusCode, e.Message)
	}
	return json.NewDecoder(res.Body).Decode(out)
}

// jwt signs the App's JWT (RS256), valid for nine minutes, backdated for
// clock drift.
func (a *AppTokens) jwt() (string, error) {
	now := a.clock()
	enc := func(v any) string {
		data, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(data)
	}
	unsigned := enc(map[string]string{"alg": "RS256", "typ": "JWT"}) + "." +
		enc(map[string]any{"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(9 * time.Minute).Unix(), "iss": fmt.Sprint(a.appID)})
	sum := sha256.Sum256([]byte(unsigned))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", errors.Join(errors.New("github app: signing the JWT"), err)
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}
