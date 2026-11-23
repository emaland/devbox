package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Tests for resume-summary.sh and devbox-agents.sh (design note 031,
// sections 2.6 and 2.7).

func (b *lifecycleBox) fakeProgram(name, body string) {
	b.t.Helper()
	script := "#!/bin/sh\n" + fakePrograms + body + "\n"
	if err := os.WriteFile(filepath.Join(b.bin, name), []byte(script), 0o755); err != nil {
		b.t.Fatal(err)
	}
}

func TestResumeSummary(t *testing.T) {
	statusWith := func(resume string) string {
		return `{"ok": true, "sessions": {"total": 2}, "turns": {"active": 0, "waitingForUser": 0}` + resume + `}`
	}
	rows := []struct {
		name  string
		setup func(b *lifecycleBox)
		want  string
	}{
		{
			name: "resume reported",
			setup: func(b *lifecycleBox) {
				b.gateway.set(200, statusWith(`, "resume": {"policy": "continue", "source": "config", "pending": 0, "resumedSinceStart": 2, "failed": 1, "skipped": 0}`))
			},
			want: "auto-resume: continue (config): continued 2, failed 1, skipped 0\n",
		},
		{
			name: "still pending when the wait runs out",
			setup: func(b *lifecycleBox) {
				b.gateway.set(200, statusWith(`, "resume": {"policy": "continue", "source": "state", "pending": 1, "resumedSinceStart": 0, "failed": 0, "skipped": 0}`))
			},
			want: "auto-resume: continue (state): continued 0, failed 0, skipped 0, still pending 1\n",
		},
		{
			name:  "build without resume",
			setup: func(b *lifecycleBox) { b.gateway.set(200, statusWith("")) },
			want:  "auto-resume: not reported by this gateway build\n",
		},
		{
			name:  "gateway not answering",
			setup: func(b *lifecycleBox) { b.gateway.set(503, "") },
			want:  "gateway: not answering (HTTP 503)\n",
		},
		{
			name:  "no gateway",
			setup: func(b *lifecycleBox) { b.remove("gateway-config.json") },
			want:  "",
		},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			b := newLifecycleBox(t)
			b.env = append(b.env, "DEVBOX_RESUME_WAIT=2", "DEVBOX_RESUME_POLL=1")
			r.setup(b)
			out, status := b.run("resume-summary.sh")
			if status != 0 || out != r.want {
				t.Fatalf("exit %d, output %q, want %q", status, out, r.want)
			}
			b.noTokenLeak(out)
		})
	}
}

// The summary waits until nothing is pending, then reports.
func TestResumeSummaryWaitsForPendingContinuations(t *testing.T) {
	b := newLifecycleBox(t)
	b.env = append(b.env, "DEVBOX_RESUME_WAIT=30", "DEVBOX_RESUME_POLL=1")
	// Pending for two requests, then the continuation has started.
	done := strings.Replace(statusJSON(1, 0, 0), `"resumedSinceStart": 0`, `"resumedSinceStart": 1`, 1)
	b.gateway.mu.Lock()
	b.gateway.sequence = []string{statusJSON(0, 0, 1), statusJSON(0, 0, 1), done}
	b.gateway.mu.Unlock()
	out, status := b.run("resume-summary.sh")
	if status != 0 || out != "auto-resume: continue (config): continued 1, failed 0, skipped 0\n" {
		t.Fatalf("exit %d, output %q", status, out)
	}
}

func TestDevboxAgentsStatus(t *testing.T) {
	b := newLifecycleBox(t)
	b.gateway.set(200, statusJSON(3, 1, 0))
	out, status := b.run("devbox-agents.sh", "status", "--json")
	if status != 0 {
		t.Fatalf("exit %d: %s", status, out)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not JSON: %q", out)
	}
	if got["state"] != "busy" || got["running"] != 2.0 || got["waiting"] != 1.0 || got["sessions"] != 3.0 {
		t.Fatalf("status: %v", got)
	}
	out, _ = b.run("devbox-agents.sh", "status")
	if out != "agents: busy; 3 sessions, 2 turns running, 1 waiting for a person\n" {
		t.Fatalf("text status: %q", out)
	}
	b.noTokenLeak(out)

	b.gateway.set(401, "")
	out, status = b.run("devbox-agents.sh", "status", "--json")
	if status != 2 || !strings.Contains(out, `"state":"unreachable"`) || !strings.Contains(out, "HTTP 401") {
		t.Fatalf("unreachable: exit %d %q", status, out)
	}

	b.remove("gateway-config.json")
	out, status = b.run("devbox-agents.sh", "status", "--json")
	if status != 3 || strings.TrimSpace(out) != `{"state":"none"}` {
		t.Fatalf("none: exit %d %q", status, out)
	}
}

func TestDevboxAgentsResumeStatusReadsTheRunningGateway(t *testing.T) {
	b := newLifecycleBox(t)
	b.fakeProgram("ahp-acp-gateway", `record ahp-acp-gateway "$@"; echo "auto-resume: off (default)"`)
	b.env = append(b.env, "DEVBOX_GATEWAY_HAS_RESUME=1")
	b.gateway.set(200, statusJSON(0, 0, 0))
	out, status := b.run("devbox-agents.sh", "resume", "status")
	if status != 0 || !strings.HasPrefix(out, "auto-resume: continue (config); pending 0") {
		t.Fatalf("exit %d %q", status, out)
	}
	if len(b.calls()) != 0 {
		t.Fatalf("asked the gateway command: %v", b.calls())
	}

	b.gateway.set(200, `{"ok": true, "turns": {"active": 0, "waitingForUser": 0}}`)
	out, status = b.run("devbox-agents.sh", "resume", "status")
	if status != 4 || !strings.Contains(out, "not reported") {
		t.Fatalf("build without resume: exit %d %q", status, out)
	}
}

// wrapperAt writes a fake of the module's wrapper outside PATH and returns
// its absolute path.
func (b *lifecycleBox) wrapperAt(name string) string {
	b.t.Helper()
	dir := filepath.Join(b.root, "store", name, "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		b.t.Fatal(err)
	}
	path := filepath.Join(dir, "ahp-acp-gateway")
	script := "#!/bin/sh\n" + fakePrograms + `record ` + name + ` "$@"` + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		b.t.Fatal(err)
	}
	return path
}

func TestDevboxAgentsResumeOnOff(t *testing.T) {
	b := newLifecycleBox(t)
	wrapper := b.wrapperAt("module-wrapper")
	b.env = append(b.env, "DEVBOX_GATEWAY_COMMAND="+wrapper)
	for _, value := range []string{"on", "off"} {
		// Without the module saying the wrapper offers resume, the command
		// is never run.
		out, status := b.run("devbox-agents.sh", "resume", value)
		if status != 4 || len(b.calls()) != 0 {
			t.Fatalf("resume %s without the command: exit %d %q calls %v", value, status, out, b.calls())
		}
	}
	b.env = append(b.env, "DEVBOX_GATEWAY_HAS_RESUME=1")
	for _, value := range []string{"on", "off"} {
		b.clearCalls()
		out, status := b.run("devbox-agents.sh", "resume", value)
		if status != 0 || strings.Join(b.calls(), "\n") != "module-wrapper resume "+value {
			t.Fatalf("resume %s: exit %d %q calls %v", value, status, out, b.calls())
		}
	}
	if _, status := b.run("devbox-agents.sh", "resume", "maybe"); status != 64 {
		t.Fatalf("bad value: exit %d", status)
	}
}

// Review finding: a copy of the gateway earlier on the user's PATH (a build
// without subcommands would start a second server) is never run.
func TestDevboxAgentsResumeIgnoresAGatewayOnPath(t *testing.T) {
	b := newLifecycleBox(t)
	b.fakeProgram("ahp-acp-gateway", `record shadow "$@"`)
	wrapper := b.wrapperAt("module-wrapper")
	b.env = append(b.env, "DEVBOX_GATEWAY_HAS_RESUME=1", "DEVBOX_GATEWAY_COMMAND="+wrapper)
	if out, status := b.run("devbox-agents.sh", "resume", "on"); status != 0 || strings.Join(b.calls(), "\n") != "module-wrapper resume on" {
		t.Fatalf("exit %d %q calls %v", status, out, b.calls())
	}
	// A bare name is refused even though one is on PATH.
	b.clearCalls()
	b.env = append(b.env, "DEVBOX_GATEWAY_COMMAND=ahp-acp-gateway")
	if out, status := b.run("devbox-agents.sh", "resume", "on"); status != 4 || len(b.calls()) != 0 {
		t.Fatalf("bare name: exit %d %q calls %v", status, out, b.calls())
	}
}

func TestDevboxAgentsResumeWithoutTheCommand(t *testing.T) {
	b := newLifecycleBox(t)
	b.env = append(b.env, "DEVBOX_GATEWAY_HAS_RESUME=1", "DEVBOX_GATEWAY_COMMAND="+filepath.Join(b.root, "no-such-gateway-command"))
	if out, status := b.run("devbox-agents.sh", "resume", "on"); status != 4 {
		t.Fatalf("exit %d %q", status, out)
	}
}

func TestDevboxAgentsStopWhenIdle(t *testing.T) {
	b := newLifecycleBox(t)
	b.fakeProgram("sudo", `record sudo "$@"; exec "$@"`)
	out, status := b.run("devbox-agents.sh", "stop-when-idle")
	if status != 0 {
		t.Fatalf("exit %d %q", status, out)
	}
	if _, err := os.Stat(filepath.Join(b.state, "stop-when-idle")); err != nil {
		t.Fatalf("marker not written: %v", err)
	}
	if !strings.Contains(strings.Join(b.calls(), "\n"), "systemctl start --no-block devbox-autostop.service") {
		t.Fatalf("no poll at once: %v", b.calls())
	}
	if out, status = b.run("devbox-agents.sh", "stop-when-idle", "cancel"); status != 0 {
		t.Fatalf("cancel: exit %d %q", status, out)
	}
	if _, err := os.Stat(filepath.Join(b.state, "stop-when-idle")); !os.IsNotExist(err) {
		t.Fatal("marker not removed")
	}
}
