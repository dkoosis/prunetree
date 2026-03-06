package cmd

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/invopop/jsonschema"
	"github.com/spf13/cobra"

	"github.com/dkoosis/snipe/internal/output"
)

var schemaCmd = &cobra.Command{
	Use:    "schema [type]",
	Short:  "Output JSON Schema for snipe types",
	Hidden: true,
	Long: `Outputs JSON Schema for snipe response types.

Types:
  response  - Full response schema with results (default)
  result    - Individual result schema
  meta      - Response metadata schema
  error     - Error response schema

Examples:
  snipe schema           # Output response schema
  snipe schema result    # Output result schema`,
	Args: cobra.MaximumNArgs(1),
	RunE: runSchema,
}

func init() {
	rootCmd.AddCommand(schemaCmd)
}

// schemaResponse is the concrete type for schema generation
// (since Response[T] is generic, we need a concrete instantiation)
type schemaResponse struct {
	Results []output.Result `json:"results"`
	Meta    output.Meta     `json:"meta"`
	Error   *output.Error   `json:"error"`
}

func runSchema(cmd *cobra.Command, args []string) error {
	typeName := "response"
	if len(args) > 0 {
		typeName = args[0]
	}

	var schema *jsonschema.Schema
	reflector := &jsonschema.Reflector{
		DoNotReference: true,
	}

	switch typeName {
	case "response":
		schema = reflector.Reflect(&schemaResponse{})
		schema.Title = "SnipeResponse"
		schema.Description = "Top-level response for all snipe commands"
	case "result":
		schema = reflector.Reflect(&output.Result{})
		schema.Title = "SnipeResult"
		schema.Description = "Individual navigation result"
	case "meta":
		schema = reflector.Reflect(&output.Meta{})
		schema.Title = "SnipeMeta"
		schema.Description = "Response metadata"
	case "error":
		schema = reflector.Reflect(&output.Error{})
		schema.Title = "SnipeError"
		schema.Description = "Error response structure"
	default:
		return fmt.Errorf("unknown type: %s (valid: response, result, meta, error)", typeName)
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(schema)
}
