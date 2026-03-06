package cmd

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/dkoosis/snipe/internal/edit"
	"github.com/dkoosis/snipe/internal/output"
	"github.com/dkoosis/snipe/internal/query"
)

var (
	editOperation   string
	editNewCode     string
	editNewCodeFile string
	editApply       bool
	editBatch       bool
	editAt          string
)

// EditResponse contains the result of an edit operation
type EditResponse struct {
	File         string `json:"file"`
	Symbol       string `json:"symbol"`
	Operation    string `json:"operation"`
	OriginalCode string `json:"original_code,omitempty"`
	NewCode      string `json:"new_code,omitempty"`
	Diff         string `json:"diff,omitempty"`
	LineStart    int    `json:"line_start"`
	LineEnd      int    `json:"line_end"`
	NewLineEnd   int    `json:"new_line_end,omitempty"`
	Applied      bool   `json:"applied"`
}

// BatchEditRequest for batch mode
type BatchEditRequest struct {
	Symbol    string `json:"symbol"`
	Operation string `json:"operation"`
	NewCode   string `json:"new_code"`
	File      string `json:"file,omitempty"`
}

var editCmd = &cobra.Command{
	Use:     "edit [symbol]",
	Short:   "AST-aware code editing",
	GroupID: "advanced",
	Long: `Performs AST-aware edits on Go source code.

Operations:
  replace_body   Replace function/method body (keeps signature)
  replace_full   Replace entire symbol declaration
  insert_after   Add code after symbol
  insert_before  Add code before symbol

Examples:
  snipe edit ProcessOrder --operation replace_body --new-code "return nil"
  snipe edit Handler --operation replace_full --new-code-file handler.go.new
  snipe edit Config --operation insert_after --new-code "func NewConfig() Config { return Config{} }"

By default, shows a preview without modifying files. Use --apply to write changes.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runEdit,
}

func init() {
	editCmd.Flags().StringVar(&editOperation, "operation", "", "Edit operation: replace_body, replace_full, insert_after, insert_before")
	editCmd.Flags().StringVar(&editNewCode, "new-code", "", "New code to insert/replace")
	editCmd.Flags().StringVar(&editNewCodeFile, "new-code-file", "", "File containing new code")
	editCmd.Flags().BoolVar(&editApply, "apply", false, "Apply changes (default: preview only)")
	editCmd.Flags().BoolVar(&editBatch, "batch", false, "Read batch operations from stdin")
	editCmd.Flags().StringVar(&editAt, "at", "", "Position to edit (file:line:col)")
	rootCmd.AddCommand(editCmd)
}

func runEdit(cmd *cobra.Command, args []string) error {
	start := time.Now()

	compact, _, _, _, _, _ := GetOutputConfig()
	w := output.NewWriter(os.Stdout, compact)

	// Handle batch mode
	if editBatch {
		return runBatchEdit(w, start)
	}

	// Single edit mode - need symbol name or --at position
	if len(args) == 0 && editAt == "" {
		return w.WriteError("edit", &output.Error{
			Code:    output.ErrInternal,
			Message: "provide a symbol name or --at position",
		})
	}

	if editOperation == "" {
		return w.WriteError("edit", &output.Error{
			Code:    output.ErrInternal,
			Message: "provide --operation: replace_body, replace_full, insert_after, insert_before",
		})
	}

	// Get new code
	newCode := editNewCode
	if editNewCodeFile != "" {
		data, err := os.ReadFile(editNewCodeFile) // #nosec G304 -- CLI tool accepts user-specified file paths
		if err != nil {
			return w.WriteError("edit", &output.Error{
				Code:    output.ErrInternal,
				Message: "read new-code-file: " + err.Error(),
			})
		}
		newCode = string(data)
	}

	if newCode == "" {
		return w.WriteError("edit", &output.Error{
			Code:    output.ErrInternal,
			Message: "provide --new-code or --new-code-file",
		})
	}

	// Find repo root and open store
	s, dir, err := OpenStore(w, "edit")
	if err != nil {
		return err
	}
	defer s.Close()

	// Resolve symbol
	var filePath string
	var symbolName string

	// Handle --at position resolution
	if editAt != "" {
		pos, err := query.ParsePosition(editAt)
		if err != nil {
			return w.WriteError("edit", &output.Error{
				Code:    output.ErrInternal,
				Message: err.Error(),
			})
		}

		// Make path absolute if relative
		if !filepath.IsAbs(pos.File) {
			pos.File = filepath.Join(dir, pos.File)
		}

		symbolID, err := query.ResolvePosition(s.DB(), pos)
		if err != nil {
			return w.WriteError("edit", &output.Error{
				Code:    output.ErrNotFound,
				Message: err.Error(),
			})
		}

		sym, err := query.LookupByID(s.DB(), symbolID)
		if err != nil || sym == nil {
			return w.WriteError("edit", &output.Error{
				Code:    output.ErrNotFound,
				Message: "symbol not found at position",
			})
		}

		filePath = sym.FilePath
		symbolName = sym.Name
		goto doEdit
	}

	// Resolve by name - block scopes name to avoid goto issues
	{
		name := args[0]

		// Check if input is a hex ID
		if len(name) == 16 {
			if _, err := hex.DecodeString(name); err == nil {
				sym, err := query.LookupByID(s.DB(), name)
				if err != nil || sym == nil {
					return w.WriteError("edit", &output.Error{
						Code:    output.ErrNotFound,
						Message: "symbol not found: " + name,
					})
				}
				filePath = sym.FilePath
				symbolName = sym.Name
				goto doEdit
			}
		}

		// Check for file:Symbol syntax
		if idx := strings.LastIndex(name, ":"); idx > 0 {
			possibleFile := name[:idx]
			possibleSymbol := name[idx+1:]
			if !strings.Contains(possibleSymbol, ":") {
				symbols, err := query.LookupByNameInFile(s.DB(), possibleSymbol, possibleFile)
				if err == nil && len(symbols) == 1 {
					filePath = symbols[0].FilePath
					symbolName = symbols[0].Name
					goto doEdit
				}
			}
		}

		// Regular name lookup
		symbols, err := query.LookupByName(s.DB(), name)
		if err != nil {
			return w.WriteError("edit", &output.Error{
				Code:    output.ErrInternal,
				Message: err.Error(),
			})
		}

		if len(symbols) == 0 {
			return w.WriteError("edit", output.NewNotFoundError(name))
		}

		if len(symbols) > 1 {
			candidates := make([]output.Candidate, len(symbols))
			for i, sym := range symbols {
				candidates[i] = sym.ToCandidate()
			}
			return w.WriteError("edit", output.NewAmbiguousError(name, candidates))
		}

		filePath = symbols[0].FilePath
		symbolName = symbols[0].Name
	}

doEdit:
	// Make path absolute
	if !filepath.IsAbs(filePath) {
		filePath = filepath.Join(dir, filePath)
	}

	// Create edit request
	req := edit.Request{
		File:      filePath,
		Symbol:    symbolName,
		Operation: edit.Operation(editOperation),
		NewCode:   newCode,
	}

	var result *edit.Result
	if editApply {
		result, err = edit.ApplyAndWrite(req)
	} else {
		result, err = edit.Apply(req)
	}

	if err != nil {
		return w.WriteError("edit", &output.Error{
			Code:    output.ErrInternal,
			Message: err.Error(),
		})
	}

	// Build response
	relPath, _ := filepath.Rel(dir, filePath)
	if relPath == "" {
		relPath = filePath
	}

	editResp := EditResponse{
		File:         relPath,
		Symbol:       symbolName,
		Operation:    editOperation,
		OriginalCode: result.OriginalCode,
		NewCode:      result.NewCode,
		Diff:         result.Diff,
		LineStart:    result.LineStart,
		LineEnd:      result.LineEnd,
		NewLineEnd:   result.NewLineEnd,
		Applied:      result.Applied,
	}

	resp := output.Response[EditResponse]{
		Protocol: output.ProtocolVersion,
		Ok:       true,
		Results:  []EditResponse{editResp},
		Meta: output.Meta{
			Command:  "edit",
			Query:    map[string]string{"symbol": symbolName, "operation": editOperation},
			RepoRoot: dir,
			Ms:       time.Since(start).Milliseconds(),
			Total:    1,
		},
	}

	return w.WriteResponse(resp)
}

func runBatchEdit(w *output.Writer, start time.Time) error {
	// Read batch operations from stdin
	var requests []BatchEditRequest
	if err := json.NewDecoder(os.Stdin).Decode(&requests); err != nil {
		return w.WriteError("edit", &output.Error{
			Code:    output.ErrInternal,
			Message: "parse batch input: " + err.Error(),
		})
	}

	if len(requests) == 0 {
		return w.WriteError("edit", &output.Error{
			Code:    output.ErrInternal,
			Message: "no operations in batch",
		})
	}

	s, dir, err := OpenStore(w, "edit")
	if err != nil {
		return err
	}
	defer s.Close()

	results := make([]EditResponse, 0, len(requests))
	var degraded []string

	for _, req := range requests {
		// Resolve symbol
		symbols, err := query.LookupByName(s.DB(), req.Symbol)
		if err != nil || len(symbols) == 0 {
			degraded = append(degraded, fmt.Sprintf("symbol_not_found:%s", req.Symbol))
			continue
		}
		if len(symbols) > 1 && req.File == "" {
			degraded = append(degraded, fmt.Sprintf("ambiguous:%s", req.Symbol))
			continue
		}

		sym := symbols[0]
		if req.File != "" {
			// Find matching file
			found := false
			for _, s := range symbols {
				if strings.Contains(s.FilePath, req.File) || strings.Contains(s.FilePathRel, req.File) {
					sym = s
					found = true
					break
				}
			}
			if !found {
				degraded = append(degraded, fmt.Sprintf("file_not_matched:%s:%s", req.Symbol, req.File))
				continue
			}
		}

		filePath := sym.FilePath
		if !filepath.IsAbs(filePath) {
			filePath = filepath.Join(dir, filePath)
		}

		editReq := edit.Request{
			File:      filePath,
			Symbol:    sym.Name,
			Operation: edit.Operation(req.Operation),
			NewCode:   req.NewCode,
		}

		var result *edit.Result
		if editApply {
			result, err = edit.ApplyAndWrite(editReq)
		} else {
			result, err = edit.Apply(editReq)
		}

		if err != nil {
			degraded = append(degraded, fmt.Sprintf("edit_failed:%s:%s", req.Symbol, err.Error()))
			continue
		}

		relPath, _ := filepath.Rel(dir, filePath)
		if relPath == "" {
			relPath = filePath
		}

		results = append(results, EditResponse{
			File:         relPath,
			Symbol:       sym.Name,
			Operation:    req.Operation,
			OriginalCode: result.OriginalCode,
			NewCode:      result.NewCode,
			Diff:         result.Diff,
			LineStart:    result.LineStart,
			LineEnd:      result.LineEnd,
			NewLineEnd:   result.NewLineEnd,
			Applied:      result.Applied,
		})
	}

	resp := output.Response[EditResponse]{
		Protocol: output.ProtocolVersion,
		Ok:       true,
		Results:  results,
		Meta: output.Meta{
			Command:  "edit",
			Query:    map[string]string{"mode": "batch"},
			RepoRoot: dir,
			Degraded: uniqueStrings(degraded),
			Ms:       time.Since(start).Milliseconds(),
			Total:    len(results),
		},
	}

	return w.WriteResponse(resp)
}
