// Package importer reads the user-editable ghr/ folder on the GHR-BOOT FAT
// partition at boot: an optional preset admin password, runner.env, and SSH
// public keys. It runs as root before ghr-web and ghr-agent start.
package importer

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Rightbracket/usb-gha-runner/ghr/internal/auth"
	"github.com/Rightbracket/usb-gha-runner/ghr/internal/config"
	"github.com/Rightbracket/usb-gha-runner/ghr/internal/sshctl"
)

const (
	BootDir      = "/run/live/medium/ghr"
	WebDir       = "/data/etc/ghr-web"
	PasswordFile = WebDir + "/admin.json"
	WebUser      = "ghr-web"
	stampFile    = config.EtcDir + "/imported-runner-env.sha256"
	// PasswordErrorFile exists while ghr/admin-password is present but
	// couldn't be applied. ghr-web keeps /setup closed while it does, so a
	// stick meant to ship with a password is never left open to the LAN.
	PasswordErrorFile = config.RunDir + "/admin-password-error"
)

type Paths struct {
	BootDir, ConfigPath, PasswordFile, StampFile, PasswordErrorFile string
}

func DefaultPaths() Paths {
	return Paths{BootDir: BootDir, ConfigPath: config.ConfigPath, PasswordFile: PasswordFile, StampFile: stampFile, PasswordErrorFile: PasswordErrorFile}
}

// Run imports what it finds. Each piece is independent, so one bad file
// doesn't block the others; errors are logged and returned joined.
func Run(p Paths) error {
	var errs []string
	if err := importPassword(p); err != nil {
		errs = append(errs, err.Error())
	}
	if err := importEnv(p); err != nil {
		errs = append(errs, err.Error())
	}
	if len(errs) > 0 {
		return fmt.Errorf("import: %s", strings.Join(errs, "; "))
	}
	return nil
}

// importPassword uses ghr/admin-password only while no password is set, so
// the setup page is never open on a stick that ships with one.
func importPassword(p Paths) (err error) {
	if p.PasswordErrorFile != "" {
		_ = os.Remove(p.PasswordErrorFile)
		defer func() {
			if err != nil {
				_ = os.MkdirAll(filepath.Dir(p.PasswordErrorFile), 0o755)
				_ = config.WriteFileAtomic(p.PasswordErrorFile, []byte(err.Error()+"\n"), 0o644)
			}
		}()
	}
	src := filepath.Join(p.BootDir, "admin-password")
	b, err := os.ReadFile(src)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("ghr/admin-password: %w", err)
	}
	st := auth.NewStore(p.PasswordFile)
	if st.HasPassword() {
		return nil
	}
	pw := strings.TrimRight(strings.TrimPrefix(string(b), "\ufeff"), "\r\n")
	if err := st.SetPassword(pw); err != nil {
		return fmt.Errorf("ghr/admin-password: %w", err)
	}
	chownWeb(filepath.Dir(p.PasswordFile))
	chownWeb(p.PasswordFile)
	log.Printf("admin password imported from %s", src)
	return nil
}

// importEnv applies ghr/runner.env whenever its contents change, so editing
// the file on another computer and rebooting reconfigures the stick.
func importEnv(p Paths) error {
	src := filepath.Join(p.BootDir, "runner.env")
	b, err := os.ReadFile(src)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	env, err := config.ParseEnvFile(src)
	if err != nil {
		return err
	}
	// The stamp covers the files runner.env points to as well, so replacing
	// the App key or editing authorized_keys on the stick takes effect too.
	h := sha256.New()
	h.Write(b)
	for _, k := range []string{"GHR_APP_PRIVATE_KEY_FILE", "GHR_SSH_AUTHORIZED_KEYS_FILE"} {
		if f := env[k]; f != "" {
			if !filepath.IsAbs(f) {
				f = filepath.Join(p.BootDir, f)
			}
			fb, _ := os.ReadFile(f)
			fmt.Fprintf(h, "\x00%s\x00%d\x00", k, len(fb))
			h.Write(fb)
		}
	}
	stamp := hex.EncodeToString(h.Sum(nil))
	if old, _ := os.ReadFile(p.StampFile); strings.TrimSpace(string(old)) == stamp {
		return nil
	}
	cur, err := config.Load(p.ConfigPath)
	if err != nil {
		return err
	}
	var next *config.Config
	if env["GHR_URL"] != "" {
		next, err = config.FromEnv(env, p.BootDir)
		if err != nil {
			return fmt.Errorf("ghr/runner.env: %w", err)
		}
	} else {
		next = cur // SSH-only runner.env
	}
	// SSH settings: keep what's there unless runner.env says otherwise.
	next.SSH = cur.SSH
	if kf := env["GHR_SSH_AUTHORIZED_KEYS_FILE"]; kf != "" {
		if !filepath.IsAbs(kf) {
			kf = filepath.Join(p.BootDir, kf)
		}
		kb, err := os.ReadFile(kf)
		if err != nil {
			return fmt.Errorf("ghr/runner.env SSH keys: %w", err)
		}
		keys, err := sshctl.CleanKeys(string(kb))
		if err != nil {
			return fmt.Errorf("ghr/runner.env SSH keys: %w", err)
		}
		next.SSH = config.SSH{Enabled: keys != "", AuthorizedKeys: keys}
	}
	switch strings.ToLower(env["GHR_SSH"]) {
	case "on", "true", "1", "yes":
		next.SSH.Enabled = next.SSH.AuthorizedKeys != ""
	case "off", "false", "0", "no":
		next.SSH.Enabled = false
	}
	if err := config.Save(p.ConfigPath, next); err != nil {
		return err
	}
	log.Printf("configuration imported from %s", src)
	return config.WriteFileAtomic(p.StampFile, []byte(stamp+"\n"), 0o600)
}

func chownWeb(path string) {
	u, err := user.Lookup(WebUser)
	if err != nil {
		return
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	_ = os.Chown(path, uid, gid)
}
