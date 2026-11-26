// Package web is ghr-web: the HTTPS configuration and status page. It runs
// as an unprivileged user and asks ghr-agent (root) to make any change.
package web

import (
	"bufio"
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/Rightbracket/usb-gha-runner/ghr/internal/auth"
	"github.com/Rightbracket/usb-gha-runner/ghr/internal/config"
)

//go:embed assets
var assets embed.FS

type Server struct {
	Auth        *auth.Store
	AgentSocket string
	Version     string
	Hostname    string
	Fingerprint string
	// SetupLockFile, when it exists, keeps first-visitor setup closed: the
	// stick has a preset password that couldn't be applied.
	SetupLockFile string

	agent *http.Client
	tpl   *template.Template
}

func New(st *auth.Store, socket, version, hostname, fingerprint string) (*Server, error) {
	tpl, err := template.New("").Funcs(template.FuncMap{
		"join": strings.Join,
	}).ParseFS(assets, "assets/*.html")
	if err != nil {
		return nil, err
	}
	s := &Server{Auth: st, AgentSocket: socket, Version: version, Hostname: hostname, Fingerprint: fingerprint, tpl: tpl}
	s.agent = &http.Client{
		Timeout: 2 * time.Minute,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		}},
	}
	return s, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(assets, "assets")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))

	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true, "password_set": s.Auth.HasPassword(), "version": s.Version, "hostname": s.Hostname})
	})
	mux.HandleFunc("GET /setup", s.getSetup)
	mux.HandleFunc("POST /setup", s.postSetup)
	mux.HandleFunc("GET /login", s.getLogin)
	mux.HandleFunc("POST /login", s.postLogin)
	mux.HandleFunc("POST /logout", func(w http.ResponseWriter, r *http.Request) {
		s.Auth.Logout(w, r)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})

	authed := http.NewServeMux()
	authed.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { s.render(w, "status", s.page(r, "Status", nil)) })
	authed.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) { s.proxy(w, r, "GET", "/v1/status", nil) })
	authed.HandleFunc("GET /config", s.getConfig)
	authed.HandleFunc("POST /config", s.postConfig)
	authed.HandleFunc("POST /config/remove", func(w http.ResponseWriter, r *http.Request) {
		var out struct {
			OK     bool     `json:"ok"`
			Errors []string `json:"errors"`
		}
		if err := s.call(r.Context(), "POST", "/v1/remove", nil, &out); err != nil {
			back(w, r, "/config", "", err.Error())
			return
		}
		if !out.OK {
			back(w, r, "/config", "", "Settings cleared, but GitHub cleanup had errors: "+strings.Join(out.Errors, "; "))
			return
		}
		back(w, r, "/config", "Runner removed and deregistered from GitHub.", "")
	})
	authed.HandleFunc("POST /config/ssh", s.postSSH)
	authed.HandleFunc("POST /config/password", s.postPassword)
	authed.HandleFunc("GET /config/runner.env", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Disposition", `attachment; filename="runner.env"`)
		s.proxy(w, r, "GET", "/v1/runner.env", nil)
	})
	authed.HandleFunc("POST /actions/prune", func(w http.ResponseWriter, r *http.Request) {
		var res struct {
			BeforePct float64 `json:"before_pct"`
			AfterPct  float64 `json:"after_pct"`
		}
		if err := s.call(r.Context(), "POST", "/v1/prune", nil, &res); err != nil {
			back(w, r, "/", "", err.Error())
			return
		}
		back(w, r, "/", fmt.Sprintf("Pruned caches: /data went from %.0f%% to %.0f%% full.", res.BeforePct, res.AfterPct), "")
	})
	authed.HandleFunc("POST /actions/{action}", func(w http.ResponseWriter, r *http.Request) {
		act := r.PathValue("action")
		if act != "reboot" && act != "poweroff" {
			http.NotFound(w, r)
			return
		}
		if err := s.call(r.Context(), "POST", "/v1/power/"+act, nil, nil); err != nil {
			back(w, r, "/", "", err.Error())
			return
		}
		s.render(w, "power", s.page(r, "Power", map[string]string{"Action": act}))
	})
	authed.HandleFunc("GET /logs", func(w http.ResponseWriter, r *http.Request) { s.render(w, "logs", s.page(r, "Logs", nil)) })
	authed.HandleFunc("GET /api/logs", s.logs)

	mux.Handle("/", s.requireAuth(authed))
	return securityHeaders(s.checkOrigin(mux))
}

// RedirectHTTP sends plain-HTTP visitors to the HTTPS page.
func RedirectHTTP() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), http.StatusMovedPermanently)
	})
}

type pageData struct {
	Title, Hostname, Version, Fingerprint, Msg, Err string
	Data                                            any
}

func (s *Server) page(r *http.Request, title string, data any) pageData {
	q := r.URL.Query()
	return pageData{Title: title, Hostname: s.Hostname, Version: s.Version, Fingerprint: s.Fingerprint, Msg: q.Get("msg"), Err: q.Get("err"), Data: data}
}

func (s *Server) render(w http.ResponseWriter, name string, p pageData) {
	var buf bytes.Buffer
	if err := s.tpl.ExecuteTemplate(&buf, name, p); err != nil {
		log.Printf("render %s: %v", name, err)
		http.Error(w, "template error", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}

func back(w http.ResponseWriter, r *http.Request, path, msg, errMsg string) {
	v := url.Values{}
	if msg != "" {
		v.Set("msg", msg)
	}
	if errMsg != "" {
		v.Set("err", errMsg)
	}
	if len(v) > 0 {
		path += "?" + v.Encode()
	}
	http.Redirect(w, r, path, http.StatusSeeOther)
}

func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.Auth.HasPassword() {
			http.Redirect(w, r, "/setup", http.StatusSeeOther)
			return
		}
		if !s.Auth.Valid(r) {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				http.Error(w, `{"error":"login required"}`, http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// checkOrigin blocks cross-site form posts. The session cookie is already
// SameSite=Strict; this also covers the setup and login forms, which run
// before there is a cookie.
func (s *Server) checkOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if o := r.Header.Get("Origin"); o != "" && o != "null" {
				u, err := url.Parse(o)
				if err != nil || u.Host != r.Host {
					http.Error(w, "cross-origin request refused", http.StatusForbidden)
					return
				}
			} else if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
				http.Error(w, "cross-site request refused", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; frame-ancestors 'none'; form-action 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// --- setup and login -------------------------------------------------------

func (s *Server) getSetup(w http.ResponseWriter, r *http.Request) {
	if s.Auth.HasPassword() {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	p := s.page(r, "Set admin password", nil)
	if msg := s.setupLocked(); msg != "" {
		p.Err = msg
	}
	s.render(w, "setup", p)
}

// setupLocked returns why first-visitor setup is closed, or "".
func (s *Server) setupLocked() string {
	if s.SetupLockFile == "" {
		return ""
	}
	b, err := os.ReadFile(s.SetupLockFile)
	if err != nil {
		return ""
	}
	return "Setup is locked: this stick has a preset password in ghr/admin-password, but it couldn't be used (" +
		strings.TrimSpace(string(b)) + "). Fix that file on the stick and reboot."
}

func (s *Server) postSetup(w http.ResponseWriter, r *http.Request) {
	if s.Auth.HasPassword() {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if msg := s.setupLocked(); msg != "" {
		back(w, r, "/setup", "", msg)
		return
	}
	client := auth.ClientKey(r)
	if !s.Auth.Allow(client) {
		back(w, r, "/setup", "", "Too many attempts. Wait a few minutes.")
		return
	}
	pw, confirm := r.PostFormValue("password"), r.PostFormValue("confirm")
	if pw != confirm {
		back(w, r, "/setup", "", "The two passwords don't match.")
		return
	}
	if err := s.Auth.SetPasswordIfUnset(pw); err != nil {
		back(w, r, "/setup", "", err.Error())
		return
	}
	s.Auth.Succeeded(client)
	s.Auth.Login(w)
	back(w, r, "/config", "Password set. Next, connect this machine to GitHub.", "")
}

func (s *Server) getLogin(w http.ResponseWriter, r *http.Request) {
	if !s.Auth.HasPassword() {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	s.render(w, "login", s.page(r, "Log in", nil))
}

func (s *Server) postLogin(w http.ResponseWriter, r *http.Request) {
	client := auth.ClientKey(r)
	if !s.Auth.Allow(client) {
		back(w, r, "/login", "", "Too many failed attempts. Wait a few minutes.")
		return
	}
	if !s.Auth.Check(r.PostFormValue("password")) {
		back(w, r, "/login", "", "Wrong password.")
		return
	}
	s.Auth.Succeeded(client)
	s.Auth.Login(w)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) postPassword(w http.ResponseWriter, r *http.Request) {
	client := auth.ClientKey(r)
	if !s.Auth.Allow(client) {
		back(w, r, "/config", "", "Too many failed attempts. Wait a few minutes.")
		return
	}
	if !s.Auth.Check(r.PostFormValue("current")) {
		back(w, r, "/config", "", "The current password is wrong.")
		return
	}
	s.Auth.Succeeded(client)
	if r.PostFormValue("password") != r.PostFormValue("confirm") {
		back(w, r, "/config", "", "The new passwords don't match.")
		return
	}
	if err := s.Auth.SetPassword(r.PostFormValue("password")); err != nil {
		back(w, r, "/config", "", err.Error())
		return
	}
	s.Auth.Login(w)
	back(w, r, "/config", "Password changed. Other sessions were logged out.", "")
}

// --- configuration -----------------------------------------------------------

type configView struct {
	Cfg        *config.Config
	Labels     string
	Always     string
	SlotCPUs   string
	SlotMemory string
	HasPAT     bool
	HasKey     bool
}

func (s *Server) getConfig(w http.ResponseWriter, r *http.Request) {
	var cfg config.Config
	if err := s.call(r.Context(), "GET", "/v1/config", nil, &cfg); err != nil {
		s.render(w, "config", pageData{Title: "Configure", Hostname: s.Hostname, Version: s.Version, Err: err.Error(), Data: configView{Cfg: &config.Config{Workers: "auto", Auth: config.Auth{Type: "app"}}}})
		return
	}
	v := configView{Cfg: &cfg, Labels: strings.Join(cfg.Labels, ", "), Always: strings.Join(config.AlwaysLabels, ", "),
		HasPAT: cfg.Auth.PAT != "", HasKey: cfg.Auth.PrivateKey != ""}
	if cfg.SlotCPUs > 0 {
		v.SlotCPUs = strconv.FormatFloat(cfg.SlotCPUs, 'f', -1, 64)
	}
	if cfg.SlotMemoryMB > 0 {
		v.SlotMemory = strconv.Itoa(cfg.SlotMemoryMB)
	}
	if v.Cfg.Auth.Type == "" {
		v.Cfg.Auth.Type = "app"
	}
	s.render(w, "config", s.page(r, "Configure", v))
}

func (s *Server) postConfig(w http.ResponseWriter, r *http.Request) {
	f := func(k string) string { return strings.TrimSpace(r.PostFormValue(k)) }
	c := config.Config{
		URL:         f("url"),
		RunnerGroup: f("runner_group"),
		NamePrefix:  f("name_prefix"),
		Workers:     strings.ToLower(f("workers")),
		Auth: config.Auth{
			Type:           f("auth_type"),
			PAT:            f("pat"),
			AppID:          f("app_id"),
			InstallationID: f("installation_id"),
			PrivateKey:     strings.TrimSpace(strings.ReplaceAll(r.PostFormValue("private_key"), "\r", "")),
		},
	}
	for _, l := range strings.Split(f("labels"), ",") {
		if l = strings.TrimSpace(l); l != "" {
			c.Labels = append(c.Labels, l)
		}
	}
	if v := f("slot_cpus"); v != "" {
		n, err := strconv.ParseFloat(v, 64)
		if err != nil || n <= 0 {
			back(w, r, "/config", "", "CPUs per slot must be a positive number.")
			return
		}
		c.SlotCPUs = n
	}
	if v := f("slot_memory_mb"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 512 {
			back(w, r, "/config", "", "Memory per slot must be at least 512 MB.")
			return
		}
		c.SlotMemoryMB = n
	}
	if c.NamePrefix != "" && !validName(c.NamePrefix) {
		back(w, r, "/config", "", fmt.Sprintf("Name prefix may only use up to %d letters, digits, - and _.", config.MaxPrefix))
		return
	}
	if err := s.call(r.Context(), "PUT", "/v1/config", c, nil); err != nil {
		back(w, r, "/config", "", err.Error())
		return
	}
	back(w, r, "/", "Saved. Worker slots are registering with GitHub.", "")
}

func validName(s string) bool {
	if len(s) > config.MaxPrefix {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func (s *Server) postSSH(w http.ResponseWriter, r *http.Request) {
	in := config.SSH{Enabled: r.PostFormValue("enabled") == "on", AuthorizedKeys: r.PostFormValue("authorized_keys")}
	if err := s.call(r.Context(), "PUT", "/v1/ssh", in, nil); err != nil {
		back(w, r, "/config", "", err.Error())
		return
	}
	msg := "SSH is off."
	if in.Enabled {
		msg = "SSH is on. Log in as ghr@" + s.Hostname + ".local with your key."
	}
	back(w, r, "/config", msg, "")
}

// --- logs --------------------------------------------------------------------

var logUnits = map[string][]string{
	"runner": {"-u", "ghr-agent.service"},
	"web":    {"-u", "ghr-web.service"},
	"docker": {"-u", "docker.service", "-u", "containerd.service", "-u", "sysbox.service", "-u", "sysbox-mgr.service", "-u", "sysbox-fs.service"},
	"system": nil,
}

// logs streams the journal as server-sent events.
func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	sel, ok := logUnits[r.URL.Query().Get("unit")]
	if !ok {
		sel = logUnits["runner"]
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", 500)
		return
	}
	args := append([]string{"--no-pager", "-f", "-n", "300", "-o", "short-iso"}, sel...)
	cmd := exec.CommandContext(r.Context(), "journalctl", args...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if err := cmd.Start(); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	w.Header().Set("Content-Type", "text/event-stream")
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		if _, err := fmt.Fprintf(w, "data: %s\n\n", strings.ReplaceAll(sc.Text(), "\r", "")); err != nil {
			return
		}
		fl.Flush()
	}
}

// --- agent calls -------------------------------------------------------------

func (s *Server) call(ctx context.Context, method, path string, body, out any) error {
	resp, err := s.do(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		return fmt.Errorf("agent: %s", resp.Status)
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

func (s *Server) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://agent"+path, rd)
	if err != nil {
		return nil, err
	}
	resp, err := s.agent.Do(req)
	if err != nil {
		return nil, fmt.Errorf("the runner service isn't answering (%v). Check the logs page", err)
	}
	return resp, nil
}

func (s *Server) proxy(w http.ResponseWriter, r *http.Request, method, path string, body any) {
	resp, err := s.do(r.Context(), method, path, body)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
