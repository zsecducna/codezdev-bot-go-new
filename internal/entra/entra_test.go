package entra

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestScriptDirHonorsEnvOverride pins ScriptDir's KIRO_SCRIPT_DIR override —
// mirroring internal/handlers/otp.go's scriptDir(), which this function is a
// local copy of (this package must not import internal/handlers).
func TestScriptDirHonorsEnvOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KIRO_SCRIPT_DIR", dir)
	if got := ScriptDir(); got != dir {
		t.Errorf("ScriptDir() = %q, want %q", got, dir)
	}
}

// TestScriptDirFallsBackToExecutableDir pins the "no env override" path: it
// must resolve to SOME existing directory (the test binary's own), never a
// blank string.
func TestScriptDirFallsBackToExecutableDir(t *testing.T) {
	t.Setenv("KIRO_SCRIPT_DIR", "")
	got := ScriptDir()
	if got == "" {
		t.Fatal("ScriptDir() = \"\", want a non-empty directory")
	}
	if fi, err := os.Stat(got); err != nil || !fi.IsDir() {
		t.Errorf("ScriptDir() = %q, want an existing directory", got)
	}
}

func TestCacheDirJoinsDotEntraAuth(t *testing.T) {
	got := CacheDir("/opt/bot")
	want := filepath.Join("/opt/bot", ".entra-auth")
	if got != want {
		t.Errorf("CacheDir(%q) = %q, want %q", "/opt/bot", got, want)
	}
}

func TestOrDefault(t *testing.T) {
	cases := []struct {
		d, def, want time.Duration
	}{
		{0, 900 * time.Second, 900 * time.Second},
		{-5 * time.Second, 900 * time.Second, 900 * time.Second},
		{45 * time.Second, 900 * time.Second, 45 * time.Second},
	}
	for _, c := range cases {
		if got := orDefault(c.d, c.def); got != c.want {
			t.Errorf("orDefault(%v, %v) = %v, want %v", c.d, c.def, got, c.want)
		}
	}
}
