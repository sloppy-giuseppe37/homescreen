package main

// rcd_test.go — checks on the FreeBSD rc.d script that the pkg builder embeds.
//
// rc.subr silently reinterprets several ${name}_* variables, and getting one
// wrong does not fail loudly: it changes what `service homescreen start` runs.
// That is worth a test, because nothing else here exercises the script.

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// rcdScript returns the rc.d script as the pkg builder writes it.
func rcdScript(t *testing.T) string {
	t.Helper()
	build, err := os.ReadFile("scripts/build-pkg-repo.sh")
	if err != nil {
		t.Fatalf("cannot read the pkg builder: %v", err)
	}
	const open = "<< 'RCEOF'\n"
	start := strings.Index(string(build), open)
	if start < 0 {
		t.Fatal("no rc.d heredoc in the pkg builder")
	}
	rest := string(build)[start+len(open):]
	end := strings.Index(rest, "\nRCEOF\n")
	if end < 0 {
		t.Fatal("unterminated rc.d heredoc")
	}
	return rest[:end]
}

// TestRCDRunsUnderDaemon guards the supervision setup: the service must run
// under daemon(8), restart when the program exits, and hand rc.subr the
// supervisor's pidfile so that stopping it doesn't just trigger a restart.
func TestRCDRunsUnderDaemon(t *testing.T) {
	script := rcdScript(t)

	for _, want := range []string{
		`command="/usr/sbin/daemon"`,
		`procname="/usr/sbin/daemon"`,
		"-r ",                            // supervise and restart
		"-R ${homescreen_restart_delay}", // ...after a configurable delay
		"-P ${pidfile}",                  // rc.subr tracks the supervisor
		"-p ${child_pidfile}",            // the child's pid lives elsewhere
		"/usr/local/bin/${name}",         // the program daemon supervises
	} {
		if !strings.Contains(script, want) {
			t.Errorf("rc.d script is missing %q", want)
		}
	}
}

// TestRCDAvoidsReservedVariables is the regression guard for a real outage:
// naming a variable homescreen_program made rc.subr replace $command with it,
// so `service homescreen start` ran the app directly — in the foreground, with
// daemon(8)'s arguments as its own and no supervision at all.
func TestRCDAvoidsReservedVariables(t *testing.T) {
	script := rcdScript(t)

	// Suffixes rc.subr claims. Each one changes how (or what) rc.subr runs;
	// homescreen_enable is the rcvar and is meant to be here.
	reserved := []string{
		"program", "user", "group", "groups", "chdir", "chroot", "nice",
		"env", "fib", "limits", "login_class", "oomprotect", "umask",
	}

	assign := regexp.MustCompile(`(?m)(?:^|[:{ ])homescreen_([a-z_]+)(?:=|:=|\})`)
	for _, m := range assign.FindAllStringSubmatch(script, -1) {
		for _, bad := range reserved {
			if m[1] == bad {
				t.Errorf("rc.d script defines homescreen_%s, which rc.subr claims for itself "+
					"(see the comment in the script); pick a name rc.subr does not read", bad)
			}
		}
	}
}
