package sshctl

import "testing"

func TestCleanKeys(t *testing.T) {
	out, err := CleanKeys("# mine\r\n\r\nssh-ed25519 AAAAC3 me@laptop\n  ecdsa-sha2-nistp256 AAAAE2 other  \n")
	if err != nil {
		t.Fatal(err)
	}
	if out != "ssh-ed25519 AAAAC3 me@laptop\necdsa-sha2-nistp256 AAAAE2 other\n" {
		t.Fatalf("got %q", out)
	}
	for _, bad := range []string{`command="rm -rf /" ssh-ed25519 AAAA`, "hunter2", "ssh-ed25519"} {
		if _, err := CleanKeys(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if out, err := CleanKeys("\n# nothing\n"); err != nil || out != "" {
		t.Errorf("empty: %q %v", out, err)
	}
}
