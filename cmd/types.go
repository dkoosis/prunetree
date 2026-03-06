package cmd

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/dkoosis/snipe/internal/output"
	"github.com/dkoosis/snipe/internal/query"
)

var (
	typesAt string
)

var typesCmd = &cobra.Command{
	Use:     "types [type-name]",
	Short:   "Show type relationships",
	GroupID: "advanced",
	Long: `Displays type information including methods, embeds, and fields.

Output includes:
  - methods: All methods with this type as receiver
  - embeds: Embedded types (v1: best-effort detection)
  - fields: Struct fields with types and tags
  - implements: Interface satisfaction (v1: partial/future)

Note: Full interface satisfaction detection requires type-checker
analysis and is planned for v2.

Examples:
  snipe types Store                    # By name
  snipe types --at internal/store.go:42  # By position
  snipe types query.SymbolRow          # Qualified name`,
	Args: cobra.MaximumNArgs(1),
	RunE: runTypes,
}

func init() {
	typesCmd.Flags().StringVar(&typesAt, "at", "", "Position to look up (file:line:col)")
	rootCmd.AddCommand(typesCmd)
}

func runTypes(cmd *cobra.Command, args []string) error {
	start := time.Now()

	compact, _, _, _, _, _ := GetOutputConfig()
	w := output.NewWriter(os.Stdout, compact)

	if len(args) == 0 && typesAt == "" {
		return w.WriteError("types", &output.Error{
			Code:    output.ErrInternal,
			Message: "provide a type name or --at position",
		})
	}

	s, dir, err := OpenStore(w, "types")
	if err != nil {
		return err
	}
	defer s.Close()

	var symbolID string
	var queryInfo map[string]string

	if typesAt != "" {
		pos, err := query.ParsePosition(typesAt)
		if err != nil {
			return w.WriteError("types", &output.Error{
				Code:    output.ErrInternal,
				Message: err.Error(),
			})
		}

		if !filepath.IsAbs(pos.File) {
			pos.File = filepath.Join(dir, pos.File)
		}

		symbolID, err = query.ResolvePosition(s.DB(), pos)
		if err != nil {
			return w.WriteError("types", &output.Error{
				Code:    output.ErrNotFound,
				Message: err.Error(),
			})
		}
		queryInfo = map[string]string{"at": typesAt}
	} else {
		name := args[0]

		// Check for hex ID
		if len(name) == 16 {
			if _, err := hex.DecodeString(name); err == nil {
				symbolID = name
				queryInfo = map[string]string{"id": name}
				goto getTypes
			}
		}

		// Check for file:Symbol syntax
		if idx := strings.LastIndex(name, ":"); idx > 0 && !strings.Contains(name[idx:], "/") {
			filePart := name[:idx]
			symbolPart := name[idx+1:]
			if symbolPart != "" && !strings.Contains(symbolPart, ":") {
				symbols, err := query.LookupByNameInFile(s.DB(), symbolPart, filePart)
				if err != nil {
					return w.WriteError("types", &output.Error{
						Code:    output.ErrInternal,
						Message: err.Error(),
					})
				}
				if len(symbols) == 1 {
					symbolID = symbols[0].ID
					queryInfo = map[string]string{"symbol": symbolPart, "file": filePart}
					goto getTypes
				} else if len(symbols) > 1 {
					candidates := make([]output.Candidate, len(symbols))
					for i, sym := range symbols {
						candidates[i] = sym.ToCandidate()
					}
					return w.WriteError("types", output.NewAmbiguousError(name, candidates))
				}
			}
		}

		// Regular lookup
		symbols, err := query.LookupByName(s.DB(), name)
		if err != nil {
			return w.WriteError("types", &output.Error{
				Code:    output.ErrInternal,
				Message: err.Error(),
			})
		}

		if len(symbols) == 0 {
			maxDist := query.DefaultMaxDistance(name)
			suggestions, err := query.FindSimilarSymbols(s.DB(), name, maxDist, 3)
			if err != nil {
				return w.WriteError("types", output.NewNotFoundError(name))
			}
			return w.WriteError("types", output.NewNotFoundError(name, suggestions...))
		}

		// Filter to type-like symbols
		var typeSymbols []query.SymbolRow
		for _, sym := range symbols {
			if isTypeKind(sym.Kind) {
				typeSymbols = append(typeSymbols, sym)
			}
		}

		if len(typeSymbols) == 0 {
			return w.WriteError("types", &output.Error{
				Code:    output.ErrNotFound,
				Message: "'" + name + "' is not a type (found " + symbols[0].Kind + ")",
			})
		}

		if len(typeSymbols) > 1 {
			candidates := make([]output.Candidate, len(typeSymbols))
			for i, sym := range typeSymbols {
				candidates[i] = sym.ToCandidate()
			}
			return w.WriteError("types", output.NewAmbiguousError(name, candidates))
		}

		symbolID = typeSymbols[0].ID
		queryInfo = map[string]string{"symbol": name}
	}

getTypes:
	typeInfo, err := query.GetTypeInfo(s.DB(), symbolID)
	if err != nil {
		return w.WriteError("types", &output.Error{
			Code:    output.ErrInternal,
			Message: err.Error(),
		})
	}

	// Build response
	result := query.TypeResult{
		Symbol:    typeInfo.Symbol.Name,
		Kind:      typeInfo.Symbol.Kind,
		File:      typeInfo.Symbol.FilePathRel,
		Signature: typeInfo.Symbol.Signature.String,
		Doc:       typeInfo.Symbol.Doc.String,
		Implements: query.ImplementsInfoOut{
			Status: typeInfo.Implements.Status,
			Note:   typeInfo.Implements.Note,
		},
	}

	// Add methods
	for _, m := range typeInfo.Methods {
		result.Methods = append(result.Methods, query.TypeMethodOut{
			Name:      m.Name,
			Signature: m.Signature,
			File:      m.File,
			Line:      m.Line,
		})
	}

	// Add embeds
	for _, e := range typeInfo.Embeds {
		result.Embeds = append(result.Embeds, query.TypeEmbedOut{
			TypeName:  e.TypeName,
			FieldName: e.FieldName,
			File:      e.File,
		})
	}

	// Add fields
	for _, f := range typeInfo.Fields {
		result.Fields = append(result.Fields, query.TypeFieldOut{
			Name:     f.Name,
			TypeExpr: f.TypeExpr,
			Tag:      f.Tag,
		})
	}

	staleFiles := query.CheckPathStaleness(s.DB(), dir, []string{typeInfo.Symbol.FilePath})

	resp := output.Response[query.TypeResult]{
		Protocol: output.ProtocolVersion,
		Ok:       true,
		Results:  []query.TypeResult{result},
		Meta: output.Meta{
			Command:    "types",
			Query:      queryInfo,
			RepoRoot:   dir,
			IndexState: query.CheckIndexState(s.DB(), dir, Version),
			Ms:         time.Since(start).Milliseconds(),
			Total:      1,
			StaleFiles: staleFiles,
		},
	}

	return w.WriteResponse(resp)
}
