// Package gh is a minimal GitHub REST client for self-hosted runner
// registration: GitHub App auth, just-in-time runner configs, and cleanup.
package gh

import (
	"bytes"
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
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Rightbracket/usb-gha-runner/ghr/internal/config"
)

type Client struct {
	cfg    *config.Config
	target config.Target
	http   *http.Client

	mu       sync.Mutex
	token    string
	tokenExp time.Time
}

func New(cfg *config.Config) (*Client, error) {
	t, err := cfg.Target()
	if err != nil {
		return nil, err
	}
	return &Client{cfg: cfg, target: t, http: &http.Client{Timeout: 30 * time.Second, CheckRedirect: sameOriginRedirect}}, nil
}

// sameOriginRedirect stops the credential from following a redirect to
// plain HTTP or to another host.
func sameOriginRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return errors.New("too many redirects")
	}
	if req.URL.Scheme != "https" || !strings.EqualFold(req.URL.Host, via[0].URL.Host) {
		return fmt.Errorf("refusing redirect to %s://%s", req.URL.Scheme, req.URL.Host)
	}
	return nil
}

// APIError carries the HTTP status so callers can react to 404/409/422.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string { return fmt.Sprintf("github api: %d: %s", e.Status, e.Body) }

func IsStatus(err error, code int) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == code
}

func (c *Client) authToken(ctx context.Context) (string, error) {
	if c.cfg.Auth.Type == "pat" {
		return c.cfg.Auth.PAT, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Until(c.tokenExp) > 5*time.Minute {
		return c.token, nil
	}
	jwt, err := AppJWT(c.cfg.Auth.AppID, c.cfg.Auth.PrivateKey, time.Now())
	if err != nil {
		return "", err
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	p := "/app/installations/" + url.PathEscape(c.cfg.Auth.InstallationID) + "/access_tokens"
	if err := c.do(ctx, "Bearer "+jwt, http.MethodPost, p, nil, &out); err != nil {
		return "", fmt.Errorf("app installation token: %w", err)
	}
	c.token, c.tokenExp = out.Token, out.ExpiresAt
	return c.token, nil
}

func (c *Client) call(ctx context.Context, method, path string, body, out any) error {
	tok, err := c.authToken(ctx)
	if err != nil {
		return err
	}
	return c.do(ctx, "Bearer "+tok, method, path, body, out)
}

func (c *Client) do(ctx context.Context, auth, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.target.APIBase+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "ghr-usb-runner")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return &APIError{Status: resp.StatusCode, Body: string(b)}
	}
	if out != nil && len(b) > 0 {
		return json.Unmarshal(b, out)
	}
	return nil
}

func (c *Client) runnersPath() string {
	if c.target.Scope == "org" {
		return "/orgs/" + url.PathEscape(c.target.Owner) + "/actions/runners"
	}
	return "/repos/" + url.PathEscape(c.target.Owner) + "/" + url.PathEscape(c.target.Repo) + "/actions/runners"
}

type Runner struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Busy   bool   `json:"busy"`
}

// JITConfig requests a just-in-time runner configuration for one job.
func (c *Client) JITConfig(ctx context.Context, name string, labels []string) (encoded string, runner Runner, err error) {
	groupID, err := c.runnerGroupID(ctx)
	if err != nil {
		return "", Runner{}, err
	}
	req := map[string]any{"name": name, "runner_group_id": groupID, "labels": labels, "work_folder": "_work"}
	var out struct {
		Runner           Runner `json:"runner"`
		EncodedJITConfig string `json:"encoded_jit_config"`
	}
	if err := c.call(ctx, http.MethodPost, c.runnersPath()+"/generate-jitconfig", req, &out); err != nil {
		return "", Runner{}, err
	}
	return out.EncodedJITConfig, out.Runner, nil
}

func (c *Client) runnerGroupID(ctx context.Context) (int64, error) {
	if c.target.Scope != "org" || c.cfg.RunnerGroup == "" {
		return 1, nil // the Default group
	}
	if id, err := strconv.ParseInt(c.cfg.RunnerGroup, 10, 64); err == nil {
		return id, nil
	}
	var out struct {
		Groups []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"runner_groups"`
	}
	p := "/orgs/" + url.PathEscape(c.target.Owner) + "/actions/runner-groups?per_page=100"
	if err := c.call(ctx, http.MethodGet, p, nil, &out); err != nil {
		return 0, err
	}
	for _, g := range out.Groups {
		if g.Name == c.cfg.RunnerGroup {
			return g.ID, nil
		}
	}
	return 0, fmt.Errorf("runner group %q not found", c.cfg.RunnerGroup)
}

// FindRunner looks a runner up by exact name.
func (c *Client) FindRunner(ctx context.Context, name string) (*Runner, error) {
	var out struct {
		Runners []Runner `json:"runners"`
	}
	if err := c.call(ctx, http.MethodGet, c.runnersPath()+"?per_page=100&name="+url.QueryEscape(name), nil, &out); err != nil {
		return nil, err
	}
	for _, r := range out.Runners {
		if r.Name == name {
			r := r
			return &r, nil
		}
	}
	return nil, nil
}

func (c *Client) DeleteRunner(ctx context.Context, id int64) error {
	err := c.call(ctx, http.MethodDelete, c.runnersPath()+"/"+strconv.FormatInt(id, 10), nil, nil)
	if IsStatus(err, 404) {
		return nil
	}
	return err
}

// AppJWT builds the RS256 JWT a GitHub App uses to request installation tokens.
func AppJWT(appID, pemKey string, now time.Time) (string, error) {
	block, _ := pem.Decode([]byte(pemKey))
	if block == nil {
		return "", errors.New("app private key is not PEM")
	}
	var key *rsa.PrivateKey
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		key = k
	} else if k8, err8 := x509.ParsePKCS8PrivateKey(block.Bytes); err8 == nil {
		rk, ok := k8.(*rsa.PrivateKey)
		if !ok {
			return "", errors.New("app private key is not RSA")
		}
		key = rk
	} else {
		return "", fmt.Errorf("parse app private key: %w", err)
	}
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{
		"iat": now.Add(-60 * time.Second).Unix(), // clock-skew allowance
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": appID,
	})
	signing := header + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + enc.EncodeToString(sig), nil
}

// Check confirms the credential works and can manage runners for the target,
// by listing runners (the same permission JIT configs need).
func (c *Client) Check(ctx context.Context) error {
	var out struct {
		Total int `json:"total_count"`
	}
	if err := c.call(ctx, http.MethodGet, c.runnersPath()+"?per_page=1", nil, &out); err != nil {
		if IsStatus(err, 403) || IsStatus(err, 404) {
			return fmt.Errorf("the credential can't manage self-hosted runners for %s (needs Administration: write on the repo, or Self-hosted runners: write on the org): %w", c.cfg.URL, err)
		}
		return err
	}
	_, err := c.runnerGroupID(ctx)
	return err
}

// DeleteSlotRunners removes the runners this machine registers, named
// exactly "<base>-<slot number>", and returns how many were removed. Other
// runners that merely share the start of the name are left alone.
func (c *Client) DeleteSlotRunners(ctx context.Context, base string) (int, error) {
	re := regexp.MustCompile("^" + regexp.QuoteMeta(base) + "-[0-9]+$")
	var victims []Runner
	for page := 1; page <= 50; page++ {
		var out struct {
			Runners []Runner `json:"runners"`
		}
		p := c.runnersPath() + "?per_page=100&page=" + strconv.Itoa(page)
		if err := c.call(ctx, http.MethodGet, p, nil, &out); err != nil {
			return 0, err
		}
		for _, r := range out.Runners {
			if re.MatchString(r.Name) {
				victims = append(victims, r)
			}
		}
		if len(out.Runners) < 100 {
			break
		}
	}
	n := 0
	var firstErr error
	for _, r := range victims {
		if err := c.DeleteRunner(ctx, r.ID); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		n++
	}
	return n, firstErr
}
