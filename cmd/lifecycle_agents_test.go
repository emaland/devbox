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

// D24: the gateway never continues a turn by itself. The boot line says how
// many turns were interrupted and wait for a person.
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
			name:  "interrupted turns",
			setup: func(b *lifecycleBox) { b.gateway.set(200, statusWithInterrupted(2)) },
			want:  "interrupted turns: 2, waiting for Try Again or a new message\n",
		},
		{
			name:  "none interrupted",
			setup: func(b *lifecycleBox) { b.gateway.set(200, statusWithInterrupted(0)) },
			want:  "interrupted turns: none\n",
		},
		// R-SEC on PR 18: only a number may reach the webhook.
		{
			name: "count is text with markup",
			setup: func(b *lifecycleBox) {
				b.gateway.set(200, statusWith(`, "resume": {"interrupted": "<!channel> <https://x.invalid|click>"}`))
			},
			want: "interrupted turns: not reported by this gateway build\n",
		},
		{
			name:  "resume is text",
			setup: func(b *lifecycleBox) { b.gateway.set(200, statusWith(`, "resume": "<!channel>"`)) },
			want:  "interrupted turns: not reported by this gateway build\n",
		},
		{
			name:  "count is a fraction",
			setup: func(b *lifecycleBox) { b.gateway.set(200, statusWith(`, "resume": {"interrupted": 2.7}`)) },
			want:  "interrupted turns: 2, waiting for Try Again or a new message\n",
		},
		{
			name:  "count below one",
			setup: func(b *lifecycleBox) { b.gateway.set(200, statusWith(`, "resume": {"interrupted": 0.4}`)) },
			want:  "interrupted turns: none\n",
		},
		{
			name:  "count negative",
			setup: func(b *lifecycleBox) { b.gateway.set(200, statusWith(`, "resume": {"interrupted": -2}`)) },
			want:  "interrupted turns: not reported by this gateway build\n",
		},
		{
			name:  "count out of range",
			setup: func(b *lifecycleBox) { b.gateway.set(200, statusWith(`, "resume": {"interrupted": 1e400}`)) },
			want:  "interrupted turns: not reported by this gateway build\n",
		},
		{
			name:  "build without the count",
			setup: func(b *lifecycleBox) { b.gateway.set(200, statusWith("")) },
			want:  "interrupted turns: not reported by this gateway build\n",
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
			r.setup(b)
			out, status := b.run("resume-summary.sh")
			if status != 0 || out != r.want {
				t.Fatalf("exit %d, output %q, want %q", status, out, r.want)
			}
			b.noTokenLeak(out)
		})
	}
}

// Nothing is going to start, so the boot line asks once and does not wait.
func TestResumeSummaryAsksOnce(t *testing.T) {
	b := newLifecycleBox(t)
	b.gateway.set(200, statusJSON(0, 0, 1))
	if out, status := b.run("resume-summary.sh"); status != 0 || out != "interrupted turns: none\n" {
		t.Fatalf("exit %d, output %q", status, out)
	}
	b.gateway.mu.Lock()
	requests := len(b.gateway.requests)
	b.gateway.mu.Unlock()
	if requests != 1 {
		t.Fatalf("%d status requests, want 1", requests)
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

	b.gateway.set(200, statusWithInterrupted(2))
	out, _ = b.run("devbox-agents.sh", "status")
	if out != "agents: idle; 3 sessions, 0 turns running, 0 waiting for a person; 2 interrupted, waiting for Try Again\n" {
		t.Fatalf("text status with interrupted turns: %q", out)
	}
	out, _ = b.run("devbox-agents.sh", "status", "--json")
	got = nil
	if err := json.Unmarshal([]byte(out), &got); err != nil || got["interrupted"] != 2.0 {
		t.Fatalf("interrupted in JSON: %q", out)
	}
	if _, has := got["resume"]; has {
		t.Fatalf("JSON still carries the deprecated resume object: %q", out)
	}

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

// D24: there is no resume switch, so devbox-agents offers no resume
// command and runs nothing for one.
func TestDevboxAgentsHasNoResumeCommand(t *testing.T) {
	b := newLifecycleBox(t)
	b.fakeProgram("ahp-acp-gateway", `record ahp-acp-gateway "$@"`)
	for _, value := range []string{"status", "on", "off"} {
		if out, status := b.run("devbox-agents.sh", "resume", value); status != 64 || len(b.calls()) != 0 {
			t.Fatalf("resume %s: exit %d %q calls %v", value, status, out, b.calls())
		}
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

// R-CODE on PR 18: the laptop reads the JSON counts into ints, so an odd
// count must not reach it as a fraction, text or a huge number. Otherwise
// every stop guard refuses until --force.
func TestDevboxAgentsStatusJSONCountsAreWholeNumbers(t *testing.T) {
	for name, tc := range map[string]struct {
		resume               string
		interrupted, pending any
	}{
		"fractions":            {`{"interrupted": 1.7, "pending": 0.5}`, 1.0, 0.0},
		"text":                 {`{"interrupted": "3", "pending": "x"}`, nil, 0.0},
		"resume not an object": {`"off"`, nil, 0.0},
		"out of range":         {`{"interrupted": 1e400, "pending": -1}`, nil, 0.0},
		// The other counts come from the gateway too.
		"other counts": {`{"interrupted": 1}`, 1.0, 0.0},
	} {
		t.Run(name, func(t *testing.T) {
			b := newLifecycleBox(t)
			sessions, turns := `{"total": 1}`, `{"active": 0, "waitingForUser": 0}`
			if name == "other counts" {
				sessions, turns = `{"total": 1.5}`, `{"active": 2.5, "waitingForUser": 0.5}`
			}
			b.gateway.set(200, `{"ok": true, "sessions": `+sessions+`, "turns": `+turns+`, "resume": `+tc.resume+`}`)
			out, status := b.run("devbox-agents.sh", "status", "--json")
			var got map[string]any
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("exit %d, not JSON: %q", status, out)
			}
			if got["interrupted"] != tc.interrupted || got["pending"] != tc.pending {
				t.Fatalf("interrupted %v pending %v, want %v %v (%q)", got["interrupted"], got["pending"], tc.interrupted, tc.pending, out)
			}
			var s agentStatus
			if err := json.Unmarshal([]byte(out), &s); err != nil {
				t.Fatalf("the laptop cannot read it: %v (%q)", err, out)
			}
		})
	}
}
