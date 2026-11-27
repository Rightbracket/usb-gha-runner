package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Rightbracket/usb-gha-runner/ghr/internal/config"
	"github.com/Rightbracket/usb-gha-runner/ghr/internal/gh"
	"github.com/Rightbracket/usb-gha-runner/ghr/internal/prune"
)

// fakeGitHub answers the runner API for repo acme/app. Only the PAT "good"
// is accepted; deleteStatus, when set, answers every DELETE.
type fakeGitHub struct {
	srv          *httptest.Server
	mu           sync.Mutex
	runners      map[int64]string
	deleted      []string
	requests     int
	deleteStatus int
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	f := &fakeGitHub{runners: map[int64]string{}}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.requests++
		if r.Header.Get("Authorization") != "Bearer good" {
			http.Error(w, `{"message":"Bad credentials"}`, 401)
			return
		}
		const base = "/api/v3/repos/acme/app/actions/runners"
		switch {
		case r.Method == "GET" && r.URL.Path == base:
			var rs []map[string]any
			for id, n := range f.runners {
				rs = append(rs, map[string]any{"id": id, "name": n})
			}
			json.NewEncoder(w).Encode(map[string]any{"total_count": len(rs), "runners": rs})
		case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, base+"/"):
			if f.deleteStatus != 0 {
				w.WriteHeader(f.deleteStatus)
				return
			}
			id, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, base+"/"), 10, 64)
			f.deleted = append(f.deleted, f.runners[id])
			delete(f.runners, id)
			w.WriteHeader(204)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHub) url() string { return f.srv.URL + "/acme/app" }

func (f *fakeGitHub) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}

type testAgent struct {
	*Agent
	path   string
	killed int
	// configuredAtStop is whether the saved config was still configured
	// when remove asked the supervisor to stop every slot.
	configuredAtStop []bool
}

func newTestAgent(t *testing.T, f *fakeGitHub, saved *config.Config) *testAgent {
	ta := &testAgent{path: filepath.Join(t.TempDir(), "config.json")}
	ta.Agent = &Agent{
		configPath: ta.path,
		runnerHost: "ghr-3f9a-0123abcd",
		newClient: func(c *config.Config) (*gh.Client, error) {
			return gh.NewWithTransport(c, f.srv.Client().Transport)
		},
		killSlots: func() { ta.killed++ },
	}
	ta.stopAll = func(time.Duration) error {
		c, err := config.Load(ta.path)
		ta.configuredAtStop = append(ta.configuredAtStop, err == nil && c.Configured())
		return nil
	}
	if saved != nil {
		if err := config.Save(ta.path, saved); err != nil {
			t.Fatal(err)
		}
	}
	return ta
}

func (ta *testAgent) do(t *testing.T, method, path, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	w := httptest.NewRecorder()
	ta.Handler().ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

func (ta *testAgent) saved(t *testing.T) *config.Config {
	t.Helper()
	c, err := config.Load(ta.path)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestPutConfigRejectsBadInput(t *testing.T) {
	f := newFakeGitHub(t)
	before := &config.Config{URL: f.url(), Workers: "1", Auth: config.Auth{Type: "pat", PAT: "good"}}
	for _, tc := range []struct {
		name, body, wantErr string
		reachesGitHub       bool
	}{
		{"not JSON", "{", "bad config", false},
		{"invalid", mustJSON(config.Config{URL: "http://github.com/acme", Auth: config.Auth{Type: "pat", PAT: "good"}}), "https only", false},
		{"credential rejected", mustJSON(config.Config{URL: f.url(), Workers: "2", Auth: config.Auth{Type: "pat", PAT: "bad"}}), "GitHub rejected the settings", true},
	} {
		ta := newTestAgent(t, f, before)
		reqs := f.requestCount()
		w, out := ta.do(t, "PUT", "/v1/config", tc.body)
		if w.Code != 400 || !strings.Contains(out["error"].(string), tc.wantErr) {
			t.Errorf("%s: %d %v, want 400 with %q", tc.name, w.Code, out, tc.wantErr)
		}
		if got := f.requestCount() > reqs; got != tc.reachesGitHub {
			t.Errorf("%s: asked GitHub = %v, want %v", tc.name, got, tc.reachesGitHub)
		}
		if c := ta.saved(t); c.Workers != "1" || c.Auth.PAT != "good" {
			t.Errorf("%s: saved config changed to %+v", tc.name, c)
		}
	}
}

func TestPutConfigKeepsSecretAndSSH(t *testing.T) {
	f := newFakeGitHub(t)
	ssh := config.SSH{Enabled: true, AuthorizedKeys: "ssh-ed25519 AAAA test"}
	ta := newTestAgent(t, f, &config.Config{URL: f.url(), Workers: "1", Auth: config.Auth{Type: "pat", PAT: "good"}, SSH: ssh})
	// The page sends the redacted PAT back, and must not change SSH here.
	in := config.Config{URL: f.url(), Workers: "3", Labels: []string{"gpu"}, Auth: config.Auth{Type: "pat", PAT: "********"}}
	w, out := ta.do(t, "PUT", "/v1/config", mustJSON(in))
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if out["auth"].(map[string]any)["pat"] != "********" {
		t.Errorf("response leaks the PAT: %v", out["auth"])
	}
	c := ta.saved(t)
	if c.Auth.PAT != "good" || c.Workers != "3" || len(c.Labels) != 1 || c.SSH != ssh {
		t.Errorf("saved %+v", c)
	}
	if fi, _ := os.Stat(ta.path); fi.Mode().Perm() != 0o640 {
		t.Errorf("config mode %v", fi.Mode().Perm())
	}
}

func TestRemoveDeregistersOnlyThisMachine(t *testing.T) {
	f := newFakeGitHub(t)
	f.runners = map[int64]string{
		1: "build-ghr-3f9a-0123abcd-1",
		2: "build-ghr-3f9a-0123abcd-2",
		3: "build-ghr-3f9a-89abcdef-1", // another stick with the same MAC suffix
		4: "build-ghr-3f9a-0123abcd-x",
		5: "laptop",
	}
	ssh := config.SSH{Enabled: true, AuthorizedKeys: "ssh-ed25519 AAAA test"}
	ta := newTestAgent(t, f, &config.Config{URL: f.url(), NamePrefix: "build", Workers: "2", Auth: config.Auth{Type: "pat", PAT: "good"}, SSH: ssh})
	w, out := ta.do(t, "POST", "/v1/remove", "")
	if w.Code != 200 || out["ok"] != true {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	sort.Strings(f.deleted)
	if strings.Join(f.deleted, ",") != "build-ghr-3f9a-0123abcd-1,build-ghr-3f9a-0123abcd-2" {
		t.Errorf("deleted %v", f.deleted)
	}
	// The cleared config is saved before the slots are stopped, so the
	// supervisor can't start new ones behind remove's back.
	if len(ta.configuredAtStop) != 1 || ta.configuredAtStop[0] {
		t.Errorf("StopAll calls (configured at the time): %v, want [false]", ta.configuredAtStop)
	}
	if ta.killed != 1 {
		t.Errorf("slot containers removed %d times, want 1", ta.killed)
	}
	c := ta.saved(t)
	if c.Configured() || c.URL != "" || c.Auth.PAT != "" || c.SSH != ssh {
		t.Errorf("after remove, config is %+v", c)
	}
}

func TestRemoveReportsGitHubErrors(t *testing.T) {
	f := newFakeGitHub(t)
	f.runners = map[int64]string{1: "ghr-3f9a-0123abcd-1"}
	f.deleteStatus = 500
	ta := newTestAgent(t, f, &config.Config{URL: f.url(), Workers: "1", Auth: config.Auth{Type: "pat", PAT: "good"}})
	w, out := ta.do(t, "POST", "/v1/remove", "")
	errs, _ := out["errors"].([]any)
	if w.Code != 200 || out["ok"] != false || len(errs) != 1 || !strings.Contains(errs[0].(string), "500") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	// The settings are cleared even so.
	if c := ta.saved(t); c.Configured() {
		t.Errorf("config still configured: %+v", c)
	}
}

func TestRemoveUnconfigured(t *testing.T) {
	f := newFakeGitHub(t)
	ta := newTestAgent(t, f, nil)
	w, out := ta.do(t, "POST", "/v1/remove", "")
	if w.Code != 200 || out["ok"] != true || f.requestCount() != 0 {
		t.Fatalf("%d %s (GitHub requests %d)", w.Code, w.Body, f.requestCount())
	}
}

func TestPruneStatusCodes(t *testing.T) {
	f := newFakeGitHub(t)
	for _, tc := range []struct {
		name    string
		res     *prune.Result
		err     error
		code    int
		wantErr string
	}{
		{"busy", nil, prune.ErrBusy, 409, "a prune is already running"},
		{"mirror down", &prune.Result{BeforePct: 91, AfterPct: 40}, errors.New("the registry mirror didn't start again"),
			500, "/data went from 91% to 40% full, but the registry mirror didn't start again"},
		{"ok", &prune.Result{BeforePct: 91, AfterPct: 40}, nil, 200, ""},
	} {
		ta := newTestAgent(t, f, nil)
		ta.prune = func(ctx context.Context, threshold float64) (*prune.Result, error) {
			if threshold != 0 {
				t.Errorf("%s: threshold %v, want 0 (always)", tc.name, threshold)
			}
			return tc.res, tc.err
		}
		// The web UI's request can time out; the prune must carry on.
		reqCtx, cancel := context.WithCancel(context.Background())
		var pruneErr error
		inner := ta.prune
		ta.prune = func(ctx context.Context, threshold float64) (*prune.Result, error) {
			cancel()
			pruneErr = ctx.Err()
			return inner(ctx, threshold)
		}
		w := httptest.NewRecorder()
		ta.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/v1/prune", nil).WithContext(reqCtx))
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		if pruneErr != nil {
			t.Errorf("%s: prune's context ended with the request: %v", tc.name, pruneErr)
		}
		if w.Code != tc.code {
			t.Errorf("%s: %d %s, want %d", tc.name, w.Code, w.Body, tc.code)
		}
		if tc.wantErr != "" && !strings.Contains(fmt.Sprint(out["error"]), tc.wantErr) {
			t.Errorf("%s: error %v, want %q", tc.name, out["error"], tc.wantErr)
		}
		if tc.code == 200 && out["after_pct"] != 40.0 {
			t.Errorf("%s: body %v", tc.name, out)
		}
	}
}
