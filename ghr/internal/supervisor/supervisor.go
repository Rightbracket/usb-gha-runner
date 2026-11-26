// Package supervisor runs N worker slots. Each slot repeatedly asks GitHub
// for a just-in-time runner config and runs exactly one job inside a fresh
// Sysbox container, which is deleted when the job ends.
package supervisor

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Rightbracket/usb-gha-runner/ghr/internal/config"
	"github.com/Rightbracket/usb-gha-runner/ghr/internal/gh"
)

const (
	Runtime      = "sysbox-runc"
	reservedMB   = 2048 // left for the host OS, dockerd, ghr-web
	pollInterval = 10 * time.Second
)

// SlotState is written to /run/ghr/slots/<n>.json for ghr-web to read.
type SlotState struct {
	Slot       int       `json:"slot"`
	RunnerName string    `json:"runner_name"`
	State      string    `json:"state"` // starting, idle, busy, draining, error, stopped
	Job        string    `json:"job,omitempty"`
	Since      time.Time `json:"since"`
	LastError  string    `json:"last_error,omitempty"`
	CPUs       float64   `json:"cpus"`
	MemoryMB   int       `json:"memory_mb"`
}

// HistoryEntry is appended to /data/ghr/history.jsonl.
type HistoryEntry struct {
	Time       time.Time `json:"time"`
	Slot       int       `json:"slot"`
	RunnerName string    `json:"runner_name"`
	Job        string    `json:"job"`
	Result     string    `json:"result"`
	Seconds    int       `json:"seconds"`
}

type Supervisor struct {
	ConfigPath string
	SlotsDir   string
	History    string
	Hostname   string
	// JobImage is the per-job container image, e.g. ghr-job:0.1.0.
	JobImage string
	// RegistryMirror, when set, is passed to each job's inner dockerd as a
	// pull-through mirror for Docker Hub (http://<docker0 ip>:5000).
	RegistryMirror string
	// Ready, when set, must report true before any slot starts. The agent
	// uses it to keep jobs off the machine until an admin password exists.
	Ready func() bool

	mu    sync.Mutex
	slots map[int]*slot
	wg    sync.WaitGroup
	hmu   sync.Mutex
}

type slot struct {
	n      int
	cfg    *config.Config
	cpus   float64
	memMB  int
	drain  chan struct{}
	once   sync.Once
	done   chan struct{}
	parent *Supervisor
}

func (s *slot) startDrain() { s.once.Do(func() { close(s.drain) }) }

// Run blocks until ctx is cancelled, reconciling slots with the config file.
func (sv *Supervisor) Run(ctx context.Context) error {
	sv.slots = map[int]*slot{}
	_ = os.MkdirAll(sv.SlotsDir, 0o755)
	var cur *config.Config
	var curMod time.Time
	t := time.NewTicker(pollInterval)
	defer t.Stop()
	wasReady := true
	for {
		ready := sv.Ready == nil || sv.Ready()
		if !ready && wasReady {
			log.Printf("no admin password set yet; no slots will start until one is")
		}
		wasReady = ready
		if fi, err := os.Stat(sv.ConfigPath); err == nil && !fi.ModTime().Equal(curMod) {
			cfg, err := config.Load(sv.ConfigPath)
			if err != nil {
				log.Printf("config: %v", err)
			} else {
				curMod = fi.ModTime()
				if ready {
					sv.reconcile(ctx, cur, cfg)
				}
				cur = cfg
			}
		}
		sv.reapFinished()
		switch {
		case !ready:
			sv.drainAll()
		case cur != nil && cur.Configured() && sv.count() < cur.WorkerCount():
			// Refill: drained slots (after a settings change) and slots
			// that were never started because we weren't ready.
			sv.reconcile(ctx, cur, cur)
		}
		select {
		case <-ctx.Done():
			sv.mu.Lock()
			for _, s := range sv.slots {
				s.startDrain()
			}
			sv.mu.Unlock()
			sv.wg.Wait()
			return nil
		case <-t.C:
		}
	}
}

func (sv *Supervisor) count() int {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	return len(sv.slots)
}

func (sv *Supervisor) drainAll() {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	for _, s := range sv.slots {
		s.startDrain()
	}
}

// StopAll drains every slot and waits for them to exit, for "Remove".
// The caller must already have saved an unconfigured config, so the
// supervisor doesn't start new slots afterwards.
func (sv *Supervisor) StopAll(timeout time.Duration) {
	sv.drainAll()
	done := make(chan struct{})
	go func() {
		for sv.count() > 0 {
			sv.reapFinished()
			time.Sleep(200 * time.Millisecond)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

func (sv *Supervisor) reapFinished() {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	for n, s := range sv.slots {
		select {
		case <-s.done:
			delete(sv.slots, n)
		default:
		}
	}
}

// reconcile drains slots that no longer fit the config and starts missing ones.
// Draining slots finish a running job first; replacements start once they exit.
func (sv *Supervisor) reconcile(ctx context.Context, old, cfg *config.Config) {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	if !cfg.Configured() {
		log.Printf("not configured yet; no slots running")
		for _, s := range sv.slots {
			s.startDrain()
		}
		return
	}
	n := cfg.WorkerCount()
	registrationChanged := old != nil && !sameRegistration(old, cfg)
	cpus, mem := sliceResources(cfg, n)
	for i, s := range sv.slots {
		if i > n || registrationChanged || s.cpus != cpus || s.memMB != mem {
			s.startDrain()
		}
	}
	for i := 1; i <= n; i++ {
		if _, ok := sv.slots[i]; ok {
			continue
		}
		s := &slot{n: i, cfg: cfg, cpus: cpus, memMB: mem, drain: make(chan struct{}), done: make(chan struct{}), parent: sv}
		sv.slots[i] = s
		sv.wg.Add(1)
		go func() {
			defer sv.wg.Done()
			defer close(s.done)
			s.loop(ctx)
		}()
	}
}

func sameRegistration(a, b *config.Config) bool {
	return a.URL == b.URL && a.RunnerGroup == b.RunnerGroup && a.NamePrefix == b.NamePrefix &&
		reflect.DeepEqual(a.AllLabels(), b.AllLabels()) && a.Auth == b.Auth
}

func sliceResources(cfg *config.Config, n int) (float64, int) {
	cpus := cfg.SlotCPUs
	if cpus <= 0 {
		cpus = float64(runtime.NumCPU()) / float64(n)
	}
	mem := cfg.SlotMemoryMB
	if mem <= 0 {
		mem = (config.MemTotalMB() - reservedMB) / n
		if mem < 1024 {
			mem = 1024
		}
	}
	return cpus, mem
}

func (sv *Supervisor) RunnerName(cfg *config.Config, n int) string {
	return fmt.Sprintf("%s-%d", cfg.RunnerBase(sv.Hostname), n)
}

func (s *slot) write(st SlotState) {
	st.Slot, st.CPUs, st.MemoryMB = s.n, s.cpus, s.memMB
	if st.Since.IsZero() {
		st.Since = time.Now()
	}
	b, _ := json.Marshal(st)
	_ = config.WriteFileAtomic(filepath.Join(s.parent.SlotsDir, strconv.Itoa(s.n)+".json"), b, 0o644)
}

func (s *slot) draining() bool {
	select {
	case <-s.drain:
		return true
	default:
		return false
	}
}

func (s *slot) loop(ctx context.Context) {
	name := s.parent.RunnerName(s.cfg, s.n)
	container := "ghr-slot-" + strconv.Itoa(s.n)
	defer s.write(SlotState{RunnerName: name, State: "stopped"})
	backoff := 5 * time.Second
	client, err := gh.New(s.cfg)
	if err != nil {
		s.write(SlotState{RunnerName: name, State: "error", LastError: err.Error()})
		return
	}
	for !s.draining() && ctx.Err() == nil {
		_ = exec.Command("docker", "rm", "-f", container).Run()
		s.write(SlotState{RunnerName: name, State: "starting"})
		jit, runner, err := s.jit(ctx, client, name)
		if err != nil {
			s.write(SlotState{RunnerName: name, State: "error", LastError: err.Error()})
			log.Printf("slot %d: %v (retry in %s)", s.n, err, backoff)
			if !s.sleep(ctx, backoff) {
				return
			}
			backoff = min(backoff*2, 5*time.Minute)
			continue
		}
		ranJob, err := s.runContainer(ctx, client, container, name, jit, runner.ID)
		if err != nil {
			log.Printf("slot %d: %v", s.n, err)
		}
		if !ranJob {
			// The JIT runner never took a job; make sure GitHub forgets it.
			dctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			_ = client.DeleteRunner(dctx, runner.ID)
			cancel()
			if err != nil && !s.draining() && ctx.Err() == nil {
				s.write(SlotState{RunnerName: name, State: "error", LastError: err.Error()})
				if !s.sleep(ctx, backoff) {
					return
				}
				backoff = min(backoff*2, 5*time.Minute)
				continue
			}
		}
		backoff = 5 * time.Second
	}
}

func (s *slot) sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-time.After(d):
		return true
	case <-s.drain:
		return false
	case <-ctx.Done():
		return false
	}
}

// jit gets a JIT config, removing a stale runner with the same name if needed.
func (s *slot) jit(ctx context.Context, client *gh.Client, name string) (string, gh.Runner, error) {
	enc, r, err := client.JITConfig(ctx, name, s.cfg.AllLabels())
	if gh.IsStatus(err, 409) {
		if old, ferr := client.FindRunner(ctx, name); ferr == nil && old != nil {
			if old.Busy {
				return "", gh.Runner{}, fmt.Errorf("runner %s is still busy on GitHub", name)
			}
			if derr := client.DeleteRunner(ctx, old.ID); derr != nil {
				return "", gh.Runner{}, derr
			}
		}
		enc, r, err = client.JITConfig(ctx, name, s.cfg.AllLabels())
	}
	return enc, r, err
}

var (
	reListening = regexp.MustCompile(`Listening for Jobs`)
	reRunning   = regexp.MustCompile(`Running job: (.+)$`)
	reDone      = regexp.MustCompile(`Job (.+) completed with result: (\w+)`)
)

// runContainer runs one job container and reports whether a job was started.
func (s *slot) runContainer(ctx context.Context, client *gh.Client, container, name, jit string, runnerID int64) (bool, error) {
	args := []string{"run", "--rm", "--name", container,
		"--runtime=" + Runtime,
		"--hostname", name,
		"--label", "ghr.slot=" + strconv.Itoa(s.n),
		"--cpus", strconv.FormatFloat(s.cpus, 'f', 2, 64),
		"--memory", strconv.Itoa(s.memMB) + "m",
		"-e", "GHR_JITCONFIG",
		"-e", "GHR_REGISTRY_MIRROR",
		s.parent.JobImage}
	cmd := exec.Command("docker", args...)
	// Passed by name so the secret never appears in the process list.
	cmd.Env = append(os.Environ(), "GHR_JITCONFIG="+jit, "GHR_REGISTRY_MIRROR="+s.parent.RegistryMirror)
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		return false, fmt.Errorf("start container: %w", err)
	}
	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait(); pw.Close() }()

	var (
		mu      sync.Mutex
		busy    bool
		ranJob  bool
		job     string
		started time.Time
	)
	go func() {
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			fmt.Printf("[slot %d] %s\n", s.n, line) // to the journal
			switch {
			case reListening.MatchString(line):
				st := "idle"
				if s.draining() {
					st = "draining"
				}
				s.write(SlotState{RunnerName: name, State: st})
			case reRunning.MatchString(line):
				mu.Lock()
				busy, ranJob, job, started = true, true, strings.TrimSpace(reRunning.FindStringSubmatch(line)[1]), time.Now()
				mu.Unlock()
				s.write(SlotState{RunnerName: name, State: "busy", Job: job})
			case reDone.MatchString(line):
				m := reDone.FindStringSubmatch(line)
				mu.Lock()
				busy = false
				sec := int(time.Since(started).Seconds())
				mu.Unlock()
				s.parent.appendHistory(HistoryEntry{Time: time.Now(), Slot: s.n, RunnerName: name, Job: strings.TrimSpace(m[1]), Result: m[2], Seconds: sec})
			}
		}
	}()

	stop := func() { _ = exec.Command("docker", "stop", "-t", "30", container).Run() }
	drainCh := s.drain
	ctxDone := ctx.Done()
	for {
		select {
		case err := <-waitErr:
			mu.Lock()
			defer mu.Unlock()
			if err != nil && !ranJob {
				return false, fmt.Errorf("job container exited before taking a job: %w", err)
			}
			return ranJob, nil
		case <-ctxDone:
			stop()
			ctxDone = nil
		case <-drainCh:
			mu.Lock()
			b := busy
			mu.Unlock()
			if !b {
				stop() // idle: stop now; busy: let the job finish
			} else {
				s.write(SlotState{RunnerName: name, State: "draining", Job: job})
			}
			drainCh = nil // only react once; a nil channel blocks forever
		}
	}
}

func (sv *Supervisor) appendHistory(e HistoryEntry) {
	sv.hmu.Lock()
	defer sv.hmu.Unlock()
	_ = os.MkdirAll(filepath.Dir(sv.History), 0o755)
	f, err := os.OpenFile(sv.History, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	b, _ := json.Marshal(e)
	f.Write(append(b, '\n'))
	f.Close()
	trimHistory(sv.History, 1000)
}

func trimHistory(path string, keep int) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) <= keep*2 {
		return
	}
	_ = config.WriteFileAtomic(path, []byte(strings.Join(lines[len(lines)-keep:], "\n")+"\n"), 0o644)
}
