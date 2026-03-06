package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/dkoosis/snipe/internal/context"
	"github.com/dkoosis/snipe/internal/store"
)

var (
	contextFormat      string
	contextFull        bool
	contextBoot        bool
	contextOutputNug   bool
	contextConventions bool
)

var contextCmd = &cobra.Command{
	Use:     "context [path]",
	Short:   "Generate Claude-optimized project context",
	GroupID: "advanced",
	Long: `Generates a structured JSON/YAML output describing the project architecture,
files, and key symbols - optimized for providing context to Claude.

The output includes:
- Project metadata (name, root, build commands)
- Architecture components and data flows
- Files organized by concern
- Key types and functions (ranked by reference count)
- Generation metadata

Examples:
  snipe context                    # Generate context for current directory
  snipe context .                  # Same as above
  snipe context --format=yaml      # Output as YAML
  snipe context --full             # Include all symbols, not just key ones
  snipe context --boot             # Minimal context for LLM boot (~2000 tokens)`,
	Args: cobra.MaximumNArgs(1),
	RunE: runContext,
}

func init() {
	contextCmd.Flags().StringVar(&contextFormat, "format", "json", "Output format: json or yaml")
	contextCmd.Flags().BoolVar(&contextFull, "full", false, "Include all symbols, not just key ones")
	contextCmd.Flags().BoolVar(&contextBoot, "boot", false, "Minimal context for LLM boot (~2000 tokens)")
	contextCmd.Flags().BoolVar(&contextOutputNug, "output-nug", false, "Output as Orca nugget YAML (for save_nug)")
	contextCmd.Flags().BoolVar(&contextConventions, "conventions", false, "Detect coding conventions")
	rootCmd.AddCommand(contextCmd)
}

func runContext(cmd *cobra.Command, args []string) error {
	// Determine directory
	dir := "."
	if len(args) > 0 {
		dir = args[0]
	}

	absDir, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}

	// Find project root
	projectRoot := findProjectRoot(absDir)
	if projectRoot == "" {
		return fmt.Errorf("not in a git repository")
	}

	// Open index
	indexPath := store.DefaultIndexPath(projectRoot)
	if !store.Exists(indexPath) {
		return fmt.Errorf("index not found at %s\nRun 'snipe index' first", indexPath)
	}

	s, err := store.Open(indexPath)
	if err != nil {
		return fmt.Errorf("open index: %w", err)
	}
	defer s.Close()

	// Generate context
	cfg := context.GenerateConfig{
		RepoRoot: projectRoot,
		DB:       s.DB(),
		Full:     contextFull,
	}

	if contextBoot {
		// Generate minimal boot context
		bootCtx, err := context.GenerateBoot(cfg)
		if err != nil {
			return fmt.Errorf("generate boot context: %w", err)
		}

		// Output as nuggets if requested
		if contextOutputNug {
			nugs := bootCtx.ToNuggets()
			return outputNuggets(nugs)
		}

		return outputContext(bootCtx, contextFormat)
	}

	if contextConventions {
		conv := context.DetectConventions(s.DB(), projectRoot)
		return outputContext(conv, contextFormat)
	}

	// Generate full context
	ctx, err := context.Generate(cfg)
	if err != nil {
		return fmt.Errorf("generate context: %w", err)
	}

	// Output as nuggets if requested
	if contextOutputNug {
		nugs := ctx.ToNuggets()
		return outputNuggets(nugs)
	}

	return outputContext(ctx, contextFormat)
}

func outputContext(output interface{}, format string) error {
	switch format {
	case "yaml":
		enc := yaml.NewEncoder(os.Stdout)
		enc.SetIndent(2)
		if err := enc.Encode(output); err != nil {
			return fmt.Errorf("encode yaml: %w", err)
		}
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(output); err != nil {
			return fmt.Errorf("encode json: %w", err)
		}
	default:
		return fmt.Errorf("unsupported format: %s (use json or yaml)", format)
	}
	return nil
}

func outputNuggets(nugs []context.Nugget) error {
	enc := yaml.NewEncoder(os.Stdout)
	enc.SetIndent(2)
	for _, nug := range nugs {
		if err := enc.Encode(nug); err != nil {
			return fmt.Errorf("encode nug yaml: %w", err)
		}
		fmt.Println("---")
	}
	return nil
}
