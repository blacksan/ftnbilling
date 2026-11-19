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

func bashOrSkip(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
}

// fn extracts one shell function (up to its closing brace in column 0) from install.sh.
func fnSrc(t *testing.T, name string) string {
	t.Helper()
	src, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(src), "\n")
	var out []string
	in := false
	for _, l := range lines {
		if strings.HasPrefix(l, name+"() {") {
			in = true
		}
		if in {
			// A closing brace inside a here-document is content, not the end.
			heredoc := strings.Contains(strings.Join(out, "\n"), "<<EOF")
			prev := ""
			if len(out) > 0 {
				prev = out[len(out)-1]
			}
			out = append(out, l)
			if l == "}" && (!heredoc || prev == "EOF") {
				return strings.Join(out, "\n")
			}
		}
	}
	t.Fatalf("function %s not found in install.sh", name)
	return ""
}

// The installer's dry run must not create the retired admin VPN, open its port
// or print its device setup.
func TestInstallDryRunHasNoAdminVPN(t *testing.T) {
	bashOrSkip(t)
	if _, err := os.Stat("/etc/wireguard/wg-admin.conf"); err == nil {
		t.Skip("this machine still has the old admin VPN config; the upgrade path prints it by name")
	}
	cmd := exec.Command("bash", "install.sh")
	cmd.Env = append(os.Environ(), "DRY_RUN=1", "DOMAIN=billing.example.co.ke", "PUBLIC_IP=203.0.113.7")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dry run failed: %v\n%s", err, out)
	}
	for _, bad := range []string{"wg-admin", "51821", "10.98.", "admin-peer", "ADM_"} {
		if strings.Contains(string(out), bad) {
			t.Errorf("dry-run output mentions %q:\n%s", bad, out)
		}
	}
	for _, want := range []string{"[14/14]", "migrate", "--setup-code", "write /etc/caddy/Caddyfile"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("dry-run output lacks %q", want)
		}
	}
	// The script itself never creates the interface: no step, no key, no unit start.
	src, _ := os.ReadFile("install.sh")
	for _, bad := range []string{"wireguard_admin", "wg-quick@$ADM_IF", "ADM_CONF", "ADMIN_WG_PORT"} {
		if strings.Contains(string(src), bad) {
			t.Errorf("install.sh still contains %q", bad)
		}
	}
}

func TestInstallCaddyfile(t *testing.T) {
	bashOrSkip(t)
	render := func(site, cb, catchall string) string {
		script := "set -e\n" + fnSrc(t, "render_caddyfile") + "\nCF_SITE=" + site + "; CF_CB=" + cb + "; CF_CATCHALL=\"$1\"\nrender_caddyfile\n"
		out, err := exec.Command("bash", "-c", script, "bash", catchall).CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		return string(out)
	}
	withDomain := render("billing.example.co.ke", "''", "http:// {\n\tredir https://billing.example.co.ke/pay 302\n}")
	for _, want := range []string{"billing.example.co.ke {", "reverse_proxy 127.0.0.1:8080", "http:// {", "redir https://billing.example.co.ke/pay 302", "X-Forwarded-For"} {
		if !strings.Contains(withDomain, want) {
			t.Errorf("Caddyfile lacks %q:\n%s", want, withDomain)
		}
	}
	for _, bad := range []string{"@admin", "Admin is not published", "403", "10.98"} {
		if strings.Contains(withDomain, bad) {
			t.Errorf("Caddyfile still has %q:\n%s", bad, withDomain)
		}
	}
	ip := render(":80", "''", "")
	if strings.Contains(ip, "http://") || strings.Contains(ip, "redir") {
		t.Errorf("an install without a domain must not redirect:\n%s", ip)
	}
}

func TestInstallLegacyFirewallRuleNumbers(t *testing.T) {
	bashOrSkip(t)
	status := `Status: active

     To                         Action      From
     --                         ------      ----
[ 1] 22/tcp                     ALLOW IN    Anywhere
[ 2] 51820/udp                  ALLOW IN    Anywhere
[ 3] 51821/udp                  ALLOW IN    Anywhere
[ 4] 10.98.0.1 8080/tcp         ALLOW IN    Anywhere on wg-admin
[ 5] Anywhere                   DENY IN     Anywhere on wg0
[ 6] 51821/udp (v6)             ALLOW IN    Anywhere (v6)
[10] Anywhere                   DENY IN     Anywhere on wg-admin
[11] 443/tcp                    ALLOW IN    Anywhere
`
	script := "set -e\n" + fnSrc(t, "legacy_rule_numbers") + "\nlegacy_rule_numbers\n"
	cmd := exec.Command("bash", "-c", script)
	cmd.Stdin = strings.NewReader(status)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if got := strings.Fields(string(out)); strings.Join(got, ",") != "10,6,4,3" {
		t.Errorf("rule numbers %v, want 10,6,4,3 (highest first, wg0 and others untouched)", got)
	}
}

// admin-reset passes the new options, stops the service before the program
// runs and starts it again afterwards (also on failure), and explains an older
// program clearly.
func TestAdminResetOptions(t *testing.T) {
	bashOrSkip(t)
	dir := t.TempDir()
	envf := filepath.Join(dir, "env")
	os.WriteFile(envf, []byte("ADMIN_PASS=abcdefghijklmnop\n"), 0o600)
	logf := filepath.Join(dir, "log") // one ordered log: systemctl and program calls
	bin := filepath.Join(dir, "bin")
	os.Mkdir(bin, 0o755)
	os.WriteFile(filepath.Join(bin, "systemctl"), []byte("#!/bin/sh\necho \"systemctl $*\" >>"+logf+"\nexit 0\n"), 0o755)
	fake := filepath.Join(dir, "prog")
	os.WriteFile(fake, []byte("#!/bin/sh\necho \"prog $*\" >>"+logf+"\nif [ \"$FAKE_FAIL\" = old ]; then echo 'flag provided but not defined: -setup-code' >&2; exit 2; fi\nif [ \"$FAKE_FAIL\" = 1 ]; then echo boom >&2; exit 1; fi\necho 'SETUP CODE: 1234-5678'\n"), 0o755)
	run := func(fail string, args ...string) (string, error, []string) {
		os.Remove(logf)
		u, _ := exec.Command("id", "-un").Output()
		cmd := exec.Command("bash", append([]string{"ftnbilling-admin-reset"}, args...)...)
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "FTN_ADMIN_RESET_NOROOT=1", "FTN_ENV_FILE="+envf, "FTN_BIN="+fake, "FTN_USER="+strings.TrimSpace(string(u)), "FAKE_FAIL="+fail)
		out, err := cmd.CombinedOutput()
		raw, _ := os.ReadFile(logf)
		return string(out), err, strings.Split(strings.TrimSpace(string(raw)), "\n")
	}
	want := func(t *testing.T, got []string, exp ...string) {
		t.Helper()
		if strings.Join(got, "|") != strings.Join(exp, "|") {
			t.Errorf("calls %q, want %q", got, exp)
		}
	}
	out, err, calls := run("", "--setup-code")
	if err != nil || !strings.Contains(out, "1234-5678") {
		t.Fatalf("--setup-code: %v %s", err, out)
	}
	want(t, calls, "systemctl is-active --quiet ftnbilling", "systemctl stop ftnbilling", "prog admin-reset-password -setup-code", "systemctl start ftnbilling")
	_, _, calls = run("", "--generate", "--revoke-sessions", "--reset-totp")
	want(t, calls, "systemctl is-active --quiet ftnbilling", "systemctl stop ftnbilling", "prog admin-reset-password -generate -revoke-sessions -reset-totp", "systemctl start ftnbilling")
	out, err, calls = run("old", "--setup-code")
	if err == nil || !strings.Contains(out, "does not support that option") {
		t.Errorf("old program: want a clear failure, got %v %s", err, out)
	}
	want(t, calls, "systemctl is-active --quiet ftnbilling", "systemctl stop ftnbilling", "prog admin-reset-password -setup-code", "systemctl start ftnbilling")
	_, err, calls = run("1")
	if err == nil {
		t.Error("a failing program must fail the script")
	}
	want(t, calls, "systemctl is-active --quiet ftnbilling", "systemctl stop ftnbilling", "prog admin-reset-password", "systemctl start ftnbilling")
	if _, err, _ := run("", "--nope"); err == nil {
		t.Error("an unknown option must fail")
	}
}

// ftnbilling-wipe: counts first, a verified backup before anything is deleted,
// the service stopped around the wipe and always started again, the WireGuard
// sync last, on both databases.
func TestWipeFlow(t *testing.T) {
	bashOrSkip(t)
	type rig struct {
		dir, data, backups, logf string
		run                      func(env []string, args ...string) (string, error, []string)
	}
	newRig := func(t *testing.T, envfile string) *rig {
		dir := t.TempDir()
		r := &rig{dir: dir, data: filepath.Join(dir, "data"), backups: filepath.Join(dir, "backups"), logf: filepath.Join(dir, "log")}
		os.MkdirAll(r.data, 0o755)
		envf := filepath.Join(dir, "env")
		os.WriteFile(envf, []byte(strings.ReplaceAll(envfile, "DATADIR", r.data)), 0o600)
		bin := filepath.Join(dir, "bin")
		os.Mkdir(bin, 0o755)
		logTo := func(name, extra string) {
			os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\necho \""+name+" $*\" >>"+r.logf+"\n"+extra+"\nexit 0\n"), 0o755)
		}
		logTo("systemctl", "[ \"$FAKE_FAIL\" = start ] && [ \"$1\" = start ] && exit 1")
		logTo("pg_restore", "[ \"$FAKE_FAIL\" = restore ] && exit 1")
		os.WriteFile(filepath.Join(bin, "pg_dump"), []byte("#!/bin/sh\necho \"pg_dump $*\" >>"+r.logf+"\nenv >"+filepath.Join(dir, "pgenv")+"\nF=$(cat "+filepath.Join(dir, "failmode")+" 2>/dev/null)\n[ \"$F\" = dump ] && exit 1\n[ \"$F\" = emptydump ] && exit 0\necho PGDMP-fake\n"), 0o755)
		fake := filepath.Join(dir, "prog")
		os.WriteFile(fake, []byte("#!/bin/sh\necho \"prog $*\" >>"+r.logf+"\nif [ \"$1\" = wipe-data ] && [ \"$2\" = -confirm ]; then [ \"$FAKE_FAIL\" = wipe ] && { echo boom >&2; exit 1; }; echo 'wiped the business data'; exit 0; fi\n[ \"$FAKE_FAIL\" = count ] && exit 1\necho 'would delete: routers 2'\n"), 0o755)
		wgs := filepath.Join(dir, "wgsync")
		os.WriteFile(wgs, []byte("#!/bin/sh\necho \"wg-sync\" >>"+r.logf+"\n[ \"$FAKE_FAIL\" = wgsync ] && exit 1\nexit 0\n"), 0o755)
		r.run = func(env []string, args ...string) (string, error, []string) {
			os.Remove(r.logf)
			os.Remove(filepath.Join(dir, "failmode"))
			for _, e := range env {
				if v, ok := strings.CutPrefix(e, "FAKE_FAIL="); ok { // pg_dump runs with a clean environment: pass the mode by file
					os.WriteFile(filepath.Join(dir, "failmode"), []byte(v), 0o600)
				}
			}
			u, _ := exec.Command("id", "-un").Output()
			cmd := exec.Command("bash", append([]string{"ftnbilling-wipe"}, args...)...)
			cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "FTN_WIPE_NOROOT=1", "FTN_ENV_FILE="+envf, "FTN_BIN="+fake,
				"FTN_USER="+strings.TrimSpace(string(u)), "FTN_BACKUP_DIR="+r.backups, "FTN_WG_SYNC="+wgs, "FTN_PGRUN=")
			cmd.Env = append(cmd.Env, env...)
			cmd.Stdin = nil // not a terminal
			out, err := cmd.CombinedOutput()
			raw, _ := os.ReadFile(r.logf)
			return string(out), err, strings.Split(strings.TrimSpace(string(raw)), "\n")
		}
		return r
	}
	want := func(t *testing.T, got []string, exp ...string) {
		t.Helper()
		if strings.Join(got, "|") != strings.Join(exp, "|") {
			t.Errorf("calls %q, want %q", got, exp)
		}
	}
	svc := []string{"systemctl is-active --quiet ftnbilling", "systemctl stop ftnbilling"}
	files := func(r *rig) []string {
		m, _ := filepath.Glob(filepath.Join(r.backups, "*"))
		return m
	}

	t.Run("json", func(t *testing.T) {
		r := newRig(t, "DATA_DIR=DATADIR\nADMIN_PASS=abcdefghijklmnop\n")
		os.WriteFile(filepath.Join(r.data, "ispbill.json"), []byte(`{"format":7}`), 0o600)
		out, err, calls := r.run(nil, "--yes")
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		want(t, calls, "prog wipe-data", svc[0], svc[1], "prog wipe-data -confirm WIPE", "systemctl start ftnbilling", "wg-sync")
		bk := files(r)
		if len(bk) != 1 || !strings.Contains(filepath.Base(bk[0]), "pre-wipe-") || !strings.HasSuffix(bk[0], ".json") {
			t.Fatalf("backups %v", bk)
		}
		if got, _ := os.ReadFile(bk[0]); string(got) != `{"format":7}` {
			t.Errorf("backup content %q", got)
		}
		if st, _ := os.Stat(bk[0]); st.Mode().Perm() != 0o600 {
			t.Errorf("backup mode %v", st.Mode())
		}
		if !strings.Contains(out, "backup: "+bk[0]) || !strings.Contains(out, "done:") {
			t.Errorf("output lacks the backup path or the done line:\n%s", out)
		}
	})

	t.Run("postgres", func(t *testing.T) {
		r := newRig(t, "DATA_DIR=DATADIR\nDATABASE_URL=postgres://ftnbilling:s3cret@/ftnbilling?host=/var/run/postgresql&sslmode=disable\n")
		os.WriteFile(filepath.Join(r.data, "outbox.json"), []byte(`{}`), 0o600)
		out, err, calls := r.run(nil, "--yes")
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		// the service is stopped BEFORE the dump, so no payment can land between the two
		if len(calls) < 5 || calls[3] != "pg_dump -Fc ftnbilling" || !strings.HasPrefix(calls[4], "pg_restore -l ") {
			t.Fatalf("calls %q", calls)
		}
		want(t, append(append([]string{}, calls[:3]...), calls[5:]...), "prog wipe-data",
			svc[0], svc[1], "prog wipe-data -confirm WIPE", "systemctl start ftnbilling", "wg-sync")
		var dump, tgz string
		for _, f := range files(r) {
			switch {
			case strings.HasSuffix(f, ".dump"):
				dump = f
			case strings.HasSuffix(f, "-files.tgz"):
				tgz = f
			}
		}
		if dump == "" || tgz == "" || !strings.Contains(filepath.Base(dump), "pre-wipe-") {
			t.Fatalf("backups %v", files(r))
		}
		if st, _ := os.Stat(dump); st.Mode().Perm() != 0o600 {
			t.Errorf("dump mode %v", st.Mode())
		}
		if strings.Contains(out, "s3cret") {
			t.Error("the output shows the database password")
		}
	})

	t.Run("a failing backup stops everything", func(t *testing.T) {
		for _, fail := range []string{"dump", "emptydump", "restore"} {
			r := newRig(t, "DATA_DIR=DATADIR\nDATABASE_URL=postgres://ftnbilling:x@/ftnbilling?host=/var/run/postgresql\n")
			out, err, calls := r.run([]string{"FAKE_FAIL=" + fail}, "--yes")
			if err == nil || !strings.Contains(out, "nothing was changed") {
				t.Errorf("%s: want a failure, got %v\n%s", fail, err, out)
			}
			for _, c := range calls {
				if strings.Contains(c, "-confirm") {
					t.Errorf("%s: ran %q after a failed backup", fail, c)
				}
			}
			// the service was stopped for the backup and must be running again
			if len(calls) < 3 || calls[len(calls)-1] != "systemctl start ftnbilling" {
				t.Errorf("%s: the service was not started again after the failed backup: %q", fail, calls)
			}
			if left := files(r); len(left) != 0 {
				t.Errorf("%s: files left behind: %v", fail, left)
			}
		}
	})

	t.Run("a failing wipe starts the service again", func(t *testing.T) {
		r := newRig(t, "DATA_DIR=DATADIR\n")
		os.WriteFile(filepath.Join(r.data, "ispbill.json"), []byte(`{}`), 0o600)
		out, err, calls := r.run([]string{"FAKE_FAIL=wipe"}, "--yes")
		if err == nil || !strings.Contains(out, "boom") {
			t.Fatalf("want the failure, got %v\n%s", err, out)
		}
		want(t, calls, "prog wipe-data", svc[0], svc[1], "prog wipe-data -confirm WIPE", "systemctl start ftnbilling")
	})

	t.Run("a service that will not start is reported, wg-sync still runs, exit is non-zero", func(t *testing.T) {
		r := newRig(t, "DATA_DIR=DATADIR\n")
		out, err, calls := r.run([]string{"FAKE_FAIL=start"}, "--yes")
		if err == nil || !strings.Contains(out, "could not start ftnbilling again") {
			t.Fatalf("want a clear start error and a non-zero exit: %v\n%s", err, out)
		}
		if calls[len(calls)-1] != "wg-sync" {
			t.Errorf("wg-sync must still run: %q", calls)
		}
		if strings.Contains(out, "the service was started again") {
			t.Error("claims the service was started")
		}
	})

	t.Run("an unreadable database name is refused, not guessed", func(t *testing.T) {
		for _, url := range []string{"postgres://ftnbilling:x@localhost", "postgres://u@h/", "not a url", "postgres://u@h/na%20me"} {
			r := newRig(t, "DATA_DIR=DATADIR\nDATABASE_URL="+url+"\n")
			out, err, calls := r.run(nil, "--yes")
			if err == nil || !strings.Contains(out, "cannot read the database name") {
				t.Errorf("%q: %v\n%s", url, err, out)
			}
			for _, c := range calls {
				if strings.Contains(c, "pg_dump") || strings.Contains(c, "-confirm") || strings.Contains(c, "systemctl stop") {
					t.Errorf("%q: ran %q", url, c)
				}
			}
		}
	})

	t.Run("pg_dump gets a clean environment and the settings file is not exported", func(t *testing.T) {
		r := newRig(t, "DATA_DIR=DATADIR\nDATA_KEY=topsecretkey\nADMIN_PASS=abcdefghijklmnop\nDATABASE_URL=postgres://ftnbilling:pw9@/ftnbilling?host=/var/run/postgresql\n")
		if out, err, _ := r.run(nil, "--yes"); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		env, _ := os.ReadFile(filepath.Join(r.dir, "pgenv"))
		for _, bad := range []string{"topsecretkey", "pw9", "DATABASE_URL", "DATA_KEY", "ADMIN_PASS"} {
			if strings.Contains(string(env), bad) {
				t.Errorf("pg_dump sees %q in its environment:\n%s", bad, env)
			}
		}
	})

	t.Run("old full copies move to the backup directory", func(t *testing.T) {
		r := newRig(t, "DATA_DIR=DATADIR\n")
		os.WriteFile(filepath.Join(r.data, "ispbill.json"), []byte(`{"format":7}`), 0o600)
		os.WriteFile(filepath.Join(r.data, "ispbill.json.v6.bak"), []byte("old6"), 0o644)
		os.WriteFile(filepath.Join(r.data, "ispbill.json.imported-20260101"), []byte("imp"), 0o644)
		os.WriteFile(filepath.Join(r.data, "other.txt"), []byte("keep"), 0o644)
		out, err, _ := r.run(nil, "--yes")
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		left, _ := filepath.Glob(filepath.Join(r.data, "ispbill.json.*"))
		if len(left) != 0 {
			t.Errorf("old data left in DATA_DIR: %v", left)
		}
		if _, err := os.Stat(filepath.Join(r.data, "other.txt")); err != nil {
			t.Error("an unrelated file was moved")
		}
		dirs, _ := filepath.Glob(filepath.Join(r.backups, "pre-wipe-*-old"))
		if len(dirs) != 1 {
			t.Fatalf("old dir: %v", files(r))
		}
		if st, _ := os.Stat(dirs[0]); st.Mode().Perm() != 0o700 {
			t.Errorf("dir mode %v", st.Mode().Perm())
		}
		for _, n := range []string{"ispbill.json.v6.bak", "ispbill.json.imported-20260101"} {
			st, err := os.Stat(filepath.Join(dirs[0], n))
			if err != nil || st.Mode().Perm() != 0o600 {
				t.Errorf("%s: %v %v", n, err, st)
			}
		}
	})

	t.Run("wg sync failure is reported", func(t *testing.T) {
		r := newRig(t, "DATA_DIR=DATADIR\n")
		out, err, _ := r.run([]string{"FAKE_FAIL=wgsync"}, "--yes")
		if err == nil || !strings.Contains(out, "done:") || !strings.Contains(out, "did not finish") {
			t.Errorf("want a warning and a non-zero exit after the done line: %v\n%s", err, out)
		}
	})

	t.Run("no confirmation, no change", func(t *testing.T) {
		r := newRig(t, "DATA_DIR=DATADIR\n")
		out, err, calls := r.run(nil) // no --yes and no terminal
		if err == nil {
			t.Fatalf("must not wipe without an answer:\n%s", out)
		}
		want(t, calls, "prog wipe-data")
		if len(files(r)) != 0 {
			t.Error("a backup was made without a confirmation")
		}
		if _, err, _ := r.run(nil, "--nope"); err == nil {
			t.Error("an unknown option must fail")
		}
		if _, err, calls := r.run([]string{"FAKE_FAIL=count"}, "--yes"); err == nil || len(calls) != 1 {
			t.Errorf("a failing dry run must stop: %v %v", err, calls)
		}
	})
}

// The installer must ship and install ftnbilling-wipe like the other tools.
func TestInstallShipsWipeTool(t *testing.T) {
	src, _ := os.ReadFile("install.sh")
	for _, want := range []string{"$APP-admin-reset $APP-wipe $APP-doctor", "admin-reset wipe doctor"} {
		if !strings.Contains(string(src), want) {
			t.Errorf("install.sh does not mention %q", want)
		}
	}
	if st, err := os.Stat("ftnbilling-wipe"); err != nil || st.Mode().Perm()&0o111 == 0 {
		t.Errorf("deploy/ftnbilling-wipe must exist and be executable: %v", err)
	}
}
