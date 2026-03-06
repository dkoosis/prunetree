package cmd

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/dkoosis/snipe/internal/output"
)

var (
	Version   = "0.1.0"
	GitCommit = "unknown"
)

var versionJSON bool

// Features lists all snipe subcommands available for LLM callers.
// Derived from knownSubcommands but hardcoded for stability.
var Features = []string{
	"def", "refs", "callers", "callees", "search",
	"context", "explain", "sym", "index", "show",
	"sim", "types", "impl", "imports", "importers",
	"pkg", "edit", "pack",
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version information",
	Run: func(cmd *cobra.Command, args []string) {
		if versionJSON {
			info := output.VersionInfo{
				Version:  Version,
				Protocol: output.ProtocolVersion,
				Features: Features,
				Commit:   GitCommit,
			}
			enc := json.NewEncoder(os.Stdout)
			_ = enc.Encode(info)
		} else {
			fmt.Printf("snipe version %s (commit: %s)\n", Version, GitCommit)
		}
	},
}

func init() {
	versionCmd.Flags().BoolVar(&versionJSON, "json", false, "Output version info as JSON")
	rootCmd.AddCommand(versionCmd)
}
