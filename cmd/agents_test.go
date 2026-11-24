package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Tests for the agents commands and the stop guard (design note 031,
// sections 2.5 and 2.6), with the ssh call faked.

type remoteAnswer struct {
	out    string
	status int
	err    error
}

// fakeRemote answers by instance and command; it records every call, and
// the events of the test (dry run, EC2 call) go in the same list, so order
// can be checked.
type fakeRemote struct {
	answers map[string]remoteAnswer // key: "<id> <command>"
	stopped map[string]bool         // instances that are not running
	events  []string
}

func (f *fakeRemote) install(t *testing.T) {
	previousRemote, previousRunning := runRemote, instanceRunning
	runRemote = func(_ context.Context, id, command string) (string, int, error) {
		f.events = append(f.events, id+" "+command)
		if answer, ok := f.answers[id+" "+command]; ok {
			return answer.out, answer.status, answer.err
		}
		return "", 0, nil
	}
	instanceRunning = func(_ context.Context, id string) (bool, error) { return !f.stopped[id], nil }
	t.Cleanup(func() { runRemote, instanceRunning = previousRemote, previousRunning })
}

func (f *fakeRemote) status(id, json string, status int) {
	f.answers[id+" devbox-agents status --json"] = remoteAnswer{out: json + "\n", status: status}
}

func newFakeRemote(t *testing.T) *fakeRemote {
	f := &fakeRemote{answers: map[string]remoteAnswer{}, stopped: map[string]bool{}}
	f.install(t)
	return f
}

const (
	idleJSON        = `{"state":"idle","sessions":2,"running":0,"waiting":0,"pending":0}`
	busyJSON        = `{"state":"busy","sessions":3,"running":2,"waiting":1,"pending":0}`
	waitingJSON     = `{"state":"waiting","sessions":1,"running":0,"waiting":1,"pending":0}`
	unreachableJSON = `{"state":"unreachable","reason":"HTTP 503"}`
	noneJSON        = `{"state":"none"}`
)

// runGuard runs the guard with recording dry run and EC2 call.
func runGuard(f *fakeRemote, g stopGuard, ids []string, dryRunErr, callErr error) (string, error) {
	var out bytes.Buffer
	g.out = &out
	err := g.run(context.Background(), ids,
		func(context.Context) error { f.events = append(f.events, "dry run"); return dryRunErr },
		func(context.Context) error { f.events = append(f.events, "ec2 call"); return callErr })
	return out.String(), err
}

func (f *fakeRemote) has(event string) bool {
	for _, e := range f.events {
		if e == event {
			return true
		}
	}
	return false
}

func (f *fakeRemote) index(event string) int {
	for i, e := range f.events {
		if e == event {
			return i
		}
	}
	return -1
}

func TestStopGuardGoesAheadWhenNothingIsWorking(t *testing.T) {
	for name, json := range map[string]string{"idle": idleJSON, "waiting": waitingJSON, "no gateway": noneJSON} {
		t.Run(name, func(t *testing.T) {
			f := newFakeRemote(t)
			f.status("i-1", json, 0)
			if _, err := runGuard(f, stopGuard{}, []string{"i-1"}, nil, nil); err != nil {
				t.Fatal(err)
			}
			check, dry, stop, call := f.index("i-1 devbox-agents status --json"), f.index("dry run"), f.index("i-1 "+remoteGatewayStop), f.index("ec2 call")
			if !(check >= 0 && check < dry && dry < stop && stop < call) {
				t.Fatalf("order: %v", f.events)
			}
		})
	}
}

func TestStopGuardRefuses(t *testing.T) {
	rows := []struct {
		name   string
		answer remoteAnswer
		want   string
	}{
		{"busy", remoteAnswer{out: busyJSON, status: 0}, "agents are working (busy: 3 sessions, 2 running, 1 waiting)"},
		{"unreachable", remoteAnswer{out: unreachableJSON, status: 2}, "gateway not answering (HTTP 503)"},
		{"ssh fails", remoteAnswer{err: errors.New("connection refused")}, "cannot read the agents' status: connection refused"},
		{"old box", remoteAnswer{status: 127}, "devbox-agents is not installed"},
		{"garbage", remoteAnswer{out: "hello", status: 0}, "cannot read the agents' status"},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			f := newFakeRemote(t)
			f.answers["i-1 devbox-agents status --json"] = r.answer
			_, err := runGuard(f, stopGuard{}, []string{"i-1"}, nil, nil)
			if err == nil || !strings.Contains(err.Error(), r.want) || !strings.Contains(err.Error(), "--force") {
				t.Fatalf("error %v, want it to contain %q and mention --force", err, r.want)
			}
			if len(f.events) != 1 {
				t.Fatalf("did more than read the status: %v", f.events)
			}

			// --force goes on, still stopping the gateway first.
			f.events = nil
			out, err := runGuard(f, stopGuard{force: true}, []string{"i-1"}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !f.has("i-1 "+remoteGatewayStop) || !f.has("ec2 call") || !strings.Contains(out, "going on (--force)") {
				t.Fatalf("--force: events %v output %q", f.events, out)
			}
		})
	}
}

func TestStopGuardChecksEveryInstanceFirst(t *testing.T) {
	f := newFakeRemote(t)
	f.status("i-1", idleJSON, 0)
	f.status("i-2", busyJSON, 0)
	_, err := runGuard(f, stopGuard{}, []string{"i-1", "i-2"}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "i-2") {
		t.Fatalf("error %v", err)
	}
	for _, e := range f.events {
		if strings.Contains(e, "systemctl") || e == "dry run" || e == "ec2 call" {
			t.Fatalf("touched something although one instance is busy: %v", f.events)
		}
	}
}

func TestStopGuardFailedDryRunTouchesNothing(t *testing.T) {
	f := newFakeRemote(t)
	f.status("i-1", idleJSON, 0)
	_, err := runGuard(f, stopGuard{}, []string{"i-1"}, errors.New("UnauthorizedOperation"), nil)
	if err == nil || f.has("i-1 "+remoteGatewayStop) || f.has("ec2 call") {
		t.Fatalf("error %v events %v", err, f.events)
	}
}

func TestStopGuardFailedEC2CallStartsTheGatewaysAgain(t *testing.T) {
	f := newFakeRemote(t)
	f.status("i-1", idleJSON, 0)
	f.status("i-2", idleJSON, 0)
	_, err := runGuard(f, stopGuard{}, []string{"i-1", "i-2"}, nil, errors.New("throttled"))
	if err == nil {
		t.Fatal("no error")
	}
	if f.index("i-1 "+remoteGatewayStart) < f.index("ec2 call") || !f.has("i-2 "+remoteGatewayStart) {
		t.Fatalf("gateways not started again after the failed call: %v", f.events)
	}
}

func TestStopGuardFailedGatewayStop(t *testing.T) {
	f := newFakeRemote(t)
	f.status("i-1", idleJSON, 0)
	f.status("i-2", idleJSON, 0)
	f.answers["i-2 "+remoteGatewayStop] = remoteAnswer{status: 1}
	_, err := runGuard(f, stopGuard{}, []string{"i-1", "i-2"}, nil, nil)
	if err == nil || f.has("ec2 call") {
		t.Fatalf("went on after a failed gateway stop: %v %v", err, f.events)
	}
	if !f.has("i-1 " + remoteGatewayStart) {
		t.Fatalf("the gateway already stopped was not started again: %v", f.events)
	}
	// R-CODE: the stop that failed may still be under way on the box (an
	// ssh drop, the timeout), so that gateway is started again too.
	if !f.has("i-2 " + remoteGatewayStart) {
		t.Fatalf("the gateway whose stop failed was not started again: %v", f.events)
	}

	f.events = nil
	if _, err := runGuard(f, stopGuard{force: true}, []string{"i-1", "i-2"}, nil, nil); err != nil || !f.has("ec2 call") {
		t.Fatalf("--force: %v %v", err, f.events)
	}
}

// R-CODE: a stopped instance has no gateway; the guard neither checks it
// nor refuses (restart brings a stopped box back; down is a no-op).
func TestStopGuardSkipsInstancesThatAreNotRunning(t *testing.T) {
	f := newFakeRemote(t)
	f.stopped["i-1"] = true
	f.status("i-2", idleJSON, 0)
	if _, err := runGuard(f, stopGuard{}, []string{"i-1", "i-2"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	for _, e := range f.events {
		if strings.HasPrefix(e, "i-1 ") {
			t.Fatalf("talked to a stopped instance: %v", f.events)
		}
	}
	if !f.has("i-2 "+remoteGatewayStop) || !f.has("ec2 call") {
		t.Fatalf("events %v", f.events)
	}
}

func TestStopGuardHardSkipsTheBox(t *testing.T) {
	f := newFakeRemote(t)
	if _, err := runGuard(f, stopGuard{hard: true}, []string{"i-1"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Join(f.events, ",") != "dry run,ec2 call" {
		t.Fatalf("--hard talked to the box: %v", f.events)
	}
}

func TestRemoteGatewayCommandsAreSafeWithoutAGateway(t *testing.T) {
	for _, command := range []string{remoteGatewayStop, remoteGatewayStart} {
		if !strings.HasPrefix(command, "if systemctl cat ahp-gateway.service >/dev/null 2>&1; then ") {
			t.Errorf("%q fails on a box without the gateway unit", command)
		}
	}
	if !strings.Contains(remoteGatewayStop, "timeout") {
		t.Error("the remote gateway stop is not bounded")
	}
}

func TestDownFlags(t *testing.T) {
	down := newDownCmd()
	for _, name := range []string{"force", "hard", "when-idle", "after"} {
		if down.Flags().Lookup(name) == nil {
			t.Errorf("devbox down has no --%s", name)
		}
	}
	if usage := down.Flags().Lookup("force").Usage; strings.Contains(usage, "OS shutdown") {
		t.Errorf("--force still describes the EC2 force stop: %q", usage)
	}
	if usage := down.Flags().Lookup("hard").Usage; !strings.Contains(usage, "EC2 force stop") {
		t.Errorf("--hard: %q", usage)
	}
	down.SetArgs([]string{"--force", "--hard", "i-1"})
	down.SetOut(&bytes.Buffer{})
	down.SetErr(&bytes.Buffer{})
	if err := down.Execute(); err == nil || !strings.Contains(err.Error(), "none of the others can be") {
		t.Errorf("--force with --hard accepted: %v", err)
	}
	restart := newRestartCmd()
	if restart.Flags().Lookup("hard") == nil || restart.Flags().Lookup("force") == nil {
		t.Error("devbox restart needs --force and --hard")
	}
	for _, command := range []string{"reboot", "terminate", "resize"} {
		var flags = map[string]func() bool{
			"reboot":    func() bool { return newRebootCmd().Flags().Lookup("force") != nil },
			"terminate": func() bool { return newTerminateCmd().Flags().Lookup("force") != nil },
			"resize":    func() bool { return newResizeCmd().Flags().Lookup("force") != nil },
		}
		if !flags[command]() {
			t.Errorf("devbox %s has no --force", command)
		}
	}
}

func TestStopWhenIdleAsksTheBox(t *testing.T) {
	f := newFakeRemote(t)
	f.answers["i-1 devbox-agents stop-when-idle"] = remoteAnswer{out: "ok\n"}
	if err := stopWhenIdle(context.Background(), []string{"i-1"}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(f.events, ",") != "i-1 devbox-agents stop-when-idle" {
		t.Fatalf("events %v", f.events)
	}
	f.answers["i-1 devbox-agents stop-when-idle"] = remoteAnswer{status: 127}
	if err := stopWhenIdle(context.Background(), []string{"i-1"}); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("old box: %v", err)
	}
}

func TestAutostopAfterCommandQuotes(t *testing.T) {
	got := autostopAfterCommand("4h'; touch /tmp/x; '")
	if !strings.Contains(got, `'4h'\''; touch /tmp/x; '\'''`) {
		t.Fatalf("not quoted: %s", got)
	}
	if strings.Contains(autostopAfterCommand("4h"), "schedule-autostop") {
		t.Fatal("still restarts a unit that does not exist")
	}
	if got := autostopAfterCommand("4h\ntouch /tmp/x"); !strings.Contains(got, "'4h\ntouch /tmp/x'") {
		t.Fatalf("a newline is not quoted: %s", got)
	}
}

// R-CODE and R-SEC: a duration the box would ignore is refused before ssh.
func TestDownAfterValidatesTheDuration(t *testing.T) {
	for value, ok := range map[string]bool{
		"4h": true, "90m": true, "45min": true, "30s": true, "2d": true, "3": true, "off": true,
		"4h30m": false, "2hours": false, "1.5h": false, "": false, "4h\n": false, "-1h": false,
	} {
		if got := autostopDurationPattern.MatchString(value); got != ok {
			t.Errorf("%q: accepted %v, want %v", value, got, ok)
		}
	}
	down := newDownCmd()
	down.SetArgs([]string{"--after", "2hours", "i-1"})
	down.SetOut(&bytes.Buffer{})
	down.SetErr(&bytes.Buffer{})
	if err := down.Execute(); err == nil || !strings.Contains(err.Error(), "the box ignores anything else") {
		t.Fatalf("--after 2hours: %v", err)
	}
}

func TestResizeResumeAndForceAreExclusive(t *testing.T) {
	resize := newResizeCmd()
	resize.SetArgs([]string{"--resume", "--force"})
	resize.SetOut(&bytes.Buffer{})
	resize.SetErr(&bytes.Buffer{})
	if err := resize.Execute(); err == nil || !strings.Contains(err.Error(), "none of the others can be") {
		t.Fatalf("--resume --force accepted: %v", err)
	}
}

func TestRemoteStatusError(t *testing.T) {
	for status, want := range map[int]string{0: "", 2: "not answering", 3: "no gateway", 4: "does not offer", 127: "not installed", 9: "exited 9"} {
		err := remoteStatusError("i-1", status)
		if (want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), want)) {
			t.Errorf("status %d: %v", status, err)
		}
	}
}

func TestEditorConfigNamesTheTokenFileNotTheToken(t *testing.T) {
	text := editorConfig(18765, "/ahp", "/home/u/.config/devbox/gateway-token")
	for _, want := range []string{
		`"address": "127.0.0.1:18765/ahp"`,
		`"chat.remoteAgentHosts"`,
		"ws://127.0.0.1:18765/ahp",
		"Token file: /home/u/.config/devbox/gateway-token",
		// R-SEC: what the user must know before using it.
		"settings.sync.ignoredSettings",
		"the token is the only barrier",
		"does not auto-stop",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("editor config lacks %s:\n%s", want, text)
		}
	}
	// No command line of another (private) program in this public repo:
	// the text names the URL and the token file, never flags.
	if strings.Contains(text, " --") || strings.Contains(text, `"--`) {
		t.Errorf("editor config prints command-line flags:\n%s", text)
	}
}

// A token-shaped test value, made at run time so no literal in the repo
// looks like a credential to the secret scan.
var testGatewayToken = strings.Repeat("test-token_", 3)

func TestCopyGatewayToken(t *testing.T) {
	f := newFakeRemote(t)
	dir := filepath.Join(t.TempDir(), "devbox")
	local := filepath.Join(dir, "gateway-token")
	remote := "i-1 cat -- '/home/u/.ahp-acp-gateway/token'"
	f.answers[remote] = remoteAnswer{out: testGatewayToken + "\n"}
	if err := copyGatewayToken(context.Background(), "i-1", "/home/u/.ahp-acp-gateway/token", local); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(local)
	if err != nil || string(data) != testGatewayToken+"\n" {
		t.Fatalf("token file %q %v", data, err)
	}
	if got := mode(t, local); got != 0o600 {
		t.Fatalf("token file mode %o", got)
	}
	// R-SEC: a planted link at the old fixed temporary name is not followed,
	// and no temporary file is left behind.
	elsewhere := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.Symlink(elsewhere, local+".tmp"); err != nil {
		t.Fatal(err)
	}
	if err := copyGatewayToken(context.Background(), "i-1", "/home/u/.ahp-acp-gateway/token", local); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(elsewhere); !os.IsNotExist(err) {
		t.Fatal("the token was written through a planted link")
	}
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".gateway-token-") {
			t.Fatalf("temporary file left behind: %s", entry.Name())
		}
	}
	for _, bad := range []remoteAnswer{{out: ""}, {out: "two words\n"}, {out: "x", status: 1}, {out: "short\n"}, {out: testGatewayToken + "\x1b[2J\n"}} {
		f.answers[remote] = bad
		if err := copyGatewayToken(context.Background(), "i-1", "/home/u/.ahp-acp-gateway/token", local); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func TestReadGatewayClientConfig(t *testing.T) {
	f := newFakeRemote(t)
	f.answers["i-1 cat /etc/ahp-gateway/config.json"] = remoteAnswer{out: `{"managedBy":"system","port":8765,"tokenFile":"/home/u/.ahp-acp-gateway/token"}`}
	got, err := readGatewayClientConfig(context.Background(), "i-1")
	if err != nil || got.Port != 8765 || got.Path != "/ahp" {
		t.Fatalf("%+v %v", got, err)
	}
	f.answers["i-1 cat /etc/ahp-gateway/config.json"] = remoteAnswer{status: 1}
	if _, err := readGatewayClientConfig(context.Background(), "i-1"); err == nil || !strings.Contains(err.Error(), "no gateway") {
		t.Fatalf("missing config: %v", err)
	}
	// R-SEC: values printed into the editor settings are checked.
	for name, config := range map[string]string{
		"port out of range": `{"port":70000,"tokenFile":"/t"}`,
		"relative token":    `{"port":8765,"tokenFile":"t"}`,
		"quote in path":     `{"port":8765,"tokenFile":"/t","path":"/ahp\""}`,
		"no leading slash":  `{"port":8765,"tokenFile":"/t","path":"ahp"}`,
	} {
		f.answers["i-1 cat /etc/ahp-gateway/config.json"] = remoteAnswer{out: config}
		if _, err := readGatewayClientConfig(context.Background(), "i-1"); err == nil {
			t.Errorf("%s: accepted %s", name, config)
		}
	}
}

func TestAgentStatusString(t *testing.T) {
	three := 3
	for s, want := range map[agentStatus]string{
		{State: "busy", Sessions: &three, Running: 2, Waiting: 1}: "busy: 3 sessions, 2 running, 1 waiting",
		{State: "idle", Pending: 1}:                               "idle: ? sessions, 0 running, 0 waiting, 1 continuations pending",
		{State: "idle", Interrupted: 2}:                           "idle: ? sessions, 0 running, 0 waiting, 2 interrupted",
		{State: "unreachable", Reason: "HTTP 401"}:                "gateway not answering (HTTP 401)",
		{State: "none"}: "no gateway",
	} {
		if got := s.String(); got != want {
			t.Errorf("%+v: %q, want %q", s, got, want)
		}
	}
}

// D24: the gateway has no resume switch. `devbox agents resume` says so and
// contacts nothing, rather than taking "resume" for an instance id.
func TestAgentsResumeSaysThereIsNoSwitch(t *testing.T) {
	for _, args := range [][]string{{"resume"}, {"resume", "on"}, {"resume", "off", "i-x"}, {"resume", "status"}} {
		f := newFakeRemote(t)
		agents := newAgentsCmd()
		var out bytes.Buffer
		agents.SetOut(&out)
		agents.SetErr(&out)
		agents.SetArgs(args)
		agents.SilenceUsage = true
		err := agents.Execute()
		// Asking to turn on what cannot happen fails, so a script notices.
		if wantErr := len(args) > 1 && args[1] == "on"; (err != nil) != wantErr {
			t.Fatalf("%v: error %v, want an error: %v", args, err, wantErr)
		}
		if !strings.Contains(out.String(), "Try Again") {
			t.Fatalf("%v: output %q", args, out.String())
		}
		if len(f.events) != 0 {
			t.Fatalf("%v: contacted the box: %v", args, f.events)
		}
	}
}
