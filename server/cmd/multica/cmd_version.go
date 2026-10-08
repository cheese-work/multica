package main

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/pkg/buildinfo"
)

func init() {
	versionCmd.Flags().String("output", "text", "Output format: text or json")
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version information",
	RunE:  runVersion,
}

func runVersion(cmd *cobra.Command, _ []string) error {
	output, _ := cmd.Flags().GetString("output")

	if output == "json" {
		plainVersion, build := buildinfo.Split(version)
		info := map[string]string{
			"version":         plainVersion,
			"build":           build,
			"display_version": version,
			"commit":          commit,
			"date":            date,
			"go":              runtime.Version(),
			"os":              runtime.GOOS,
			"arch":            runtime.GOARCH,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(info)
	}

	fmt.Printf("multica %s (commit: %s, built: %s)\n", version, commit, date)
	fmt.Printf("go: %s, os/arch: %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
	return nil
}
