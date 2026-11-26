package importer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Rightbracket/usb-gha-runner/ghr/internal/auth"
	"github.com/Rightbracket/usb-gha-runner/ghr/internal/config"
)

func paths(t *testing.T) Paths {
	d := t.TempDir()
	boot := filepath.Join(d, "boot", "ghr")
	os.MkdirAll(boot, 0o755)
	return Paths{BootDir: boot, ConfigPath: filepath.Join(d, "etc", "config.json"),
		PasswordFile: filepath.Join(d, "web", "admin.json"), StampFile: filepath.Join(d, "etc", "stamp")}
}

func TestPresetPassword(t *testing.T) {
	p := paths(t)
	os.WriteFile(filepath.Join(p.BootDir, "admin-password"), []byte("preset password\r\n"), 0o600)
	if err := Run(p); err != nil {
		t.Fatal(err)
	}
	st := auth.NewStore(p.PasswordFile)
	if !st.Check("preset password") {
		t.Fatal("preset password not imported")
	}
	// A later change on the stick doesn't override a password set since.
	st.SetPassword("changed in the UI")
	os.WriteFile(filepath.Join(p.BootDir, "admin-password"), []byte("another one!"), 0o600)
	Run(p)
	if !st.Check("changed in the UI") {
		t.Fatal("existing password was overwritten")
	}
}

func TestRunnerEnvImportedOnChange(t *testing.T) {
	p := paths(t)
	os.WriteFile(filepath.Join(p.BootDir, "keys.pub"), []byte("ssh-ed25519 AAAA me\n"), 0o600)
	env := "GHR_URL=https://github.com/acme\nGHR_PAT=tok\nGHR_WORKERS=2\nGHR_SSH_AUTHORIZED_KEYS_FILE=keys.pub\n"
	os.WriteFile(filepath.Join(p.BootDir, "runner.env"), []byte(env), 0o600)
	if err := Run(p); err != nil {
		t.Fatal(err)
	}
	c, _ := config.Load(p.ConfigPath)
	if c.URL != "https://github.com/acme" || c.Workers != "2" || !c.SSH.Enabled {
		t.Fatalf("config %+v", c)
	}
	// Edits in the UI survive a reboot when runner.env is unchanged.
	c.Workers = "4"
	config.Save(p.ConfigPath, c)
	Run(p)
	c, _ = config.Load(p.ConfigPath)
	if c.Workers != "4" {
		t.Fatal("unchanged runner.env was re-imported")
	}
	// Changing the file re-imports it.
	os.WriteFile(filepath.Join(p.BootDir, "runner.env"), []byte(env+"GHR_SSH=off\n"), 0o600)
	Run(p)
	c, _ = config.Load(p.ConfigPath)
	if c.Workers != "2" || c.SSH.Enabled {
		t.Fatalf("changed runner.env not applied: %+v", c)
	}
}
