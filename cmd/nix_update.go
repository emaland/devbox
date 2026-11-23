package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/spf13/cobra"

	"github.com/emaland/devbox/internal/config"
)

// nixupStartMarker is echoed at the top of the remote apply script so the
// completion poller can show only the current run's journal, not the history
// journalctl -u keeps for prior runs of the (reused) transient unit name.
const nixupStartMarker = "@@DEVBOX-NIXUP-START@@"

func newNixUpdateCmd() *cobra.Command {
	var nixFile string

	cmd := &cobra.Command{
		Use:   "nix-update [instance-id]",
		Short: "Push configuration.nix to an instance and run nixos-rebuild switch",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			instanceID := ""
			if len(args) == 1 {
				instanceID = args[0]
			} else {
				id, err := autoDetectRunningInstance(cmd.Context(), ec2Client)
				if err != nil {
					return err
				}
				instanceID = id
			}
			return nixUpdate(cmd.Context(), dcfg, ec2Client, instanceID, nixFile)
		},
	}

	cmd.Flags().StringVar(&nixFile, "file", "terraform/configuration.nix", "path to configuration.nix")

	return cmd
}

func nixUpdate(ctx context.Context, dcfg config.DevboxConfig, client *ec2.Client, instanceID, nixFile string) error {
	// Check everything the push needs from this machine (template, extra keys
	// file, hosted zone id) before touching the box, so a local problem is
	// reported once and is not mistaken for an ssh failure below.
	dcfg, err := preflightNixConfig(ctx, dcfg, nixFile, findHostedZoneID)
	if err != nil {
		return err
	}

	ip, err := instancePublicIP(ctx, client, instanceID)
	if err != nil {
		return err
	}

	// Try configured user first, fall back to root (fresh instances only
	// have root SSH until the config is applied and devbox-fetch-ssh-key runs).
	// No volume id here — `nix-update` targets a box whose volume is already
	// formatted, so the format service no-ops on the unrendered marker.
	err = pushNixConfigToHost(ctx, dcfg, ip, instanceID, nixFile, dcfg.SSHUser, "")
	// A refusal by the box's install step is the same whoever logs in; only a
	// failure to reach the box as the configured user is worth retrying.
	var refused installRefusedError
	if err != nil && dcfg.SSHUser != "root" && !errors.As(err, &refused) {
		fmt.Println("Retrying as root (fresh instance)...")
		err = pushNixConfigToHost(ctx, dcfg, ip, instanceID, nixFile, "root", "")
	}
	return err
}

// pushNixConfig looks up a running instance's IP and pushes configuration.nix
// via root SSH (for fresh instances where emaland keys aren't set up yet).
func pushNixConfig(ctx context.Context, dcfg config.DevboxConfig, client *ec2.Client, instanceID, volumeID string) error {
	nixFile := defaultNixFile()
	if _, err := readNixConfigSource(nixFile); err != nil {
		return err
	}

	ip, err := instancePublicIP(ctx, client, instanceID)
	if err != nil {
		return err
	}

	// Wait for SSH to become available on the freshly-booted stub. A cold
	// instance can take a couple of minutes to bring up sshd, so poll for up
	// to 5 minutes; returning early here (instead of charging ahead into scp)
	// gives the caller a clear "SSH never came up" error rather than a cryptic
	// scp failure.
	fmt.Println("Waiting for SSH to become available...")
	keyPath := dcfg.ResolveSSHKeyPath()
	sshReady := false
	for i := 0; i < 30; i++ {
		testCmd := exec.CommandContext(ctx, "ssh",
			"-i", keyPath,
			"-o", "StrictHostKeyChecking=no",
			"-o", "UserKnownHostsFile=/dev/null",
			"-o", "ConnectTimeout=5",
			"root@"+ip, "true",
		)
		if testCmd.Run() == nil {
			sshReady = true
			break
		}
		if i%3 == 2 {
			fmt.Printf("  still waiting for SSH on %s (%ds elapsed)...\n", ip, (i+1)*10)
		}
		time.Sleep(10 * time.Second)
	}
	if !sshReady {
		return fmt.Errorf("SSH on %s (%s) did not become available within 5 minutes", instanceID, ip)
	}

	return pushNixConfigToHost(ctx, dcfg, ip, instanceID, nixFile, "root", volumeID)
}

// Where a push puts things on the box.
const (
	remoteConfigDir         = "/etc/nixos"
	remoteSavedDir          = "/home/emaland/.config/devbox"
	remoteSetTarName        = "devbox-set.tar"
	remoteInstallScriptName = "devbox-install-set.sh"
)

// installRefusedError is a push the box's install step refused.
type installRefusedError struct{ error }

func (e installRefusedError) Unwrap() error { return e.error }

// describeInstallFailure turns install_set.sh's exit status into words.
func describeInstallFailure(status string) string {
	switch status {
	case "3":
		return "the pushed config is an older layout than the one on the box; update the devbox CLI"
	case "4":
		return "the operator's local.nix may be needed and could not be found, or the saved copy on /home is unusable or not mounted"
	default:
		return "install step exited " + status
	}
}

// remoteUploadDir is what `mktemp -d /tmp/devbox-push.XXXXXXXXXX` prints.
var remoteUploadDir = regexp.MustCompile(`^/tmp/devbox-push\.[A-Za-z0-9]{10}$`)

// remoteApplyScript is what runs on the box, as root, inside a transient
// systemd unit: install the uploaded set, rebuild, keep the saved copy in
// step, and report the result in a marker file the poller reads.
//
// It contains no single quote, because the caller single-quotes it.
func remoteApplyScript(uploadDir, stamp string) string {
	installer := `sh ` + uploadDir + `/` + remoteInstallScriptName

	// /home is mounted first: installing the set may need the saved copy on it
	// (the operator's local.nix on a fresh root volume). On a fresh box /home
	// is not mounted yet; mounting the data volume at its canonical mountpoint
	// is what the switch's home.mount then finds already satisfied.
	mountHome := `if ! mountpoint -q /home; then mount /dev/disk/by-label/home-data /home 2>/dev/null || true; fi`

	// Whether the saved copy can be seen, for the install step: a home volume
	// that exists but is not mounted hides it; a blank new volume has none.
	savedState := `if mountpoint -q /home; then DEVBOX_SAVED_STATE=mounted; ` +
		`elif blkid -L home-data >/dev/null 2>&1; then DEVBOX_SAVED_STATE=unmounted; ` +
		`else DEVBOX_SAVED_STATE=none; fi; export DEVBOX_SAVED_STATE`

	// The saved copy belongs to the user and is written as the user, never by
	// root. The script is handed over on standard input, so the user does not
	// need to read the upload directory. Only when /home is the real volume:
	// a copy written to the root volume underneath it would be hidden, then
	// restored over newer sets at the next boot.
	persist := `if mountpoint -q /home && id emaland >/dev/null 2>&1; then ` +
		`runuser -u emaland -- mkdir -p ` + remoteSavedDir + ` && ` +
		`runuser -u emaland -- sh -s save ` + remoteConfigDir + ` ` + remoteSavedDir + ` <` + uploadDir + `/` + remoteInstallScriptName + ` ` +
		`|| echo "devbox: warning: could not update the saved config set on /home"; ` +
		`else echo "devbox: /home is not mounted yet, or the user does not exist yet; the saved config set was not updated"; fi`

	// Reuse a local Nix binary cache kept on the persistent /home volume so a
	// fresh box (post resize/recover) substitutes prebuilt paths from local gp3
	// disk instead of re-downloading the whole closure from cache.nixos.org.
	// The store lives on the *root* volume, which is new on every swap; only
	// /home is recycled, so the closure is staged there. The staged paths are
	// unsigned, so require-sigs=false (our own volume). Best effort: any
	// failure just falls back to the network.
	cacheDir := "/home/.nix-cache"
	cachePre := `OPT=""; if [ -d ` + cacheDir + ` ]; then OPT="--option extra-substituters file://` + cacheDir + ` --option extra-trusted-substituters file://` + cacheDir + ` --option require-sigs false"; echo "devbox: substituting from local nix cache ` + cacheDir + `"; fi`
	// cachePost repopulates the cache for the next fresh box after the rc
	// marker is written, so the client returns as soon as the switch is done
	// and the first-time copy (GBs) finishes inside the unit. Populated
	// regardless of the switch's exit code: a flaky activation-time service
	// still yields a valid /run/current-system.
	cachePost := `if mountpoint -q /home; then echo "devbox: updating local nix cache in background"; mkdir -p ` + cacheDir + `; nix copy --to "file://` + cacheDir + `" /run/current-system 2>/dev/null || true; chown -R emaland:users ` + cacheDir + ` 2>/dev/null || true; fi`

	// The script captures nixos-rebuild's own exit code (the trailing persist
	// would otherwise mask it) and writes it to a marker file that signals
	// completion to the poller. It sources /etc/set-environment for NIX_PATH
	// (the transient unit's environment is otherwise minimal), and echoes a
	// start marker so the poller shows only this run's log. If the set cannot
	// be installed, nixos-rebuild is not run and the marker says so with an
	// "install:" prefix, which the poller reports as a failure rather than as
	// a rebuild warning.
	return `echo ` + nixupStartMarker + `; ` +
		`. /etc/set-environment 2>/dev/null || true; ` +
		`export PATH=/run/wrappers/bin:/run/current-system/sw/bin:$PATH; ` +
		`systemctl is-system-running --wait >/dev/null 2>&1 || true; ` +
		mountHome + `; ` +
		savedState + `; ` +
		`if ` + installer + ` install ` + uploadDir + `/` + remoteSetTarName + ` ` + remoteConfigDir + ` ` + remoteSavedDir + ` ` + stamp + `; then ` +
		persist + `; ` +
		cachePre + `; ` +
		`nixos-rebuild switch $OPT; rc=$?; ` +
		persist + `; ` +
		`echo "$rc" | tee /tmp/devbox-nixup.rc >/dev/null; ` +
		cachePost + `; ` +
		`else echo "install:$?" | tee /tmp/devbox-nixup.rc >/dev/null; fi; ` +
		`rm -rf ` + uploadDir
}

func instancePublicIP(ctx context.Context, client *ec2.Client, instanceID string) (string, error) {
	desc, err := client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	})
	if err != nil {
		return "", fmt.Errorf("describing instance: %w", err)
	}
	if len(desc.Reservations) == 0 || len(desc.Reservations[0].Instances) == 0 {
		return "", fmt.Errorf("instance %s not found", instanceID)
	}
	inst := desc.Reservations[0].Instances[0]
	if inst.PublicIpAddress == nil {
		return "", fmt.Errorf("instance %s has no public IP (is it running?)", instanceID)
	}
	return *inst.PublicIpAddress, nil
}

func pushNixConfigToHost(ctx context.Context, dcfg config.DevboxConfig, ip, instanceID, nixFile, sshUser, volumeID string) error {
	keyPath := dcfg.ResolveSSHKeyPath()

	sshTarget := sshUser + "@" + ip
	sshOpts := []string{
		"-i", keyPath,
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
	}

	// Render the config (substitute @@MARKER@@ placeholders) and pack it with
	// the modules it imports, so the instance never receives literal markers
	// or a main file without its tree. Upload the archive and the script that
	// installs it.
	packed, installSetScript, err := setForPush(ctx, dcfg, nixFile, volumeID, findHostedZoneID)
	if err != nil {
		return err
	}
	staging, err := os.MkdirTemp("", "devbox-push-*")
	if err != nil {
		return fmt.Errorf("creating temp dir: %w", err)
	}
	defer os.RemoveAll(staging)
	localTar := filepath.Join(staging, remoteSetTarName)
	localScript := filepath.Join(staging, remoteInstallScriptName)
	if err := os.WriteFile(localTar, packed, 0o600); err != nil {
		return fmt.Errorf("writing the config set: %w", err)
	}
	if err := os.WriteFile(localScript, installSetScript, 0o700); err != nil {
		return fmt.Errorf("writing the install script: %w", err)
	}

	// Upload into a directory of its own on the box, made by mktemp with mode
	// 0700, never to fixed names in /tmp, where anything could have put a file
	// of its own first.
	mkdirArgs := append([]string{}, sshOpts...)
	mkdirArgs = append(mkdirArgs, sshTarget, `mktemp -d /tmp/devbox-push.XXXXXXXXXX`)
	out, err := exec.CommandContext(ctx, "ssh", mkdirArgs...).Output()
	if err != nil {
		return fmt.Errorf("creating an upload directory on %s: %w", instanceID, err)
	}
	remoteDir := strings.TrimSpace(string(out))
	if !remoteUploadDir.MatchString(remoteDir) {
		return fmt.Errorf("unexpected upload directory from %s: %q", instanceID, remoteDir)
	}

	// The remote script removes the directory when it runs; if it never
	// starts, remove it here.
	launched := false
	defer func() {
		if launched {
			return
		}
		cleanup := append([]string{}, sshOpts...)
		cleanup = append(cleanup, sshTarget, `rm -rf `+remoteDir)
		_ = exec.Command("ssh", cleanup...).Run()
	}()

	fmt.Printf("Uploading %s and its modules to %s (%s)...\n", nixFile, instanceID, ip)
	scpArgs := append([]string{}, sshOpts...)
	scpArgs = append(scpArgs, localTar, localScript, sshTarget+":"+remoteDir+"/")
	scpCmd := exec.CommandContext(ctx, "scp", scpArgs...)
	scpCmd.Stdout = os.Stdout
	scpCmd.Stderr = os.Stderr
	if err := scpCmd.Run(); err != nil {
		return fmt.Errorf("scp failed: %w", err)
	}

	applyScript := remoteApplyScript(remoteDir, newSetStamp(time.Now()))

	// Run the switch as a transient systemd unit and poll for completion,
	// instead of holding one long SSH channel open across the whole rebuild.
	// nixos-rebuild switch restarts sshd mid-activation; a synchronous channel
	// has no keepalive and stalls on the dropped connection for many minutes.
	// Detaching it into `systemd-run` lets the switch finish independently while
	// we watch it over short, keepalive'd connections that reconnect freely.
	// applyScript contains no single quotes, so single-quoting it is safe.
	const unit = "devbox-nixup"
	launch := `sudo systemctl stop ` + unit + ` 2>/dev/null; ` +
		`sudo systemctl reset-failed ` + unit + ` 2>/dev/null; ` +
		`sudo rm -f /tmp/devbox-nixup.rc; ` +
		`sudo systemd-run --unit=` + unit + ` --collect /bin/sh -c '` + applyScript + `'`
	launchArgs := append([]string{}, sshOpts...)
	launchArgs = append(launchArgs, sshTarget, launch)
	if out, err := exec.CommandContext(ctx, "ssh", launchArgs...).CombinedOutput(); err != nil {
		return fmt.Errorf("starting remote nixos-rebuild: %w\n%s", err, out)
	}
	launched = true

	fmt.Println("Running nixos-rebuild switch (this can take several minutes on a fresh box)...")
	pollOpts := append([]string{}, sshOpts...)
	pollOpts = append(pollOpts,
		"-o", "ServerAliveInterval=10", "-o", "ServerAliveCountMax=3",
		"-o", "ConnectTimeout=10")
	query := `journalctl -u ` + unit + ` -o cat --no-pager 2>/dev/null; ` +
		`echo "@@RC@@"; cat /tmp/devbox-nixup.rc 2>/dev/null`
	printed := 0
	deadline := time.Now().Add(30 * time.Minute)
	for {
		args := append([]string{}, pollOpts...)
		args = append(args, sshTarget, query)
		out, _ := exec.CommandContext(ctx, "ssh", args...).CombinedOutput()
		logPart, rcPart, found := strings.Cut(string(out), "@@RC@@")
		if found {
			// journalctl -u shows every past run of this unit name; skip to the
			// last start marker so we only stream the current invocation (and
			// print nothing until that marker appears).
			lines := strings.Split(strings.TrimRight(logPart, "\n"), "\n")
			lastStart := -1
			for i, l := range lines {
				if strings.Contains(l, nixupStartMarker) {
					lastStart = i
				}
			}
			if lastStart >= 0 {
				if printed < lastStart+1 {
					printed = lastStart + 1
				}
				for ; printed < len(lines); printed++ {
					if lines[printed] != "" {
						fmt.Println("  " + lines[printed])
					}
				}
			}
			if rc := strings.TrimSpace(rcPart); rc != "" {
				if status, failed := strings.CutPrefix(rc, "install:"); failed {
					return installRefusedError{fmt.Errorf("the config set was not installed on %s (%s), and nixos-rebuild was not run. The reason is in the output above", instanceID, describeInstallFailure(status))}
				}
				if rc != "0" {
					fmt.Printf("\nWarning: nixos-rebuild exited %s (usually service-activation warnings, not a build failure).\n", rc)
				}
				break
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after 30m waiting for nixos-rebuild on %s; it may still be running (check: journalctl -u %s)", instanceID, unit)
		}
		time.Sleep(10 * time.Second)
	}

	fmt.Printf("NixOS configuration updated on %s.\n", instanceID)
	return nil
}
