package web

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Rightbracket/usb-gha-runner/ghr/internal/auth"
	"github.com/Rightbracket/usb-gha-runner/ghr/internal/config"
)

// fakeAgent records what ghr-web sends to the root agent.
type fakeAgent struct {
	mu     sync.Mutex
	config config.Config
	ssh    config.SSH
	calls  []string
	// prune, when set, answers POST /v1/prune without holding mu.
	prune http.HandlerFunc
}

func startFakeAgent(t *testing.T) (*fakeAgent, string) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "agent.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	fa := &fakeAgent{config: config.Config{Workers: "auto", Auth: config.Auth{Type: "app"}}}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/prune" && fa.prune != nil {
			fa.prune(w, r)
			return
		}
		fa.mu.Lock()
		defer fa.mu.Unlock()
		fa.calls = append(fa.calls, r.Method+" "+r.URL.Path)
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/config":
			json.NewEncoder(w).Encode(fa.config.Redacted())
		case "PUT /v1/config":
			var c config.Config
			json.NewDecoder(r.Body).Decode(&c)
			if c.URL == "https://github.com/bad" {
				w.WriteHeader(400)
				io.WriteString(w, `{"error":"GitHub rejected the settings: 401"}`)
				return
			}
			fa.config = c
			io.WriteString(w, "{}")
		case "PUT /v1/ssh":
			json.NewDecoder(r.Body).Decode(&fa.ssh)
			io.WriteString(w, "{}")
		case "GET /v1/status":
			io.WriteString(w, `{"configured":false,"disks_ok":true}`)
		default:
			io.WriteString(w, "{}")
		}
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return fa, sock
}

func newTestServer(t *testing.T) (*httptest.Server, *fakeAgent, *http.Client) {
	t.Helper()
	ts, fa, c, _ := newTestServerS(t)
	return ts, fa, c
}

func newTestServerS(t *testing.T) (*httptest.Server, *fakeAgent, *http.Client, *Server) {
	t.Helper()
	fa, sock := startFakeAgent(t)
	st := auth.NewStore(filepath.Join(t.TempDir(), "admin.json"))
	s, err := New(st, sock, "test", "ghr-test", "AA:BB")
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewTLSServer(s.Handler())
	t.Cleanup(ts.Close)
	jar, _ := cookiejar.New(nil)
	c := ts.Client()
	c.Jar = jar
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return ts, fa, c, s
}

func post(t *testing.T, c *http.Client, u string, v url.Values) *http.Response {
	t.Helper()
	resp, err := c.PostForm(u, v)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func TestFirstVisitorFlow(t *testing.T) {
	ts, fa, c := newTestServer(t)

	// Everything redirects to setup until a password exists.
	resp, _ := c.Get(ts.URL + "/config")
	if loc := resp.Header.Get("Location"); loc != "/setup" {
		t.Fatalf("expected redirect to /setup, got %q", loc)
	}
	resp, _ = c.Get(ts.URL + "/api/health")
	var h map[string]any
	json.NewDecoder(resp.Body).Decode(&h)
	if h["password_set"] != false {
		t.Fatalf("health %v", h)
	}

	if r := post(t, c, ts.URL+"/setup", url.Values{"password": {"short"}, "confirm": {"short"}}); !strings.Contains(r.Header.Get("Location"), "err=") {
		t.Fatalf("short password accepted: %s", r.Header.Get("Location"))
	}
	r := post(t, c, ts.URL+"/setup", url.Values{"password": {"correct horse"}, "confirm": {"correct horse"}})
	if !strings.HasPrefix(r.Header.Get("Location"), "/config") {
		t.Fatalf("setup: %s", r.Header.Get("Location"))
	}

	// A second visitor can't take over.
	other := &http.Client{Transport: c.Transport, CheckRedirect: c.CheckRedirect}
	r = post(t, other, ts.URL+"/setup", url.Values{"password": {"evil password"}, "confirm": {"evil password"}})
	if r.Header.Get("Location") != "/login" {
		t.Fatalf("second setup should bounce to login, got %s", r.Header.Get("Location"))
	}

	// Logged in: the config page renders and saving reaches the agent.
	resp, _ = c.Get(ts.URL + "/config")
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(body), "GitHub registration") {
		t.Fatalf("config page %d", resp.StatusCode)
	}
	r = post(t, c, ts.URL+"/config", url.Values{
		"url": {"https://github.com/acme/app"}, "workers": {"AUTO"}, "labels": {"gpu, big"},
		"auth_type": {"pat"}, "pat": {"github_pat_x"},
	})
	if r.Header.Get("Location") == "" || strings.Contains(r.Header.Get("Location"), "err=") {
		t.Fatalf("save config: %s", r.Header.Get("Location"))
	}
	fa.mu.Lock()
	got := fa.config
	fa.mu.Unlock()
	if got.URL != "https://github.com/acme/app" || got.Workers != "auto" || got.Auth.PAT != "github_pat_x" || len(got.Labels) != 2 {
		t.Fatalf("agent got %+v", got)
	}
	r = post(t, c, ts.URL+"/config", url.Values{"url": {"https://github.com/bad"}, "auth_type": {"pat"}, "pat": {"x"}})
	if !strings.Contains(r.Header.Get("Location"), "GitHub+rejected") {
		t.Fatalf("agent error not shown: %s", r.Header.Get("Location"))
	}

	// SSH toggle.
	post(t, c, ts.URL+"/config/ssh", url.Values{"enabled": {"on"}, "authorized_keys": {"ssh-ed25519 AAAA me"}})
	if !fa.ssh.Enabled || fa.ssh.AuthorizedKeys != "ssh-ed25519 AAAA me" {
		t.Fatalf("ssh %+v", fa.ssh)
	}

	// Status API is proxied for logged-in users only.
	resp, _ = c.Get(ts.URL + "/api/status")
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	resp, _ = other.Get(ts.URL + "/api/status")
	if resp.StatusCode != 401 {
		t.Fatalf("anonymous status %d", resp.StatusCode)
	}
}

func TestCrossOriginRefused(t *testing.T) {
	ts, _, c := newTestServer(t)
	req, _ := http.NewRequest("POST", ts.URL+"/setup", strings.NewReader("password=aaaaaaaaaaaa&confirm=aaaaaaaaaaaa"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://evil.example")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 403 {
		t.Fatalf("cross-origin setup got %d", resp.StatusCode)
	}
}

func TestLoginRateLimit(t *testing.T) {
	ts, _, c := newTestServer(t)
	post(t, c, ts.URL+"/setup", url.Values{"password": {"correct horse"}, "confirm": {"correct horse"}})
	anon := &http.Client{Transport: c.Transport, CheckRedirect: c.CheckRedirect}
	for i := 0; i < 5; i++ {
		post(t, anon, ts.URL+"/login", url.Values{"password": {"nope"}})
	}
	r := post(t, anon, ts.URL+"/login", url.Values{"password": {"correct horse"}})
	if !strings.Contains(r.Header.Get("Location"), "Too+many") {
		t.Fatalf("expected rate limit, got %s", r.Header.Get("Location"))
	}
}

func TestEnsureCertStable(t *testing.T) {
	dir := t.TempDir()
	_, _, fp1, err := EnsureCert(dir, "ghr-abcd")
	if err != nil {
		t.Fatal(err)
	}
	_, _, fp2, _ := EnsureCert(dir, "ghr-abcd")
	if fp1 != fp2 || len(fp1) != 95 {
		t.Fatalf("fingerprints %s %s", fp1, fp2)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "fingerprint.txt")); strings.TrimSpace(string(b)) != fp1 {
		t.Fatal("fingerprint.txt mismatch")
	}
}

func TestLoginRateLimitConcurrent(t *testing.T) {
	ts, _, c := newTestServer(t)
	post(t, c, ts.URL+"/setup", url.Values{"password": {"correct horse"}, "confirm": {"correct horse"}})
	anon := &http.Client{Transport: c.Transport, CheckRedirect: c.CheckRedirect}
	var wg sync.WaitGroup
	var mu sync.Mutex
	wrong := 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := anon.PostForm(ts.URL+"/login", url.Values{"password": {"nope"}})
			if err != nil {
				return
			}
			r.Body.Close()
			if strings.Contains(r.Header.Get("Location"), "Wrong+password") {
				mu.Lock()
				wrong++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wrong > 5 {
		t.Fatalf("%d of 20 parallel guesses were checked; the limit is 5", wrong)
	}
}

func TestSetupLockedByBadPreset(t *testing.T) {
	fa, sock := startFakeAgent(t)
	_ = fa
	d := t.TempDir()
	lock := filepath.Join(d, "admin-password-error")
	os.WriteFile(lock, []byte("password must be at least 10 characters\n"), 0o644)
	s, err := New(auth.NewStore(filepath.Join(d, "admin.json")), sock, "test", "ghr-test", "AA:BB")
	if err != nil {
		t.Fatal(err)
	}
	s.SetupLockFile = lock
	ts := httptest.NewTLSServer(s.Handler())
	defer ts.Close()
	c := ts.Client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	r := post(t, c, ts.URL+"/setup", url.Values{"password": {"attacker pw!"}, "confirm": {"attacker pw!"}})
	if !strings.Contains(r.Header.Get("Location"), "Setup+is+locked") || s.Auth.HasPassword() {
		t.Fatalf("setup not locked: %s", r.Header.Get("Location"))
	}
}

func TestPruneMessages(t *testing.T) {
	for _, tc := range []struct {
		name  string
		agent http.HandlerFunc
		want  string
	}{
		{"slow prune", func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() },
			"msg=Pruning+is+taking+a+while+and+continues+in+the+background"},
		{"mirror restart failed", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(500)
			io.WriteString(w, `{"error":"the registry mirror didn't start again after pruning"}`)
		}, "err=the+registry+mirror+didn%27t+start+again"},
		{"already running", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(409)
			io.WriteString(w, `{"error":"a prune is already running; try again when it has finished"}`)
		}, "err=a+prune+is+already+running"},
		{"done", func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"before_pct":91,"after_pct":40}`)
		}, "msg=Pruned+caches%3A+%2Fdata+went+from+91%25+to+40%25+full."},
	} {
		ts, fa, c, s := newTestServerS(t)
		fa.prune = tc.agent
		if tc.name == "slow prune" {
			s.agent.Timeout = 50 * time.Millisecond
		}
		post(t, c, ts.URL+"/setup", url.Values{"password": {"correct horse"}, "confirm": {"correct horse"}})
		r := post(t, c, ts.URL+"/actions/prune", nil)
		if loc := r.Header.Get("Location"); r.StatusCode != http.StatusSeeOther || !strings.Contains(loc, tc.want) {
			t.Errorf("%s: %d %q, want %q", tc.name, r.StatusCode, loc, tc.want)
		}
	}
}
