package supervisor

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Rightbracket/usb-gha-runner/ghr/internal/config"
	"github.com/Rightbracket/usb-gha-runner/ghr/internal/gh"
)

// fakeAPI stands in for GitHub. del, when set, answers the n-th DELETE
// (counting from 1); otherwise DELETE succeeds.
type fakeAPI struct {
	mu      sync.Mutex
	del     func(n int) error
	deletes int
	jits    [][]string
	nextID  int64
}

func (f *fakeAPI) JITConfig(_ context.Context, name string, labels []string) (string, gh.Runner, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jits = append(f.jits, labels)
	f.nextID++
	return "ENC", gh.Runner{ID: f.nextID, Name: name}, nil
}

func (f *fakeAPI) FindRunner(context.Context, string) (*gh.Runner, error) { return nil, nil }

func (f *fakeAPI) DeleteRunner(context.Context, int64) error {
	f.mu.Lock()
	f.deletes++
	n, del := f.deletes, f.del
	f.mu.Unlock()
	if del != nil {
		return del(n)
	}
	return nil
}

func (f *fakeAPI) deleteCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.deletes
}

func (f *fakeAPI) jitLabels() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]string(nil), f.jits...)
}

// fakeContainer is one "docker run": the test writes its output and decides
// when it exits.
type fakeContainer struct {
	name string
	out  *io.PipeWriter
	wait chan error
	once sync.Once
}

func (c *fakeContainer) exit(err error) {
	c.once.Do(func() {
		c.out.Close()
		c.wait <- err
	})
}

// say writes a runner output line and returns once the supervisor has
// handled it. io.Pipe hands each Write straight to a Read, and readLines
// handles every buffered line before it reads again, so when the second
// Write returns the first line has been classified.
func (c *fakeContainer) say(line string) {
	_, _ = io.WriteString(c.out, line+"\n")
	_, _ = io.WriteString(c.out, "(sync)\n")
}

// fakeDocker records docker CLI calls. A container exits on its
// exitOnStop-th "docker stop" (1 when zero).
type fakeDocker struct {
	mu         sync.Mutex
	live       map[string]*fakeContainer
	started    chan *fakeContainer
	starts     int
	stops      int
	stopsBy    map[string]int
	exitOnStop int
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{live: map[string]*fakeContainer{}, started: make(chan *fakeContainer, 100), stopsBy: map[string]int{}}
}

func (d *fakeDocker) start(args, env []string) (io.Reader, <-chan error, error) {
	name := ""
	for i, a := range args {
		if a == "--name" && i+1 < len(args) {
			name = args[i+1]
		}
	}
	pr, pw := io.Pipe()
	c := &fakeContainer{name: name, out: pw, wait: make(chan error, 1)}
	d.mu.Lock()
	d.live[name] = c
	d.starts++
	d.mu.Unlock()
	d.started <- c
	return pr, c.wait, nil
}

func (d *fakeDocker) run(args ...string) error {
	if len(args) == 0 {
		return nil
	}
	name := args[len(args)-1]
	d.mu.Lock()
	c := d.live[name]
	exit := false
	switch args[0] {
	case "stop":
		d.stops++
		d.stopsBy[name]++
		n := d.exitOnStop
		if n == 0 {
			n = 1
		}
		exit = c != nil && d.stopsBy[name] >= n
	case "rm":
		exit = c != nil
	}
	if exit {
		delete(d.live, name)
		d.stopsBy[name] = 0
	}
	d.mu.Unlock()
	if exit {
		c.exit(fmt.Errorf("stopped"))
	}
	return nil
}

func (d *fakeDocker) counts() (starts, stops, live int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.starts, d.stops, len(d.live)
}

// fakeTimer is one retry wait the supervisor asked for.
type fakeTimer struct {
	d  time.Duration
	ch chan time.Time
}

// harness runs one slot's runContainer against fakes, with a clock that
// moves only when the test fires a timer.
type harness struct {
	t      *testing.T
	api    *fakeAPI
	docker *fakeDocker
	timers chan fakeTimer
	ctx    context.Context
	cancel context.CancelFunc
	s      *slot
	c      *fakeContainer
	result chan error
	ranJob atomic.Bool

	mu  sync.Mutex
	now time.Time
}

func newHarness(t *testing.T, del func(n int) error) *harness {
	h := &harness{t: t, api: &fakeAPI{del: del}, docker: newFakeDocker(),
		timers: make(chan fakeTimer, 1000), result: make(chan error, 1), now: time.Unix(1_800_000_000, 0)}
	h.ctx, h.cancel = context.WithCancel(context.Background())
	t.Cleanup(h.cancel)
	dir := t.TempDir()
	sv := &Supervisor{
		SlotsDir: filepath.Join(dir, "slots"),
		History:  filepath.Join(dir, "history.jsonl"),
		startJob: h.docker.start,
		docker:   h.docker.run,
		now: func() time.Time {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.now
		},
		after: func(d time.Duration) <-chan time.Time {
			ch := make(chan time.Time, 1)
			h.timers <- fakeTimer{d, ch}
			return ch
		},
	}
	h.s = &slot{n: 1, cfg: &config.Config{}, cpus: 1, memMB: 1024, drain: make(chan struct{}), done: make(chan struct{}), parent: sv}
	return h
}

// start runs the container and waits for it to print "Listening for Jobs".
func (h *harness) start() {
	go func() {
		ran, err := h.s.runContainer(h.ctx, h.api, "ghr-slot-1", "ghr-test-1", "ENC", 7)
		h.ranJob.Store(ran)
		h.result <- err
	}()
	select {
	case h.c = <-h.docker.started:
	case <-time.After(5 * time.Second):
		h.t.Fatal("container never started")
	}
	h.c.say("2026-11-26 18:00:00Z: Listening for Jobs")
}

func (h *harness) elapsed() time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.now.Sub(time.Unix(1_800_000_000, 0))
}

// nextTimer waits for the supervisor to schedule its next retry.
func (h *harness) nextTimer() fakeTimer {
	h.t.Helper()
	select {
	case tm := <-h.timers:
		return tm
	case <-time.After(5 * time.Second):
		h.t.Fatalf("no retry scheduled (DELETEs %d, at +%s)", h.api.deleteCount(), h.elapsed())
		return fakeTimer{}
	}
}

func (h *harness) fire(tm fakeTimer) {
	h.mu.Lock()
	h.now = h.now.Add(tm.d)
	now := h.now
	h.mu.Unlock()
	tm.ch <- now
}

func (h *harness) stops() int { _, s, _ := h.docker.counts(); return s }

// wait returns runContainer's result.
func (h *harness) wait() (bool, error) {
	h.t.Helper()
	select {
	case err := <-h.result:
		return h.ranJob.Load(), err
	case <-time.After(5 * time.Second):
		h.t.Fatal("runContainer did not return")
		return false, nil
	}
}

// shutdown cancels the context, which stops even a busy runner.
func (h *harness) shutdown() {
	h.t.Helper()
	h.cancel()
	if _, err := h.wait(); err == nil {
		h.t.Error("container stopped before a job: want an error")
	}
}

func status(code int) error { return &gh.APIError{Status: code} }

// While GitHub answers 422 (the runner has a job it hasn't printed), the
// container is never stopped, however long that lasts.
func TestDrainWaitsWhileDeleteIs422(t *testing.T) {
	h := newHarness(t, func(int) error { return status(422) })
	h.start()
	h.s.startDrain()
	for i := 0; i < 40; i++ { // 200 s of 422s
		tm := h.nextTimer()
		if tm.d != 5*time.Second {
			t.Fatalf("retry %d after %s, want 5s", i, tm.d)
		}
		if h.stops() != 0 {
			t.Fatalf("stopped after %d DELETEs answered 422", h.api.deleteCount())
		}
		h.fire(tm)
	}
	h.nextTimer()
	if h.stops() != 0 {
		t.Fatal("stopped while GitHub still answered 422")
	}
	h.shutdown()
}

// When GitHub can't answer (401), the slot retries for 60 s, then stops
// the runner as idle.
func TestDrainGivesUpOn401After60s(t *testing.T) {
	h := newHarness(t, func(int) error { return status(401) })
	h.start()
	h.s.startDrain()
	for {
		tm := h.nextTimer()
		if tm.d == time.Second { // the stop's retry
			break
		}
		if tm.d != 10*time.Second {
			t.Fatalf("retry after %s, want 10s", tm.d)
		}
		if h.stops() != 0 {
			t.Fatalf("stopped at +%s, before the 60 s deadline", h.elapsed())
		}
		h.fire(tm)
	}
	if e := h.elapsed(); e != 60*time.Second {
		t.Errorf("stopped at +%s, want +60s", e)
	}
	if h.stops() != 1 || h.api.deleteCount() != 7 {
		t.Errorf("stops=%d DELETEs=%d, want 1 and 7", h.stops(), h.api.deleteCount())
	}
	if ran, err := h.wait(); ran || err == nil {
		t.Errorf("ranJob=%v err=%v", ran, err)
	}
}

// A 204 deregisters the runner once; stop retries don't DELETE again.
func TestDrainDeletesOnceThenStops(t *testing.T) {
	h := newHarness(t, nil)
	h.docker.exitOnStop = 2 // the first stop misses the container
	h.start()
	h.s.startDrain()
	tm := h.nextTimer()
	if tm.d != time.Second || h.stops() != 1 || h.api.deleteCount() != 1 {
		t.Fatalf("after 204: retry %s, stops=%d DELETEs=%d", tm.d, h.stops(), h.api.deleteCount())
	}
	h.fire(tm)
	if ran, err := h.wait(); ran || err == nil {
		t.Errorf("ranJob=%v err=%v", ran, err)
	}
	if h.stops() != 2 || h.api.deleteCount() != 1 {
		t.Errorf("stops=%d DELETEs=%d, want 2 and 1", h.stops(), h.api.deleteCount())
	}
}

// The 60 s deadline counts errors in a row: a 5xx after a long run of
// 422s starts a fresh deadline instead of stopping at once.
func TestDrain5xxAfterLong422DoesNotStop(t *testing.T) {
	h := newHarness(t, func(n int) error {
		if n <= 14 { // 70 s of 422s
			return status(422)
		}
		return status(502)
	})
	h.start()
	h.s.startDrain()
	for i := 0; i < 14; i++ {
		h.fire(h.nextTimer())
	}
	tm := h.nextTimer()
	if h.api.deleteCount() != 15 || tm.d != 10*time.Second || h.stops() != 0 {
		t.Fatalf("after the 5xx: DELETEs=%d retry=%s stops=%d, want 15, 10s, 0", h.api.deleteCount(), tm.d, h.stops())
	}
	h.shutdown()
}

// A job that starts while the last failing DELETE is in flight keeps the
// container running, even past the deadline.
func TestDrainJobStartsDuringFailingDelete(t *testing.T) {
	var h *harness
	h = newHarness(t, func(n int) error {
		if n == 7 { // the DELETE at the deadline
			h.c.say("2026-11-26 18:01:00Z: Running job: build")
		}
		return status(401)
	})
	h.start()
	h.s.startDrain()
	for i := 0; i < 6; i++ {
		tm := h.nextTimer()
		if tm.d != 10*time.Second {
			t.Fatalf("retry %d after %s", i, tm.d)
		}
		h.fire(tm)
	}
	// No timer follows: the slot now waits for the job to end.
	select {
	case tm := <-h.timers:
		t.Fatalf("scheduled a %s retry for a busy runner", tm.d)
	case <-time.After(100 * time.Millisecond):
	}
	h.c.say("2026-11-26 18:02:00Z: Job build completed with result: Succeeded")
	h.c.exit(nil)
	ran, err := h.wait()
	if !ran || err != nil {
		t.Errorf("ranJob=%v err=%v", ran, err)
	}
	if h.stops() != 0 {
		t.Errorf("stopped a runner with a job (%d stops)", h.stops())
	}
	b, _ := os.ReadFile(h.s.parent.History)
	if !strings.Contains(string(b), `"job":"build","result":"Succeeded"`) {
		t.Errorf("history: %s", b)
	}
}

// A busy runner is left alone on drain: no DELETE, no stop.
func TestDrainBusyRunnerFinishesJob(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	h.c.say("2026-11-26 18:01:00Z: Running job: build")
	h.s.startDrain()
	h.c.say("still building")
	h.c.say("2026-11-26 18:02:00Z: Job build completed with result: Failed")
	h.c.exit(nil)
	if ran, err := h.wait(); !ran || err != nil {
		t.Errorf("ranJob=%v err=%v", ran, err)
	}
	if h.stops() != 0 || h.api.deleteCount() != 0 {
		t.Errorf("stops=%d DELETEs=%d, want 0 and 0", h.stops(), h.api.deleteCount())
	}
}

// Shutting down stops the container straight away without asking GitHub.
func TestCancelStopsWithoutDelete(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	h.cancel()
	tm := h.nextTimer()
	if tm.d != time.Second || h.stops() != 1 || h.api.deleteCount() != 0 {
		t.Errorf("retry=%s stops=%d DELETEs=%d, want 1s, 1, 0", tm.d, h.stops(), h.api.deleteCount())
	}
	if _, err := h.wait(); err == nil {
		t.Error("want an error for a container stopped before a job")
	}
}

// runHarness runs a whole Supervisor against fakes, in real time with a
// short poll interval.
type runHarness struct {
	t      *testing.T
	sv     *Supervisor
	api    *fakeAPI
	docker *fakeDocker
	ready  atomic.Bool
	cfg    string
	mod    time.Time
	done   chan error
	cancel context.CancelFunc
}

func newRunHarness(t *testing.T, ready bool) *runHarness {
	dir := t.TempDir()
	h := &runHarness{t: t, api: &fakeAPI{}, docker: newFakeDocker(), cfg: filepath.Join(dir, "config.json"),
		mod: time.Unix(1_800_000_000, 0), done: make(chan error, 1)}
	h.ready.Store(ready)
	h.sv = &Supervisor{
		ConfigPath: h.cfg,
		SlotsDir:   filepath.Join(dir, "slots"),
		History:    filepath.Join(dir, "history.jsonl"),
		Hostname:   "ghr-test-0123abcd",
		JobImage:   "ghr-job:test",
		Ready:      h.ready.Load,
		newClient:  func(*config.Config) (runnerAPI, error) { return h.api, nil },
		startJob:   h.docker.start,
		docker:     h.docker.run,
		poll:       10 * time.Millisecond,
	}
	return h
}

// save writes the config with a new mtime, so the supervisor reloads it.
func (h *runHarness) save(c *config.Config) {
	h.t.Helper()
	if err := config.Save(h.cfg, c); err != nil {
		h.t.Fatal(err)
	}
	h.mod = h.mod.Add(time.Minute)
	if err := os.Chtimes(h.cfg, h.mod, h.mod); err != nil {
		h.t.Fatal(err)
	}
}

func (h *runHarness) run() {
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() { h.done <- h.sv.Run(ctx) }()
	h.t.Cleanup(func() {
		cancel()
		select {
		case <-h.done:
		case <-time.After(10 * time.Second):
			h.t.Error("Run did not return after cancel")
		}
	})
}

// live reports how many job containers are running.
func (h *runHarness) live() int { _, _, l := h.docker.counts(); return l }

func (h *runHarness) waitFor(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			s, st, l := h.docker.counts()
			h.t.Fatalf("timed out waiting for %s (starts=%d stops=%d live=%d slots=%d)", what, s, st, l, h.sv.count())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func workers(n int, labels ...string) *config.Config {
	return &config.Config{URL: "https://github.com/acme/app", Workers: fmt.Sprint(n), Labels: labels, Auth: config.Auth{Type: "pat", PAT: "tok"}}
}

func TestRunWaitsForReady(t *testing.T) {
	h := newRunHarness(t, false)
	h.save(workers(2))
	h.run()
	time.Sleep(200 * time.Millisecond) // 20 polls
	if s, _, _ := h.docker.counts(); s != 0 || h.sv.count() != 0 {
		t.Fatalf("started %d containers (%d slots) before an admin password existed", s, h.sv.count())
	}
	h.ready.Store(true)
	h.waitFor("2 slots", func() bool { return h.live() == 2 && h.sv.count() == 2 })
}

func TestRunRefillsAfterLabelChange(t *testing.T) {
	h := newRunHarness(t, true)
	h.save(workers(2, "old"))
	h.run()
	h.waitFor("2 slots", func() bool { return h.live() == 2 })
	h.save(workers(2, "new"))
	h.waitFor("2 new slots", func() bool {
		s, _, l := h.docker.counts()
		return s == 4 && l == 2 && h.sv.count() == 2
	})
	jits := h.api.jitLabels()
	if len(jits) != 4 {
		t.Fatalf("JIT requests: %v", jits)
	}
	for i, l := range jits {
		want := "old"
		if i >= 2 {
			want = "new"
		}
		if l[len(l)-1] != want {
			t.Errorf("JIT %d labels %v, want ...%s", i, l, want)
		}
	}
	// Each old runner was deregistered before it was stopped.
	if d := h.api.deleteCount(); d < 2 {
		t.Errorf("%d DELETEs for 2 drained runners", d)
	}
}

func TestStopAllLeavesNoSlots(t *testing.T) {
	h := newRunHarness(t, true)
	h.save(workers(3))
	h.run()
	h.waitFor("3 slots", func() bool { return h.live() == 3 })
	h.save(&config.Config{Workers: "auto", Auth: config.Auth{Type: "app"}}) // as agent.remove does
	if err := h.sv.StopAll(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	if n := h.sv.count(); n != 0 {
		t.Fatalf("%d slots after StopAll", n)
	}
	starts, _, _ := h.docker.counts()
	time.Sleep(200 * time.Millisecond)
	if s, _, l := h.docker.counts(); s != starts || l != 0 || h.sv.count() != 0 {
		t.Errorf("after StopAll: %d new starts, %d live, %d slots", s-starts, l, h.sv.count())
	}
}
