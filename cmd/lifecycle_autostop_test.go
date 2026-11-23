package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Tests for terraform/devbox/lifecycle/autostop.sh: the decision table in
// design note 031, section 2.3.

const hour = 3600

func (b *lifecycleBox) autostop() string {
	b.t.Helper()
	out, status := b.run("autostop.sh")
	if status != 0 {
		b.t.Fatalf("autostop.sh exited %d:\n%s", status, out)
	}
	b.noTokenLeak(out)
	return out
}

// stopped reports whether stop-instances was called, and checks the order
// of a stop: notice, gateway stop, then the instance.
func (b *lifecycleBox) stopped() bool {
	b.t.Helper()
	gatewayStop, instanceStop := -1, -1
	for i, call := range b.calls() {
		if call == "systemctl stop ahp-gateway.service" {
			gatewayStop = i
		}
		if strings.HasPrefix(call, "aws ec2 stop-instances") {
			instanceStop = i
			if call != "aws ec2 stop-instances --region test-region-1 --instance-ids i-0fake" {
				b.t.Errorf("stop-instances called as %q", call)
			}
		}
	}
	if instanceStop >= 0 && gatewayStop > instanceStop {
		b.t.Errorf("the gateway was stopped after the instance: %v", b.calls())
	}
	if instanceStop >= 0 && len(b.hook.posts()) == 0 {
		b.t.Error("the box stopped without a notice")
	}
	return instanceStop >= 0
}

func (b *lifecycleBox) lastLog() string {
	lines := b.logLines()
	if len(lines) == 0 {
		return ""
	}
	return lines[len(lines)-1]
}

func TestAutostopFirstPollStartsTheCountdown(t *testing.T) {
	b := newLifecycleBox(t)
	b.autostop()
	if got := strings.TrimSpace(b.read(filepath.Join(b.state, "last-active"))); got != "1700000000" {
		t.Fatalf("last-active = %q", got)
	}
	if b.stopped() {
		t.Fatal("stopped on the first poll")
	}
}

func TestAutostopDecisionTable(t *testing.T) {
	type row struct {
		name     string
		setup    func(b *lifecycleBox)
		idleFor  int64 // seconds between the first poll and the one checked
		wantStop bool
		wantLog  string // substring of the last log line
	}
	rows := []row{
		{
			name:     "window off: nothing at all",
			setup:    func(b *lifecycleBox) { b.write("autostop-after", "off\n") },
			idleFor:  100 * hour,
			wantStop: false,
		},
		{
			name:     "login present, window long past",
			setup:    func(b *lifecycleBox) { b.write("who", "someone pts/0 2026-11-23 10:00\n") },
			idleFor:  5 * hour,
			wantStop: false,
			wantLog:  "active (login)",
		},
		{
			name:     "inbound ssh connection, window long past",
			setup:    func(b *lifecycleBox) { b.write("ss", "0 0 10.0.0.5:22 10.0.0.9:51000\n") },
			idleFor:  5 * hour,
			wantStop: false,
			wantLog:  "active (login)",
		},
		{
			name:     "agent busy, window long past",
			setup:    func(b *lifecycleBox) { b.gateway.set(200, statusJSON(2, 1, 0)) },
			idleFor:  5 * hour,
			wantStop: false,
			wantLog:  "active (agent busy)",
		},
		{
			name:     "continuation pending counts as busy",
			setup:    func(b *lifecycleBox) { b.gateway.set(200, statusJSON(0, 0, 1)) },
			idleFor:  5 * hour,
			wantStop: false,
			wantLog:  "active (agent busy)",
		},
		{
			name:     "every turn waiting for a person, window elapsed",
			setup:    func(b *lifecycleBox) { b.gateway.set(200, statusJSON(2, 2, 0)) },
			idleFor:  4 * hour,
			wantStop: true,
			wantLog:  "gateway stopped",
		},
		{
			name:     "waiting, window not elapsed",
			setup:    func(b *lifecycleBox) { b.gateway.set(200, statusJSON(1, 1, 0)) },
			idleFor:  3 * hour,
			wantStop: false,
			wantLog:  "idle (agent waiting for a person)",
		},
		{
			name:     "gateway down, window elapsed",
			setup:    func(b *lifecycleBox) { b.gateway.server.Close() },
			idleFor:  4 * hour,
			wantStop: true,
		},
		{
			name:     "gateway without a status endpoint, window not elapsed",
			setup:    func(b *lifecycleBox) { b.gateway.set(404, "not found") },
			idleFor:  hour,
			wantStop: false,
			wantLog:  "idle (gateway unreachable: HTTP 404)",
		},
		{
			name:     "status without turn counts is not activity",
			setup:    func(b *lifecycleBox) { b.gateway.set(200, `{"ok": true}`) },
			idleFor:  hour,
			wantStop: false,
			wantLog:  "idle (gateway unreachable: answer has no turn counts)",
		},
		{
			name:     "no gateway installed, window elapsed",
			setup:    func(b *lifecycleBox) { b.remove("gateway-config.json") },
			idleFor:  4 * hour,
			wantStop: true,
		},
		{
			name:     "idle, window not elapsed",
			idleFor:  4*hour - 1,
			wantStop: false,
			wantLog:  "| auto-stop | idle",
		},
		{
			name:     "idle window from the file",
			setup:    func(b *lifecycleBox) { b.write("autostop-after", "30m\n") },
			idleFor:  30 * 60,
			wantStop: true,
		},
		{
			name:     "an unreadable window file falls back to the default",
			setup:    func(b *lifecycleBox) { b.write("autostop-after", "soon\n") },
			idleFor:  4*hour - 1,
			wantStop: false,
		},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			b := newLifecycleBox(t)
			if r.setup != nil {
				r.setup(b)
			}
			b.autostop()
			b.clearCalls()
			b.advance(r.idleFor)
			b.autostop()
			if got := b.stopped(); got != r.wantStop {
				t.Fatalf("stopped = %v, want %v; calls %v; log:\n%s", got, r.wantStop, b.calls(), b.read(b.logFile))
			}
			if r.wantLog != "" && !strings.Contains(b.lastLog(), r.wantLog) {
				t.Fatalf("last log line %q does not contain %q; log:\n%s", b.lastLog(), r.wantLog, b.read(b.logFile))
			}
			if r.name == "window off: nothing at all" && (len(b.logLines()) != 0 || len(b.calls()) != 0) {
				t.Fatalf("window off still did something: %v %v", b.logLines(), b.calls())
			}
		})
	}
}

func TestAutostopBusyCap(t *testing.T) {
	b := newLifecycleBox(t)
	b.gateway.set(200, statusJSON(1, 0, 0))
	// Busy at every poll, every 10 minutes, for just under the 12 h cap.
	for elapsed := int64(0); elapsed < 12*hour; elapsed += 600 {
		b.autostop()
		if b.stopped() {
			t.Fatalf("stopped after %ds of busy, before the cap", elapsed)
		}
		b.advance(600)
	}
	b.clearCalls()
	b.autostop()
	if !b.stopped() {
		t.Fatalf("not stopped at the cap; log:\n%s", b.read(b.logFile))
	}
	posts := b.hook.posts()
	if len(posts) != 1 || !strings.Contains(posts[0], "cap") || !strings.Contains(posts[0], "1 running, 0 waiting") {
		t.Fatalf("notice at the cap: %q", posts)
	}
	busyLines := 0
	for _, line := range b.logLines() {
		if strings.Contains(line, "active (agent busy)") {
			busyLines++
		}
	}
	if busyLines != 1 {
		t.Fatalf("an unchanged decision was logged %d times, want once:\n%s", busyLines, b.read(b.logFile))
	}
}

func TestAutostopBusyCapNeedsContinuousBusy(t *testing.T) {
	for _, interruption := range []string{"waiting", "login"} {
		t.Run(interruption, func(t *testing.T) {
			b := newLifecycleBox(t)
			b.gateway.set(200, statusJSON(1, 0, 0))
			b.autostop()
			b.advance(11 * hour)
			// One poll that is not busy restarts the cap.
			if interruption == "waiting" {
				b.gateway.set(200, statusJSON(1, 1, 0))
			} else {
				b.write("who", "someone pts/0\n")
			}
			b.autostop()
			b.remove("who")
			b.gateway.set(200, statusJSON(1, 0, 0))
			b.advance(600)
			b.autostop()
			b.advance(2 * hour)
			b.clearCalls()
			b.autostop()
			if b.stopped() {
				t.Fatalf("stopped by the cap although busy was interrupted; log:\n%s", b.read(b.logFile))
			}
		})
	}
}

func TestAutostopBusyCapOffAndFromFile(t *testing.T) {
	b := newLifecycleBox(t)
	b.write("autostop-busy-cap", "off\n")
	b.gateway.set(200, statusJSON(1, 0, 0))
	b.autostop()
	b.advance(48 * hour)
	b.autostop()
	if b.stopped() {
		t.Fatal("cap off still stopped a busy box")
	}

	b = newLifecycleBox(t)
	b.write("autostop-busy-cap", "2h\n")
	b.gateway.set(200, statusJSON(1, 0, 0))
	b.autostop()
	b.advance(2 * hour)
	b.autostop()
	if !b.stopped() {
		t.Fatal("cap from the file was not applied")
	}
}

func TestAutostopLogsAnUnreachableGatewayOnceAndOnChange(t *testing.T) {
	b := newLifecycleBox(t)
	b.gateway.set(404, "")
	for i := 0; i < 5; i++ {
		b.autostop()
		b.advance(600)
	}
	b.gateway.set(500, "")
	b.autostop()
	var unreachable []string
	for _, line := range b.logLines() {
		if strings.Contains(line, "unreachable") {
			unreachable = append(unreachable, line)
		}
	}
	if len(unreachable) != 2 || !strings.Contains(unreachable[0], "HTTP 404") || !strings.Contains(unreachable[1], "HTTP 500") {
		t.Fatalf("unreachable lines: %q", unreachable)
	}
}

func TestAutostopUnusableTokenFile(t *testing.T) {
	for name, content := range map[string]string{
		"quote":      `abc"def` + "\n",
		"two lines":  "abcdef\nghijkl\n",
		"empty":      "",
		"backslash":  `abc\def`,
		"whitespace": "abc def\n",
	} {
		t.Run(name, func(t *testing.T) {
			b := newLifecycleBox(t)
			b.gateway.set(200, statusJSON(1, 0, 0))
			b.write("token", content)
			b.autostop()
			if !strings.Contains(b.lastLog(), "token file unusable") {
				t.Fatalf("last log %q", b.lastLog())
			}
			b.gateway.mu.Lock()
			requests := len(b.gateway.requests)
			b.gateway.mu.Unlock()
			if requests != 0 {
				t.Fatalf("a request was sent with an unusable token file")
			}
		})
	}
}

func TestAutostopSendsTheTokenOnlyAsAHeader(t *testing.T) {
	b := newLifecycleBox(t)
	b.gateway.set(200, statusJSON(1, 0, 0))
	b.autostop()
	b.gateway.mu.Lock()
	ok, bad := b.gateway.authOK, b.gateway.authBad
	b.gateway.mu.Unlock()
	if ok != 1 || bad != 0 {
		t.Fatalf("authorised requests %d, refused %d", ok, bad)
	}
}

func TestAutostopFailedGatewayStopStillStopsTheInstance(t *testing.T) {
	b := newLifecycleBox(t)
	b.write("stop-exit", "1\n")
	b.autostop()
	b.advance(4 * hour)
	b.autostop()
	if !b.stopped() {
		t.Fatal("instance not stopped after a failed gateway stop")
	}
	if !strings.Contains(b.read(b.logFile), "did not stop cleanly") {
		t.Fatalf("failure not logged:\n%s", b.read(b.logFile))
	}
}

func TestAutostopFailedInstanceStopRestartsTheGateway(t *testing.T) {
	b := newLifecycleBox(t)
	b.write("aws-exit", "255\n")
	b.autostop()
	b.advance(4 * hour)
	out, status := b.run("autostop.sh")
	if status == 0 {
		t.Fatalf("exit 0 after a failed stop-instances:\n%s", out)
	}
	calls := strings.Join(b.calls(), "\n")
	if !strings.Contains(calls, "systemctl start --no-block ahp-gateway.service") {
		t.Fatalf("gateway not started again: %v", b.calls())
	}
	if !strings.Contains(b.read(b.logFile), "stop-instances failed") {
		t.Fatalf("not logged:\n%s", b.read(b.logFile))
	}
}

func TestAutostopGatewayAlreadyStoppedIsNotStoppedAgain(t *testing.T) {
	b := newLifecycleBox(t)
	b.write("gateway", "inactive\n")
	b.autostop()
	b.advance(4 * hour)
	b.autostop()
	for _, call := range b.calls() {
		if call == "systemctl stop ahp-gateway.service" {
			t.Fatal("stopped a gateway that was not running")
		}
	}
	if !b.stopped() {
		t.Fatal("instance not stopped")
	}
}

func TestAutostopStopWhenIdle(t *testing.T) {
	b := newLifecycleBox(t)
	b.write("who", "someone pts/0\n")
	b.gateway.set(200, statusJSON(1, 0, 0))
	if err := os.MkdirAll(b.state, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.state, "stop-when-idle"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	b.autostop()
	if b.stopped() {
		t.Fatal("stopped while busy")
	}
	// Not busy any more; the person is still logged in.
	b.gateway.set(200, statusJSON(1, 1, 0))
	b.advance(60)
	b.autostop()
	if !b.stopped() {
		t.Fatalf("not stopped once idle; log:\n%s", b.read(b.logFile))
	}
	if !strings.Contains(b.read(b.logFile), "requested") {
		t.Fatalf("log does not say why:\n%s", b.read(b.logFile))
	}
}

func TestAutostopDurations(t *testing.T) {
	for value, wantSeconds := range map[string]int64{
		"90s": 90, "45m": 45 * 60, "45min": 45 * 60, "2h": 2 * hour, "3": 3 * hour, "1d": 86400,
	} {
		t.Run(value, func(t *testing.T) {
			b := newLifecycleBox(t)
			b.write("autostop-after", value+"\n")
			b.autostop()
			b.advance(wantSeconds - 1)
			b.autostop()
			if b.stopped() {
				t.Fatalf("%s: stopped one second early", value)
			}
			b.advance(1)
			b.autostop()
			if !b.stopped() {
				t.Fatalf("%s: not stopped at %ds; log:\n%s", value, wantSeconds, b.read(b.logFile))
			}
		})
	}
}

func (b *lifecycleBox) requestStopWhenIdle() {
	b.t.Helper()
	if err := os.MkdirAll(b.state, 0o755); err != nil {
		b.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.state, "stop-when-idle"), nil, 0o644); err != nil {
		b.t.Fatal(err)
	}
}

func TestAutostopStopWhenIdleWaitsOutAnUnreachableGateway(t *testing.T) {
	b := newLifecycleBox(t)
	b.requestStopWhenIdle()
	b.gateway.set(503, "")
	// Many polls in quick succession (a requested stop runs one at once) are
	// not 30 minutes.
	for poll := 1; poll <= 5; poll++ {
		b.autostop()
		if b.stopped() {
			t.Fatalf("stopped on quick unreachable poll %d", poll)
		}
		b.advance(10)
	}
	b.advance(20 * 60)
	// An answer in between starts the clock again.
	b.gateway.set(200, statusJSON(0, 0, 1))
	b.autostop()
	b.gateway.set(503, "")
	b.advance(60)
	b.autostop()
	b.advance(29 * 60)
	b.autostop()
	if b.stopped() {
		t.Fatal("stopped before 30 minutes unreachable after an answer")
	}
	b.advance(60)
	b.autostop()
	if !b.stopped() {
		t.Fatalf("not stopped after 30 minutes unreachable; log:\n%s", b.read(b.logFile))
	}
}

func TestAutostopStopWhenIdleWithNoGateway(t *testing.T) {
	b := newLifecycleBox(t)
	b.requestStopWhenIdle()
	b.remove("gateway-config.json")
	b.autostop()
	if !b.stopped() {
		t.Fatal("requested stop with no gateway installed did not stop")
	}
}

// An attached editor is not a person: a laptop left awake with a tunnel up
// must not keep a stuck agent running past the cap.
func TestAutostopAttachedClientDoesNotHoldOffTheCap(t *testing.T) {
	b := newLifecycleBox(t)
	b.write("autostop-busy-cap", "2h\n")
	b.gateway.set(200, strings.Replace(statusJSON(1, 0, 0), `"ok": true`, `"ok": true, "clients": 1`, 1))
	b.autostop()
	b.advance(2 * hour)
	b.autostop()
	if !b.stopped() {
		t.Fatal("cap not applied with a client attached")
	}
}
