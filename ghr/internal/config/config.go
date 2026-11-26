// Package config holds the persistent runner configuration stored on the
// stick's data partition, plus the import path for a runner.env drop-in file.
package config

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Default locations on the running system.
const (
	DataDir     = "/data"
	EtcDir      = "/data/etc/ghr"
	ConfigPath  = "/data/etc/ghr/config.json"
	RunDir      = "/run/ghr"
	SlotsDir    = "/run/ghr/slots"
	HistoryPath = "/data/ghr/history.jsonl"
	BootDiskRun = "/run/ghr/boot-disk"
	// MaxPrefix keeps PREFIX-ghr-XXXX-<stick ID>-NN within GitHub's and
	// Linux's 64-character limits.
	MaxPrefix = 40
	StickIDPath = "/data/etc/ghr/stick-id"
	MaxWorkers  = 32
)

type Auth struct {
	// Type is "app" (GitHub App, recommended) or "pat" (fine-grained PAT).
	Type           string `json:"type"`
	PAT            string `json:"pat,omitempty"`
	AppID          string `json:"app_id,omitempty"`
	InstallationID string `json:"installation_id,omitempty"`
	PrivateKey     string `json:"private_key,omitempty"`
}

type SSH struct {
	Enabled        bool   `json:"enabled"`
	AuthorizedKeys string `json:"authorized_keys,omitempty"`
}

type Config struct {
	// URL is https://github.com/<owner> (org scope) or
	// https://github.com/<owner>/<repo> (repo scope).
	URL          string   `json:"url"`
	RunnerGroup  string   `json:"runner_group,omitempty"` // org scope only; default group when empty
	NamePrefix   string   `json:"name_prefix,omitempty"`
	Labels       []string `json:"labels,omitempty"`
	Workers      string   `json:"workers"`                  // "auto" or "1".."32"
	SlotCPUs     float64  `json:"slot_cpus,omitempty"`      // 0 = even split
	SlotMemoryMB int      `json:"slot_memory_mb,omitempty"` // 0 = even split
	Auth         Auth     `json:"auth"`
	SSH          SSH      `json:"ssh"`
}

// AlwaysLabels are added to every runner regardless of configuration.
var AlwaysLabels = []string{"self-hosted", "linux", "x64", "usb-live"}

// Target describes where runners register.
type Target struct {
	Scope   string // "repo" or "org"
	Owner   string
	Repo    string
	APIBase string // e.g. https://api.github.com
}

func (c *Config) Target() (Target, error) {
	u, err := url.Parse(strings.TrimSpace(c.URL))
	if err != nil || u.Host == "" || u.Scheme != "https" {
		return Target{}, fmt.Errorf("url must look like https://github.com/<owner>[/<repo>] (https only, so the credential is never sent in clear text)")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	t := Target{APIBase: "https://api.github.com"}
	if !strings.EqualFold(u.Host, "github.com") {
		t.APIBase = "https://" + u.Host + "/api/v3" // GitHub Enterprise Server
	}
	switch {
	case len(parts) == 1 && parts[0] != "":
		t.Scope, t.Owner = "org", parts[0]
	case len(parts) == 2 && parts[0] != "" && parts[1] != "":
		t.Scope, t.Owner, t.Repo = "repo", parts[0], strings.TrimSuffix(parts[1], ".git")
	default:
		return Target{}, fmt.Errorf("url must look like https://github.com/<owner>[/<repo>]")
	}
	return t, nil
}

// AllLabels returns the configured labels plus the always-on ones, de-duplicated.
func (c *Config) AllLabels() []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range append(append([]string{}, AlwaysLabels...), c.Labels...) {
		l = strings.TrimSpace(l)
		if l == "" || seen[strings.ToLower(l)] {
			continue
		}
		seen[strings.ToLower(l)] = true
		out = append(out, l)
	}
	return out
}

// WorkerCount resolves Workers to a concrete number of slots.
func (c *Config) WorkerCount() int {
	w := strings.TrimSpace(strings.ToLower(c.Workers))
	if w == "" || w == "auto" {
		return AutoWorkers(runtime.NumCPU(), MemTotalMB())
	}
	n, err := strconv.Atoi(w)
	if err != nil || n < 1 {
		return 1
	}
	if n > MaxWorkers {
		return MaxWorkers
	}
	return n
}

// AutoWorkers = min(threads/2, RAM GB/4), at least 1, at most MaxWorkers.
// RAM is counted in decimal GB, because MemTotal on a "16 GB" machine reads a
// little under 16 GiB once firmware and the kernel take their share.
func AutoWorkers(threads int, memMB int) int {
	n := threads / 2
	memGB := int(int64(memMB) * 1 << 20 / 1e9)
	if m := memGB / 4; m < n {
		n = m
	}
	if n < 1 {
		n = 1
	}
	if n > MaxWorkers {
		n = MaxWorkers
	}
	return n
}

func (c *Config) Validate() error {
	if _, err := c.Target(); err != nil {
		return err
	}
	if len(c.NamePrefix) > MaxPrefix || strings.Trim(c.NamePrefix, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_") != "" {
		return fmt.Errorf("name prefix must be at most %d letters, digits, - or _", MaxPrefix)
	}
	w := strings.TrimSpace(strings.ToLower(c.Workers))
	if w != "" && w != "auto" {
		n, err := strconv.Atoi(w)
		if err != nil || n < 1 || n > MaxWorkers {
			return fmt.Errorf("workers must be auto or 1..%d", MaxWorkers)
		}
	}
	switch c.Auth.Type {
	case "pat":
		if c.Auth.PAT == "" {
			return errors.New("a personal access token is required")
		}
	case "app":
		if c.Auth.AppID == "" || c.Auth.InstallationID == "" || c.Auth.PrivateKey == "" {
			return errors.New("GitHub App needs app ID, installation ID and private key")
		}
	default:
		return errors.New("auth type must be app or pat")
	}
	return nil
}

// Configured reports whether a usable registration config exists.
func (c *Config) Configured() bool { return c != nil && c.Validate() == nil }

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &Config{Workers: "auto", Auth: Auth{Type: "app"}}, nil
	}
	if err != nil {
		return nil, err
	}
	c := &Config{}
	if err := json.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if c.Workers == "" {
		c.Workers = "auto"
	}
	return c, nil
}

// Save writes atomically with mode 0640 (secrets inside).
func Save(path string, c *Config) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, b, 0o640)
}

func WriteFileAtomic(path string, b []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Redacted returns a copy safe to send to the browser.
func (c *Config) Redacted() *Config {
	r := *c
	r.Labels = append([]string{}, c.Labels...)
	if r.Auth.PAT != "" {
		r.Auth.PAT = "********"
	}
	if r.Auth.PrivateKey != "" {
		r.Auth.PrivateKey = "********"
	}
	return &r
}

// MergeSecrets keeps existing secrets when the incoming value is empty or the
// redaction placeholder.
//
// Saved secrets are only reused when the GitHub host is unchanged and the
// credential is the same kind (and the same App), so a session that can't
// read the secrets also can't send them somewhere new. Otherwise blank secret
// fields stay blank and Validate asks for them again.
func (c *Config) MergeSecrets(old *Config) {
	blank := func(v string) bool { return v == "" || v == "********" }
	if blank(c.Auth.PAT) {
		c.Auth.PAT = ""
	}
	if blank(c.Auth.PrivateKey) {
		c.Auth.PrivateKey = ""
	}
	if old == nil || !sameHost(c.URL, old.URL) || c.Auth.Type != old.Auth.Type {
		return
	}
	switch c.Auth.Type {
	case "pat":
		if c.Auth.PAT == "" {
			c.Auth.PAT = old.Auth.PAT
		}
	case "app":
		if c.Auth.PrivateKey == "" && c.Auth.AppID == old.Auth.AppID {
			c.Auth.PrivateKey = old.Auth.PrivateKey
		}
	}
}

func sameHost(a, b string) bool {
	ua, err1 := url.Parse(strings.TrimSpace(a))
	ub, err2 := url.Parse(strings.TrimSpace(b))
	return err1 == nil && err2 == nil && ua.Host != "" &&
		strings.EqualFold(ua.Scheme, ub.Scheme) && strings.EqualFold(ua.Host, ub.Host)
}

// RunnerBase is the start of every runner name this machine registers:
// "<host>" or "<prefix>-<host>", where host is RunnerHost's result.
func (c *Config) RunnerBase(host string) string {
	if c.NamePrefix == "" || c.NamePrefix == host {
		return host
	}
	return c.NamePrefix + "-" + host
}

// RunnerHost is the per-stick part of runner names: the hostname
// (ghr-XXXX, only 16 bits of the MAC, shared by every VM with a default
// MAC) plus a random ID created once and kept on the stick at path, so
// sticks that share a prefix or a MAC suffix never claim each other's
// runners. If the ID can't be read or stored, it falls back to the
// hostname alone and returns the error.
func RunnerHost(hostname, path string) (string, error) {
	id, err := stickID(path)
	if err != nil {
		return hostname, err
	}
	return hostname + "-" + id, nil
}

func stickID(path string) (string, error) {
	if b, err := os.ReadFile(path); err == nil {
		id := strings.TrimSpace(string(b))
		if len(id) == 8 && isHex(id) {
			return id, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	id := hex.EncodeToString(buf)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(id+"\n"), 0o600); err != nil {
		return "", err
	}
	return id, os.Rename(tmp, path)
}

func isHex(s string) bool {
	for _, r := range s {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}

// ParseEnvFile reads runner.env (KEY=VALUE lines, # comments, optional quotes).
func ParseEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(strings.TrimPrefix(sc.Text(), "\ufeff"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		out[strings.TrimSpace(k)] = v
	}
	return out, sc.Err()
}

// FromEnv builds a Config from runner.env values. dir is the folder holding
// runner.env so that *_FILE paths can be relative to it.
func FromEnv(env map[string]string, dir string) (*Config, error) {
	readRel := func(p string) (string, error) {
		if p == "" {
			return "", nil
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		b, err := os.ReadFile(p)
		return strings.TrimSpace(string(b)), err
	}
	c := &Config{
		URL:         env["GHR_URL"],
		RunnerGroup: env["GHR_RUNNER_GROUP"],
		NamePrefix:  env["GHR_NAME_PREFIX"],
		Workers:     env["GHR_WORKERS"],
	}
	if c.Workers == "" {
		c.Workers = "auto"
	}
	for _, l := range strings.Split(env["GHR_LABELS"], ",") {
		if l = strings.TrimSpace(l); l != "" {
			c.Labels = append(c.Labels, l)
		}
	}
	if env["GHR_APP_ID"] != "" {
		key := env["GHR_APP_PRIVATE_KEY"]
		if key == "" {
			var err error
			if key, err = readRel(env["GHR_APP_PRIVATE_KEY_FILE"]); err != nil {
				return nil, fmt.Errorf("read app private key: %w", err)
			}
		}
		c.Auth = Auth{Type: "app", AppID: env["GHR_APP_ID"], InstallationID: env["GHR_APP_INSTALLATION_ID"], PrivateKey: key}
	} else if env["GHR_PAT"] != "" {
		c.Auth = Auth{Type: "pat", PAT: env["GHR_PAT"]}
	}
	if keys, err := readRel(env["GHR_SSH_AUTHORIZED_KEYS_FILE"]); err == nil && keys != "" {
		c.SSH = SSH{Enabled: true, AuthorizedKeys: keys}
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// ToEnv renders a runner.env for copying the config to other sticks.
// Secrets are included only when withSecrets is true.
func (c *Config) ToEnv(withSecrets bool) string {
	var b strings.Builder
	b.WriteString("# runner.env exported from ghr-web. Put it in the ghr/ folder of a stick.\n")
	fmt.Fprintf(&b, "GHR_URL=%s\n", c.URL)
	if c.RunnerGroup != "" {
		fmt.Fprintf(&b, "GHR_RUNNER_GROUP=%s\n", c.RunnerGroup)
	}
	if c.NamePrefix != "" {
		fmt.Fprintf(&b, "GHR_NAME_PREFIX=%s\n", c.NamePrefix)
	}
	fmt.Fprintf(&b, "GHR_LABELS=%s\n", strings.Join(c.Labels, ","))
	fmt.Fprintf(&b, "GHR_WORKERS=%s\n", c.Workers)
	switch c.Auth.Type {
	case "app":
		fmt.Fprintf(&b, "GHR_APP_ID=%s\nGHR_APP_INSTALLATION_ID=%s\n", c.Auth.AppID, c.Auth.InstallationID)
		b.WriteString("GHR_APP_PRIVATE_KEY_FILE=app-key.pem\n")
	case "pat":
		if withSecrets {
			fmt.Fprintf(&b, "GHR_PAT=%s\n", c.Auth.PAT)
		} else {
			b.WriteString("GHR_PAT=\n")
		}
	}
	return b.String()
}

// MemTotalMB reads MemTotal from /proc/meminfo.
func MemTotalMB() int {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			f := strings.Fields(line)
			if len(f) >= 2 {
				kb, _ := strconv.Atoi(f[1])
				return kb / 1024
			}
		}
	}
	return 0
}
