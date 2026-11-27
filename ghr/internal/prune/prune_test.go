package prune

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeDocker replaces execCmd: it records each command and fails the ones
// named in fail. Commands still run when ctx is cancelled, as exec would not.
func fakeDocker(t *testing.T, fail ...string) *[]string {
	var ran []string
	old := execCmd
	t.Cleanup(func() { execCmd = old })
	execCmd = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if ctx.Err() != nil {
			t.Errorf("%s %v run with a cancelled context", name, args)
		}
		c := strings.Join(append([]string{name}, args...), " ")
		ran = append(ran, c)
		if onCmd != nil {
			onCmd(c)
		}
		if slices.Contains(fail, c) {
			// Like docker: the daemon's reason first, then a generic line.
			return exec.Command("sh", "-c", `echo "Error response from daemon: boom" >&2; echo "Error: failed to start containers: ghr-mirror" >&2; exit 1`)
		}
		return exec.Command("true")
	}
	return &ran
}

// onCmd, when set, is called with each command fakeDocker runs.
var onCmd func(string)

// fakeData swaps in a lock file, a mirror cache and a /data usage figure.
func fakeData(t *testing.T, pct float64) string {
	oldLock, oldUsed, oldCache := lockPath, usedPct, mirrorCache
	t.Cleanup(func() { lockPath, usedPct, mirrorCache, onCmd = oldLock, oldUsed, oldCache, nil })
	lockPath = filepath.Join(t.TempDir(), "run", "prune.lock")
	usedPct = func(string) (float64, error) { return pct, nil }
	mirrorCache = cacheDir(t)
	return mirrorCache
}

func cacheDir(t *testing.T) string {
	d := filepath.Join(t.TempDir(), "docker")
	if err := os.MkdirAll(filepath.Join(d, "blobs"), 0o755); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestClearMirrorKeepsCacheWhenStopFails(t *testing.T) {
	ran := fakeDocker(t, "docker stop ghr-mirror")
	d := cacheDir(t)
	if _, err := clearMirror(context.Background(), d); err != nil {
		t.Errorf("mirror started again, but got %v", err)
	}
	if _, err := os.Stat(d); err != nil {
		t.Errorf("cache removed although the stop failed: %v", err)
	}
	if want := []string{"docker stop ghr-mirror", "docker start ghr-mirror"}; !slices.Equal(*ran, want) {
		t.Errorf("ran %v, want %v", *ran, want)
	}
}

func TestClearMirrorFinishesWhenCancelled(t *testing.T) {
	ran := fakeDocker(t)
	d := cacheDir(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := clearMirror(ctx, d); err != nil {
		t.Error(err)
	}
	if _, err := os.Stat(d); !os.IsNotExist(err) {
		t.Errorf("cache still there after a successful stop: %v", err)
	}
	if want := []string{"docker stop ghr-mirror", "docker start ghr-mirror"}; !slices.Equal(*ran, want) {
		t.Errorf("ran %v, want %v", *ran, want)
	}
}

func TestSecondPruneIsRefused(t *testing.T) {
	ran := fakeDocker(t)
	fakeData(t, 90)
	unlock, err := lock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lock(); !errors.Is(err, ErrBusy) {
		t.Fatalf("second lock: %v, want ErrBusy", err)
	}
	if _, err := Run(context.Background(), 0); !errors.Is(err, ErrBusy) || len(*ran) != 0 {
		t.Fatalf("Run while locked: err=%v, ran %v", err, *ran)
	}
	unlock()
	// Run holds the lock while its steps run.
	var during []error
	onCmd = func(c string) {
		u, err := lock()
		if err == nil {
			u()
			t.Errorf("lock() succeeded during %q", c)
		} else if !errors.Is(err, ErrBusy) {
			t.Errorf("lock() during %q: %v, want ErrBusy", c, err)
		}
		during = append(during, err)
	}
	if _, err := Run(context.Background(), 0); err != nil {
		t.Fatalf("Run after unlock: %v", err)
	}
	onCmd = nil
	if len(during) != len(*ran) || len(during) == 0 {
		t.Errorf("lock() tried during %d of %d steps", len(during), len(*ran))
	}
	// Run released the lock when it returned.
	unlock, err = lock()
	if err != nil {
		t.Fatalf("lock after Run: %v", err)
	}
	unlock()
}

// A nightly prune stopped by SIGTERM starts no further step and leaves the
// registry mirror running, whether the signal lands mid-way or during the
// last step.
func TestCancelledPruneLeavesMirror(t *testing.T) {
	for _, tc := range []struct{ during, mustNotRun string }{
		{"docker builder prune -af", "docker volume prune -af"},
		{"podman system prune -af", "ghr-mirror"},
	} {
		ran := fakeDocker(t)
		cache := fakeData(t, 90)
		ctx, cancel := context.WithCancel(context.Background())
		onCmd = func(c string) {
			if c == tc.during {
				cancel()
			}
		}
		if _, err := Run(ctx, 80); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled during %q: err=%v, want context.Canceled", tc.during, err)
		}
		for _, c := range *ran {
			if strings.Contains(c, tc.mustNotRun) || strings.Contains(c, "ghr-mirror") {
				t.Errorf("cancelled during %q, but ran %q (all: %v)", tc.during, c, *ran)
			}
		}
		if _, err := os.Stat(cache); err != nil {
			t.Errorf("mirror cache removed: %v", err)
		}

		// Cancelled before it starts: nothing runs.
		*ran = nil
		if _, err := Run(ctx, 80); !errors.Is(err, context.Canceled) || len(*ran) != 0 {
			t.Errorf("pre-cancelled Run: err=%v ran %v", err, *ran)
		}
	}
}

func TestMirrorRestartFailureIsReported(t *testing.T) {
	fakeDocker(t, "docker start ghr-mirror")
	fakeData(t, 90)
	r, err := Run(context.Background(), 80)
	if err == nil || !strings.Contains(err.Error(), "didn't start again") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err=%v, want the failed docker start", err)
	}
	if r == nil || !slices.ContainsFunc(r.Steps, func(s string) bool { return strings.Contains(s, "docker start ghr-mirror") }) {
		t.Errorf("steps %v", r)
	}
}

// A box without a mirror container (stop and start both fail) is fine.
func TestNoMirrorIsNotAnError(t *testing.T) {
	fakeDocker(t, "docker stop ghr-mirror", "docker start ghr-mirror")
	cache := fakeData(t, 90)
	if _, err := Run(context.Background(), 0); err != nil {
		t.Fatalf("err=%v, want nil without a mirror", err)
	}
	if _, err := os.Stat(cache); err != nil {
		t.Errorf("cache removed although the mirror didn't stop: %v", err)
	}
}

func TestPruneClearsMirrorWhenStillFull(t *testing.T) {
	ran := fakeDocker(t)
	cache := fakeData(t, 90)
	r, err := Run(context.Background(), 80)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(*ran, "docker stop ghr-mirror") || !slices.Contains(r.Steps, "cleared registry mirror cache") {
		t.Errorf("ran %v, steps %v", *ran, r.Steps)
	}
	if _, err := os.Stat(cache); !os.IsNotExist(err) {
		t.Errorf("cache still there: %v", err)
	}
}
