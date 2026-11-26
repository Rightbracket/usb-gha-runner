package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTarget(t *testing.T) {
	cases := []struct {
		url, scope, owner, repo, api string
		bad                          bool
	}{
		{"https://github.com/acme/app", "repo", "acme", "app", "https://api.github.com", false},
		{"https://github.com/acme/app.git/", "repo", "acme", "app", "https://api.github.com", false},
		{"https://github.com/acme", "org", "acme", "", "https://api.github.com", false},
		{"https://ghe.example.com/acme", "org", "acme", "", "https://ghe.example.com/api/v3", false},
		{"github.com/acme", "", "", "", "", true},
		{"https://github.com/", "", "", "", "", true},
		{"https://github.com/a/b/c", "", "", "", "", true},
	}
	for _, c := range cases {
		tg, err := (&Config{URL: c.url}).Target()
		if c.bad {
			if err == nil {
				t.Errorf("%s: expected error", c.url)
			}
			continue
		}
		if err != nil || tg.Scope != c.scope || tg.Owner != c.owner || tg.Repo != c.repo || tg.APIBase != c.api {
			t.Errorf("%s: got %+v, %v", c.url, tg, err)
		}
	}
}

func TestAutoWorkers(t *testing.T) {
	cases := []struct{ threads, memMB, want int }{
		{16, 64 * 1024, 8}, // CPU bound
		{16, 15800, 4},     // a "16 GB" box reads a bit under 16 GiB
		{4, 8 * 1024, 2},
		{2, 2048, 1},           // never below 1
		{128, 1024 * 1024, 32}, // never above 32
	}
	for _, c := range cases {
		if got := AutoWorkers(c.threads, c.memMB); got != c.want {
			t.Errorf("AutoWorkers(%d, %d) = %d, want %d", c.threads, c.memMB, got, c.want)
		}
	}
}

func TestValidateWorkers(t *testing.T) {
	base := Config{URL: "https://github.com/acme", Auth: Auth{Type: "pat", PAT: "x"}}
	for _, w := range []string{"auto", "", "1", "32", "AUTO"} {
		c := base
		c.Workers = w
		if err := c.Validate(); err != nil {
			t.Errorf("workers=%q: %v", w, err)
		}
	}
	for _, w := range []string{"0", "33", "-1", "two"} {
		c := base
		c.Workers = w
		if c.Validate() == nil {
			t.Errorf("workers=%q should be rejected", w)
		}
	}
}

func TestEnvRoundTrip(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "app-key.pem"), []byte("-----BEGIN RSA PRIVATE KEY-----\nabc\n-----END RSA PRIVATE KEY-----\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "runner.env"), []byte("\ufeff# comment\nGHR_URL=\"https://github.com/acme/app\"\nexport GHR_LABELS=gpu, big\nGHR_WORKERS=3\nGHR_APP_ID=123\nGHR_APP_INSTALLATION_ID=456\nGHR_APP_PRIVATE_KEY_FILE=app-key.pem\r\n"), 0o600)
	env, err := ParseEnvFile(filepath.Join(dir, "runner.env"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := FromEnv(env, dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.URL != "https://github.com/acme/app" || c.Workers != "3" || c.Auth.Type != "app" || !strings.Contains(c.Auth.PrivateKey, "abc") {
		t.Fatalf("unexpected config %+v", c)
	}
	if got := strings.Join(c.AllLabels(), ","); got != "self-hosted,linux,x64,usb-live,gpu,big" {
		t.Errorf("labels = %s", got)
	}
	out := c.ToEnv(false)
	if strings.Contains(out, "abc") {
		t.Error("exported runner.env leaked the private key")
	}
	if !strings.Contains(out, "GHR_APP_ID=123") {
		t.Errorf("export missing app id:\n%s", out)
	}
}

func TestRedactAndMerge(t *testing.T) {
	old := &Config{URL: "https://github.com/acme/app", Auth: Auth{Type: "pat", PAT: "secret"}}
	r := old.Redacted()
	if r.Auth.PAT == "secret" || old.Auth.PAT != "secret" {
		t.Fatal("redaction wrong")
	}
	in := &Config{URL: "https://github.com/acme/other", Auth: Auth{Type: "pat", PAT: "********"}}
	in.MergeSecrets(old)
	if in.Auth.PAT != "secret" {
		t.Fatal("placeholder should keep the stored secret for the same host")
	}
}

func TestMergeSecretsNotToAnotherHost(t *testing.T) {
	old := &Config{URL: "https://github.com/acme", Auth: Auth{Type: "app", AppID: "1", PrivateKey: "KEY", PAT: "secret"}}
	for name, in := range map[string]*Config{
		"other host":  {URL: "https://evil.example/acme", Auth: Auth{Type: "app", AppID: "1"}},
		"other type":  {URL: "https://github.com/acme", Auth: Auth{Type: "pat"}},
		"other app":   {URL: "https://github.com/acme", Auth: Auth{Type: "app", AppID: "2"}},
		"placeholder": {URL: "https://evil.example/acme", Auth: Auth{Type: "app", AppID: "1", PrivateKey: "********"}},
	} {
		in.MergeSecrets(old)
		if in.Auth.PrivateKey != "" || in.Auth.PAT != "" {
			t.Errorf("%s: reused a saved secret: %+v", name, in.Auth)
		}
	}
}

func TestTargetHTTPSOnly(t *testing.T) {
	if _, err := (&Config{URL: "http://github.com/acme"}).Target(); err == nil {
		t.Error("http URL accepted")
	}
	if _, err := (&Config{URL: "https://ghe.example.com/acme/app"}).Target(); err != nil {
		t.Error(err)
	}
}

func TestRunnerBase(t *testing.T) {
	if got := (&Config{}).RunnerBase("ghr-3f9a"); got != "ghr-3f9a" {
		t.Error(got)
	}
	if got := (&Config{NamePrefix: "build"}).RunnerBase("ghr-3f9a"); got != "build-ghr-3f9a" {
		t.Error(got)
	}
}

func TestLoadSave(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "config.json")
	c, err := Load(p)
	if err != nil || c.Workers != "auto" {
		t.Fatalf("missing file should give defaults: %+v %v", c, err)
	}
	c.URL = "https://github.com/acme"
	if err := Save(p, c); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(p)
	if fi.Mode().Perm() != 0o640 {
		t.Errorf("mode %v", fi.Mode())
	}
	c2, _ := Load(p)
	if c2.URL != c.URL {
		t.Error("round trip failed")
	}
}
