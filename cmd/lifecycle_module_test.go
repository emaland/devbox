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
		Autostop  gatewayUnit       `json:"autostop"`
		Timer     map[string]string `json:"timer"`
		Spot      gatewayUnit       `json:"spot"`
		Gateway   gatewayUnit       `json:"gateway"`
		Packages  []string          `json:"packages"`
		Settings  map[string]any    `json:"settings"`
		HasResume bool              `json:"hasResume"`
	}
	evalJSON(t, "let s = "+lifecycleSystem(t, "{}", "{}")+`; c = s.config; in {
  autostop = { inherit (c.systemd.services.devbox-autostop) description wantedBy wants after restartIfChanged environment unitConfig serviceConfig; };
  timer = c.systemd.timers.devbox-autostop.timerConfig;
  spot = { inherit (c.systemd.services.devbox-spot-watch) description wantedBy wants after restartIfChanged environment unitConfig serviceConfig; };
  gateway = { inherit (c.systemd.services.ahp-gateway) description wantedBy wants after restartIfChanged environment unitConfig serviceConfig; };
  packages = map (p: p.name) c.environment.systemPackages;
  settings = c.services.ahp-gateway.settings;
  hasResume = c.services.devbox-lifecycle.generated.hasResume;
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
	// The resume default is config only; the unit must not override a
	// persisted off.
	if _, set := got.Gateway.Environment["AHP_GATEWAY_RESUME"]; set {
		t.Error("the unit sets AHP_GATEWAY_RESUME, which would override the persisted switch")
	}

	resume, _ := got.Settings["resume"].(map[string]any)
	if resume["onStart"] != "continue" || resume["maxAttemptsPerTurn"] != 1.0 {
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
	if got.HasResume {
		t.Error("the public build offers no resume command, so devbox-agents must not run one")
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

func TestLifecycleModuleResumeFlagFollowsTheWrapper(t *testing.T) {
	needNix(t)
	var has bool
	evalJSON(t, "("+lifecycleSystem(t, `{ managementCommands = [ "status" "resume" ]; }`, "{}")+
		").config.services.devbox-lifecycle.generated.hasResume", "", &has)
	if !has {
		t.Fatal("resume offered by the wrapper but not by devbox-agents")
	}
}

func TestLifecycleModuleResumeDefaults(t *testing.T) {
	needNix(t)
	rows := []struct {
		name, gateway, lifecycle string
		want                     any
	}{
		{"a user value wins", `{ settings.resume.onStart = "off"; }`, "{}", "off"},
		{"resumeOnBoot off sets nothing", "{}", "{ resumeOnBoot = false; }", nil},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			var settings map[string]any
			evalJSON(t, "("+lifecycleSystem(t, r.gateway, r.lifecycle)+").config.services.ahp-gateway.settings", "", &settings)
			resume, _ := settings["resume"].(map[string]any)
			var got any
			if resume != nil {
				got = resume["onStart"]
			}
			if got != r.want {
				t.Fatalf("resume.onStart = %v, want %v (settings %v)", got, r.want, settings)
			}
		})
	}
}

// The setting reaches the file the gateway starts from.
func TestLifecycleModuleResumeReachesTheGatewayConfig(t *testing.T) {
	needNix(t)
	out, err := exec.Command("nix-build", "--no-out-link", "-E",
		"("+lifecycleSystem(t, "{}", "{}")+").config.services.ahp-gateway.generated.baseConfig").Output()
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
	var config struct {
		Resume map[string]any `json:"resume"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	if config.Resume["onStart"] != "continue" || config.Resume["maxAttemptsPerTurn"] != 1.0 {
		t.Fatalf("base config resume: %v", config.Resume)
	}
}

func TestLifecycleModuleStopTimeoutAssertion(t *testing.T) {
	needNix(t)
	expression := "(" + lifecycleSystem(t, "{ stopTimeoutSec = 61; }", "{}") + ").config.system.build.toplevel.drvPath"
	out, err := exec.Command("nix-instantiate", "--eval", "-E", expression).CombinedOutput()
	if err == nil || !strings.Contains(string(out), "stopTimeoutSec must be at most 60") {
		t.Fatalf("a 61 s stop timeout was accepted:\n%s", lastLines(string(out), 10))
	}
	expression = "(" + lifecycleSystem(t, "{ stopTimeoutSec = 61; }", "{ spotWatch.enable = false; }") + ").config.system.build.toplevel.drvPath"
	if out, err := exec.Command("nix-instantiate", "--eval", "-E", expression).CombinedOutput(); err != nil {
		t.Fatalf("without the watcher the bound does not apply, but evaluation failed:\n%s", lastLines(string(out), 10))
	}
}
