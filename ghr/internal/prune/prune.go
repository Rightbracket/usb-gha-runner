// Package prune frees space on /data by clearing builder caches. Images and
// containers labelled ghr.keep=true (the job image and the registry mirror
// container) are never removed. The registry image itself is kept while the
// mirror container uses it, and is reloaded from the stick if it goes missing.
package prune

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
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

// Run prunes when /data is above threshold percent full. threshold <= 0
// prunes unconditionally (the "Prune now" button).
func Run(ctx context.Context, threshold float64) (*Result, error) {
	before, err := UsedPct(config.DataDir)
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
		line, _ := run(ctx, s...)
		r.Steps = append(r.Steps, line)
	}
	// Still too full: drop the registry mirror's cached layers too.
	if after, _ := UsedPct(config.DataDir); threshold > 0 && after >= threshold || threshold <= 0 {
		r.Steps = append(r.Steps, clearMirror(ctx, mirrorCache)...)
	}
	r.AfterPct, _ = UsedPct(config.DataDir)
	return r, nil
}

// mirrorCache holds the registry mirror's cached layers.
var mirrorCache = "/data/registry-mirror/docker"

// clearMirror stops the mirror, deletes its cache and starts it again. Once
// begun it runs to the end even if ctx is cancelled, so the mirror is never
// left stopped, and the cache is deleted only after the stop succeeded.
func clearMirror(ctx context.Context, dir string) []string {
	ctx = context.WithoutCancel(ctx)
	line, err := run(ctx, "docker", "stop", "ghr-mirror")
	steps := []string{line}
	if err != nil {
		steps = append(steps, "kept registry mirror cache: the mirror didn't stop")
	} else if err := os.RemoveAll(dir); err != nil {
		steps = append(steps, "clear mirror: "+err.Error())
	} else {
		steps = append(steps, "cleared registry mirror cache")
	}
	line, _ = run(ctx, "docker", "start", "ghr-mirror")
	return append(steps, line)
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
