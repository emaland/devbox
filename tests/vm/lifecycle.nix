# NixOS VM tests for the devbox lifecycle (design note 031, section 6):
#
#   1. a busy agent keeps the box up past the idle window; once idle it stops
#   2. a spot interruption notice from a fake metadata service stops the
#      gateway cleanly, well inside the unit's stop timeout
#   3. after a reboot the interrupted turn is continued once, and a second
#      reboot with nothing interrupted continues nothing
#
# The gateway is a stub (stub-gateway.js) that speaks only the status
# endpoint and records clean stops and continuations; whether the real
# gateway continues a turn exactly once is the gateway's own test. Needs a
# machine with KVM:
#
#   nix-build tests/vm/lifecycle.nix
#
# This box has no KVM; scripts/check.sh only evaluates the test.
{ pkgs ? import <nixpkgs> { } }:

let
  stubPackage = pkgs.runCommand "ahp-acp-gateway-0.0.0-stub" { pname = "ahp-acp-gateway"; version = "0.0.0-stub"; } ''
    d=$out/lib/node_modules/ahp-acp-gateway
    mkdir -p $d/dist/src
    cp ${./stub-gateway.js} $d/dist/src/cli.js
    echo '{"version": 1, "providers": [{"id": "claude", "command": "npx", "args": [], "enabled": true}]}' > $d/ahp-acp-gateway.default.json
  '';

  # IMDSv2 with files: <dir>/<path under latest/meta-data>.
  fakeImds = pkgs.writeText "fake-imds.py" ''
    import http.server, os
    ROOT = "/run/fake-imds"
    class Handler(http.server.BaseHTTPRequestHandler):
        def do_PUT(self):
            self.send_response(200); self.end_headers(); self.wfile.write(b"session")
        def do_GET(self):
            if self.headers.get("X-aws-ec2-metadata-token") != "session":
                self.send_response(401); self.end_headers(); return
            path = os.path.join(ROOT, self.path.removeprefix("/latest/meta-data/"))
            if not os.path.isfile(path):
                self.send_response(404); self.end_headers(); return
            self.send_response(200); self.end_headers()
            with open(path, "rb") as f: self.wfile.write(f.read())
    http.server.HTTPServer(("127.0.0.1", 8111), Handler).serve_forever()
  '';

  imdsEnvironment = { DEVBOX_IMDS = pkgs.lib.mkForce "http://127.0.0.1:8111"; };
in
pkgs.testers.runNixOSTest {
  name = "devbox-lifecycle";

  nodes.machine = { pkgs, lib, ... }: {
    imports = [
      ../../terraform/devbox/modules/ahp-gateway.nix
      ../../terraform/devbox/modules/devbox-lifecycle.nix
    ];
    users.users.alice = { isNormalUser = true; uid = 1000; linger = true; };
    services.ahp-gateway = {
      enable = true;
      user = "alice";
      package = stubPackage;
      providers.claude = { command = "${pkgs.coreutils}/bin/true"; };
    };
    services.devbox-lifecycle.enable = true;

    systemd.services.fake-imds = {
      wantedBy = [ "multi-user.target" ];
      before = [ "devbox-spot-watch.service" ];
      serviceConfig.ExecStartPre = "${pkgs.coreutils}/bin/mkdir -p /run/fake-imds";
      serviceConfig.ExecStart = "${pkgs.python3}/bin/python3 ${fakeImds}";
    };
    systemd.services.devbox-spot-watch.environment = imdsEnvironment;
    systemd.services.devbox-autostop.environment = imdsEnvironment;
    # The poll is triggered by the test, not by the timer.
    systemd.timers.devbox-autostop.enable = false;
  };

  testScript = ''
    import json
    state = "/home/alice/.ahp-acp-gateway"

    def turns(active, waiting=0):
        machine.succeed(f"echo '{json.dumps({'active': active, 'waitingForUser': waiting})}' > {state}/turns.json")

    def poll():
        machine.succeed("systemctl start devbox-autostop.service")

    machine.wait_for_unit("ahp-gateway.service")
    machine.wait_until_succeeds("curl -sf http://127.0.0.1:8765/healthz")
    machine.succeed("echo instance-id-of-the-vm > /run/fake-imds/instance-id")
    machine.succeed("mkdir -p /run/fake-imds/placement && echo test-region-1a > /run/fake-imds/placement/availability-zone")

    with subtest("a busy agent keeps the box up past the idle window"):
        machine.succeed("mkdir -p /etc/devbox && echo 1h > /etc/devbox/autostop-after")
        turns(1)
        poll()
        # Two hours since the last activity, with the agent still working.
        machine.succeed("echo $(( $(date +%s) - 7200 )) > /run/devbox-autostop/last-active")
        poll()
        machine.fail("grep -q 'stopping:' /var/log/boot-history")
        machine.succeed("grep -q 'active (agent busy)' /var/log/boot-history")

        # A turn waiting for a person is not activity: the box stops. There
        # are no AWS credentials in the VM, so stop-instances fails, which
        # starts the gateway again.
        turns(1, 1)
        machine.succeed("echo $(( $(date +%s) - 7200 )) > /run/devbox-autostop/last-active")
        poll()
        machine.succeed("grep -q 'stopping: idle' /var/log/boot-history")
        machine.succeed("grep -q 'gateway stopped in' /var/log/boot-history")
        machine.succeed("grep -q 'stop-instances failed' /var/log/boot-history")
        machine.wait_for_unit("ahp-gateway.service")
        machine.succeed("echo off > /etc/devbox/autostop-after")

    with subtest("a spot interruption notice stops the gateway cleanly"):
        turns(1)
        stops_before = int(machine.succeed(f"cat {state}/clean-stops | wc -l"))
        machine.succeed("mkdir -p /run/fake-imds/spot && "
                        "echo '{\"action\": \"stop\", \"time\": \"'$(date -u -d '+2 min' +%Y-%m-%dT%H:%M:%SZ)'\"}' > /run/fake-imds/spot/instance-action")
        # Detection within a few polls, and the stop well inside the 45 s unit timeout.
        machine.wait_until_succeeds("grep -q 'spot | gateway stopped in' /var/log/boot-history", timeout=30)
        machine.fail("systemctl is-active ahp-gateway.service")
        stops_after = int(machine.succeed(f"cat {state}/clean-stops | wc -l"))
        assert stops_after == stops_before + 1, f"clean stops {stops_before} -> {stops_after}"
        machine.succeed("rm /run/fake-imds/spot/instance-action")
        machine.succeed("systemctl start ahp-gateway.service")
        machine.wait_for_unit("ahp-gateway.service")

    with subtest("after a reboot the interrupted turn is continued once"):
        machine.succeed(f"rm -f {state}/continued")
        turns(1)
        machine.shutdown()
        machine.start()
        machine.wait_for_unit("ahp-gateway.service")
        machine.wait_until_succeeds("curl -sf http://127.0.0.1:8765/healthz")
        continued = machine.succeed(f"cat {state}/continued | wc -l").strip()
        assert continued == "1", f"continued {continued} turns, want 1"
        machine.succeed("grep -q '\"onStart\": *\"continue\"' /run/ahp-gateway/config.json")

        # Nothing interrupted this time: nothing continued.
        turns(0)
        machine.shutdown()
        machine.start()
        machine.wait_for_unit("ahp-gateway.service")
        continued = machine.succeed(f"cat {state}/continued | wc -l").strip()
        assert continued == "1", f"continued {continued} turns after a clean idle reboot, want still 1"
  '';
}
