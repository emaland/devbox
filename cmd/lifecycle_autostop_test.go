package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Tests for terraform/devbox/lifecycle/autostop.sh: the decision table in
// design note 031, section 2.3.

// Review finding (R-CODE, PR 11): with auto-stop off, a requested stop was
// never checked, so `devbox down --when-idle` never stopped the box.
func TestAutostopStopWhenIdleWithTheWindowOff(t *testing.T) {
	b := newLifecycleBox(t)
	b.write("autostop-after", "off\n")
	b.gateway.set(200, statusJSON(1, 0, 0))
	b.requestStopWhenIdle()
	b.autostop()
	if b.stopped() {
		t.Fatal("stopped while busy")
	}
	b.gateway.set(200, statusJSON(0, 0, 0))
	b.advance(600)
	b.autostop()
	if !b.stopped() {
		t.Fatalf("a requested stop with auto-stop off never happened; log:\n%s", b.read(b.logFile))
	}
	// Without a request, off still means nothing at all, busy or not.
	b = newLifecycleBox(t)
	b.write("autostop-after", "off\n")
	b.gateway.set(200, statusJSON(1, 0, 0))
	b.autostop()
	b.advance(100 * hour)
	b.autostop()
	if b.stopped() || len(b.logLines()) != 0 {
		t.Fatalf("off without a request did something: %v", b.logLines())
	}
}

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
	// Each stop-instances closes one stop. Within it (since the previous
	// one), the order must be: notice, gateway stop (when the gateway was
	// running), then stop-instances.
	calls := b.calls()
	found := false
	segment := 0
	for k, call := range calls {
		if !strings.HasPrefix(call, "aws ec2 stop-instances") {
			continue
		}
		found = true
		if call != "aws ec2 stop-instances --region test-region-1 --instance-ids i-0fake" {
			b.t.Errorf("stop-instances called as %q", call)
		}
		notice, gatewayStop := -1, -1
		for i := segment; i < k; i++ {
			switch calls[i] {
			case "webhook post":
				if notice < 0 {
					notice = i
				}
			case "systemctl stop ahp-gateway.service":
				gatewayStop = i
			}
		}
		if notice < 0 {
			b.t.Errorf("the box stopped without a notice: %v", calls[segment:k+1])
		} else if gatewayStop >= 0 && gatewayStop < notice {
			b.t.Errorf("the gateway was stopped before the notice: %v", calls[segment:k+1])
		}
		segment = k + 1
	}
	// Auto-stop stops the gateway only on its way to stop-instances, so a
	// gateway stop with no stop-instances after it came after the instance.
	for i := segment; i < len(calls); i++ {
		if calls[i] == "systemctl stop ahp-gateway.service" {
			b.t.Errorf("the gateway was stopped after the instance: %v", calls)
		}
	}
	return found
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
			// An idle window longer than the scenario, so only the cap can
			// stop the box (a waiting poll after 11 h would otherwise be an
			// idle stop).
			b.write("autostop-after", "1d\n")
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
		"quote":     `abc"def` + "\n",
		"two lines": "abcdef\nghijkl\n",
		"empty":     "",
		"backslash": `abc\def`,
		"tab":       "abc\tdef\n",
		"two CRs":   "abcdef\r\r\n",
		// The gateway refuses both (printable ASCII 0x21 to 0x7e, and only a
		// whole CRLF or LF line end is dropped).
		"whitespace": "abc def\n",
		"lone CR":    "abcdef\r",
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

// Review finding: with stop-instances failing, every later poll stopped the
// gateway again, using up its turns' one continuation.
func TestAutostopFailedInstanceStopIsNotRetriedEveryPoll(t *testing.T) {
	for _, row := range []string{"cap", "idle", "requested"} {
		t.Run(row, func(t *testing.T) {
			b := newLifecycleBox(t)
			b.write("aws-exit", "255\n")
			switch row {
			case "cap":
				b.write("autostop-busy-cap", "2h\n")
				b.gateway.set(200, statusJSON(1, 0, 0))
			case "requested":
				b.requestStopWhenIdle()
			}
			b.autostopAllowFailure()
			if row != "requested" {
				b.advance(4 * hour)
				b.autostopAllowFailure()
			}
			if b.count(gatewayStop) != 1 {
				t.Fatalf("setup: gateway stops %d; calls %v", b.count(gatewayStop), b.calls())
			}
			posts := len(b.hook.posts())
			for i := 0; i < 3; i++ {
				b.advance(600)
				b.autostopAllowFailure()
			}
			if b.count(gatewayStop) != 1 {
				t.Fatalf("the gateway was stopped again after a failed stop-instances: %d stops; log:\n%s", b.count(gatewayStop), b.read(b.logFile))
			}
			if len(b.hook.posts()) != posts {
				t.Fatalf("notices repeated: %q", b.hook.posts())
			}
			if row == "requested" {
				if _, err := os.Stat(filepath.Join(b.state, "stop-when-idle")); !os.IsNotExist(err) {
					t.Fatal("the request is still there after a failed stop")
				}
			}
		})
	}
}

// The next try comes after a full window or cap.
func TestAutostopTriesAgainAfterAFullWindow(t *testing.T) {
	b := newLifecycleBox(t)
	b.write("aws-exit", "255\n")
	b.autostopAllowFailure()
	b.advance(4 * hour)
	b.autostopAllowFailure()
	b.remove("aws-exit")
	b.advance(4*hour - 600)
	b.autostop()
	if b.count(gatewayStop) != 1 {
		t.Fatal("tried again before a full window")
	}
	b.advance(600)
	b.autostop()
	if b.count(gatewayStop) != 2 || !b.stopped() {
		t.Fatalf("not tried again after a full window: %v", b.calls())
	}
}

// Review finding: auto-stop must not start the gateway again while a spot
// interruption is under way.
func TestAutostopFailedInstanceStopLeavesTheGatewayDownDuringASpotNotice(t *testing.T) {
	b := newLifecycleBox(t)
	b.write("aws-exit", "255\n")
	b.autostopAllowFailure()
	b.advance(4 * hour)
	notice := filepath.Join(b.root, "spot", "notice-stop_x")
	if err := os.MkdirAll(notice, 0o700); err != nil {
		t.Fatal(err)
	}
	b.write("spot/notice-stop_x/seen", "")
	b.write("spot/notice-stop_x/time", fmt.Sprintf("%d\n", fakeStart+4*hour+120))
	b.autostopAllowFailure()
	if b.count(gatewayStart) != 0 {
		t.Fatalf("started the gateway during a spot interruption: %v", b.calls())
	}
	if !strings.Contains(b.read(b.logFile), "spot interruption under way") {
		t.Fatalf("log:\n%s", b.read(b.logFile))
	}
}

func (b *lifecycleBox) autostopAllowFailure() {
	b.t.Helper()
	out, _ := b.run("autostop.sh")
	b.noTokenLeak(out)
}

// Review finding: a resume field that is not an object counts as nothing
// pending, not as a broken answer.
func TestGatewayStatusResumeNotAnObject(t *testing.T) {
	b := newLifecycleBox(t)
	b.gateway.set(200, `{"ok": true, "turns": {"active": 1, "waitingForUser": 1}, "resume": "off"}`)
	out, status := b.run("gateway-status.sh")
	if status != 0 || !strings.HasPrefix(out, "waiting\n") {
		t.Fatalf("exit %d output %q", status, out)
	}
}

// Review finding (R-SEC): the token goes only to a listener owned by the
// token file's owner. Another user holding the port while the gateway is
// down gets nothing.
func TestGatewayStatusSendsTheTokenOnlyToTheGatewaysUser(t *testing.T) {
	b := newLifecycleBox(t)
	b.write("listener-uid", fmt.Sprintf("%d\n", os.Getuid()+1))
	out, status := b.run("gateway-status.sh")
	if status != 2 || !strings.Contains(out, "not the gateway's user") {
		t.Fatalf("exit %d output %q", status, out)
	}
	b.gateway.mu.Lock()
	requests := len(b.gateway.requests)
	b.gateway.mu.Unlock()
	if requests != 0 {
		t.Fatal("the token was sent to a listener of another user")
	}

	b.remove("listener-uid")
	b.write("no-listener", "")
	if out, status := b.run("gateway-status.sh"); status != 2 || !strings.Contains(out, "nothing listening") {
		t.Fatalf("no listener: exit %d output %q", status, out)
	}
}

func TestGatewayStatusRefusesAnUnsafeTokenFile(t *testing.T) {
	cases := map[string]func(b *lifecycleBox, token string){
		"readable by others": func(b *lifecycleBox, token string) {
			if err := os.Chmod(token, 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"a link": func(b *lifecycleBox, token string) {
			real := filepath.Join(b.root, "elsewhere")
			if err := os.Rename(token, real); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, token); err != nil {
				t.Fatal(err)
			}
		},
		"a FIFO": func(b *lifecycleBox, token string) {
			if err := os.Remove(token); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(token, 0o600); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			b := newLifecycleBox(t)
			setup(b, filepath.Join(b.root, "token"))
			done := make(chan struct{})
			var out string
			var status int
			go func() { out, status = b.run("gateway-status.sh"); close(done) }()
			select {
			case <-done:
			case <-time.After(20 * time.Second):
				t.Fatal("gateway-status.sh hung on the token file")
			}
			if status != 2 || !strings.Contains(out, "token file unusable") {
				t.Fatalf("exit %d output %q", status, out)
			}
		})
	}
}

// R-SEC finding: the webhook file is read, never run, and a line meant to
// inject into curl's config is refused.
func TestNotifyWebhookFileParsing(t *testing.T) {
	cases := map[string]string{
		"command substitution": "SLACK_WEBHOOK_URL=$(touch {marker})\n",
		"backticks":            "SLACK_WEBHOOK_URL=`touch {marker}`\n",
		"quote into config":    "SLACK_WEBHOOK_URL=\"{url}\\\"\nupload-file = \\\"/etc/passwd\"\n",
		// A URL user part: "127.0.0.1:1" before the @, the real host after.
		"user part":            "SLACK_WEBHOOK_URL=http://127.0.0.1:1{at}elsewhere.invalid/hook\n",
		"plain http elsewhere": "SLACK_WEBHOOK_URL=http://example.invalid/hook\n",
		"a second statement":   "touch {marker}; SLACK_WEBHOOK_URL={url}; touch {marker}\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			b := newLifecycleBox(t)
			marker := filepath.Join(b.root, "ran")
			content = strings.NewReplacer("{marker}", marker, "{url}", b.hook.server.URL+"/hook", "{at}", "@").Replace(content)
			b.write("webhook", content)
			if out, status := b.run("notify.sh", "hello"); status != 0 {
				t.Fatalf("exit %d %q", status, out)
			}
			if _, err := os.Stat(marker); err == nil {
				t.Fatal("the webhook file was run")
			}
			if posts := b.hook.posts(); len(posts) != 0 {
				t.Fatalf("posted with an unusable address: %q", posts)
			}
		})
	}
	// The plain form still works, and the rest of the file is not run.
	b := newLifecycleBox(t)
	marker := filepath.Join(b.root, "ran")
	b.write("webhook", "touch "+marker+"\nexport SLACK_WEBHOOK_URL="+b.hook.server.URL+"/hook\n$(touch "+marker+")\n")
	if _, status := b.run("notify.sh", "hello"); status != 0 || len(b.hook.posts()) != 1 {
		t.Fatalf("a good webhook line did not post: %q", b.hook.posts())
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the webhook file was run")
	}
}

// Issue 14, point 1: whatever token file the gateway accepts is accepted
// here too. Otherwise every poll is "unreachable" and a busy agent no longer
// keeps the box up.
func TestGatewayStatusAcceptsWhatTheGatewayAccepts(t *testing.T) {
	cases := map[string]func(b *lifecycleBox, token string){
		"mode 0400": func(b *lifecycleBox, token string) {
			if err := os.Chmod(token, 0o400); err != nil {
				t.Fatal(err)
			}
		},
		"mode 0700": func(b *lifecycleBox, token string) {
			if err := os.Chmod(token, 0o700); err != nil {
				t.Fatal(err)
			}
		},
		"a CRLF line end": func(b *lifecycleBox, token string) {
			b.write("token", lifecycleToken+"\r\n")
		},
		"no line end": func(b *lifecycleBox, token string) {
			b.write("token", lifecycleToken)
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			b := newLifecycleBox(t)
			b.gateway.set(200, statusJSON(1, 0, 0))
			setup(b, filepath.Join(b.root, "token"))
			out, status := b.run("gateway-status.sh")
			if status != 0 || !strings.HasPrefix(out, "busy\n") {
				t.Fatalf("exit %d output %q", status, out)
			}
		})
	}
	b := newLifecycleBox(t)
	if err := os.Chmod(filepath.Join(b.root, "token"), 0o640); err != nil {
		t.Fatal(err)
	}
	if out, status := b.run("gateway-status.sh"); status != 2 || !strings.Contains(out, "token file unusable") {
		t.Fatalf("group-readable: exit %d output %q", status, out)
	}
}

// Issue 14, point 3: ss prints no uid: for a socket owned by root. Such a
// listener is root's, not "nothing listening", and gets no token unless the
// token file is root's too.
func TestGatewayStatusSeesARootListener(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("the token file would be root's")
	}
	b := newLifecycleBox(t)
	b.write("listener-uid", "0\n")
	out, status := b.run("gateway-status.sh")
	if status != 2 || !strings.Contains(out, "not the gateway's user") {
		t.Fatalf("exit %d output %q", status, out)
	}
	b.gateway.mu.Lock()
	requests := len(b.gateway.requests)
	b.gateway.mu.Unlock()
	if requests != 0 {
		t.Fatal("the token was sent to root's listener")
	}
}

// Issue 14, point 2: a notice that found the gateway already down is never
// marked done. It stops counting as an interruption under way 5 min after
// its action time, so a failed stop after that starts the gateway again.
func TestAutostopSpotNoticeEndsAfterItsActionTime(t *testing.T) {
	for name, tc := range map[string]struct {
		time    string // the notice's time file, %d is the action time
		started int
	}{
		"within 5 min": {time: fmt.Sprintf("%d\n", fakeStart+4*hour-299), started: 0},
		"at 5 min":     {time: fmt.Sprintf("%d\n", fakeStart+4*hour-300), started: 1},
		"after 5 min":  {time: fmt.Sprintf("%d\n", fakeStart+4*hour-301), started: 1},
		// No usable time: still an interruption under way.
		"time not a number": {time: "soon\n", started: 0},
	} {
		t.Run(name, func(t *testing.T) {
			b := newLifecycleBox(t)
			b.write("aws-exit", "255\n")
			b.autostopAllowFailure()
			b.advance(4 * hour)
			if err := os.MkdirAll(filepath.Join(b.root, "spot", "notice-stop_x"), 0o700); err != nil {
				t.Fatal(err)
			}
			b.write("spot/notice-stop_x/seen", "")
			b.write("spot/notice-stop_x/stopped", "not running\n")
			b.write("spot/notice-stop_x/time", tc.time)
			b.autostopAllowFailure()
			if got := b.count(gatewayStart); got != tc.started {
				t.Fatalf("gateway started %d times, want %d: %v", got, tc.started, b.calls())
			}
		})
	}
}

// R-SEC and R-CODE on PR 17: the listener's owner comes from ss's own
// columns, never from text later on the line. The cgroup path is printed
// after uid: and a local user can choose it, spaces included.
func TestGatewayStatusReadsTheListenerByColumn(t *testing.T) {
	owner := os.Getuid()
	other := owner + 1
	refused := map[string]string{
		"owner in the cgroup path":                 fmt.Sprintf("LISTEN 0 4096 127.0.0.1:PORT 0.0.0.0:* uid:%d ino:1 sk:1 cgroup:/user.slice/x uid:%d <->\n", other, owner),
		"address and owner in the cgroup path":     fmt.Sprintf("LISTEN 0 4096 0.0.0.0:PORT 0.0.0.0:* uid:%d ino:1 sk:1 cgroup:/x 127.0.0.1:PORT uid:%d <->\n", other, owner),
		"another listener on the wildcard address": fmt.Sprintf("LISTEN 0 4096 127.0.0.1:PORT 0.0.0.0:* uid:%d ino:1 sk:1 cgroup:/x <->\nLISTEN 0 4096 0.0.0.0:PORT 0.0.0.0:* uid:%d ino:2 sk:2 cgroup:/y <->\n", owner, other),
		"no uid or ino column":                     "LISTEN 0 4096 127.0.0.1:PORT 0.0.0.0:* sk:1 cgroup:/x <->\n",
	}
	for name, lines := range refused {
		t.Run(name, func(t *testing.T) {
			b := newLifecycleBox(t)
			b.write("listener-lines", lines)
			out, status := b.run("gateway-status.sh")
			if status != 2 {
				t.Fatalf("exit %d output %q", status, out)
			}
			b.gateway.mu.Lock()
			requests := len(b.gateway.requests)
			b.gateway.mu.Unlock()
			if requests != 0 {
				t.Fatalf("the token was sent (%q)", out)
			}
		})
	}
	// The real owner with another uid later on the line is still the owner.
	b := newLifecycleBox(t)
	b.gateway.set(200, statusJSON(0, 0, 0))
	b.write("listener-lines", fmt.Sprintf("LISTEN 0 4096 127.0.0.1:PORT 0.0.0.0:* uid:%d ino:1 sk:1 cgroup:/x uid:%d <->\n", owner, other))
	if out, status := b.run("gateway-status.sh"); status != 0 {
		t.Fatalf("the gateway's own listener was refused: exit %d output %q", status, out)
	}
}

// R-SEC on PR 17, second round: the "?" placeholder for a listener that
// matches no owner was expanded as a file name pattern, so a one-character
// file in the working directory could turn it into the owner's uid: here
// owner 7 and a file named 7. (R-TEST, round 5: the fake gives a usable mode
// and owner, so the script reaches the listener loop.)
func TestGatewayStatusPlaceholderIsNotAPattern(t *testing.T) {
	b := newLifecycleBox(t)
	realStat, err := exec.LookPath("stat")
	if err != nil {
		t.Skip("stat not on PATH")
	}
	// The token file looks like owner 7's.
	b.fakeProgram("stat", fmt.Sprintf(`case "$*" in *%%u*) echo "600 7" ;; *) exec %s "$@" ;; esac`, realStat))
	b.workdir = t.TempDir()
	if err := os.WriteFile(filepath.Join(b.workdir, "7"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	b.write("listener-lines", "LISTEN 0 4096 0.0.0.0:PORT 0.0.0.0:* uid:1234 ino:1 sk:1 cgroup:/x <->\n")
	out, status := b.run("gateway-status.sh")
	b.gateway.mu.Lock()
	requests := len(b.gateway.requests)
	b.gateway.mu.Unlock()
	if status != 2 || requests != 0 {
		t.Fatalf("exit %d, %d requests, output %q", status, requests, out)
	}
}

// R-SEC on PR 17, second round: run as root, the token file is read with
// the file owner's own rights, so a file swapped in after the checks (a
// link to a file only root can read) is not read by root.
func TestGatewayStatusReadsTheTokenAsItsOwner(t *testing.T) {
	b := newLifecycleBox(t)
	realID, err := exec.LookPath("id")
	if err != nil {
		t.Skip("id not on PATH")
	}
	realStat, err := exec.LookPath("stat")
	if err != nil {
		t.Skip("stat not on PATH")
	}
	realTimeout, err := exec.LookPath("timeout")
	if err != nil {
		t.Skip("timeout not on PATH")
	}
	// The script believes it is root; any other question goes to the real id.
	b.fakeProgram("id", fmt.Sprintf(`[ "$*" = -u ] && { echo 0; exit 0; }; exec %s "$@"`, realID))
	// Round 3: the file's own group is not the owner's; it must not be used.
	b.fakeProgram("stat", fmt.Sprintf(`case "$*" in *%%g*) echo 4242 ;; *) exec %s "$@" ;; esac`, realStat))
	b.fakeProgram("timeout", fmt.Sprintf(`record timeout "$@"; exec %s "$@"`, realTimeout))
	b.fakeProgram("setpriv", `record setpriv "$@"
while [ $# -gt 0 ]; do case "$1" in --*) shift ;; *) break ;; esac; done
exec "$@"`)
	b.write("listener-uid", fmt.Sprintf("%d\n", os.Getuid()))
	b.gateway.set(200, statusJSON(0, 0, 0))
	out, status := b.run("gateway-status.sh")
	if status != 0 {
		t.Fatalf("exit %d output %q", status, out)
	}
	primary, err := exec.Command(realID, "-g", fmt.Sprint(os.Getuid())).Output()
	if err != nil {
		t.Fatal(err)
	}
	calls := b.calls()
	var timeoutAt, setprivAt = -1, -1
	for n, call := range calls {
		if timeoutAt < 0 && strings.HasPrefix(call, "timeout ") && strings.Contains(call, " setpriv ") {
			timeoutAt = n
		}
		if setprivAt < 0 && strings.HasPrefix(call, "setpriv ") {
			setprivAt = n
		}
	}
	want := fmt.Sprintf("setpriv --reuid=%d --regid=%s --clear-groups env -i head -c 4096 -- %s",
		os.Getuid(), strings.TrimSpace(string(primary)), filepath.Join(b.root, "token"))
	// The limit is root's, outside the drop, so the owner cannot stop it.
	if timeoutAt < 0 || setprivAt < 0 || timeoutAt > setprivAt || calls[setprivAt] != want {
		t.Fatalf("the token was not read as its owner, under root's time limit; calls:\n%s\nwant %q", strings.Join(calls, "\n"), want)
	}
}

// R-SEC on PR 17, second round: root's curl ignores ~/.curlrc and any
// proxy, so the Authorization header goes only to the loopback port.
func TestGatewayStatusCurlIgnoresConfigAndProxies(t *testing.T) {
	b := newLifecycleBox(t)
	b.env = append(b.env, "http_proxy=http://127.0.0.1:9", "HTTP_PROXY=http://127.0.0.1:9", "ALL_PROXY=http://127.0.0.1:9")
	b.gateway.set(200, statusJSON(0, 0, 0))
	if out, status := b.run("gateway-status.sh"); status != 0 {
		t.Fatalf("a proxy in the environment was used: exit %d output %q", status, out)
	}
	argv := b.read(filepath.Join(b.root, "curl-argv"))
	if !strings.HasPrefix(argv, "-q ") {
		t.Fatalf("curl not started with -q: %q", argv)
	}
}

// R-SEC on PR 17, round 4: the reader runs as the token's owner, who may
// open its output through /proc and hold it. Root must not wait on any
// write end the owner can hold. Here a process left behind by the reader
// keeps its output open for 20 s.
func TestGatewayStatusDoesNotWaitOnTheReadersOutput(t *testing.T) {
	b := newLifecycleBox(t)
	realHead, err := exec.LookPath("head")
	if err != nil {
		t.Skip("head not on PATH")
	}
	b.fakeProgram("head", fmt.Sprintf(`case "$*" in
  *"$FAKE_ROOT/token"*) (exec sleep 20) & echo $! >"$FAKE_ROOT/holder-pid" ;;
esac
exec %s "$@"`, realHead))
	t.Cleanup(func() {
		if pid, err := strconv.Atoi(strings.TrimSpace(b.read(filepath.Join(b.root, "holder-pid")))); err == nil {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	b.gateway.set(200, statusJSON(0, 0, 0))
	started := time.Now()
	out, status := b.run("gateway-status.sh")
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("waited %v on a write end the reader left open", elapsed.Round(time.Second))
	}
	if status != 0 {
		t.Fatalf("exit %d output %q", status, out)
	}
}

// R-SEC on PR 17, round 4: the listener's owner controls the answer. A huge
// one is refused, not stored and parsed by root.
// curl's --max-filesize and the script's own size check are two layers: this
// fails only when both are gone.
func TestGatewayStatusRefusesAHugeAnswer(t *testing.T) {
	b := newLifecycleBox(t)
	padding := strings.Repeat("x", 1<<20)
	b.gateway.set(200, `{"ok": true, "turns": {"active": 0, "waitingForUser": 0}, "padding": "`+padding+`"}`)
	out, status := b.run("gateway-status.sh")
	if status != 2 || strings.Contains(out, "xxxx") {
		t.Fatalf("exit %d output %.200q", status, out)
	}
}

// R-SEC on PR 17, round 5: a token file owned by root is refused. The
// gateway never runs as root, and reading as the owner drops nothing then.
func TestGatewayStatusRefusesARootOwnedTokenFile(t *testing.T) {
	b := newLifecycleBox(t)
	realStat, err := exec.LookPath("stat")
	if err != nil {
		t.Skip("stat not on PATH")
	}
	b.fakeProgram("stat", fmt.Sprintf(`case "$*" in *%%u*) out="$(exec %s "$@")" || exit 1; echo "${out%% *} 0" ;; *) exec %s "$@" ;; esac`, realStat, realStat))
	b.write("listener-uid", "0\n")
	out, status := b.run("gateway-status.sh")
	b.gateway.mu.Lock()
	requests := len(b.gateway.requests)
	b.gateway.mu.Unlock()
	if status != 2 || requests != 0 || !strings.Contains(out, "owned by root") {
		t.Fatalf("exit %d, %d requests, output %q", status, requests, out)
	}
}

// R-SEC on PR 17, round 5: an answer of several JSON documents is not one
// status.
func TestGatewayStatusRefusesSeveralDocuments(t *testing.T) {
	b := newLifecycleBox(t)
	b.gateway.set(200, `{"ok": true} {"ok": true, "turns": {"active": 0, "waitingForUser": 0}}`)
	if out, status := b.run("gateway-status.sh"); status != 2 {
		t.Fatalf("exit %d output %q", status, out)
	}
}

// R-TEST on PR 17, round 5: an empty answer is not an idle gateway.
func TestGatewayStatusRefusesAnEmptyAnswer(t *testing.T) {
	b := newLifecycleBox(t)
	b.gateway.set(200, "")
	if out, status := b.run("gateway-status.sh"); status != 2 || !strings.Contains(out, "empty answer") {
		t.Fatalf("exit %d output %q", status, out)
	}
}
