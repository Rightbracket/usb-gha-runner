// Package prune frees space on /data by clearing builder caches. Images and
// containers labelled ghr.keep=true (the job image and the registry mirror
// container) are never removed. The registry image itself is kept while the
// mirror container uses it, and is reloaded from the stick if it goes missing.
package prune

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Rightbracket/usb-gha-runner/ghr/internal/config"
)

type Result struct {
	BeforePct float64  `json:"before_pct"`
	AfterPct  float64  `json:"after_pct"`
	Skipped   bool     `json:"skipped"`
	Steps     []string `json:"steps"`
}

// UsedPct returns how full the filesystem holding dir is, in percent.
func UsedPct(dir string) (float64, error) {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(dir, &fs); err != nil {
		return 0, err
	}
	if fs.Blocks == 0 {
		return 0, nil
	}
	return 100 * (1 - float64(fs.Bavail)/float64(fs.Blocks)), nil
}

// ErrBusy is returned when another prune (the nightly one or "Prune now")
// holds the lock.
var ErrBusy = errors.New("a prune is already running")

// lockPath is the cross-process lock that serialises prunes; tests move it.
var lockPath = config.RunDir + "/prune.lock"

// usedPct is UsedPct; tests replace it.
var usedPct = UsedPct

// lock takes the prune lock without waiting. The kernel drops it if the
// process dies, so a crashed prune never leaves it held.
func lock() (unlock func(), err error) {
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return nil, err
	}
	// 0600: flock works on any open descriptor, so a file others can open
	// would let any local user hold the lock.
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrBusy
		}
		return nil, fmt.Errorf("prune lock: %w", err)
	}
	return func() { f.Close() }, nil // closing the file releases the lock
}

// Run prunes when /data is above threshold percent full. threshold <= 0
// prunes unconditionally (the "Prune now" button). Only one prune runs at a
// time; a second one gets ErrBusy. Once ctx is cancelled no further step
// starts, and the registry mirror is left alone.
func Run(ctx context.Context, threshold float64) (*Result, error) {
	unlock, err := lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	before, err := usedPct(config.DataDir)
	if err != nil {
		return nil, err
	}
	r := &Result{BeforePct: before}
	if threshold > 0 && before < threshold {
		r.Skipped, r.AfterPct = true, before
		return r, nil
	}
	steps := [][]string{
		{"docker", "container", "prune", "-f", "--filter", "label!=ghr.keep=true"},
		{"docker", "image", "prune", "-af", "--filter", "label!=ghr.keep=true"},
		{"docker", "builder", "prune", "-af"},
		{"docker", "volume", "prune", "-af"},
		{"podman", "system", "prune", "-af"},
	}
	for _, s := range steps {
		if err := ctx.Err(); err != nil {
			return r, err
		}
		line, _ := run(ctx, s...)
		r.Steps = append(r.Steps, line)
	}
	if err := ctx.Err(); err != nil {
		return r, err // stopped (SIGTERM): don't take the mirror down now
	}
	// Still too full: drop the registry mirror's cached layers too.
	var mirrorErr error
	if after, _ := usedPct(config.DataDir); threshold > 0 && after >= threshold || threshold <= 0 {
		var steps []string
		steps, mirrorErr = clearMirror(ctx, mirrorCache)
		r.Steps = append(r.Steps, steps...)
	}
	r.AfterPct, _ = usedPct(config.DataDir)
	return r, mirrorErr
}

// mirrorCache holds the registry mirror's cached layers.
var mirrorCache = "/data/registry-mirror/docker"

// clearMirror stops the mirror, deletes its cache and starts it again. Once
// begun it runs to the end even if ctx is cancelled, so the mirror is never
// left stopped, and the cache is deleted only after the stop succeeded. It
// returns an error if it stopped the mirror and the mirror didn't start
// again. A box without a mirror (ensureMirror allows that) is not an error.
func clearMirror(ctx context.Context, dir string) ([]string, error) {
	ctx = context.WithoutCancel(ctx)
	line, stopErr := run(ctx, "docker", "stop", "ghr-mirror")
	steps := []string{line}
	if stopErr != nil {
		steps = append(steps, "kept registry mirror cache: the mirror didn't stop")
	} else if err := os.RemoveAll(dir); err != nil {
		steps = append(steps, "clear mirror: "+err.Error())
	} else {
		steps = append(steps, "cleared registry mirror cache")
	}
	line, err := run(ctx, "docker", "start", "ghr-mirror")
	steps = append(steps, line)
	if err != nil && stopErr == nil {
		return steps, fmt.Errorf("the registry mirror didn't start again after pruning, so jobs pull from Docker Hub directly until it does (%s)", line)
	}
	return steps, nil
}

// execCmd builds the commands that run executes; tests replace it.
var execCmd = exec.CommandContext

func run(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	out, err := execCmd(ctx, args[0], args[1:]...).CombinedOutput()
	line := fmt.Sprintf("%v: ok", args)
	if err != nil {
		line = fmt.Sprintf("%v: %v: %s", args, err, lastLine(out))
	}
	log.Print(line)
	return line, err
}

func lastLine(b []byte) string {
	s := string(b)
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '\n' {
			return s[i+1:]
		}
	}
	return s
}
