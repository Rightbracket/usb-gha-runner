// Package agent is the root-side daemon. It runs the job supervisor and
// serves a small fixed set of actions to ghr-web over a unix socket, so the
// web UI itself never needs root.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Rightbracket/usb-gha-runner/ghr/internal/config"
	"github.com/Rightbracket/usb-gha-runner/ghr/internal/gh"
	"github.com/Rightbracket/usb-gha-runner/ghr/internal/importer"
	"github.com/Rightbracket/usb-gha-runner/ghr/internal/prune"
	"github.com/Rightbracket/usb-gha-runner/ghr/internal/sshctl"
	"github.com/Rightbracket/usb-gha-runner/ghr/internal/status"
	"github.com/Rightbracket/usb-gha-runner/ghr/internal/supervisor"
)

const (
	SocketPath  = "/run/ghr/agent.sock"
	SocketGroup = "ghr-web"
	// ImagesArchive holds ghr-job and the registry image, saved at image build time.
	ImagesArchive = "/run/live/medium/live/ghr-images.tar.zst"
	VersionFile   = "/usr/share/ghr/version"
	MirrorName    = "ghr-mirror"
	MirrorDir     = "/data/registry-mirror"
	CacheFile     = config.RunDir + "/cache.json"
)

// MirrorImage is the registry image saved in ImagesArchive. build.sh sets it
// from versions.env with -ldflags -X so the two can't drift apart.
var MirrorImage = "registry:2.8.3"

type Agent struct {
	cfgMu     sync.Mutex // serialises config writes
	collector *status.Collector
	version   string
	sv        *supervisor.Supervisor
	// runnerHost is the per-stick part of runner names (config.RunnerHost).
	runnerHost string

	// Test seams; zero values mean the real config file, GitHub client and
	// docker CLI.
	configPath string
	newClient  func(*config.Config) (*gh.Client, error)
	killSlots  func()
}

func (a *Agent) cfgPath() string {
	if a.configPath != "" {
		return a.configPath
	}
	return config.ConfigPath
}

func (a *Agent) client(cfg *config.Config) (*gh.Client, error) {
	if a.newClient != nil {
		return a.newClient(cfg)
	}
	return gh.New(cfg)
}

func (a *Agent) removeSlotContainers() {
	if a.killSlots != nil {
		a.killSlots()
		return
	}
	stopSlots()
}

// Version returns the image release, used to tag the job image.
func Version() string {
	b, err := os.ReadFile(VersionFile)
	if err != nil {
		return "dev"
	}
	return strings.TrimSpace(string(b))
}

func Run(ctx context.Context) error {
	a := &Agent{collector: status.Default(), version: Version()}
	if err := os.MkdirAll(config.RunDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(config.EtcDir, 0o700); err != nil {
		return err
	}

	jobImage := "ghr-job:" + a.version
	a.cleanupStaleSlots()
	if err := ensureImages(jobImage); err != nil {
		log.Printf("job image: %v", err) // the supervisor will keep retrying docker run
	}
	mirror := ensureMirror()

	if cfg, err := config.Load(config.ConfigPath); err == nil {
		if err := sshctl.Apply(cfg.SSH); err != nil {
			log.Printf("ssh: %v", err)
		}
	}

	go a.cacheLoop(ctx)

	host, _ := os.Hostname()
	rh, err := config.RunnerHost(host, config.StickIDPath)
	if err != nil {
		log.Printf("stick ID: %v; runner names use the hostname only", err)
	}
	a.runnerHost = rh
	sv := &supervisor.Supervisor{
		ConfigPath:     config.ConfigPath,
		SlotsDir:       config.SlotsDir,
		History:        config.HistoryPath,
		Hostname:       rh,
		JobImage:       jobImage,
		RegistryMirror: mirror,
		// No jobs until someone owns the web page: otherwise a stick set up
		// with only runner.env would leave /setup open to anything on the LAN.
		Ready: func() bool { _, err := os.Stat(importer.PasswordFile); return err == nil },
	}
	a.sv = sv
	// If serve fails, stop the supervisor too instead of leaving it running.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	svDone := make(chan struct{})
	go func() {
		defer close(svDone)
		if err := sv.Run(ctx); err != nil {
			log.Printf("supervisor: %v", err)
		}
	}()

	err = a.serve(ctx)
	cancel()
	// Job containers belong to dockerd, not to this unit, so wait for the
	// supervisor to stop them before exiting (stopping nftables also stops
	// the agent, and no job may outlive the firewall). Stay under the unit's
	// TimeoutStopSec=90.
	select {
	case <-svDone:
	case <-time.After(60 * time.Second):
		log.Printf("job slots did not stop in time; removing their containers")
		stopSlots()
	}
	return err
}

func (a *Agent) serve(ctx context.Context) error {
	_ = os.Remove(SocketPath)
	ln, err := net.Listen("unix", SocketPath)
	if err != nil {
		return err
	}
	if g, err := user.LookupGroup(SocketGroup); err == nil {
		gid, _ := strconv.Atoi(g.Gid)
		_ = os.Chown(SocketPath, 0, gid)
	}
	if err := os.Chmod(SocketPath, 0o660); err != nil {
		return err
	}
	srv := &http.Server{Handler: a.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { <-ctx.Done(); _ = srv.Close() }()
	err = srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Handler exposes the fixed action set. Every request body is size-limited
// and validated; nothing here runs arbitrary commands.
func (a *Agent) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, a.collector.Collect())
	})
	mux.HandleFunc("GET /v1/config", func(w http.ResponseWriter, r *http.Request) {
		cfg, err := config.Load(config.ConfigPath)
		if err != nil {
			httpErr(w, err, 500)
			return
		}
		writeJSON(w, cfg.Redacted())
	})
	mux.HandleFunc("PUT /v1/config", a.putConfig)
	mux.HandleFunc("POST /v1/remove", a.remove)
	mux.HandleFunc("PUT /v1/ssh", a.putSSH)
	mux.HandleFunc("POST /v1/prune", func(w http.ResponseWriter, r *http.Request) {
		// A prune can outlast the web UI's request; let it finish anyway.
		res, err := prune.Run(context.WithoutCancel(r.Context()), 0)
		if err != nil {
			httpErr(w, err, 500)
			return
		}
		writeJSON(w, res)
	})
	mux.HandleFunc("GET /v1/runner.env", func(w http.ResponseWriter, r *http.Request) {
		cfg, err := config.Load(config.ConfigPath)
		if err != nil {
			httpErr(w, err, 500)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, cfg.ToEnv(false))
	})
	mux.HandleFunc("POST /v1/power/{action}", func(w http.ResponseWriter, r *http.Request) {
		act := r.PathValue("action")
		if act != "reboot" && act != "poweroff" {
			httpErr(w, errors.New("unknown power action"), 400)
			return
		}
		writeJSON(w, map[string]string{"ok": act})
		go func() {
			time.Sleep(time.Second) // let the response go out first
			_ = exec.Command("systemctl", "--no-block", act).Run()
		}()
	})
	return mux
}

func (a *Agent) putConfig(w http.ResponseWriter, r *http.Request) {
	var in config.Config
	if err := json.NewDecoder(io.LimitReader(r.Body, 256<<10)).Decode(&in); err != nil {
		httpErr(w, fmt.Errorf("bad config: %w", err), 400)
		return
	}
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	old, err := config.Load(a.cfgPath())
	if err != nil {
		httpErr(w, err, 500)
		return
	}
	in.MergeSecrets(old)
	in.SSH = old.SSH // SSH has its own endpoint
	if err := in.Validate(); err != nil {
		httpErr(w, err, 400)
		return
	}
	// Check the credential before saving, so a typo shows up on the page
	// instead of as a slot error loop.
	client, err := a.client(&in)
	if err == nil {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		err = client.Check(ctx)
		cancel()
	}
	if err != nil {
		httpErr(w, fmt.Errorf("GitHub rejected the settings: %w", err), 400)
		return
	}
	if err := config.Save(a.cfgPath(), &in); err != nil {
		httpErr(w, err, 500)
		return
	}
	writeJSON(w, in.Redacted())
}

// remove stops every slot, deregisters this machine's runners from GitHub
// and clears the registration settings (SSH settings are kept).
func (a *Agent) remove(w http.ResponseWriter, r *http.Request) {
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	cfg, err := config.Load(a.cfgPath())
	if err != nil {
		httpErr(w, err, 500)
		return
	}
	cleared := &config.Config{Workers: "auto", Auth: config.Auth{Type: "app"}, SSH: cfg.SSH}
	if err := config.Save(a.cfgPath(), cleared); err != nil {
		httpErr(w, err, 500)
		return
	}
	// Let the slot loops see the drain and exit before deregistering, so none
	// of them registers a fresh runner behind our back.
	var errs []string
	if a.sv != nil {
		if err := a.sv.StopAll(45 * time.Second); err != nil {
			errs = append(errs, err.Error())
		}
	}
	a.removeSlotContainers()
	if cfg.Configured() {
		if client, err := a.client(cfg); err == nil {
			ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
			defer cancel()
			if n, err := client.DeleteSlotRunners(ctx, cfg.RunnerBase(a.runnerHost)); err != nil {
				errs = append(errs, err.Error())
			} else {
				log.Printf("removed %d runner registrations", n)
			}
		}
	}
	writeJSON(w, map[string]any{"ok": len(errs) == 0, "errors": errs})
}

func (a *Agent) putSSH(w http.ResponseWriter, r *http.Request) {
	var in config.SSH
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in); err != nil {
		httpErr(w, err, 400)
		return
	}
	keys, err := sshctl.CleanKeys(in.AuthorizedKeys)
	if err != nil {
		httpErr(w, err, 400)
		return
	}
	in.AuthorizedKeys = keys
	if in.Enabled && keys == "" {
		httpErr(w, errors.New("add at least one public key before turning SSH on"), 400)
		return
	}
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	cfg, err := config.Load(config.ConfigPath)
	if err != nil {
		httpErr(w, err, 500)
		return
	}
	cfg.SSH = in
	if err := config.Save(config.ConfigPath, cfg); err != nil {
		httpErr(w, err, 500)
		return
	}
	if err := sshctl.Apply(in); err != nil {
		httpErr(w, err, 500)
		return
	}
	writeJSON(w, in)
}

func stopSlots() {
	out, _ := exec.Command("docker", "ps", "-aq", "--filter", "label=ghr.slot").Output()
	ids := strings.Fields(string(out))
	if len(ids) > 0 {
		_ = exec.Command("docker", append([]string{"rm", "-f"}, ids...)...).Run()
	}
}

func (a *Agent) cleanupStaleSlots() {
	stopSlots()
	files, _ := filepath.Glob(filepath.Join(config.SlotsDir, "*.json"))
	for _, f := range files {
		_ = os.Remove(f)
	}
}

// ensureImages loads the job and mirror images shipped on the stick into
// Docker on /data, once per image release, and again if either is missing.
func ensureImages(jobImage string) error {
	if exec.Command("docker", "image", "inspect", jobImage, MirrorImage).Run() == nil {
		return nil
	}
	if _, err := os.Stat(ImagesArchive); err != nil {
		return fmt.Errorf("%s not found and image not loaded: %w", ImagesArchive, err)
	}
	log.Printf("loading job images from %s", ImagesArchive)
	cmd := exec.Command("sh", "-c", "zstd -dc "+ImagesArchive+" | docker load")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker load: %w", err)
	}
	// Drop job images from older releases.
	out, _ := exec.Command("docker", "images", "--format", "{{.Repository}}:{{.Tag}}", "ghr-job").Output()
	for _, img := range strings.Fields(string(out)) {
		if img != jobImage {
			_ = exec.Command("docker", "rmi", img).Run()
		}
	}
	return nil
}

// ensureMirror starts the Docker Hub pull-through cache on the docker0
// bridge and returns its URL for job containers, or "" if it can't run.
func ensureMirror() string {
	out, err := exec.Command("docker", "network", "inspect", "bridge", "--format", "{{(index .IPAM.Config 0).Gateway}}").Output()
	gw := strings.TrimSpace(string(out))
	if err != nil || net.ParseIP(gw) == nil {
		log.Printf("mirror: no docker0 gateway (%v)", err)
		return ""
	}
	if exec.Command("docker", "image", "inspect", MirrorImage).Run() != nil {
		log.Printf("mirror: %s not loaded; skipping", MirrorImage)
		return ""
	}
	// The container survives an upgrade with /data, and --restart=always
	// brings it back at boot, so it is replaced when it runs another image.
	want, _ := exec.Command("docker", "image", "inspect", "-f", "{{.Id}}", MirrorImage).Output()
	state, _ := exec.Command("docker", "inspect", "-f", "{{.State.Running}} {{.Image}}", MirrorName).Output()
	if mirrorStale(string(state), string(want)) {
		_ = exec.Command("docker", "rm", "-f", MirrorName).Run()
		_ = os.MkdirAll(MirrorDir, 0o755)
		cmd := exec.Command("docker", "run", "-d", "--restart=always", "--name", MirrorName,
			"--label", "ghr.keep=true",
			"-p", gw+":5000:5000",
			"-v", MirrorDir+":/var/lib/registry",
			"-e", "REGISTRY_PROXY_REMOTEURL=https://registry-1.docker.io",
			MirrorImage)
		if out, err := cmd.CombinedOutput(); err != nil {
			log.Printf("mirror: %v: %s", err, out)
			return ""
		}
	}
	return "http://" + gw + ":5000"
}

// mirrorStale reports whether the mirror container must be recreated, given
// its "{{.State.Running}} {{.Image}}" (empty if it doesn't exist) and the
// image ID that MirrorImage resolves to now.
func mirrorStale(state, wantID string) bool {
	f := strings.Fields(state)
	want := strings.TrimSpace(wantID)
	return len(f) != 2 || f[0] != "true" || want == "" || f[1] != want
}

// cacheLoop records builder cache size for the status page every 10 minutes.
func (a *Agent) cacheLoop(ctx context.Context) {
	for {
		var total int64
		for _, d := range []string{"/data/docker", "/data/containers", "/data/sysbox", MirrorDir} {
			out, err := exec.CommandContext(ctx, "nice", "du", "-sb", "--one-file-system", d).Output()
			if err == nil {
				if f := strings.Fields(string(out)); len(f) > 0 {
					n, _ := strconv.ParseInt(f[0], 10, 64)
					total += n
				}
			}
		}
		b, _ := json.Marshal(map[string]any{"bytes": total, "time": time.Now()})
		_ = config.WriteFileAtomic(CacheFile, b, 0o644)
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Minute):
		}
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func httpErr(w http.ResponseWriter, err error, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
