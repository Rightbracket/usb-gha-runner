package agent

import "testing"

func TestMirrorStale(t *testing.T) {
	const cur, old = "sha256:new", "sha256:old"
	for _, c := range []struct {
		state, want string
		stale       bool
	}{
		{"true sha256:new\n", cur + "\n", false},
		{"true " + old, cur, true},  // upgraded in place: old container still running
		{"false " + cur, cur, true}, // stopped
		{"", cur, true},             // no container
		{"true " + cur, "", true},   // image ID unknown
	} {
		if got := mirrorStale(c.state, c.want); got != c.stale {
			t.Errorf("mirrorStale(%q, %q) = %v, want %v", c.state, c.want, got, c.stale)
		}
	}
}
