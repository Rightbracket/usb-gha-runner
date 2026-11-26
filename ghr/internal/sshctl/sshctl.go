// Package sshctl turns the SSH server on or off. SSH is key-only, logs in as
// the "ghr" admin user, and keeps its host key and authorized keys on /data
// so they survive reboots. The firewall opens port 22 only while it's on.
package sshctl

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Rightbracket/usb-gha-runner/ghr/internal/config"
)

var (
	Dir            = filepath.Join(config.EtcDir, "ssh")
	AuthorizedKeys = filepath.Join(Dir, "authorized_keys")
	HostKey        = filepath.Join(Dir, "ssh_host_ed25519_key")
)

var keyTypes = []string{
	"ssh-ed25519", "ssh-rsa", "ecdsa-sha2-nistp256", "ecdsa-sha2-nistp384",
	"ecdsa-sha2-nistp521", "sk-ssh-ed25519@openssh.com", "sk-ecdsa-sha2-nistp256@openssh.com",
}

// CleanKeys keeps blank lines and comments out and rejects anything that
// isn't a plain public key line. Options (command=, from=) aren't allowed.
func CleanKeys(in string) (string, error) {
	var out []string
	for i, line := range strings.Split(strings.ReplaceAll(in, "\r", ""), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		ok := false
		for _, t := range keyTypes {
			if f[0] == t {
				ok = true
			}
		}
		if !ok || len(f) < 2 {
			return "", fmt.Errorf("line %d is not an SSH public key (it should start with ssh-ed25519, ssh-rsa or ecdsa-…)", i+1)
		}
		out = append(out, line)
	}
	if len(out) == 0 {
		return "", nil
	}
	return strings.Join(out, "\n") + "\n", nil
}

// Apply writes the keys, then starts or stops sshd and the firewall opening.
func Apply(s config.SSH) error {
	if err := os.MkdirAll(Dir, 0o755); err != nil {
		return err
	}
	keys, err := CleanKeys(s.AuthorizedKeys)
	if err != nil {
		return err
	}
	if err := config.WriteFileAtomic(AuthorizedKeys, []byte(keys), 0o644); err != nil {
		return err
	}
	if !s.Enabled {
		_ = exec.Command("systemctl", "stop", "ssh.service").Run()
		_ = exec.Command("nft", "delete", "element", "inet", "ghr", "tcp_open", "{ 22 }").Run()
		return nil
	}
	if keys == "" {
		return errors.New("SSH needs at least one authorized key")
	}
	if _, err := os.Stat(HostKey); err != nil {
		if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "ghr", "-f", HostKey).CombinedOutput(); err != nil {
			return fmt.Errorf("ssh-keygen: %v: %s", err, out)
		}
	}
	if out, err := exec.Command("nft", "add", "element", "inet", "ghr", "tcp_open", "{ 22 }").CombinedOutput(); err != nil {
		return fmt.Errorf("open port 22: %v: %s", err, out)
	}
	if out, err := exec.Command("systemctl", "start", "ssh.service").CombinedOutput(); err != nil {
		return fmt.Errorf("start sshd: %v: %s", err, out)
	}
	return nil
}
