package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// run extracts the env-file helpers from install.sh and runs a script with them.
func runHelpers(t *testing.T, envfile, script string) string {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	prelude := `set -e
DRY=0; ENVF="$1"
src=install.sh
eval "$(sed -n '/^envget()/p' $src)"
for f in setkv addkv fillkv; do eval "$(awk "/^$f\\(\\)/,/^}/" $src)"; done
`
	cmd := exec.Command("bash", "-c", prelude+script, "bash", envfile)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	return string(out)
}

func TestInstallFillsEmptyDataKeyOnly(t *testing.T) {
	cases := []struct{ in, want string }{
		{"A=1\nDATA_KEY=\nB=x\n", "A=1\nDATA_KEY=NEWKEY\nB=x\n"},
		{"DATA_KEY=\"\"\n", "DATA_KEY=NEWKEY\n"},
		{"DATA_KEY=''\n", "DATA_KEY=NEWKEY\n"},
		{"DATA_KEY=existing=\n", "DATA_KEY=existing=\n"}, // never replace a real key
		{"DATA_KEY=abc\n", "DATA_KEY=abc\n"},
		{"A=1\n", "A=1\n"}, // a missing line is addkv's job
	}
	for _, c := range cases {
		f := filepath.Join(t.TempDir(), "env")
		os.WriteFile(f, []byte(c.in), 0o600)
		runHelpers(t, f, `fillkv DATA_KEY NEWKEY`)
		got, _ := os.ReadFile(f)
		if string(got) != c.want {
			t.Errorf("%q: got %q want %q", c.in, got, c.want)
		}
	}
	// addkv still adds a missing key and leaves an existing one alone.
	f := filepath.Join(t.TempDir(), "env")
	os.WriteFile(f, []byte("X=1\n"), 0o600)
	runHelpers(t, f, `addkv DATA_KEY K1; addkv DATA_KEY K2`)
	got, _ := os.ReadFile(f)
	if !strings.Contains(string(got), "DATA_KEY=K1\n") || strings.Contains(string(got), "K2") {
		t.Errorf("addkv: %q", got)
	}
}
