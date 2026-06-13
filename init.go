package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/spf13/cobra"
)

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Write a template config.json to fill in",
	Long: `Write a template config.json with every section the commands read, using
placeholder values you replace with real Drive folder ids and names. Refuses to
overwrite an existing file unless --force is given. Use --config to write
somewhere other than ./config.json.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfgPath, _ := cmd.Flags().GetString("config")
		force, _ := cmd.Flags().GetBool("force")
		return runInit(cfgPath, force)
	},
}

func init() {
	initCmd.Flags().Bool("force", false, "overwrite an existing config file")
}

func runInit(path string, force bool) error {
	if _, err := os.Stat(path); err == nil && !force {
		return fmt.Errorf("%s already exists; pass --force to overwrite it", path)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("checking %s: %w", path, err)
	}

	tmpl, err := configTemplate()
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(tmpl), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}

	fmt.Fprintf(os.Stderr, "Wrote %s. Replace the placeholder values before running other commands.\n", path)
	return nil
}
