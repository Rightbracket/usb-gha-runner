package prune

import (
	"context"
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
		if slices.Contains(fail, c) {
			return exec.Command("false")
		}
		return exec.Command("true")
	}
	return &ran
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
	clearMirror(context.Background(), d)
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
	clearMirror(ctx, d)
	if _, err := os.Stat(d); !os.IsNotExist(err) {
		t.Errorf("cache still there after a successful stop: %v", err)
	}
	if want := []string{"docker stop ghr-mirror", "docker start ghr-mirror"}; !slices.Equal(*ran, want) {
		t.Errorf("ran %v, want %v", *ran, want)
	}
}
