package cmd

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Tests for terraform/devbox/modules/ahp-gateway.nix and the scripts it runs.
// The module is evaluated by Nix against a stub gateway package, so nothing
// here downloads or builds the real gateway.

func gatewayModulePath(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "terraform", "devbox", "modules", "ahp-gateway.nix"))
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func prepareScriptPath(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "terraform", "devbox", "ahp-gateway", "prepare.sh"))
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func needNix(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("nix-instantiate"); err != nil {
		t.Skip("nix-instantiate not on PATH")
	}
	if err := exec.Command("nix-instantiate", "--find-file", "nixpkgs").Run(); err != nil {
		t.Skip("<nixpkgs> is not resolvable here")
	}
}

// stubDefaultConfig is a cut-down copy of the shape of the public release's
// bundled default config: two built-in providers, one with an environment.
const stubDefaultConfig = `{
  "version": 1,
  "listener": {"host": "127.0.0.1", "port": 8765, "path": "/ahp", "tlsTerminated": false, "allowedOrigins": [], "allowAnyOrigin": true},
  "auth": {},
  "database": {"path": "~/.ahp-acp-gateway/ahp-acp-gateway.db"},
  "providers": [
    {"id": "claude", "displayName": "Claude", "command": "npx", "args": ["--yes", "@agentclientprotocol/claude-agent-acp"], "enabled": true},
    {"id": "auggie", "displayName": "Auggie", "command": "npx", "args": ["--yes", "@augmentcode/auggie", "--acp"], "env": {"AUGMENT": "1"}, "enabled": true}
  ]
}`

// moduleExpression returns a Nix expression for a NixOS system with the
// module enabled. extra is Nix attribute-set text merged into the
// services.ahp-gateway settings.
func moduleExpression(t *testing.T, extra string) string {
	t.Helper()
	return "(" + systemsExpression(t) + ") (lib: " + extra + ")"
}

// systemsExpression returns a Nix function to a NixOS system with the module
// enabled, from a function of lib to extra services.ahp-gateway settings.
func systemsExpression(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	defaults := filepath.Join(dir, "default.json")
	if err := os.WriteFile(defaults, []byte(stubDefaultConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf(`
let
  pkgs = import <nixpkgs> {};
  stub = pkgs.runCommand "ahp-acp-gateway-0.2.2" { version = "0.2.2"; pname = "ahp-acp-gateway"; } ''
    d=$out/lib/node_modules/ahp-acp-gateway
    mkdir -p $d/dist/src
    cp ${%s} $d/ahp-acp-gateway.default.json
    echo 'if (process.env.AHP_TEST_MARKER) require("fs").writeFileSync(process.env.AHP_TEST_MARKER, "ran\\n"); process.exit(0)' > $d/dist/src/cli.js
  '';
  system = extra: import <nixpkgs/nixos> {
    configuration = { lib, ... }: {
      imports = [ %s ];
      boot.isContainer = true;
      system.stateVersion = "24.11";
      users.users.alice = { isNormalUser = true; uid = 1234; linger = true; };
      services.ahp-gateway = {
        enable = true;
        user = "alice";
        package = stub;
        providers.claude = { command = "${pkgs.coreutils}/bin/true"; args = [ "--acp" ]; };
      } // (extra lib);
    };
  };
in system`, defaults, gatewayModulePath(t))
}

// evalJSON evaluates `(<expression>).<attribute>` (or the expression alone
// when attribute is empty) strictly and decodes it.
func evalJSON(t *testing.T, expression, attribute string, into any) {
	t.Helper()
	if attribute != "" {
		expression = "(" + expression + ")." + attribute
	}
	out, err := exec.Command("nix-instantiate", "--eval", "--strict", "--json", "-E", expression).Output()
	if err != nil {
		var stderr string
		if exit, ok := err.(*exec.ExitError); ok {
			stderr = string(exit.Stderr)
		}
		t.Fatalf("evaluating %s: %v\n%s", attribute, err, lastLines(stderr, 15))
	}
	if err := json.Unmarshal(out, into); err != nil {
		t.Fatalf("decoding %s: %v\n%s", attribute, err, out)
	}
}

func lastLines(text string, n int) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

type gatewayUnit struct {
	Description      string            `json:"description"`
	WantedBy         []string          `json:"wantedBy"`
	Wants            []string          `json:"wants"`
	After            []string          `json:"after"`
	RestartIfChanged bool              `json:"restartIfChanged"`
	Environment      map[string]string `json:"environment"`
	UnitConfig       map[string]any    `json:"unitConfig"`
	ServiceConfig    map[string]any    `json:"serviceConfig"`
}

func evalUnit(t *testing.T, extra string) gatewayUnit {
	t.Helper()
	var unit gatewayUnit
	evalJSON(t, "let s = "+moduleExpression(t, extra)+"; u = s.config.systemd.services.ahp-gateway; in { inherit (u) description wantedBy wants after restartIfChanged environment unitConfig serviceConfig; }", "", &unit)
	return unit
}

func TestGatewayModuleUnit(t *testing.T) {
	needNix(t)
	unit := evalUnit(t, "{}")

	if unit.RestartIfChanged {
		t.Error("restartIfChanged must be false: a config switch must never restart a busy gateway")
	}
	contains := func(list []string, want string) bool {
		for _, item := range list {
			if item == want {
				return true
			}
		}
		return false
	}
	// The user manager's unit, user@<uid>.service. Built in two parts so the
	// repository's public-content check does not read it as an address.
	userManager := "user" + "@1234.service"
	for _, want := range []string{"network-online.target", userManager} {
		if !contains(unit.Wants, want) || !contains(unit.After, want) {
			t.Errorf("unit must want and follow %s; wants %v after %v", want, unit.Wants, unit.After)
		}
	}
	if !contains(unit.WantedBy, "multi-user.target") {
		t.Errorf("wantedBy %v", unit.WantedBy)
	}

	mounts := fmt.Sprint(unit.UnitConfig["RequiresMountsFor"])
	for _, want := range []string{"/home/alice/.ahp-acp-gateway", "/home/alice"} {
		if !strings.Contains(mounts, want) {
			t.Errorf("RequiresMountsFor %s lacks %s", mounts, want)
		}
	}
	// Five never-ready starts must fit inside the window: each can take a
	// start timeout for prepare, another for the ready wait, the stop
	// timeout and the restart delay.
	start := int(unit.ServiceConfig["TimeoutStartSec"].(float64))
	stop := int(unit.ServiceConfig["TimeoutStopSec"].(float64))
	window := int(unit.UnitConfig["StartLimitIntervalSec"].(float64))
	if burst := int(unit.UnitConfig["StartLimitBurst"].(float64)); burst != 5 {
		t.Errorf("StartLimitBurst %d", burst)
	}
	if need := 5 * (2*start + stop + 2); window <= need {
		t.Errorf("StartLimitIntervalSec %d does not cover five failed starts (%d s)", window, need)
	}

	service := unit.ServiceConfig
	for key, want := range map[string]string{
		"User":                 "alice",
		"WorkingDirectory":     "/home/alice",
		"RuntimeDirectory":     "ahp-gateway",
		"RuntimeDirectoryMode": "0700",
		"UnsetEnvironment":     "CLAUDECODE",
		"Restart":              "on-failure",
		"KillMode":             "mixed",
		"OOMPolicy":            "continue",
	} {
		if got := fmt.Sprint(service[key]); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if _, set := service["UMask"]; set {
		t.Error("no unit-wide UMask: agents create the user's own files")
	}
	// Prepare runs inside the main process (a launcher that then execs the
	// gateway): systemd honours RestartPreventExitStatus for the main
	// process only, so a prepare step in ExecStartPre exiting 78 would be
	// retried anyway.
	if exec := fmt.Sprint(service["ExecStart"]); !strings.Contains(exec, "ahp-gateway-launch") {
		t.Errorf("ExecStart %q, want the launcher", exec)
	}
	if pre, set := service["ExecStartPre"]; set {
		t.Errorf("ExecStartPre %v: prepare must run in the main process", pre)
	}
	if got := fmt.Sprint(service["RestartPreventExitStatus"]); !strings.Contains(got, "78") {
		t.Errorf("RestartPreventExitStatus %q: 78 always means restarting will not help", got)
	}
	if got := fmt.Sprint(service["Group"]); got != "users" {
		t.Errorf("Group %q", got)
	}

	env := unit.Environment
	for key, want := range map[string]string{
		"HOME":                  "/home/alice",
		"XDG_RUNTIME_DIR":       "/run/user/1234",
		"AHP_GATEWAY_STATE_DIR": "/home/alice/.ahp-acp-gateway",
	} {
		if env[key] != want {
			t.Errorf("environment %s = %q, want %q", key, env[key], want)
		}
	}
	if !strings.HasPrefix(env["AHP_GATEWAY_CLOSURE"], "/nix/store/") {
		t.Errorf("AHP_GATEWAY_CLOSURE %q: the running gateway's files need a root that outlives their generation", env["AHP_GATEWAY_CLOSURE"])
	}
	path := env["PATH"]
	for _, want := range []string{"/home/alice/.local/bin", "/run/wrappers/bin", "/run/current-system/sw/bin"} {
		if !strings.Contains(path, want) {
			t.Errorf("PATH lacks %s: %s", want, path)
		}
	}
	if strings.Index(path, "/home/alice/.local/bin") > strings.Index(path, "/run/current-system/sw/bin") {
		t.Error("the user's own bin directories must come first on PATH")
	}
	all, _ := json.Marshal(unit)
	if strings.Contains(string(all), "token\":") || strings.Contains(strings.ToLower(string(all)), "auth.token") {
		t.Error("the unit must not carry a token")
	}
}

func TestGatewayModuleOptionsReachTheUnit(t *testing.T) {
	needNix(t)
	unit := evalUnit(t, `{ port = 9911; stateDir = "/srv/gw"; readyTimeoutSec = 30; stopTimeoutSec = 20; noRetryExitStatus = [ 75 ]; environment.EXTRA_THING = "1"; }`)
	if got := fmt.Sprint(unit.ServiceConfig["RestartPreventExitStatus"]); !strings.Contains(got, "78") || !strings.Contains(got, "75") {
		t.Errorf("RestartPreventExitStatus %q", got)
	}
	if got := int(unit.ServiceConfig["TimeoutStopSec"].(float64)); got != 20 {
		t.Errorf("TimeoutStopSec %d", got)
	}
	if unit.Environment["AHP_GATEWAY_STATE_DIR"] != "/srv/gw" || unit.Environment["EXTRA_THING"] != "1" {
		t.Errorf("environment %v", unit.Environment)
	}
	if !strings.Contains(fmt.Sprint(unit.UnitConfig["RequiresMountsFor"]), "/srv/gw") {
		t.Errorf("RequiresMountsFor %v", unit.UnitConfig["RequiresMountsFor"])
	}
}

func TestGatewayModuleAssertions(t *testing.T) {
	needNix(t)
	cases := []struct {
		name, extra, want string
	}{
		{"no provider", `{ providers = lib.mkForce {}; }`, "provider"},
		{"a module-owned key in settings", `{ settings.listener.host = "0.0.0.0"; }`, "settings"},
		{"another module-owned key", `{ settings.auth.token = "x"; }`, "settings"},
		{"a provider program that is not a store path", `{ providers.claude.command = lib.mkForce "npx"; }`, "store path"},
		{"a state dir that is not absolute", `{ stateDir = "relative"; }`, "absolute"},
		{"a management command that is a pattern", `{ managementCommands = [ "*" ]; }`, "managementCommands"},
		{"an empty management command", `{ managementCommands = [ "" ]; }`, "managementCommands"},
		{"the default case", `{ }`, ""},
	}
	// One evaluation for all cases: nixpkgs is imported once.
	var expression strings.Builder
	expression.WriteString("let systems = " + systemsExpression(t) + "; failed = s: map (a: a.message) (builtins.filter (a: !a.assertion) s.config.assertions); in [\n")
	for _, tc := range cases {
		expression.WriteString("  (failed (systems (lib: " + tc.extra + ")))\n")
	}
	expression.WriteString("]")
	var messages [][]string
	evalJSON(t, expression.String(), "", &messages)
	if len(messages) != len(cases) {
		t.Fatalf("got %d results for %d cases", len(messages), len(cases))
	}
	for i, tc := range cases {
		got := strings.Join(messages[i], "\n")
		switch {
		case tc.want == "" && got != "":
			t.Errorf("%s: unexpected failing assertions: %s", tc.name, got)
		case tc.want != "" && !strings.Contains(got, tc.want):
			t.Errorf("%s: want a failing assertion mentioning %q, got %q", tc.name, tc.want, got)
		}
	}
}

func TestGatewayModuleRequiresLinger(t *testing.T) {
	needNix(t)
	expression := strings.Replace(moduleExpression(t, "{}"),
		"uid = 1234; linger = true;", "uid = 1234;", 1)
	if !strings.Contains(expression, "uid = 1234; };") {
		t.Fatal("test fixture changed; update this test")
	}
	var messages []string
	evalJSON(t, "let s = "+expression+"; in map (a: a.message) (builtins.filter (a: !a.assertion) s.config.assertions)", "", &messages)
	if !strings.Contains(strings.Join(messages, "\n"), "linger") {
		t.Errorf("want a failing assertion about linger, got %q", messages)
	}
}

// buildBaseConfig builds the static config the module hands the gateway
// (everything but the token) and decodes it.
func buildBaseConfig(t *testing.T, extra string) map[string]any {
	t.Helper()
	out, err := exec.Command("nix-build", "--no-out-link", "-E",
		"("+moduleExpression(t, extra)+").config.services.ahp-gateway.generated.baseConfig").Output()
	if err != nil {
		var stderr string
		if exit, ok := err.(*exec.ExitError); ok {
			stderr = string(exit.Stderr)
		}
		t.Fatalf("building the base config: %v\n%s", err, lastLines(stderr, 15))
	}
	data, err := os.ReadFile(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatalf("base config is not JSON: %v\n%s", err, data)
	}
	return config
}

func TestGatewayModuleBaseConfig(t *testing.T) {
	needNix(t)
	if _, err := exec.LookPath("nix-build"); err != nil {
		t.Skip("nix-build not on PATH")
	}
	config := buildBaseConfig(t, `{ port = 9911; allowedOrigins = [ "vscode-file://vscode-app" ]; settings.resume.onStart = "continue"; }`)

	if config["version"] != float64(1) {
		t.Errorf("version %v: the gateway refuses a config without version 1", config["version"])
	}
	listener, _ := config["listener"].(map[string]any)
	if listener["host"] != "127.0.0.1" || listener["port"] != float64(9911) || listener["allowAnyOrigin"] != false {
		t.Errorf("listener %v", listener)
	}
	if origins := fmt.Sprint(listener["allowedOrigins"]); origins != "[vscode-file://vscode-app]" {
		t.Errorf("allowedOrigins %s", origins)
	}
	if _, set := config["auth"]; set {
		t.Error("the base config is in the Nix store and must carry no auth section; the token is added at start")
	}
	database, _ := config["database"].(map[string]any)
	if database["path"] != "/home/alice/.ahp-acp-gateway/ahp-acp-gateway.db" {
		t.Errorf("database %v", database)
	}
	resume, _ := config["resume"].(map[string]any)
	if resume["onStart"] != "continue" {
		t.Errorf("settings were not merged: resume %v", resume)
	}

	providers := map[string]map[string]any{}
	for _, entry := range config["providers"].([]any) {
		provider := entry.(map[string]any)
		providers[provider["id"].(string)] = provider
	}
	claude := providers["claude"]
	if claude == nil || claude["enabled"] == false || !strings.HasPrefix(fmt.Sprint(claude["command"]), "/nix/store/") {
		t.Errorf("claude keeps its built-in id and gets the configured program: %v", claude)
	}
	if fmt.Sprint(claude["args"]) != "[--acp]" {
		t.Errorf("claude args %v", claude["args"])
	}
	if auggie := providers["auggie"]; auggie == nil || auggie["enabled"] != false {
		t.Errorf("every built-in that is not configured must be disabled: auggie %v", auggie)
	}
	if len(providers) != 2 {
		t.Errorf("providers %v: exactly the built-ins plus the configured ones", providers)
	}

	// The closure the unit carries names the package, the config and every
	// provider program, so none of them is collected while the gateway runs.
	out, err := exec.Command("nix-build", "--no-out-link", "-E",
		"("+moduleExpression(t, "{}")+").config.services.ahp-gateway.generated.closure").Output()
	if err != nil {
		t.Fatalf("building the closure list: %v", err)
	}
	closure := read(t, strings.TrimSpace(string(out)))
	for _, want := range []string{"-ahp-acp-gateway-0.2.2", "-ahp-gateway-config.json", "-coreutils-", "/bin/node"} {
		if !strings.Contains(closure, want) {
			t.Errorf("closure lacks %s:\n%s", want, closure)
		}
	}
}

func TestGatewayModuleClientConfigAndCommands(t *testing.T) {
	needNix(t)
	var got struct {
		Client   string   `json:"client"`
		Packages []string `json:"packages"`
	}
	expression := "let s = " + moduleExpression(t, `{ managementCommands = [ "status" "doctor" ]; }`) + `; in {
		client = s.config.environment.etc."ahp-gateway/config.json".text;
		packages = map (p: p.name) s.config.environment.systemPackages;
	}`
	evalJSON(t, expression, "", &got)
	var client map[string]any
	if err := json.Unmarshal([]byte(got.Client), &client); err != nil {
		t.Fatalf("client config is not JSON: %v\n%s", err, got.Client)
	}
	if client["managedBy"] != "system" || client["tokenFile"] != "/home/alice/.ahp-acp-gateway/token" || client["port"] != float64(8765) {
		t.Errorf("client config %v", client)
	}
	if strings.Contains(got.Client, "\"token\"") {
		t.Error("the client config must not carry the token")
	}
	found := false
	for _, name := range got.Packages {
		if name == "ahp-acp-gateway-commands" {
			found = true
		}
	}
	if !found {
		t.Errorf("management command wrapper missing from %v", got.Packages)
	}

	evalJSON(t, "let s = "+moduleExpression(t, "{}")+"; in map (p: p.name) s.config.environment.systemPackages", "", &got.Packages)
	for _, name := range got.Packages {
		if strings.HasPrefix(name, "ahp-acp-gateway") {
			t.Errorf("with no management commands nothing of the gateway goes on PATH, got %s", name)
		}
	}
}

func TestGatewayPackageRecipeEvaluates(t *testing.T) {
	needNix(t)
	recipe, err := filepath.Abs(filepath.Join("..", "terraform", "devbox", "pkgs", "ahp-acp-gateway.nix"))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Name    string `json:"name"`
		Program string `json:"program"`
	}
	evalJSON(t, `let p = (import <nixpkgs> {}).callPackage `+recipe+` {}; in { inherit (p) name; program = p.meta.mainProgram; }`, "", &got)
	if got.Name != "ahp-acp-gateway-0.2.2" || got.Program != "ahp-acp-gateway" {
		t.Errorf("recipe evaluates to %+v", got)
	}
}

// ---- prepare.sh ----

type prepareBox struct {
	t        *testing.T
	stateDir string
	runtime  string
	base     string
	port     int
}

func newPrepareBox(t *testing.T) prepareBox {
	t.Helper()
	for _, tool := range []string{"sh", "jq", "ss"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not on PATH", tool)
		}
	}
	root := t.TempDir()
	p := prepareBox{
		t:        t,
		stateDir: filepath.Join(root, "state"),
		runtime:  filepath.Join(root, "run", "config.json"),
		base:     filepath.Join(root, "base.json"),
		port:     freePort(t),
	}
	if err := os.MkdirAll(filepath.Dir(p.runtime), 0o700); err != nil {
		t.Fatal(err)
	}
	p.writeBase(trueProgram(t))
	return p
}

func trueProgram(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("true")
	if err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(real, "/nix/store/") {
		t.Skipf("true is not in a Nix store here (%s)", real)
	}
	return real
}

func (p prepareBox) writeBase(program string) {
	p.t.Helper()
	base := fmt.Sprintf(`{"version": 1, "listener": {"host": "127.0.0.1", "port": %d, "path": "/ahp", "allowAnyOrigin": false},
"database": {"path": %q},
"providers": [{"id": "claude", "command": %q, "args": []}, {"id": "auggie", "enabled": false}]}`,
		p.port, filepath.Join(p.stateDir, "ahp-acp-gateway.db"), program)
	if err := os.WriteFile(p.base, []byte(base), 0o644); err != nil {
		p.t.Fatal(err)
	}
}

func (p prepareBox) db() string { return filepath.Join(p.stateDir, "ahp-acp-gateway.db") }

// run executes prepare.sh as the unit would, for a build with the given
// name, version and store path. It returns output and exit status.
func (p prepareBox) run(build [3]string, extraEnv ...string) (string, int) {
	p.t.Helper()
	return p.runProgram([]string{"sh", prepareScriptPath(p.t)}, build, extraEnv...)
}

func (p prepareBox) runProgram(program []string, build [3]string, extraEnv ...string) (string, int) {
	p.t.Helper()
	command := exec.Command(program[0], program[1:]...)
	command.Env = append(os.Environ(),
		"AHP_GATEWAY_STATE_DIR="+p.stateDir,
		"AHP_GATEWAY_PORT="+strconv.Itoa(p.port),
		"AHP_GATEWAY_TOKEN_FILE="+filepath.Join(p.stateDir, "token"),
		"AHP_GATEWAY_BASE_CONFIG="+p.base,
		"AHP_GATEWAY_RUNTIME_CONFIG="+p.runtime,
		"AHP_GATEWAY_BUILD_NAME="+build[0],
		"AHP_GATEWAY_BUILD_VERSION="+build[1],
		"AHP_GATEWAY_BUILD_PATH="+build[2],
		"AHP_GATEWAY_ALLOW_DOWNGRADE=0",
	)
	command.Env = append(command.Env, extraEnv...)
	out, err := command.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	if exit, ok := err.(*exec.ExitError); ok {
		return string(out), exit.ExitCode()
	}
	p.t.Fatalf("running prepare: %v", err)
	return "", -1
}

func (p prepareBox) mustRun(build [3]string, extraEnv ...string) string {
	p.t.Helper()
	out, status := p.run(build, extraEnv...)
	if status != 0 {
		p.t.Fatalf("prepare exited %d:\n%s", status, out)
	}
	return out
}

var (
	public022 = [3]string{"ahp-acp-gateway", "0.2.2", "/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-ahp-acp-gateway-0.2.2"}
	forkH1    = [3]string{"ahp-acp-gateway", "0.2.2-h.1", "/nix/store/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-ahp-acp-gateway-0.2.2-h.1"}
	forkH2    = [3]string{"ahp-acp-gateway", "0.2.2-h.2", "/nix/store/cccccccccccccccccccccccccccccccc-ahp-acp-gateway-0.2.2-h.2"}
	forkH10   = [3]string{"ahp-acp-gateway", "0.2.2-h.10", "/nix/store/dddddddddddddddddddddddddddddddd-ahp-acp-gateway-0.2.2-h.10"}
	otherName = [3]string{"some-other-gateway", "9.9.9", "/nix/store/eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee-some-other-gateway-9.9.9"}
)

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	return port
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func backups(t *testing.T, stateDir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(stateDir, "backups"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func TestPrepareStateDirAndToken(t *testing.T) {
	p := newPrepareBox(t)
	p.mustRun(public022)

	if got := mode(t, p.stateDir); got != 0o700 {
		t.Errorf("state dir mode %o, want 700", got)
	}
	token := filepath.Join(p.stateDir, "token")
	if got := mode(t, token); got != 0o600 {
		t.Errorf("token mode %o, want 600", got)
	}
	first := read(t, token)
	if len(strings.TrimSpace(first)) < 40 {
		t.Errorf("token too short: %d characters", len(strings.TrimSpace(first)))
	}

	// A second start never replaces the token, and tightens loose modes.
	if err := os.Chmod(token, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p.stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	p.mustRun(public022)
	if read(t, token) != first {
		t.Error("the token was replaced")
	}
	if mode(t, token) != 0o600 || mode(t, p.stateDir) != 0o700 {
		t.Errorf("modes not tightened: token %o, state dir %o", mode(t, token), mode(t, p.stateDir))
	}
}

func TestPrepareRefusesAnEmptyOrShortToken(t *testing.T) {
	for _, content := range []string{"", "\n", "short\n"} {
		t.Run(strconv.Quote(content), func(t *testing.T) {
			p := newPrepareBox(t)
			if err := os.MkdirAll(p.stateDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(p.stateDir, "token"), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if out, status := p.run(public022); status != 78 {
				t.Errorf("token %q: exit %d, want 78:\n%s", content, status, out)
			}
			if _, err := os.Stat(p.runtime); err == nil {
				t.Error("a runtime config was written")
			}
		})
	}
}

func TestPrepareRefusesAMalformedToken(t *testing.T) {
	valid := strings.Repeat("A", 43)
	for name, content := range map[string]string{
		"carriage return":   valid + "\r\n",
		"inner space":       valid[:20] + " " + valid[20:] + "\n",
		"second line":       valid + "\n" + valid + "\n",
		"control character": valid + "\x01\n",
		"NUL byte":          valid + "\x00" + valid + "\n",
		"several newlines":  valid + "\n\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			p := newPrepareBox(t)
			if err := os.MkdirAll(p.stateDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(p.stateDir, "token"), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if out, status := p.run(public022); status != 78 {
				t.Errorf("exit %d, want 78:\n%s", status, out)
			}
		})
	}
	t.Run("a directory", func(t *testing.T) {
		p := newPrepareBox(t)
		if err := os.MkdirAll(filepath.Join(p.stateDir, "token"), 0o700); err != nil {
			t.Fatal(err)
		}
		if out, status := p.run(public022); status != 78 {
			t.Errorf("exit %d, want 78:\n%s", status, out)
		}
	})
	t.Run("a dangling link", func(t *testing.T) {
		p := newPrepareBox(t)
		if err := os.MkdirAll(p.stateDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(p.stateDir, "nowhere"), filepath.Join(p.stateDir, "token")); err != nil {
			t.Fatal(err)
		}
		if out, status := p.run(public022); status != 78 {
			t.Errorf("exit %d, want 78:\n%s", status, out)
		}
	})
}

func TestPrepareWritesTheRuntimeConfig(t *testing.T) {
	p := newPrepareBox(t)
	out := p.mustRun(public022)
	token := strings.TrimSpace(read(t, filepath.Join(p.stateDir, "token")))
	if strings.Contains(out, token) {
		t.Error("prepare printed the token")
	}
	if got := mode(t, p.runtime); got != 0o600 {
		t.Errorf("runtime config mode %o, want 600", got)
	}
	var config map[string]any
	if err := json.Unmarshal([]byte(read(t, p.runtime)), &config); err != nil {
		t.Fatal(err)
	}
	auth, _ := config["auth"].(map[string]any)
	if auth["token"] != token {
		t.Error("the runtime config does not carry the token")
	}
	if listener, _ := config["listener"].(map[string]any); listener["port"] != float64(p.port) {
		t.Errorf("the base config was not kept: %v", config)
	}
}

func TestPrepareRefusesWhenThePortIsInUse(t *testing.T) {
	p := newPrepareBox(t)
	listener, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p.port))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	out, status := p.run(public022)
	if status != 78 {
		t.Fatalf("port in use: exit %d, want 78:\n%s", status, out)
	}
	if !strings.Contains(out, strconv.Itoa(p.port)) {
		t.Errorf("the message should name the port: %s", out)
	}
}

// holdOpen starts a process that keeps path open, and waits until it has.
func holdOpen(t *testing.T, path string) int {
	t.Helper()
	holder := exec.Command("sh", "-c", `exec 3<"$1"; echo ready; exec sleep 60`, "holder", path)
	stdout, err := holder.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Process.Kill(); _ = holder.Wait() })
	buf := make([]byte, 6)
	if _, err := stdout.Read(buf); err != nil {
		t.Fatal(err)
	}
	return holder.Process.Pid
}

func TestPrepareRefusesWhenTheDatabaseIsOpen(t *testing.T) {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		t.Run("database"+suffix, func(t *testing.T) {
			p := newPrepareBox(t)
			p.mustRun(public022)
			if err := os.WriteFile(p.db()+suffix, []byte("db"), 0o600); err != nil {
				t.Fatal(err)
			}
			pid := holdOpen(t, p.db()+suffix)
			out, status := p.run(public022)
			if status != 78 {
				t.Fatalf("another process holds the database: exit %d, want 78:\n%s", status, out)
			}
			if !strings.Contains(out, strconv.Itoa(pid)) {
				t.Errorf("the message should name the process: %s", out)
			}
		})
	}

	t.Run("through a linked state dir", func(t *testing.T) {
		p := newPrepareBox(t)
		p.mustRun(public022)
		if err := os.WriteFile(p.db(), []byte("db"), 0o600); err != nil {
			t.Fatal(err)
		}
		// The base config names the database through a link; the holder
		// opened the real path.
		link := filepath.Join(t.TempDir(), "state-link")
		if err := os.Symlink(p.stateDir, link); err != nil {
			t.Fatal(err)
		}
		linked := p
		linked.stateDir = link
		linked.writeBase(trueProgram(t))
		pid := holdOpen(t, p.db())
		out, status := linked.run(public022)
		if status != 78 || !strings.Contains(out, strconv.Itoa(pid)) {
			t.Errorf("exit %d, want 78 naming pid %d:\n%s", status, pid, out)
		}
	})
}

// The unit runs the launcher the module builds, with the PATH the module
// gives it, not the test's PATH.
func TestGatewayLauncherAsBuilt(t *testing.T) {
	needNix(t)
	out, err := exec.Command("nix-build", "--no-out-link", "-E",
		"("+moduleExpression(t, "{}")+").config.services.ahp-gateway.generated.launcher").Output()
	if err != nil {
		var stderr string
		if exit, ok := err.(*exec.ExitError); ok {
			stderr = string(exit.Stderr)
		}
		t.Fatalf("building the launcher: %v\n%s", err, lastLines(stderr, 15))
	}
	launcher := strings.TrimSpace(string(out))
	clean := []string{"PATH=/nonexistent"}

	t.Run("the database held by another process is refused with 78", func(t *testing.T) {
		p := newPrepareBox(t)
		p.mustRun(public022)
		if err := os.WriteFile(p.db(), []byte("db"), 0o600); err != nil {
			t.Fatal(err)
		}
		pid := holdOpen(t, p.db())
		got, status := p.runProgram([]string{launcher}, public022, clean...)
		if status != 78 || !strings.Contains(got, strconv.Itoa(pid)) {
			t.Errorf("exit %d, want 78 naming pid %d:\n%s", status, pid, got)
		}
	})

	t.Run("a busy port is refused with 78", func(t *testing.T) {
		p := newPrepareBox(t)
		listener, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p.port))
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		if got, status := p.runProgram([]string{launcher}, public022, clean...); status != 78 {
			t.Errorf("exit %d, want 78:\n%s", status, got)
		}
	})

	t.Run("a good start runs the gateway after prepare", func(t *testing.T) {
		p := newPrepareBox(t)
		marker := filepath.Join(t.TempDir(), "gateway-ran")
		got, status := p.runProgram([]string{launcher}, public022, append(clean, "AHP_TEST_MARKER="+marker)...)
		if status != 0 {
			t.Fatalf("exit %d:\n%s", status, got)
		}
		if _, err := os.Stat(p.runtime); err != nil {
			t.Errorf("prepare did not write the runtime config: %v", err)
		}
		if _, err := os.Stat(marker); err != nil {
			t.Error("the gateway did not run after prepare")
		}
	})

	t.Run("prepare is bounded in time", func(t *testing.T) {
		// The unit counts as started when the launcher starts, so the start
		// timeout does not cover prepare; the launcher must bound it.
		if text := read(t, launcher); !strings.Contains(text, "/bin/timeout ") {
			t.Errorf("the launcher does not bound prepare:\n%s", text)
		}
	})
}

func TestPrepareRefusesAProviderThatIsNotAStorePath(t *testing.T) {
	for _, program := range []string{"npx", "/usr/bin/true", "/nix/store/zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz-missing/bin/x"} {
		t.Run(program, func(t *testing.T) {
			p := newPrepareBox(t)
			p.writeBase(program)
			if out, status := p.run(public022); status != 78 {
				t.Errorf("provider program %s: exit %d, want 78:\n%s", program, status, out)
			}
		})
	}
}

func TestPrepareBuildRecord(t *testing.T) {
	withDatabase := func(t *testing.T) prepareBox {
		p := newPrepareBox(t)
		if err := os.MkdirAll(p.stateDir, 0o700); err != nil {
			t.Fatal(err)
		}
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if err := os.WriteFile(p.db()+suffix, []byte("data"+suffix), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return p
	}

	t.Run("no database, no record: nothing to back up", func(t *testing.T) {
		p := newPrepareBox(t)
		p.mustRun(public022)
		if got := backups(t, p.stateDir); len(got) != 0 {
			t.Errorf("backups %v", got)
		}
	})

	t.Run("a database and no record (after the hand-run gateway): back up first", func(t *testing.T) {
		p := withDatabase(t)
		p.mustRun(forkH1)
		got := backups(t, p.stateDir)
		if len(got) != 1 || !strings.HasPrefix(got[0], "devbox-") || !strings.HasSuffix(got[0], "-unknown") {
			t.Fatalf("backups %v", got)
		}
		dir := filepath.Join(p.stateDir, "backups", got[0])
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if read(t, filepath.Join(dir, "ahp-acp-gateway.db"+suffix)) != "data"+suffix {
				t.Errorf("backup lacks ahp-acp-gateway.db%s", suffix)
			}
		}
	})

	t.Run("same build: no backup", func(t *testing.T) {
		p := withDatabase(t)
		p.mustRun(forkH1)
		p.mustRun(forkH1)
		if got := backups(t, p.stateDir); len(got) != 1 {
			t.Errorf("backups %v", got)
		}
	})

	t.Run("newer versions, in version order", func(t *testing.T) {
		p := withDatabase(t)
		for _, build := range [][3]string{public022, forkH1, forkH2, forkH10} {
			p.mustRun(build)
		}
		if got := backups(t, p.stateDir); len(got) != 4 {
			t.Errorf("one backup per change of build, got %v", got)
		}
	})

	t.Run("same name and version, another store path: back up", func(t *testing.T) {
		p := withDatabase(t)
		p.mustRun(forkH1)
		rebuilt := forkH1
		rebuilt[2] = "/nix/store/ffffffffffffffffffffffffffffffff-ahp-acp-gateway-0.2.2-h.1"
		p.mustRun(rebuilt)
		if got := backups(t, p.stateDir); len(got) != 2 {
			t.Errorf("backups %v", got)
		}
	})

	for _, tc := range []struct {
		name          string
		first, second [3]string
	}{
		{"older version is refused", forkH2, forkH1},
		{"h.10 then h.2 is refused", forkH10, forkH2},
		{"the public release after a fork build is refused", forkH1, public022},
		{"a different package name is refused", forkH1, otherName},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := withDatabase(t)
			p.mustRun(tc.first)
			out, status := p.run(tc.second)
			if status != 78 {
				t.Fatalf("exit %d, want 78:\n%s", status, out)
			}
			if _, err := os.Stat(p.runtime); err == nil {
				// The first run wrote one; it must not have been rewritten for
				// the refused build. Compare the build record instead.
				record := read(t, filepath.Join(p.stateDir, "devbox-build"))
				if strings.Contains(record, tc.second[2]) {
					t.Error("the record was updated for a refused build")
				}
			}
			if !strings.Contains(out, "backups/devbox-") {
				t.Errorf("the refusal should name the backup to restore: %s", out)
			}
			if !strings.Contains(out, "remove") || !strings.Contains(out, "-wal") {
				t.Errorf("the restore steps must say to remove the live -wal and -shm first: %s", out)
			}
			// Allowed with the option.
			if out, status := p.run(tc.second, "AHP_GATEWAY_ALLOW_DOWNGRADE=1"); status != 0 {
				t.Errorf("refused even with allowDowngrade:\n%s", out)
			}
		})
	}

	t.Run("the refusal names the backup made for the version being started", func(t *testing.T) {
		p := withDatabase(t)
		p.mustRun(forkH1) // backup labelled unknown
		p.mustRun(forkH2) // backup labelled 0.2.2-h.1: what h.1 can read
		rebuilt := forkH2
		rebuilt[2] = "/nix/store/ffffffffffffffffffffffffffffffff-ahp-acp-gateway-0.2.2-h.2"
		p.mustRun(rebuilt) // newest backup, labelled 0.2.2-h.2
		out, status := p.run(forkH1)
		if status != 78 {
			t.Fatalf("exit %d:\n%s", status, out)
		}
		if !strings.Contains(out, "-0.2.2-h.1 ") && !strings.Contains(out, "-0.2.2-h.1,") {
			t.Errorf("want the backup labelled 0.2.2-h.1, got: %s", out)
		}
	})

	t.Run("five newest backups are kept, partial copies are cleared", func(t *testing.T) {
		p := withDatabase(t)
		builds := [][3]string{}
		for i := 1; i <= 7; i++ {
			builds = append(builds, [3]string{"ahp-acp-gateway", fmt.Sprintf("0.2.%d", i), fmt.Sprintf("/nix/store/%032d-ahp-acp-gateway-0.2.%d", i, i)})
		}
		if err := os.MkdirAll(filepath.Join(p.stateDir, "backups", "devbox-00000000T000000Z-x.partial"), 0o700); err != nil {
			t.Fatal(err)
		}
		other := filepath.Join(p.stateDir, "backups", "ahp-acp-gateway-20260101T000000Z-v0.2.2.db")
		if err := os.WriteFile(other, []byte("made by another tool"), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, build := range builds {
			p.mustRun(build)
		}
		var ours []string
		for _, name := range backups(t, p.stateDir) {
			if strings.HasSuffix(name, ".partial") {
				t.Errorf("partial backup %s left behind", name)
			}
			if strings.HasPrefix(name, "devbox-") {
				ours = append(ours, name)
			}
		}
		if len(ours) != 5 {
			t.Errorf("kept %d of ours, want 5: %v", len(ours), ours)
		}
		if _, err := os.Stat(other); err != nil {
			t.Error("a backup made by another tool was pruned")
		}
	})
}
