package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/aws/smithy-go"
	"github.com/spf13/cobra"

	"github.com/instantcocoa/devbox/internal/config"
)

// Agents on the box, and the guard every stop path goes through (design
// note 031, sections 2.5 and 2.6). The box side is the devbox-agents command
// installed by terraform/devbox/modules/devbox-lifecycle.nix.

// remoteRunner runs a shell command on an instance over the existing ssh
// path and returns its standard output and exit status. A connection
// failure is an error, not a status.
type remoteRunner func(ctx context.Context, instanceID, command string) (stdout string, status int, err error)

// runRemote is a variable so the tests can fake the ssh call.
var runRemote remoteRunner = sshRun

func sshArgs(dcfg config.DevboxConfig, ip string, extra ...string) []string {
	args := []string{
		"-i", dcfg.ResolveSSHKeyPath(),
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=15",
		"-o", "LogLevel=ERROR",
	}
	args = append(args, extra...)
	return append(args, dcfg.SSHUser+"@"+ip)
}

func sshRun(ctx context.Context, instanceID, command string) (string, int, error) {
	ip, err := instancePublicIP(ctx, ec2Client, instanceID)
	if err != nil {
		return "", 0, err
	}
	var stdout bytes.Buffer
	ssh := exec.CommandContext(ctx, "ssh", append(sshArgs(dcfg, ip), command)...)
	ssh.Stdout = &stdout
	ssh.Stderr = os.Stderr
	err = ssh.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return stdout.String(), 0, nil
	case errors.As(err, &exit) && exit.ExitCode() != 255:
		return stdout.String(), exit.ExitCode(), nil
	default:
		return "", 0, fmt.Errorf("ssh to %s: %w", instanceID, err)
	}
}

// agentStatus is devbox-agents' `status --json`.
type agentStatus struct {
	State    string `json:"state"` // busy, waiting, idle, unreachable, none
	Reason   string `json:"reason"`
	Sessions *int   `json:"sessions"`
	Running  int    `json:"running"`
	Waiting  int    `json:"waiting"`
	Pending  int    `json:"pending"`
}

func (s agentStatus) String() string {
	switch s.State {
	case "unreachable":
		return "gateway not answering (" + s.Reason + ")"
	case "none":
		return "no gateway"
	}
	sessions := "?"
	if s.Sessions != nil {
		sessions = strconv.Itoa(*s.Sessions)
	}
	text := fmt.Sprintf("%s: %s sessions, %d running, %d waiting", s.State, sessions, s.Running, s.Waiting)
	if s.Pending > 0 {
		text += fmt.Sprintf(", %d continuations pending", s.Pending)
	}
	return text
}

// readAgents asks the box. An error means the status could not be read.
func readAgents(ctx context.Context, instanceID string) (agentStatus, error) {
	out, status, err := runRemote(ctx, instanceID, "devbox-agents status --json")
	if err != nil {
		return agentStatus{}, err
	}
	if status == 127 {
		return agentStatus{}, fmt.Errorf("devbox-agents is not installed on %s; push the current NixOS config first", instanceID)
	}
	var s agentStatus
	if jsonErr := json.Unmarshal([]byte(strings.TrimSpace(out)), &s); jsonErr != nil || s.State == "" {
		return agentStatus{}, fmt.Errorf("devbox-agents on %s answered %q (exit %d)", instanceID, strings.TrimSpace(out), status)
	}
	return s, nil
}

// stopGuard is the check-then-stop sequence of note 031 section 2.5, used
// by every command that stops, reboots or replaces a running instance.
type stopGuard struct {
	force bool // go on when busy or when the status cannot be read
	hard  bool // skip the box entirely (wedged instance)
	out   io.Writer
}

const (
	remoteGatewayStop  = "if systemctl cat ahp-gateway.service >/dev/null 2>&1; then sudo timeout 90 systemctl stop ahp-gateway.service; fi"
	remoteGatewayStart = "if systemctl cat ahp-gateway.service >/dev/null 2>&1; then sudo systemctl start --no-block ahp-gateway.service; fi"
)

// instanceRunning reports whether an instance is running or starting; only
// those have a gateway to check. A variable so the tests can fake it.
var instanceRunning = func(ctx context.Context, id string) (bool, error) {
	return instanceIsRunning(ctx, ec2Client, id)
}

// runningOnly keeps the instances that are running or starting.
func (g stopGuard) runningOnly(ctx context.Context, ids []string) ([]string, error) {
	var running []string
	for _, id := range ids {
		up, err := instanceRunning(ctx, id)
		if err != nil {
			return nil, err
		}
		if up {
			running = append(running, id)
		} else {
			fmt.Fprintf(g.out, "%s: not running; no agents to check\n", id)
		}
	}
	return running, nil
}

// check reads every instance before any is touched; one refusal stops all.
func (g stopGuard) check(ctx context.Context, ids []string) error {
	if g.hard {
		return nil
	}
	var refusals []string
	for _, id := range ids {
		s, err := readAgents(ctx, id)
		switch {
		case err != nil:
			refusals = append(refusals, fmt.Sprintf("%s: cannot read the agents' status: %v", id, err))
		case s.State == "busy":
			refusals = append(refusals, fmt.Sprintf("%s: agents are working (%s)", id, s))
		case s.State == "unreachable":
			refusals = append(refusals, fmt.Sprintf("%s: %s", id, s))
		default:
			fmt.Fprintf(g.out, "%s: %s\n", id, s)
		}
	}
	if len(refusals) == 0 {
		return nil
	}
	if g.force {
		for _, refusal := range refusals {
			fmt.Fprintf(g.out, "%s; going on (--force)\n", refusal)
		}
		return nil
	}
	return fmt.Errorf("refusing:\n  %s\nUse --force to go on anyway (the gateway is still stopped cleanly first), or 'devbox down --when-idle' to stop once nothing is working", strings.Join(refusals, "\n  "))
}

// run is the whole sequence: check, dry run, stop each gateway, the EC2
// call, and on its failure the gateways are started again.
func (g stopGuard) run(ctx context.Context, ids []string, dryRun func(context.Context) error, call func(context.Context) error) error {
	// Only running instances have a gateway: a stopped one is neither
	// checked nor reached over ssh.
	running := ids
	if !g.hard {
		var err error
		if running, err = g.runningOnly(ctx, ids); err != nil {
			return err
		}
	}
	if err := g.check(ctx, running); err != nil {
		return err
	}
	if err := dryRun(ctx); err != nil {
		return err
	}
	var touched []string
	if !g.hard {
		for _, id := range running {
			_, status, err := runRemote(ctx, id, remoteGatewayStop)
			// Started or not, the stop may be under way on the box (an ssh
			// drop, the timeout): this gateway may need starting again too.
			touched = append(touched, id)
			if err == nil && status == 0 {
				fmt.Fprintf(g.out, "%s: gateway stopped\n", id)
				continue
			}
			if err == nil {
				err = fmt.Errorf("exit %d", status)
			}
			if !g.force {
				g.restart(ctx, touched)
				return fmt.Errorf("%s: stopping the gateway failed: %w; no instance was stopped (use --force to go on)", id, err)
			}
			fmt.Fprintf(g.out, "%s: stopping the gateway failed (%v); going on (--force)\n", id, err)
		}
	}
	if err := call(ctx); err != nil {
		g.restart(ctx, touched)
		return err
	}
	return nil
}

// restart starts each gateway again. Starting an active unit does nothing,
// so this is safe for gateways whose stop did not happen.
func (g stopGuard) restart(ctx context.Context, ids []string) {
	for _, id := range ids {
		if _, status, err := runRemote(ctx, id, remoteGatewayStart); err != nil || status != 0 {
			fmt.Fprintf(g.out, "%s: could not start the gateway again; if the instance is still running, run 'sudo systemctl start ahp-gateway' there (if it stopped, the gateway starts at its next boot)\n", id)
		} else {
			fmt.Fprintf(g.out, "%s: gateway started again\n", id)
		}
	}
}

// dryRunAllowed turns an EC2 DryRun answer into nil when the call would
// have been allowed.
func dryRunAllowed(err error) error {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && apiErr.ErrorCode() == "DryRunOperation" {
		return nil
	}
	if err == nil {
		return nil
	}
	return fmt.Errorf("the EC2 call would fail, so nothing was touched: %w", err)
}

func newAgentsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agents [instance-id]",
		Short: "Show the agent sessions and turns on the devbox",
		Long: `Show what the AHP gateway on the devbox is doing: sessions, turns
running, turns waiting for a person, and continuations pending.

  devbox agents                          Status of the auto-detected devbox
  devbox agents resume status|on|off     Auto-resume of interrupted turns`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := instanceArg(cmd.Context(), args)
			if err != nil {
				return err
			}
			out, status, err := runRemote(cmd.Context(), id, "devbox-agents status")
			if err != nil {
				return err
			}
			fmt.Print(out)
			return remoteStatusError(id, status)
		},
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "resume status|on|off [instance-id]",
		Short: "Show or set auto-resume of turns interrupted by a stop",
		Long: `Auto-resume continues, once, a turn that was interrupted when the
gateway stopped (a spot interruption, an auto-stop, 'devbox down'). It is on
by default on the devbox. 'off' also cancels continuations not yet started.`,
		Args:      cobra.RangeArgs(1, 2),
		ValidArgs: []string{"status", "on", "off"},
		RunE: func(cmd *cobra.Command, args []string) error {
			switch args[0] {
			case "status", "on", "off":
			default:
				return fmt.Errorf("unknown value %q: use status, on or off", args[0])
			}
			id, err := instanceArg(cmd.Context(), args[1:])
			if err != nil {
				return err
			}
			out, status, err := runRemote(cmd.Context(), id, "devbox-agents resume "+args[0])
			if err != nil {
				return err
			}
			fmt.Print(out)
			return remoteStatusError(id, status)
		},
	})
	return cmd
}

func instanceArg(ctx context.Context, args []string) (string, error) {
	if len(args) == 1 {
		return args[0], nil
	}
	return autoDetectRunningInstance(ctx, ec2Client)
}

func remoteStatusError(id string, status int) error {
	switch status {
	case 0:
		return nil
	case 2:
		return fmt.Errorf("%s: the gateway is not answering", id)
	case 3:
		return fmt.Errorf("%s: no gateway is installed", id)
	case 4:
		return fmt.Errorf("%s: this gateway build does not offer that", id)
	case 127:
		return fmt.Errorf("devbox-agents is not installed on %s; push the current NixOS config first", id)
	default:
		return fmt.Errorf("%s: devbox-agents exited %d", id, status)
	}
}

// gatewayClientConfig is /etc/ahp-gateway/config.json on the box.
type gatewayClientConfig struct {
	Port      int    `json:"port"`
	TokenFile string `json:"tokenFile"`
	Path      string `json:"path"`
}

func gatewayTokenPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "devbox", "gateway-token"), nil
}

// editorConfig is what `devbox tunnel` prints: how VS Code and Zed reach the
// gateway at the local end of the tunnel.
func editorConfig(localPort int, path, tokenPath string) string {
	return fmt.Sprintf(`Gateway at the local end of this tunnel: ws://127.0.0.1:%[1]d%[2]s
Token file: %[3]s

VS Code (settings.json; "connectionToken" is the content of the token file):
  "chat.remoteAgentHostsEnabled": true,
  "chat.remoteAgentHosts": [
    { "name": "devbox", "address": "127.0.0.1:%[1]d%[2]s", "connectionToken": "<token>" }
  ]
  If you use Settings Sync, add "chat.remoteAgentHosts" to
  "settings.sync.ignoredSettings" first, or the token is uploaded with your
  settings to every machine you sign in on.

Zed: point the AHP bridge at the URL above, with the token file above.

Every user and program on this machine can reach 127.0.0.1:%[1]d while the
tunnel is up; the token is the only barrier. Do not run a tunnel on a shared
machine, and turn off the editor's auto-connect while no tunnel is up.
Folders you open must exist on the devbox: the agent runs there.
While the tunnel is open the devbox counts as in use and does not auto-stop.
`, localPort, path, tokenPath)
}

func newTunnelCmd() *cobra.Command {
	var port int
	cmd := &cobra.Command{
		Use:   "tunnel [instance-id]",
		Short: "Forward the devbox gateway to this machine's loopback for VS Code and Zed",
		Long: `Open an ssh port forward from 127.0.0.1:<port> here to the AHP gateway
on the devbox, which listens on the devbox's loopback only. The gateway's
token is copied to ~/.config/devbox/gateway-token (mode 0600) and the editor
settings are printed. Stop the tunnel with Ctrl-C.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if port < 1024 || port > 65535 {
				return fmt.Errorf("--port %d: use an unprivileged port, 1024 to 65535", port)
			}
			id, err := instanceArg(ctx, args)
			if err != nil {
				return err
			}
			gateway, err := readGatewayClientConfig(ctx, id)
			if err != nil {
				return err
			}
			tokenPath, err := gatewayTokenPath()
			if err != nil {
				return err
			}
			if err := copyGatewayToken(ctx, id, gateway.TokenFile, tokenPath); err != nil {
				return err
			}
			fmt.Print(editorConfig(port, gateway.Path, tokenPath))
			ip, err := instancePublicIP(ctx, ec2Client, id)
			if err != nil {
				return err
			}
			sshBin, err := exec.LookPath("ssh")
			if err != nil {
				return fmt.Errorf("ssh not found in PATH: %w", err)
			}
			forward := fmt.Sprintf("127.0.0.1:%d:127.0.0.1:%d", port, gateway.Port)
			fmt.Printf("\nForwarding %s (Ctrl-C to stop)...\n", forward)
			return syscall.Exec(sshBin, append([]string{"ssh"}, sshArgs(dcfg, ip, "-N", "-o", "ExitOnForwardFailure=yes", "-L", forward)...), os.Environ())
		},
	}
	cmd.Flags().IntVar(&port, "port", 18765, "local port to listen on (127.0.0.1 only)")
	return cmd
}

func readGatewayClientConfig(ctx context.Context, id string) (gatewayClientConfig, error) {
	out, status, err := runRemote(ctx, id, "cat /etc/ahp-gateway/config.json")
	if err != nil {
		return gatewayClientConfig{}, err
	}
	if status != 0 {
		return gatewayClientConfig{}, fmt.Errorf("%s has no gateway installed (/etc/ahp-gateway/config.json is missing)", id)
	}
	var gateway gatewayClientConfig
	if err := json.Unmarshal([]byte(out), &gateway); err != nil || gateway.Port < 1 || gateway.Port > 65535 ||
		!strings.HasPrefix(gateway.TokenFile, "/") {
		return gatewayClientConfig{}, fmt.Errorf("%s: unexpected gateway client config", id)
	}
	if gateway.Path == "" {
		gateway.Path = "/ahp"
	}
	if !urlPathPattern.MatchString(gateway.Path) {
		return gatewayClientConfig{}, fmt.Errorf("%s: unexpected gateway path in the client config", id)
	}
	return gateway, nil
}

// urlPathPattern is the listener path: printed into JSON and a URL as is.
var urlPathPattern = regexp.MustCompile(`^/[A-Za-z0-9._~/-]*$`)

// tokenPattern is the token the gateway module generates (base64url).
var tokenPattern = regexp.MustCompile(`^[A-Za-z0-9._~+/=-]{16,}$`)

// copyGatewayToken copies the token over ssh into a 0600 file. The token is
// never printed.
func copyGatewayToken(ctx context.Context, id, remotePath, localPath string) error {
	out, status, err := runRemote(ctx, id, "cat -- "+shellQuote(remotePath))
	if err != nil {
		return err
	}
	token := strings.TrimRight(out, "\r\n")
	if status != 0 || !tokenPattern.MatchString(token) {
		return fmt.Errorf("%s: could not read the gateway token (is the gateway running?)", id)
	}
	dir := filepath.Dir(localPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// A new file of our own (O_EXCL, mode 0600 from the start), never a
	// fixed name that could already exist or be a link; renamed into place.
	file, err := os.CreateTemp(dir, ".gateway-token-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if _, err := file.WriteString(token + "\n"); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(temporary, localPath)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
