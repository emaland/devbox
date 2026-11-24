package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Tests for terraform/devbox/ahp-gateway/boot-check.sh, the checklist the
// owner runs on a real box after a reboot. It only reads: a fake systemctl
// and the stub gateway stand in for the box.

type bootBox struct {
	t        *testing.T
	dir      string
	stateDir string
	config   string
	port     int
	pid      int
}

// newBootBox lays out what the module puts on a box: the client config in
// /etc, a state dir with a token, and a running gateway.
func newBootBox(t *testing.T, gatewayMode string) bootBox {
	t.Helper()
	for _, tool := range []string{"sh", "curl", "ss", "jq", "stat"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not on PATH", tool)
		}
	}
	dir := t.TempDir()
	b := bootBox{t: t, dir: dir, stateDir: filepath.Join(dir, "state"), port: freePort(t)}
	if err := os.MkdirAll(b.stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.stateDir, "token"), []byte(strings.Repeat("A", 43)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	client, _ := json.Marshal(map[string]any{
		"managedBy": "system", "service": "ahp-gateway.service", "port": b.port,
		"stateDir": b.stateDir, "tokenFile": filepath.Join(b.stateDir, "token"), "path": "/ahp",
	})
	b.config = filepath.Join(dir, "config.json")
	if err := os.WriteFile(b.config, client, 0o644); err != nil {
		t.Fatal(err)
	}
	if gatewayMode != "" {
		b.pid = startStub(t, gatewayMode, b.port)
		b.waitListening()
	}
	b.systemctl(map[string]string{})
	return b
}

// waitListening polls until the stub gateway answers.
func (b bootBox) waitListening() {
	b.t.Helper()
	for i := 0; i < 100; i++ {
		if exec.Command("curl", "-s", "-o", "/dev/null", "--max-time", "1", "http://127.0.0.1:"+strconv.Itoa(b.port)+"/healthz").Run() == nil {
			return
		}
		exec.Command("sleep", "0.1").Run()
	}
	b.t.Fatal("the stub gateway did not start")
}

// systemctl writes a fake systemctl that answers `show` with the box's unit
// state; overrides change single properties.
func (b bootBox) systemctl(overrides map[string]string) {
	b.t.Helper()
	properties := map[string]string{
		"ahp-gateway.service ActiveState":                            "active",
		"ahp-gateway.service NRestarts":                              "0",
		"ahp-gateway.service MainPID":                                strconv.Itoa(b.pid),
		"ahp-gateway.service ActiveEnterTimestampMonotonic":          "20000000",
		"home.mount ActiveEnterTimestampMonotonic":                   "10000000",
		"ahp-gateway-check.timer ActiveState":                        "active",
		"ahp-gateway-failed.service ActiveState":                     "inactive",
		"ahp-gateway-failed.service ExecMainStartTimestampMonotonic": "0",
	}
	for key, value := range overrides {
		properties[key] = value
	}
	var script strings.Builder
	script.WriteString("#!/bin/sh\n# fake systemctl: show -p A,B <unit> -> A=..\\nB=..\nunit=\"\"; props=\"\"\nwhile [ $# -gt 0 ]; do case \"$1\" in show) ;; -p) shift; props=\"$1\" ;; --*) ;; *) unit=\"$1\" ;; esac; shift; done\nfor p in $(echo \"$props\" | tr , ' '); do case \"$unit $p\" in\n")
	for key, value := range properties {
		fmt.Fprintf(&script, "  %q) echo \"%s=%s\" ;;\n", key, strings.Fields(key)[1], value)
	}
	script.WriteString("  *) echo \"$p=\" ;;\nesac; done\n")
	path := filepath.Join(b.dir, "bin")
	if err := os.MkdirAll(path, 0o755); err != nil {
		b.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "systemctl"), []byte(script.String()), 0o755); err != nil {
		b.t.Fatal(err)
	}
}

func (b bootBox) run() (string, int) {
	b.t.Helper()
	command := exec.Command("sh", gatewayScript(b.t, "boot-check.sh"))
	command.Env = append(os.Environ(),
		"PATH="+filepath.Join(b.dir, "bin")+":"+os.Getenv("PATH"),
		"AHP_BOOT_CHECK_CONFIG="+b.config,
	)
	out, err := command.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	if exit, ok := err.(*exec.ExitError); ok {
		return string(out), exit.ExitCode()
	}
	b.t.Fatal(err)
	return "", -1
}

func failLines(out string) []string {
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "FAIL") {
			lines = append(lines, line)
		}
	}
	return lines
}

func TestBootCheckPassesOnAGoodBox(t *testing.T) {
	b := newBootBox(t, "ok")
	out, status := b.run()
	if status != 0 || len(failLines(out)) != 0 {
		t.Fatalf("exit %d:\n%s", status, out)
	}
	for _, want := range []string{"unit active", "after /home", "no restarts", "state dir", "token", "answers", "loopback", "without a token", "timer"} {
		if !strings.Contains(out, want) {
			t.Errorf("no line about %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, strings.Repeat("A", 43)) {
		t.Error("the check printed the token")
	}
}

func TestBootCheckFindsEachProblem(t *testing.T) {
	cases := []struct {
		name  string
		setUp func(b *bootBox)
		want  string
	}{
		{"unit not active", func(b *bootBox) { b.systemctl(map[string]string{"ahp-gateway.service ActiveState": "failed"}) }, "unit active"},
		{"started before /home", func(b *bootBox) {
			b.systemctl(map[string]string{"ahp-gateway.service ActiveEnterTimestampMonotonic": "5000000"})
		}, "after /home"},
		{"restarted", func(b *bootBox) { b.systemctl(map[string]string{"ahp-gateway.service NRestarts": "2"}) }, "no restarts"},
		{"state dir too open", func(b *bootBox) {
			if err := os.Chmod(b.stateDir, 0o755); err != nil {
				b.t.Fatal(err)
			}
		}, "state dir"},
		{"token too open", func(b *bootBox) {
			if err := os.Chmod(filepath.Join(b.stateDir, "token"), 0o644); err != nil {
				b.t.Fatal(err)
			}
		}, "token"},
		{"open to connections without a token", func(b *bootBox) { b.replaceGateway("open") }, "without a token"},
		{"not loopback only", func(b *bootBox) { b.replaceGateway("all") }, "loopback"},
		{"timer off", func(b *bootBox) { b.systemctl(map[string]string{"ahp-gateway-check.timer ActiveState": "inactive"}) }, "timer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := newBootBox(t, "ok")
			tc.setUp(&b)
			out, status := b.run()
			if status == 0 {
				t.Fatalf("passed:\n%s", out)
			}
			failed := strings.Join(failLines(out), "\n")
			if !strings.Contains(failed, tc.want) {
				t.Errorf("want a FAIL line about %q, got:\n%s", tc.want, out)
			}
		})
	}
}

func TestBootCheckWithNoGatewayRunning(t *testing.T) {
	b := newBootBox(t, "")
	b.systemctl(map[string]string{"ahp-gateway.service ActiveState": "inactive", "ahp-gateway.service MainPID": "0"})
	out, status := b.run()
	if status == 0 {
		t.Fatalf("passed:\n%s", out)
	}
	if !strings.Contains(strings.Join(failLines(out), "\n"), "answers") {
		t.Errorf("want a FAIL line about the gateway answering:\n%s", out)
	}
}

// replaceGateway runs a gateway in the given mode on a fresh port instead,
// and points the client config and the unit's state at it.
func (b *bootBox) replaceGateway(mode string) {
	b.t.Helper()
	b.port = freePort(b.t)
	client, _ := json.Marshal(map[string]any{
		"managedBy": "system", "service": "ahp-gateway.service", "port": b.port,
		"stateDir": b.stateDir, "tokenFile": filepath.Join(b.stateDir, "token"), "path": "/ahp",
	})
	if err := os.WriteFile(b.config, client, 0o644); err != nil {
		b.t.Fatal(err)
	}
	b.pid = startStub(b.t, mode, b.port)
	b.waitListening()
	b.systemctl(map[string]string{})
}
