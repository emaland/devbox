package cmd

import (
	"encoding/json"
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
)

// A harness for the scripts in terraform/devbox/lifecycle (design note 031).
// The scripts take every program from PATH. Here who, ss, date, aws and
// systemctl are fakes that record their calls; curl, jq and the rest are
// real, and talk to local HTTP servers standing in for the gateway's status
// endpoint, the instance metadata service and the webhook.

const lifecycleToken = "test-token-abcdefghijklmnopqrstuvwxyz0123456789_-"

func lifecycleScript(t *testing.T, name string) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "terraform", "devbox", "lifecycle", name))
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// fakeGateway answers /status with whatever the test sets, and only to the
// right token.
type fakeGateway struct {
	mu       sync.Mutex
	code     int
	body     string
	sequence []string // bodies for the next requests, in order; then body stays
	requests []string // every request line and header, for leak checks
	authOK   int
	authBad  int
	server   *httptest.Server
}

func newFakeGateway(t *testing.T) *fakeGateway {
	g := &fakeGateway{code: 200, body: statusJSON(0, 0, 0)}
	g.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		var record strings.Builder
		fmt.Fprintf(&record, "%s %s\n", r.Method, r.URL.RequestURI())
		for name, values := range r.Header {
			fmt.Fprintf(&record, "%s: %s\n", name, strings.Join(values, ","))
		}
		g.requests = append(g.requests, record.String())
		if r.URL.Path != "/status" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+lifecycleToken || r.URL.RawQuery != "" {
			g.authBad++
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		g.authOK++
		if len(g.sequence) > 0 {
			g.body, g.sequence = g.sequence[0], g.sequence[1:]
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(g.code)
		io.WriteString(w, g.body)
	}))
	t.Cleanup(g.server.Close)
	return g
}

func (g *fakeGateway) set(code int, body string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.code, g.body = code, body
}

func (g *fakeGateway) port() int {
	_, port, _ := strings.Cut(strings.TrimPrefix(g.server.URL, "http://"), ":")
	n, _ := strconv.Atoi(port)
	return n
}

// statusJSON is a status answer in the gateway's shape.
func statusJSON(active, waiting, pending int) string {
	return fmt.Sprintf(`{"ok": true, "version": "0.2.2-h.3", "sessions": {"total": 3},
"turns": {"active": %d, "waitingForUser": %d, "oldestActiveStartedAt": null, "lastActivityAt": null},
"resume": {"policy": "continue", "source": "config", "maxAttemptsPerTurn": 1, "pending": %d, "resumedSinceStart": 0, "failed": 0, "skipped": 0}}`,
		active, waiting, pending)
}

// fakeIMDS is an IMDSv2 service: a session token from PUT, then reads that
// must carry it.
type fakeIMDS struct {
	mu             sync.Mutex
	tokens         map[string]bool
	issued         int
	instanceAction string // "" answers 404
	rebalance      string
	server         *httptest.Server
}

func newFakeIMDS(t *testing.T) *fakeIMDS {
	m := &fakeIMDS{tokens: map[string]bool{}}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if r.URL.Path == "/latest/api/token" {
			if r.Method != http.MethodPut || r.Header.Get("X-aws-ec2-metadata-token-ttl-seconds") == "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			m.issued++
			token := fmt.Sprintf("imds-session-%d", m.issued)
			m.tokens[token] = true
			io.WriteString(w, token)
			return
		}
		if !m.tokens[r.Header.Get("X-aws-ec2-metadata-token")] {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		answer := map[string]string{
			"/latest/meta-data/instance-id":                      "i-0fake",
			"/latest/meta-data/placement/availability-zone":      "test-region-1a",
			"/latest/meta-data/spot/instance-action":             m.instanceAction,
			"/latest/meta-data/events/recommendations/rebalance": m.rebalance,
		}[r.URL.Path]
		if answer == "" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, answer)
	}))
	t.Cleanup(m.server.Close)
	return m
}

// expireTokens makes every issued session token invalid, as at expiry.
func (m *fakeIMDS) expireTokens() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tokens = map[string]bool{}
}

func (m *fakeIMDS) setNotices(instanceAction, rebalance string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.instanceAction, m.rebalance = instanceAction, rebalance
}

func (m *fakeIMDS) tokensIssued() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.issued
}

// fakeWebhook records what is posted to it.
type fakeWebhook struct {
	mu     sync.Mutex
	texts  []string
	server *httptest.Server
}

func newFakeWebhook(t *testing.T) *fakeWebhook {
	h := &fakeWebhook{}
	h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Text string `json:"text"`
		}
		json.NewDecoder(r.Body).Decode(&payload)
		h.mu.Lock()
		h.texts = append(h.texts, payload.Text)
		h.mu.Unlock()
	}))
	t.Cleanup(h.server.Close)
	return h
}

func (h *fakeWebhook) posts() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.texts...)
}

type lifecycleBox struct {
	t       *testing.T
	root    string
	bin     string
	state   string
	logFile string
	gateway *fakeGateway
	imds    *fakeIMDS
	hook    *fakeWebhook
	env     []string
}

const fakePrograms = `
record() { printf '%s\n' "$*" >>"$FAKE_ROOT/calls"; }
`

func newLifecycleBox(t *testing.T) *lifecycleBox {
	t.Helper()
	for _, tool := range []string{"sh", "curl", "jq", "sed", "timeout", "mktemp"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not on PATH", tool)
		}
	}
	realDate, err := exec.LookPath("date")
	if err != nil {
		t.Skip("date not on PATH")
	}
	root := t.TempDir()
	b := &lifecycleBox{
		t:       t,
		root:    root,
		bin:     filepath.Join(root, "bin"),
		state:   filepath.Join(root, "state"),
		logFile: filepath.Join(root, "boot-history"),
		gateway: newFakeGateway(t),
		imds:    newFakeIMDS(t),
		hook:    newFakeWebhook(t),
	}
	if err := os.MkdirAll(b.bin, 0o755); err != nil {
		t.Fatal(err)
	}

	fakes := map[string]string{
		"who": `cat "$FAKE_ROOT/who" 2>/dev/null; exit 0`,
		// The listener query (-l) answers with the fake gateway's port,
		// owned by $FAKE_ROOT/listener-uid or else by this user.
		"ss": `case "$*" in
  *-Htlne*)
    uid="$(cat "$FAKE_ROOT/listener-uid" 2>/dev/null || id -u)"
    [ -f "$FAKE_ROOT/no-listener" ] || echo "LISTEN 0 4096 127.0.0.1:$FAKE_PORT 0.0.0.0:* uid:$uid ino:1 sk:1 cgroup:/x <->"
    exit 0 ;;
esac
record ss "$@"; cat "$FAKE_ROOT/ss" 2>/dev/null; exit 0`,
		// The clock is a file. A call that parses a given time (-d) is the
		// real date's.
		"date": fmt.Sprintf(`now="$(cat "$FAKE_ROOT/now")"
for argument in "$@"; do
  case "$argument" in -d | --date=*) exec %[1]s "$@" ;; esac
done
case "${1:-}" in
  +%%s) echo "$now" ;;
  *) exec %[1]s -d "@$now" "$@" ;;
esac`, realDate),
		"aws": `record aws "$@"; exit "$(cat "$FAKE_ROOT/aws-exit" 2>/dev/null || echo 0)"`,
		"systemctl": `record systemctl "$@"
case "$1" in
  is-active) [ "$(cat "$FAKE_ROOT/gateway" 2>/dev/null)" = active ] ;;
  stop)
    code="$(cat "$FAKE_ROOT/stop-exit" 2>/dev/null || echo 0)"
    [ "$code" = 0 ] && echo inactive >"$FAKE_ROOT/gateway"
    exit "$code" ;;
  start) echo active >"$FAKE_ROOT/gateway" ;;
esac`,
	}
	for name, body := range fakes {
		script := "#!/bin/sh\n" + fakePrograms + body + "\n"
		if err := os.WriteFile(filepath.Join(b.bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	tokenFile := filepath.Join(root, "token")
	b.write("token", lifecycleToken+"\n")
	if err := os.Chmod(tokenFile, 0o600); err != nil {
		t.Fatal(err)
	}
	b.write("gateway-config.json", fmt.Sprintf(`{"managedBy": "system", "port": %d, "tokenFile": %q, "path": "/ahp"}`, b.gateway.port(), tokenFile))
	b.write("webhook", "export SLACK_WEBHOOK_URL="+b.hook.server.URL+"/hook\n")
	b.write("gateway", "active\n")
	b.setNow(1_700_000_000)

	b.env = append(os.Environ(),
		"PATH="+b.bin+":"+os.Getenv("PATH"),
		"FAKE_ROOT="+root,
		"FAKE_PORT="+strconv.Itoa(b.gateway.port()),
		"DEVBOX_LIFECYCLE_LIB="+filepath.Dir(lifecycleScript(t, "autostop.sh")),
		"DEVBOX_LOG="+b.logFile,
		"DEVBOX_AUTOSTOP_STATE="+b.state,
		"DEVBOX_AUTOSTOP_WINDOW_FILE="+filepath.Join(root, "autostop-after"),
		"DEVBOX_AUTOSTOP_CAP_FILE="+filepath.Join(root, "autostop-busy-cap"),
		"DEVBOX_GATEWAY_CONFIG="+filepath.Join(root, "gateway-config.json"),
		"DEVBOX_GATEWAY_STOP_TIMEOUT=10",
		"DEVBOX_WEBHOOK_FILE="+filepath.Join(root, "webhook"),
		"DEVBOX_IMDS="+b.imds.server.URL,
		"DEVBOX_SPOT_STATE="+filepath.Join(root, "spot"),
		"DEVBOX_PROC_STAT="+filepath.Join(root, "proc-stat"),
	)
	// Booted ten minutes before the fake clock's start.
	b.write("proc-stat", "cpu  1 2 3\nbtime 1699999400\nprocesses 10\n")
	return b
}

func (b *lifecycleBox) write(name, content string) {
	b.t.Helper()
	if err := os.WriteFile(filepath.Join(b.root, name), []byte(content), 0o644); err != nil {
		b.t.Fatal(err)
	}
}

func (b *lifecycleBox) remove(name string) {
	b.t.Helper()
	if err := os.Remove(filepath.Join(b.root, name)); err != nil && !os.IsNotExist(err) {
		b.t.Fatal(err)
	}
}

func (b *lifecycleBox) read(path string) string {
	b.t.Helper()
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		b.t.Fatal(err)
	}
	return string(data)
}

func (b *lifecycleBox) setNow(epoch int64) { b.write("now", strconv.FormatInt(epoch, 10)+"\n") }

func (b *lifecycleBox) advance(seconds int64) {
	b.t.Helper()
	now, err := strconv.ParseInt(strings.TrimSpace(b.read(filepath.Join(b.root, "now"))), 10, 64)
	if err != nil {
		b.t.Fatal(err)
	}
	b.setNow(now + seconds)
}

// run executes a lifecycle script and returns its output and exit status.
func (b *lifecycleBox) run(script string, args ...string) (string, int) {
	b.t.Helper()
	command := exec.Command("sh", append([]string{lifecycleScript(b.t, script)}, args...)...)
	command.Env = b.env
	out, err := command.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	if exit, ok := err.(*exec.ExitError); ok {
		return string(out), exit.ExitCode()
	}
	b.t.Fatalf("running %s: %v", script, err)
	return "", -1
}

func (b *lifecycleBox) calls() []string {
	text := strings.TrimSpace(b.read(filepath.Join(b.root, "calls")))
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

func (b *lifecycleBox) clearCalls() { b.remove("calls") }

func (b *lifecycleBox) logLines() []string {
	text := strings.TrimSpace(b.read(b.logFile))
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

// noTokenLeak fails when the gateway token shows up anywhere but the
// Authorization header: fake program arguments, the log, the notices, or a
// request line.
func (b *lifecycleBox) noTokenLeak(output string) {
	b.t.Helper()
	places := map[string]string{
		"calls":   strings.Join(b.calls(), "\n"),
		"log":     b.read(b.logFile),
		"notices": strings.Join(b.hook.posts(), "\n"),
		"output":  output,
	}
	b.gateway.mu.Lock()
	for _, request := range b.gateway.requests {
		first, _, _ := strings.Cut(request, "\n")
		places["request line"] += first + "\n"
	}
	b.gateway.mu.Unlock()
	for place, text := range places {
		if strings.Contains(text, lifecycleToken) {
			b.t.Errorf("the gateway token appears in the %s", place)
		}
	}
}
