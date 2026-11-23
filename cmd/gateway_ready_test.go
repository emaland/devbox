package cmd

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Tests for the readiness check, the failure notice and the periodic check
// that terraform/devbox/modules/ahp-gateway.nix runs around the gateway, and
// for how systemd treats the unit when the gateway misbehaves.

func gatewayScript(t *testing.T, name string) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "terraform", "devbox", "ahp-gateway", name))
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// stubGatewaySource is a stand-in gateway. STUB_MODE picks its behaviour:
//
//	ok        /healthz 200; a WebSocket upgrade without a token gets 401
//	open      as ok, but the upgrade is accepted (no token check)
//	never     /healthz always 503
//	exit78    exits 78 after a moment, as a refusing gateway does
//	all       as ok, but bound to every address, not loopback only
const stubGatewaySource = `
const http = require("http");
const mode = process.env.STUB_MODE || "ok";
const port = Number(process.env.STUB_PORT);
if (mode === "exit78") { setTimeout(() => process.exit(78), 500); setInterval(() => {}, 1000); }
else {
  const server = http.createServer((req, res) => {
    if (req.url === "/healthz") { res.writeHead(mode === "never" ? 503 : 200); res.end("{}"); return; }
    res.writeHead(404); res.end();
  });
  server.on("upgrade", (req, socket) => {
    if (mode === "open") { socket.end("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"); return; }
    socket.end("HTTP/1.1 401 Unauthorized\r\nContent-Length: 0\r\n\r\n");
  });
  server.listen(port, mode === "all" ? "0.0.0.0" : "127.0.0.1");
}
`

func needNode(t *testing.T) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH")
	}
	return node
}

func writeStubGateway(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stub-gateway.js")
	if err := os.WriteFile(path, []byte(stubGatewaySource), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// startStub runs the stub gateway in the given mode and returns its pid.
func startStub(t *testing.T, mode string, port int) int {
	t.Helper()
	command := exec.Command(needNode(t), writeStubGateway(t))
	command.Env = append(os.Environ(), "STUB_MODE="+mode, "STUB_PORT="+strconv.Itoa(port))
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = command.Wait(); close(done) }()
	t.Cleanup(func() {
		_ = command.Process.Kill()
		<-done
	})
	return command.Process.Pid
}

// runReady runs the readiness check as ExecStartPost would, with MAINPID set
// to mainPID, and returns output, exit status and how long it took.
func runReady(t *testing.T, port, mainPID int, timeout time.Duration, extraEnv ...string) (string, int, time.Duration) {
	t.Helper()
	for _, tool := range []string{"sh", "curl", "ss"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not on PATH", tool)
		}
	}
	command := exec.Command("sh", gatewayScript(t, "ready.sh"))
	command.Env = append(os.Environ(),
		"MAINPID="+strconv.Itoa(mainPID),
		"AHP_GATEWAY_PORT="+strconv.Itoa(port),
		"AHP_GATEWAY_READY_PATH=/healthz",
		"AHP_GATEWAY_READY_TIMEOUT="+strconv.Itoa(int(timeout.Seconds())),
	)
	command.Env = append(command.Env, extraEnv...)
	start := time.Now()
	out, err := command.CombinedOutput()
	elapsed := time.Since(start)
	if err == nil {
		return string(out), 0, elapsed
	}
	if exit, ok := err.(*exec.ExitError); ok {
		return string(out), exit.ExitCode(), elapsed
	}
	t.Fatalf("running the readiness check: %v", err)
	return "", -1, 0
}

func TestReadyCheck(t *testing.T) {
	t.Run("a gateway that answers, is ours, and refuses a tokenless upgrade is ready", func(t *testing.T) {
		port := freePort(t)
		pid := startStub(t, "ok", port)
		out, status, _ := runReady(t, port, pid, 20*time.Second)
		if status != 0 {
			t.Errorf("exit %d:\n%s", status, out)
		}
	})

	t.Run("never ready: fails at the deadline", func(t *testing.T) {
		port := freePort(t)
		pid := startStub(t, "never", port)
		out, status, elapsed := runReady(t, port, pid, 3*time.Second)
		if status == 0 {
			t.Fatalf("a gateway that never answered was called ready:\n%s", out)
		}
		if elapsed < 3*time.Second || elapsed > 15*time.Second {
			t.Errorf("took %s for a 3 s deadline", elapsed)
		}
	})

	t.Run("the gateway exits while we wait: stop at once", func(t *testing.T) {
		port := freePort(t)
		pid := startStub(t, "exit78", port)
		out, status, elapsed := runReady(t, port, pid, 60*time.Second)
		if status == 0 {
			t.Fatalf("called ready:\n%s", out)
		}
		if elapsed > 15*time.Second {
			t.Errorf("waited %s after the gateway had exited", elapsed)
		}
		if !strings.Contains(out, "exited") {
			t.Errorf("the message should say the gateway exited: %s", out)
		}
	})

	t.Run("a slow prepare: the ready deadline starts when prepare has finished", func(t *testing.T) {
		port := freePort(t)
		marker := filepath.Join(t.TempDir(), "prepared")
		// A launcher whose prepare takes longer than the ready timeout.
		node := needNode(t)
		launcher := exec.Command("sh", "-c", `sleep 4; : > "$1"; exec "$2" "$3"`, "launcher", marker, node, writeStubGateway(t))
		launcher.Env = append(os.Environ(), "STUB_MODE=ok", "STUB_PORT="+strconv.Itoa(port))
		if err := launcher.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() { _ = launcher.Wait(); close(done) }()
		t.Cleanup(func() { _ = launcher.Process.Kill(); <-done })
		out, status, _ := runReady(t, port, launcher.Process.Pid, 2*time.Second,
			"AHP_GATEWAY_PREPARED_MARKER="+marker, "AHP_GATEWAY_PREPARE_TIMEOUT=30")
		if status != 0 {
			t.Errorf("a correct gateway behind a slow prepare was failed:\n%s", out)
		}
	})

	t.Run("prepare never finishes: fails at prepare's own bound", func(t *testing.T) {
		port := freePort(t)
		marker := filepath.Join(t.TempDir(), "prepared")
		sleeper := exec.Command("sleep", "60")
		if err := sleeper.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = sleeper.Process.Kill(); _ = sleeper.Wait() })
		out, status, elapsed := runReady(t, port, sleeper.Process.Pid, 2*time.Second,
			"AHP_GATEWAY_PREPARED_MARKER="+marker, "AHP_GATEWAY_PREPARE_TIMEOUT=3")
		if status == 0 {
			t.Fatalf("called ready:\n%s", out)
		}
		if elapsed < 3*time.Second || elapsed > 15*time.Second {
			t.Errorf("took %s for a 3 s prepare bound", elapsed)
		}
		if !strings.Contains(out, "prepare") {
			t.Errorf("the message should say prepare did not finish: %s", out)
		}
	})

	t.Run("the listener belongs to another process", func(t *testing.T) {
		port := freePort(t)
		startStub(t, "ok", port)
		other := startStub(t, "never", freePort(t))
		out, status, elapsed := runReady(t, port, other, 60*time.Second)
		if status == 0 {
			t.Fatalf("called ready although the port belongs to another process:\n%s", out)
		}
		if elapsed > 15*time.Second {
			t.Errorf("a listener that is not ours is a failure at once, not after the deadline (took %s)", elapsed)
		}
	})

	t.Run("a tokenless upgrade is accepted", func(t *testing.T) {
		port := freePort(t)
		pid := startStub(t, "open", port)
		out, status, elapsed := runReady(t, port, pid, 60*time.Second)
		if status == 0 {
			t.Fatalf("an open listener was called ready:\n%s", out)
		}
		if !strings.Contains(out, "token") {
			t.Errorf("the message should say the listener takes connections without a token: %s", out)
		}
		if elapsed > 15*time.Second {
			t.Errorf("took %s", elapsed)
		}
	})

	t.Run("bound beyond loopback", func(t *testing.T) {
		port := freePort(t)
		pid := startStub(t, "all", port)
		out, status, _ := runReady(t, port, pid, 20*time.Second)
		if status == 0 {
			t.Fatalf("a listener on every address was called ready:\n%s", out)
		}
		if !strings.Contains(out, "127.0.0.1") {
			t.Errorf("the message should name the expected address: %s", out)
		}
	})
}

// ---- failure notice ----

type webhook struct {
	server *httptest.Server
	mu     sync.Mutex
	bodies []string
}

func newWebhook(t *testing.T) *webhook {
	t.Helper()
	w := &webhook{}
	w.server = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.mu.Lock()
		w.bodies = append(w.bodies, string(body))
		w.mu.Unlock()
		rw.WriteHeader(200)
	}))
	t.Cleanup(w.server.Close)
	return w
}

func (w *webhook) received() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.bodies...)
}

func runNotice(t *testing.T, log, webhookFile, reason string) (string, int) {
	t.Helper()
	for _, tool := range []string{"sh", "curl", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not on PATH", tool)
		}
	}
	command := exec.Command("sh", gatewayScript(t, "notice.sh"), reason)
	command.Env = append(os.Environ(), "AHP_NOTICE_LOG="+log, "AHP_NOTICE_WEBHOOK_FILE="+webhookFile)
	out, err := command.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	if exit, ok := err.(*exec.ExitError); ok {
		return string(out), exit.ExitCode()
	}
	t.Fatal(err)
	return "", -1
}

func TestFailureNotice(t *testing.T) {
	t.Run("a line in the log and a post to the webhook", func(t *testing.T) {
		hook := newWebhook(t)
		dir := t.TempDir()
		log := filepath.Join(dir, "boot-history")
		secrets := filepath.Join(dir, "secrets")
		content := "OTHER=1\nexport SLACK_WEBHOOK_URL=\"" + hook.server.URL + "/hook\"\n"
		if err := os.WriteFile(secrets, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if out, status := runNotice(t, log, secrets, "the gateway service failed"); status != 0 {
			t.Fatalf("exit %d:\n%s", status, out)
		}
		if got := read(t, log); !strings.Contains(got, "ahp-gateway") || !strings.Contains(got, "the gateway service failed") {
			t.Errorf("log line: %q", got)
		}
		bodies := hook.received()
		if len(bodies) != 1 || !strings.Contains(bodies[0], "the gateway service failed") {
			t.Errorf("webhook got %q", bodies)
		}
	})

	t.Run("no webhook file: the log line is still written", func(t *testing.T) {
		dir := t.TempDir()
		log := filepath.Join(dir, "boot-history")
		if out, status := runNotice(t, log, filepath.Join(dir, "missing"), "reason"); status != 0 {
			t.Fatalf("exit %d:\n%s", status, out)
		}
		if got := read(t, log); !strings.Contains(got, "reason") {
			t.Errorf("log line: %q", got)
		}
	})

	t.Run("the webhook file is read, never run", func(t *testing.T) {
		dir := t.TempDir()
		marker := filepath.Join(dir, "ran")
		secrets := filepath.Join(dir, "secrets")
		content := "touch " + marker + "\nSLACK_WEBHOOK_URL=$(touch " + marker + ")\n"
		if err := os.WriteFile(secrets, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		runNotice(t, filepath.Join(dir, "log"), secrets, "reason")
		if _, err := os.Stat(marker); err == nil {
			t.Error("the notice ran code from the webhook file")
		}
	})

	t.Run("an unreachable webhook does not fail the notice", func(t *testing.T) {
		dir := t.TempDir()
		secrets := filepath.Join(dir, "secrets")
		if err := os.WriteFile(secrets, []byte("SLACK_WEBHOOK_URL=http://127.0.0.1:"+strconv.Itoa(freePort(t))+"/x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		log := filepath.Join(dir, "log")
		if out, status := runNotice(t, log, secrets, "reason"); status != 0 {
			t.Errorf("exit %d:\n%s", status, out)
		}
		if got := read(t, log); !strings.Contains(got, "reason") {
			t.Errorf("log line: %q", got)
		}
	})
}

// ---- periodic check ----

// fakeSystemctl puts a `systemctl` on PATH that reports the given state.
func fakeSystemctl(t *testing.T, active string, restarts int) string {
	t.Helper()
	dir := t.TempDir()
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not on PATH")
	}
	shell, _ = filepath.EvalSymlinks(shell)
	script := fmt.Sprintf("#!%s\nprintf 'ActiveState=%s\\nNRestarts=%d\\n'\n", shell, active, restarts)
	if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// storeBin returns the real directory of a program on PATH (in the Nix
// store on this box), so a test can give a script a PATH with exactly the
// programs the module gives it and nothing else, no sh in particular.
func storeBin(t *testing.T, program string) string {
	t.Helper()
	path, err := exec.LookPath(program)
	if err != nil {
		t.Skipf("%s not on PATH", program)
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(real)
}

// noticeWrapper is an executable stand-in for the module's notice wrapper:
// it runs notice.sh with the tools it needs, whatever PATH it is called with.
func noticeWrapper(t *testing.T) string {
	t.Helper()
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not on PATH")
	}
	shell, _ = filepath.EvalSymlinks(shell)
	wrapper := filepath.Join(t.TempDir(), "notice")
	script := "#!" + shell + "\nPATH=" + os.Getenv("PATH") + " exec " + shell + " " + gatewayScript(t, "notice.sh") + " \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return wrapper
}

func runCheck(t *testing.T, systemctlDir, stateFile, log string) (string, int) {
	t.Helper()
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not on PATH")
	}
	// The check unit's PATH: systemctl, coreutils and sed. No sh.
	path := strings.Join([]string{systemctlDir, storeBin(t, "date"), storeBin(t, "sed")}, ":")
	command := exec.Command(shell, gatewayScript(t, "check.sh"))
	command.Env = append(os.Environ(),
		"PATH="+path,
		"AHP_CHECK_STATE="+stateFile,
		"AHP_CHECK_RESTARTS=3",
		"AHP_NOTICE_SCRIPT="+noticeWrapper(t),
		"AHP_NOTICE_LOG="+log,
		"AHP_NOTICE_WEBHOOK_FILE=",
	)
	out, err := command.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	if exit, ok := err.(*exec.ExitError); ok {
		return string(out), exit.ExitCode()
	}
	t.Fatal(err)
	return "", -1
}

func TestPeriodicCheck(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not on PATH")
	}
	logLines := func(t *testing.T, log string) int {
		data, err := os.ReadFile(log)
		if os.IsNotExist(err) {
			return 0
		}
		if err != nil {
			t.Fatal(err)
		}
		return strings.Count(string(data), "\n")
	}

	t.Run("not running: a notice", func(t *testing.T) {
		dir := t.TempDir()
		log := filepath.Join(dir, "log")
		runCheck(t, fakeSystemctl(t, "failed", 0), filepath.Join(dir, "state"), log)
		if logLines(t, log) != 1 || !strings.Contains(read(t, log), "not running") {
			t.Errorf("log: %q", read(t, log))
		}
	})

	t.Run("still starting: nothing", func(t *testing.T) {
		dir := t.TempDir()
		log := filepath.Join(dir, "log")
		runCheck(t, fakeSystemctl(t, "activating", 0), filepath.Join(dir, "state"), log)
		if n := logLines(t, log); n != 0 {
			t.Errorf("%d notices for a gateway that is still starting", n)
		}
	})

	t.Run("running and quiet: nothing", func(t *testing.T) {
		dir := t.TempDir()
		log := filepath.Join(dir, "log")
		runCheck(t, fakeSystemctl(t, "active", 0), filepath.Join(dir, "state"), log)
		if n := logLines(t, log); n != 0 {
			t.Errorf("%d notices", n)
		}
	})

	t.Run("a slow crash loop: a notice once restarts grow past the threshold", func(t *testing.T) {
		dir := t.TempDir()
		log, state := filepath.Join(dir, "log"), filepath.Join(dir, "state")
		runCheck(t, fakeSystemctl(t, "active", 1), state, log)
		runCheck(t, fakeSystemctl(t, "active", 3), state, log) // +2: under the threshold
		if n := logLines(t, log); n != 0 {
			t.Fatalf("%d notices too early", n)
		}
		runCheck(t, fakeSystemctl(t, "active", 7), state, log) // +4 since the last look
		if logLines(t, log) != 1 || !strings.Contains(read(t, log), "restarted") {
			t.Errorf("log: %q", read(t, log))
		}
		runCheck(t, fakeSystemctl(t, "active", 7), state, log) // quiet again
		if n := logLines(t, log); n != 1 {
			t.Errorf("%d notices; a quiet look must not repeat it", n)
		}
	})
}

// ---- the module wires these in ----

func TestGatewayModuleReadinessAndNotice(t *testing.T) {
	needNix(t)
	var got struct {
		Post      any               `json:"post"`
		OnFailure []string          `json:"onFailure"`
		Env       map[string]string `json:"env"`
		Failed    map[string]any    `json:"failed"`
		FailedReq any               `json:"failedRequires"`
		Check     map[string]any    `json:"check"`
		CheckReq  any               `json:"checkRequires"`
		Service   map[string]any    `json:"service"`
		Timer     map[string]any    `json:"timer"`
		TimerBy   []string          `json:"timerWantedBy"`
	}
	expression := "let s = " + moduleExpression(t, `{ readyPath = "/readyz"; readyTimeoutSec = 30; failureNotice.webhookFile = "/home/alice/.secrets"; }`) + `; c = s.config.systemd; in {
		post = c.services.ahp-gateway.serviceConfig.ExecStartPost;
		service = c.services.ahp-gateway.serviceConfig;
		onFailure = c.services.ahp-gateway.onFailure;
		env = c.services.ahp-gateway.environment;
		failed = c.services.ahp-gateway-failed.serviceConfig;
		failedRequires = c.services.ahp-gateway-failed.unitConfig.RequiresMountsFor or null;
		check = c.services.ahp-gateway-check.serviceConfig;
		checkRequires = c.services.ahp-gateway-check.unitConfig.RequiresMountsFor or null;
		timer = c.timers.ahp-gateway-check.timerConfig;
		timerWantedBy = c.timers.ahp-gateway-check.wantedBy;
	}`
	evalJSON(t, expression, "", &got)

	if post := fmt.Sprint(got.Post); !strings.Contains(post, "ahp-gateway-ready") {
		t.Errorf("ExecStartPost %q, want the readiness check", post)
	}
	if fmt.Sprint(got.OnFailure) != "[ahp-gateway-failed.service]" {
		t.Errorf("onFailure %v", got.OnFailure)
	}
	if got.Env["AHP_GATEWAY_READY_PATH"] != "/readyz" || got.Env["AHP_GATEWAY_READY_TIMEOUT"] != "30" {
		t.Errorf("readiness settings not passed: %v", got.Env)
	}
	if fmt.Sprint(got.Failed["Type"]) != "oneshot" || !strings.Contains(fmt.Sprint(got.Failed["ExecStart"]), "notice") {
		t.Errorf("failure unit %v", got.Failed)
	}
	// Both must work when /home never mounted: that is one of the cases they
	// exist for.
	if got.FailedReq != nil || got.CheckReq != nil {
		t.Errorf("the notice units must not need /home: %v %v", got.FailedReq, got.CheckReq)
	}
	// A notice that hangs (a huge or endless webhook file, a webhook that
	// never answers) must not block the next one.
	for name, unit := range map[string]map[string]any{"failed": got.Failed, "check": got.Check} {
		if fmt.Sprint(unit["TimeoutStartSec"]) == "" || fmt.Sprint(unit["TimeoutStartSec"]) == "<nil>" {
			t.Errorf("%s unit has no start timeout: %v", name, unit)
		}
	}
	// The ready wait starts when prepare has finished, and the start timeout
	// covers both.
	if got.Env["AHP_GATEWAY_PREPARED_MARKER"] != "/run/ahp-gateway/prepared" || got.Env["AHP_GATEWAY_PREPARE_TIMEOUT"] == "" {
		t.Errorf("prepare marker settings: %v", got.Env)
	}
	prepare, _ := strconv.Atoi(got.Env["AHP_GATEWAY_PREPARE_TIMEOUT"])
	if start := int(got.Service["TimeoutStartSec"].(float64)); start < prepare+30 {
		t.Errorf("TimeoutStartSec %d does not cover prepare (%d) plus the ready wait (30)", start, prepare)
	}
	if got.Timer["OnBootSec"] != "5min" || got.Timer["OnUnitActiveSec"] != "1h" {
		t.Errorf("timer %v", got.Timer)
	}
	if fmt.Sprint(got.TimerBy) != "[timers.target]" {
		t.Errorf("timer wantedBy %v", got.TimerBy)
	}
}

// ---- how systemd treats the unit: transient user units ----

// needUserManager skips unless a systemd user manager is reachable.
func needUserManager(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"systemd-run", "systemctl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not on PATH", tool)
		}
	}
	if err := exec.Command("systemctl", "--user", "is-system-running").Run(); err != nil {
		if out, _ := exec.Command("systemctl", "--user", "is-system-running").Output(); !strings.Contains(string(out), "degraded") {
			t.Skip("no systemd user manager here")
		}
	}
}

// transientUnit starts a user unit running the stub gateway with the
// readiness check, under the restart settings the module uses, scaled down,
// and returns its name.
func transientUnit(t *testing.T, mode string, extra ...string) string {
	t.Helper()
	needUserManager(t)
	node := needNode(t)
	port := freePort(t)
	name := fmt.Sprintf("devbox-test-%d-%d.service", os.Getpid(), time.Now().UnixNano())
	// Not --collect: systemd would unload a failed unit at once, and there
	// would be nothing left to inspect. Cleanup stops and resets it instead.
	args := []string{"--user", "--unit=" + name, "--no-block",
		"-p", "Restart=on-failure",
		"-p", "RestartSec=100ms",
		"-p", "RestartPreventExitStatus=78",
		"-p", "StartLimitBurst=3",
		"-p", "StartLimitIntervalSec=120",
		"-p", "TimeoutStartSec=20",
		"-p", "ExecStartPost=/bin/sh " + gatewayScript(t, "ready.sh"),
		"--setenv=PATH=" + os.Getenv("PATH"),
		"--setenv=STUB_MODE=" + mode,
		"--setenv=STUB_PORT=" + strconv.Itoa(port),
		"--setenv=AHP_GATEWAY_PORT=" + strconv.Itoa(port),
		"--setenv=AHP_GATEWAY_READY_PATH=/healthz",
		"--setenv=AHP_GATEWAY_READY_TIMEOUT=2",
	}
	args = append(args, extra...)
	args = append(args, node, writeStubGateway(t))
	if out, err := exec.Command("systemd-run", args...).CombinedOutput(); err != nil {
		t.Fatalf("systemd-run: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("systemctl", "--user", "stop", name).Run()
		_ = exec.Command("systemctl", "--user", "reset-failed", name).Run()
	})
	return name
}

func unitProperties(t *testing.T, name string) map[string]string {
	t.Helper()
	out, err := exec.Command("systemctl", "--user", "show", "-p", "ActiveState,SubState,Result,NRestarts,ExecMainStatus,MainPID", name).Output()
	if err != nil {
		t.Fatalf("systemctl show: %v", err)
	}
	properties := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if key, value, ok := strings.Cut(line, "="); ok {
			properties[key] = value
		}
	}
	return properties
}

// waitFor polls the unit until done says so or the deadline passes.
func waitFor(t *testing.T, name string, deadline time.Duration, done func(map[string]string) bool) map[string]string {
	t.Helper()
	end := time.Now().Add(deadline)
	for {
		properties := unitProperties(t, name)
		if done(properties) {
			return properties
		}
		if time.Now().After(end) {
			t.Fatalf("unit %s did not settle within %s: %v", name, deadline, properties)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestUnitBehaviour(t *testing.T) {
	t.Run("ready: the unit becomes active with the gateway as its main process", func(t *testing.T) {
		name := transientUnit(t, "ok")
		properties := waitFor(t, name, 30*time.Second, func(p map[string]string) bool {
			return p["ActiveState"] == "active" || p["ActiveState"] == "failed"
		})
		if properties["ActiveState"] != "active" || properties["MainPID"] == "0" {
			t.Errorf("unit: %v", properties)
		}
	})

	t.Run("exit 78 during the ready wait is not retried and does not wait for the deadline", func(t *testing.T) {
		start := time.Now()
		name := transientUnit(t, "exit78")
		properties := waitFor(t, name, 30*time.Second, func(p map[string]string) bool {
			return p["ActiveState"] == "failed" || p["ActiveState"] == "inactive"
		})
		if properties["NRestarts"] != "0" || properties["ExecMainStatus"] != "78" {
			t.Errorf("unit: %v", properties)
		}
		if elapsed := time.Since(start); elapsed > 20*time.Second {
			t.Errorf("took %s", elapsed)
		}
	})

	t.Run("never ready: retried up to the start limit, then failed", func(t *testing.T) {
		name := transientUnit(t, "never")
		properties := waitFor(t, name, 60*time.Second, func(p map[string]string) bool {
			return p["ActiveState"] == "failed"
		})
		if properties["Result"] != "start-limit-hit" {
			t.Errorf("want result start-limit-hit, got %v", properties)
		}
		if n, _ := strconv.Atoi(properties["NRestarts"]); n < 2 {
			t.Errorf("want retries up to the limit, got %v", properties)
		}
	})
}
