package cmd

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	devboxconfig "github.com/emaland/devbox/internal/config"
)

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Show or modify devbox configuration",
		Long: `Show the resolved devbox configuration and where each value came from
(the config file or a built-in default), or change a value.

  devbox config                       Show all settings
  devbox config set <key> <value>     Set a value in the config file
  devbox config path                  Print the config file path`,
		// Config inspection needs no AWS client — just load the config file.
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			var err error
			dcfg, err = devboxconfig.LoadConfig()
			return err
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return showConfig()
		},
	}
	cmd.AddCommand(newConfigSetCmd(), newConfigPathCmd())
	return cmd
}

func newConfigSetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set <key> <value>",
		Short: "Set a configuration value",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := devboxconfig.Set(args[0], args[1]); err != nil {
				return err
			}
			path, _ := devboxconfig.ConfigPath()
			fmt.Printf("Set %s = %s in %s\n", args[0], args[1], path)
			return nil
		},
	}
}

func newConfigPathCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "path",
		Short: "Print the config file path",
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := devboxconfig.ConfigPath()
			if err != nil {
				return err
			}
			fmt.Println(path)
			return nil
		},
	}
}

func showConfig() error {
	cfg, err := devboxconfig.LoadConfig()
	if err != nil {
		return err
	}
	entries, err := cfg.Entries()
	if err != nil {
		return err
	}

	path, _ := devboxconfig.ConfigPath()
	if _, statErr := os.Stat(path); statErr == nil {
		fmt.Printf("Config file: %s\n\n", path)
	} else {
		fmt.Printf("Config file: %s (not present — all values are defaults)\n\n", path)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "KEY\tVALUE\tSOURCE")
	for _, e := range entries {
		value := e.Value
		if e.Secret && value != "" {
			value = "***set***"
		}
		if value == "" {
			value = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", e.Key, value, e.Source)
	}
	w.Flush()
	return nil
}
