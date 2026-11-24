package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Evaluation tests for terraform/devbox/modules/devbox-lifecycle.nix (design
// note 031), together with the gateway module it configures.

func lifecycleModulePath(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "terraform", "devbox", "modules", "devbox-lifecycle.nix"))
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// lifecycleSystem is a NixOS system with both modules. gateway and lifecycle
// are Nix attribute-set text merged into each module's settings.
func lifecycleSystem(t *testing.T, gateway, lifecycle string) string {
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
    echo 'process.exit(0)' > $d/dist/src/cli.js
  '';
in import <nixpkgs/nixos> {
  configuration = { lib, ... }: {
    imports = [ %s %s ];
    boot.isContainer = true;
    system.stateVersion = "24.11";
    users.users.alice = { isNormalUser = true; uid = 1234; linger = true; };
    services.ahp-gateway = {
      enable = true;
      user = "alice";
      package = stub;
      providers.claude = { command = "${pkgs.coreutils}/bin/true"; };
    } // (%s);
    services.devbox-lifecycle = { enable = true; } // (%s);
  };
}`, defaults, gatewayModulePath(t), lifecycleModulePath(t), gateway, lifecycle)
}

func TestLifecycleModuleUnits(t *testing.T) {
	needNix(t)
	var got struct {
		Autostop gatewayUnit       `json:"autostop"`
		Timer    map[string]string `json:"timer"`
		Spot     gatewayUnit       `json:"spot"`
		Gateway  gatewayUnit       `json:"gateway"`
		Packages []string          `json:"packages"`
		Settings map[string]any    `json:"settings"`
		Agents   string            `json:"agents"`
	}
	evalJSON(t, "let s = "+lifecycleSystem(t, "{}", "{}")+`; c = s.config; in {
  autostop = { inherit (c.systemd.services.devbox-autostop) description wantedBy wants after restartIfChanged environment unitConfig serviceConfig; };
  timer = c.systemd.timers.devbox-autostop.timerConfig;
  spot = { inherit (c.systemd.services.devbox-spot-watch) description wantedBy wants after restartIfChanged environment unitConfig serviceConfig; };
  gateway = { inherit (c.systemd.services.ahp-gateway) description wantedBy wants after restartIfChanged environment unitConfig serviceConfig; };
  packages = map (p: p.name) c.environment.systemPackages;
  settings = c.services.ahp-gateway.settings;
  agents = c.services.devbox-lifecycle.generated.devboxAgents.text;
}`, "", &got)

	if got.Timer["OnUnitActiveSec"] != "10min" || got.Timer["Unit"] != "devbox-autostop.service" {
		t.Errorf("auto-stop timer: %v", got.Timer)
	}
	if got.Autostop.Environment["DEVBOX_AUTOSTOP_DEFAULT_WINDOW"] != "4h" || got.Autostop.Environment["DEVBOX_AUTOSTOP_DEFAULT_CAP"] != "12h" {
		t.Errorf("auto-stop defaults: %v", got.Autostop.Environment)
	}
	// The gateway's stop bound (45 s) plus 15 s.
	for name, unit := range map[string]gatewayUnit{"auto-stop": got.Autostop, "spot watcher": got.Spot} {
		if unit.Environment["DEVBOX_GATEWAY_STOP_TIMEOUT"] != "60" || unit.Environment["DEVBOX_GATEWAY_UNIT"] != "ahp-gateway.service" {
			t.Errorf("%s gateway settings: %v", name, unit.Environment)
		}
		if !strings.HasPrefix(unit.Environment["DEVBOX_LIFECYCLE_LIB"], "/nix/store/") {
			t.Errorf("%s scripts are not in the store: %v", name, unit.Environment["DEVBOX_LIFECYCLE_LIB"])
		}
	}
	if got.Spot.ServiceConfig["Restart"] != "always" || got.Spot.Environment["DEVBOX_SPOT_POLL_SECONDS"] != "5" {
		t.Errorf("spot watcher: %v %v", got.Spot.ServiceConfig, got.Spot.Environment)
	}
	if !contains(got.Spot.WantedBy, "multi-user.target") {
		t.Errorf("spot watcher not started at boot: %v", got.Spot.WantedBy)
	}

	// Item 3: the gateway is ordered against /home, so it is stopped before
	// the unmount, and its stop fits the spot budget.
	mounts := fmt.Sprint(got.Gateway.UnitConfig["RequiresMountsFor"])
	if !strings.Contains(mounts, "/home/alice") {
		t.Errorf("gateway not ordered against /home: %v", got.Gateway.UnitConfig)
	}
	if got.Gateway.ServiceConfig["TimeoutStopSec"] != 45.0 {
		t.Errorf("gateway TimeoutStopSec %v", got.Gateway.ServiceConfig["TimeoutStopSec"])
	}
	// D24: the host never continues a turn by itself, so the module sets
	// no resume settings.
	if _, set := got.Settings["resume"]; set {
		t.Errorf("resume settings: %v", got.Settings)
	}
	found := false
	for _, name := range got.Packages {
		if name == "devbox-agents" {
			found = true
		}
	}
	if !found {
		t.Errorf("devbox-agents not installed: %v", got.Packages)
	}
	if strings.Contains(got.Agents, "DEVBOX_GATEWAY_COMMAND") || strings.Contains(got.Agents, "HAS_RESUME") {
		t.Errorf("devbox-agents is still given a gateway command to run:\n%s", got.Agents)
	}
}

// R-TEST: the template's boot notification. Changing it must not post a
// spurious "booted" at deploy, it must come after the gateway so the resume
// line can report, and it must post through notify.sh, never by running the
// secrets file.
func TestTemplateBootNotification(t *testing.T) {
	needNix(t)
	template, err := filepath.Abs(filepath.Join("..", "terraform", "configuration.nix"))
	if err != nil {
		t.Fatal(err)
	}
	var unit struct {
		RestartIfChanged bool     `json:"restartIfChanged"`
		After            []string `json:"after"`
		Script           string   `json:"script"`
		ScriptDrvs       []string `json:"scriptDrvs"`
		Notify           string   `json:"notify"`
		Lifecycle        bool     `json:"lifecycle"`
	}
	// --read-write-mode writes the derivations, so the script can be built.
	expression := fmt.Sprintf(`let s = import <nixpkgs/nixos> { configuration = %s; };
  u = s.config.systemd.services.devbox-slack-notify; in {
  inherit (u) restartIfChanged after;
  script = builtins.unsafeDiscardStringContext u.serviceConfig.ExecStart;
  scriptDrvs = builtins.attrNames (builtins.getContext u.serviceConfig.ExecStart);
  notify = builtins.unsafeDiscardStringContext "${s.config.services.devbox-lifecycle.generated.notify}";
  lifecycle = s.config.services.devbox-lifecycle.enable;
}`, template)
	out, err := exec.Command("nix-instantiate", "--eval", "--strict", "--json", "--read-write-mode", "-E", expression).Output()
	if err != nil {
		var stderr string
		if exit, ok := err.(*exec.ExitError); ok {
			stderr = string(exit.Stderr)
		}
		t.Fatalf("evaluating the template: %v\n%s", err, lastLines(stderr, 15))
	}
	if err := json.Unmarshal(out, &unit); err != nil {
		t.Fatal(err)
	}
	if unit.RestartIfChanged {
		t.Error("devbox-slack-notify restarts on a config switch, which posts a spurious 'booted'")
	}
	if !contains(unit.After, "ahp-gateway.service") {
		t.Errorf("devbox-slack-notify is not ordered after the gateway: %v", unit.After)
	}
	if !unit.Lifecycle {
		t.Error("the template does not enable devbox-lifecycle")
	}
	if !strings.HasPrefix(unit.Script, "/nix/store/") {
		t.Fatalf("ExecStart %q", unit.Script)
	}
	// Build the one small script (its derivation is in the string's
	// context) and read it.
	for _, drv := range unit.ScriptDrvs {
		if out, err := exec.Command("nix-store", "--realise", drv).CombinedOutput(); err != nil {
			t.Fatalf("realising %s: %v\n%s", drv, err, lastLines(string(out), 10))
		}
	}
	data, err := os.ReadFile(unit.Script)
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	if !strings.Contains(body, unit.Notify) {
		t.Error("the boot notification does not post through notify.sh")
	}
	if strings.Contains(body, ". \"$SECRETS\"") || strings.Contains(body, "source ") {
		t.Error("the boot notification runs the secrets file")
	}
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// D24: resumeOnBoot is gone. A config that still sets it gets a message
// saying why, not a silent no-op.
func TestLifecycleModuleResumeOnBootIsRemoved(t *testing.T) {
	needNix(t)
	expression := "(" + lifecycleSystem(t, "{}", "{ resumeOnBoot = true; }") + ").config.system.build.toplevel.drvPath"
	out, err := exec.Command("nix-instantiate", "--eval", "-E", expression).CombinedOutput()
	if err == nil || !strings.Contains(string(out), "resumeOnBoot") || !strings.Contains(string(out), "Try Again") {
		t.Fatalf("resumeOnBoot accepted, or refused without the reason:\n%s", lastLines(string(out), 10))
	}
}

func TestLifecycleModuleStopTimeoutAssertion(t *testing.T) {
	needNix(t)
	expression := "(" + lifecycleSystem(t, "{ stopTimeoutSec = 61; }", "{}") + ").config.system.build.toplevel.drvPath"
	out, err := exec.Command("nix-instantiate", "--eval", "-E", expression).CombinedOutput()
	if err == nil || !strings.Contains(string(out), "stopTimeoutSec must be at most 60") {
		t.Fatalf("a 61 s stop timeout was accepted:\n%s", lastLines(string(out), 10))
	}
	// The bound itself is allowed.
	expression = "(" + lifecycleSystem(t, "{ stopTimeoutSec = 60; }", "{}") + ").config.system.build.toplevel.drvPath"
	if out, err := exec.Command("nix-instantiate", "--eval", "-E", expression).CombinedOutput(); err != nil {
		t.Fatalf("a 60 s stop timeout was refused:\n%s", lastLines(string(out), 10))
	}
	expression = "(" + lifecycleSystem(t, "{ stopTimeoutSec = 61; }", "{ spotWatch.enable = false; }") + ").config.system.build.toplevel.drvPath"
	if out, err := exec.Command("nix-instantiate", "--eval", "-E", expression).CombinedOutput(); err != nil {
		t.Fatalf("without the watcher the bound does not apply, but evaluation failed:\n%s", lastLines(string(out), 10))
	}
}
