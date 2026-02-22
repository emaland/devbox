package cmd

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/spf13/cobra"

	"github.com/emaland/devbox/internal/awsutil"
	devboxconfig "github.com/emaland/devbox/internal/config"
)

func newInfraCmd() *cobra.Command {
	var (
		dnsZoneID       string
		sshPublicKey    string
		sshPublicKeyFile string
		dir             string
		autoApprove     bool
	)

	cmd := &cobra.Command{
		Use:   "infra",
		Short: "Run terraform to set up (or update) devbox infrastructure",
		Long: `Automates the terraform workflow for devbox infrastructure.

Auto-detects dns_zone_id from your devbox config's dns_zone and ssh_public_key
from local SSH key files. Writes terraform.tfvars, runs init/validate/plan,
and prompts before apply.`,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			// Only load devbox config — no EC2 client needed.
			var err error
			dcfg, err = devboxconfig.LoadConfig()
			if err != nil {
				return err
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInfra(cmd.Context(), dcfg, dnsZoneID, sshPublicKey, sshPublicKeyFile, dir, autoApprove)
		},
	}

	cmd.Flags().StringVar(&dnsZoneID, "dns-zone-id", "", "Route 53 hosted zone ID (auto-detected from config)")
	cmd.Flags().StringVar(&sshPublicKey, "ssh-public-key", "", "SSH public key string")
	cmd.Flags().StringVar(&sshPublicKeyFile, "ssh-public-key-file", "", "Path to SSH public key file")
	cmd.Flags().StringVar(&dir, "dir", "./terraform", "Path to terraform directory")
	cmd.Flags().BoolVar(&autoApprove, "auto-approve", false, "Skip the y/N confirmation prompt")

	return cmd
}

func runInfra(ctx context.Context, dcfg devboxconfig.DevboxConfig, dnsZoneID, sshPublicKey, sshPublicKeyFile, dir string, autoApprove bool) error {
	// 1. Check terraform binary
	if _, err := exec.LookPath("terraform"); err != nil {
		return fmt.Errorf("terraform not found in PATH; install it from https://developer.hashicorp.com/terraform/install")
	}

	// 2. Check AWS credentials
	awsCfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return err
	}
	if err := checkAWSCredentials(ctx, awsCfg); err != nil {
		return err
	}

	// 3. Auto-detect dns_zone_id
	if dnsZoneID == "" {
		fmt.Printf("Detecting DNS zone for %s...\n", dcfg.DNSZone)
		r53Client := route53.NewFromConfig(awsCfg)
		zoneID, err := awsutil.FindHostedZone(ctx, r53Client, dcfg.DNSZone)
		if err != nil {
			return fmt.Errorf("auto-detecting dns_zone_id: %w\n\nUse --dns-zone-id to set it manually", err)
		}
		// FindHostedZone returns e.g. "/hostedzone/ZXXXXX", strip the prefix
		dnsZoneID = strings.TrimPrefix(zoneID, "/hostedzone/")
		fmt.Printf("  dns_zone_id = %s\n", dnsZoneID)
	}

	// 4. Auto-detect ssh_public_key
	if sshPublicKey == "" {
		if sshPublicKeyFile != "" {
			data, err := os.ReadFile(sshPublicKeyFile)
			if err != nil {
				return fmt.Errorf("reading SSH public key file: %w", err)
			}
			sshPublicKey = strings.TrimSpace(string(data))
		} else {
			key, err := detectSSHPublicKey(dcfg)
			if err != nil {
				return fmt.Errorf("auto-detecting ssh_public_key: %w\n\nUse --ssh-public-key or --ssh-public-key-file to set it manually", err)
			}
			sshPublicKey = key
		}
		fmt.Printf("  ssh_public_key = %s\n", truncateKey(sshPublicKey))
	}

	// 5. Write terraform.tfvars
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolving terraform dir: %w", err)
	}
	if err := writeTFVars(absDir, dnsZoneID, sshPublicKey); err != nil {
		return err
	}
	fmt.Printf("Wrote %s\n", filepath.Join(absDir, "terraform.tfvars"))

	// 6. Run terraform init (only if .terraform/ doesn't exist)
	dotTF := filepath.Join(absDir, ".terraform")
	if _, err := os.Stat(dotTF); os.IsNotExist(err) {
		fmt.Println("\nRunning terraform init...")
		if err := runTerraform(ctx, absDir, "init"); err != nil {
			return err
		}
	}

	// 7. Run terraform validate
	fmt.Println("\nRunning terraform validate...")
	if err := runTerraform(ctx, absDir, "validate"); err != nil {
		return err
	}

	// 8. Run terraform plan
	fmt.Println("\nRunning terraform plan...")
	if err := runTerraform(ctx, absDir, "plan"); err != nil {
		return err
	}

	// 9. Prompt
	if !autoApprove {
		ok, err := promptYesNo("Apply these changes?")
		if err != nil {
			return err
		}
		if !ok {
			fmt.Println("Aborted.")
			return nil
		}
	}

	// 10. Run terraform apply
	fmt.Println("\nRunning terraform apply...")
	if err := runTerraform(ctx, absDir, "apply", "-auto-approve"); err != nil {
		return err
	}

	fmt.Println("\nInfrastructure is up to date.")
	return nil
}

func detectSSHPublicKey(dcfg devboxconfig.DevboxConfig) (string, error) {
	// Try .pub derived from configured SSHKeyPath (.pem -> .pub)
	keyPath := dcfg.ResolveSSHKeyPath()
	candidates := []string{}

	if strings.HasSuffix(keyPath, ".pem") {
		candidates = append(candidates, strings.TrimSuffix(keyPath, ".pem")+".pub")
	}

	home, err := os.UserHomeDir()
	if err == nil {
		candidates = append(candidates,
			filepath.Join(home, ".ssh", "id_ed25519.pub"),
			filepath.Join(home, ".ssh", "id_rsa.pub"),
		)
	}

	for _, path := range candidates {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		key := strings.TrimSpace(string(data))
		if key != "" {
			fmt.Printf("  Found SSH public key: %s\n", path)
			return key, nil
		}
	}

	return "", fmt.Errorf("no SSH public key found; tried: %s", strings.Join(candidates, ", "))
}

func writeTFVars(dir, zoneID, sshKey string) error {
	content := fmt.Sprintf(`# Generated by devbox infra — re-run to update.
dns_zone_id    = %q
ssh_public_key = %q
`, zoneID, sshKey)

	path := filepath.Join(dir, "terraform.tfvars")
	return os.WriteFile(path, []byte(content), 0644)
}

func runTerraform(ctx context.Context, dir string, args ...string) error {
	fullArgs := append([]string{"-chdir=" + dir}, args...)
	cmd := exec.CommandContext(ctx, "terraform", fullArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

func promptYesNo(prompt string) (bool, error) {
	fmt.Printf("\n%s [y/N] ", prompt)
	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		return false, scanner.Err()
	}
	answer := strings.TrimSpace(strings.ToLower(scanner.Text()))
	return answer == "y" || answer == "yes", nil
}

func truncateKey(key string) string {
	if len(key) > 40 {
		return key[:40] + "..."
	}
	return key
}

func checkAWSCredentials(ctx context.Context, cfg aws.Config) error {
	stsClient := sts.NewFromConfig(cfg)
	_, err := stsClient.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return fmt.Errorf("%w\n\n%s", err, awsCredentialGuidance)
	}
	return nil
}
