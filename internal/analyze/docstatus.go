package analyze

import (
	"go/ast"
	"regexp"
	"strings"

	"github.com/dkoosis/snipe/internal/output"
)

// CheckDocStatus evaluates documentation freshness for a function.
// Uses strict criteria: only marks stale when doc references params/returns that don't exist.
func CheckDocStatus(fn *ast.FuncDecl, doc string) output.DocStatus {
	if doc == "" {
		return output.DocStatus{
			Status: output.DocMissing,
		}
	}

	// Extract actual parameter names from signature
	actualParams := extractParamNames(fn)
	actualReturns := extractReturnNames(fn)

	// Extract referenced identifiers from doc
	docRefs := extractDocReferences(doc)

	// Check for stale references
	var reasons []string

	for _, ref := range docRefs {
		// Check if this looks like a parameter reference
		if isParamLike(ref) {
			if !contains(actualParams, ref) && !contains(actualReturns, ref) {
				// This might be a renamed/removed parameter
				if couldBeParam(ref, actualParams) {
					reasons = append(reasons, "param '"+ref+"' not in signature")
				}
			}
		}
	}

	if len(reasons) > 0 {
		return output.DocStatus{
			Status:  output.DocStale,
			Reasons: reasons,
		}
	}

	return output.DocStatus{
		Status: output.DocFresh,
	}
}

// extractParamNames returns all parameter names from a function signature.
func extractParamNames(fn *ast.FuncDecl) []string {
	var names []string

	if fn.Type.Params != nil {
		for _, field := range fn.Type.Params.List {
			for _, name := range field.Names {
				names = append(names, name.Name)
			}
		}
	}

	// Include receiver if present
	if fn.Recv != nil {
		for _, field := range fn.Recv.List {
			for _, name := range field.Names {
				names = append(names, name.Name)
			}
		}
	}

	return names
}

// extractReturnNames returns named return variable names.
func extractReturnNames(fn *ast.FuncDecl) []string {
	var names []string

	if fn.Type.Results != nil {
		for _, field := range fn.Type.Results.List {
			for _, name := range field.Names {
				names = append(names, name.Name)
			}
		}
	}

	return names
}

// extractDocReferences extracts potential parameter/variable references from doc text.
// Looks for common documentation patterns.
func extractDocReferences(doc string) []string {
	var refs []string
	seen := make(map[string]bool)

	// Pattern 1: backtick-quoted identifiers like `paramName`
	backtickRe := regexp.MustCompile("`([a-zA-Z_][a-zA-Z0-9_]*)`")
	for _, match := range backtickRe.FindAllStringSubmatch(doc, -1) {
		if len(match) > 1 && !seen[match[1]] {
			refs = append(refs, match[1])
			seen[match[1]] = true
		}
	}

	// Pattern 2: "the X parameter" or "X parameter"
	paramRe := regexp.MustCompile(`\b([a-zA-Z_][a-zA-Z0-9_]*)\s+parameter\b`)
	for _, match := range paramRe.FindAllStringSubmatch(doc, -1) {
		if len(match) > 1 && !seen[match[1]] && match[1] != "the" {
			refs = append(refs, match[1])
			seen[match[1]] = true
		}
	}

	// Pattern 3: "returns X" where X is an identifier
	returnsRe := regexp.MustCompile(`returns?\s+([a-zA-Z_][a-zA-Z0-9_]*)\b`)
	for _, match := range returnsRe.FindAllStringSubmatch(strings.ToLower(doc), -1) {
		if len(match) > 1 && !seen[match[1]] {
			// Only include if it looks like a variable name
			if isVarLike(match[1]) {
				refs = append(refs, match[1])
				seen[match[1]] = true
			}
		}
	}

	return refs
}

// isParamLike returns true if the string looks like a parameter name.
func isParamLike(s string) bool {
	// Single letter or camelCase starting with lowercase
	if len(s) == 0 {
		return false
	}

	// Check first char is lowercase letter
	if s[0] < 'a' || s[0] > 'z' {
		return false
	}

	return true
}

// isVarLike returns true if the string looks like a variable name (not a type/keyword).
func isVarLike(s string) bool {
	// Common non-variable words that appear after "returns"
	nonVars := map[string]bool{
		"true": true, "false": true, "nil": true, "error": true,
		"the": true, "a": true, "an": true, "if": true, "whether": true,
		"bool": true, "int": true, "string": true, "slice": true,
	}
	return !nonVars[strings.ToLower(s)]
}

// couldBeParam returns true if ref could plausibly be a parameter reference.
// Uses heuristics to avoid false positives on common words.
func couldBeParam(ref string, actualParams []string) bool {
	// Skip very short references that could be articles/pronouns
	if len(ref) < 2 {
		return false
	}

	// Skip common words that appear in docs
	commonWords := map[string]bool{
		"the": true, "is": true, "it": true, "to": true, "for": true,
		"this": true, "that": true, "with": true, "from": true, "by": true,
		"returns": true, "takes": true, "uses": true, "creates": true,
		"new": true, "old": true, "first": true, "last": true,
		"error": true, "err": true, "nil": true, "true": true, "false": true,
	}
	if commonWords[strings.ToLower(ref)] {
		return false
	}

	// If we have actual params and ref is similar to one, it's likely stale
	for _, p := range actualParams {
		if levenshteinDistance(strings.ToLower(ref), strings.ToLower(p)) <= 2 {
			// Similar but not exact - could be a rename
			if ref != p {
				return true
			}
		}
	}

	return true
}

// contains checks if slice contains string.
func contains(slice []string, s string) bool {
	for _, item := range slice {
		if item == s {
			return true
		}
	}
	return false
}

// levenshteinDistance computes edit distance between two strings.
func levenshteinDistance(a, b string) int {
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}

	if len(a) > len(b) {
		a, b = b, a
	}

	prev := make([]int, len(a)+1)
	curr := make([]int, len(a)+1)

	for i := range prev {
		prev[i] = i
	}

	for j := 1; j <= len(b); j++ {
		curr[0] = j
		for i := 1; i <= len(a); i++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[i] = min(
				prev[i]+1,      // deletion
				curr[i-1]+1,    // insertion
				prev[i-1]+cost, // substitution
			)
		}
		prev, curr = curr, prev
	}

	return prev[len(a)]
}

// ExtractPurpose extracts the purpose from doc comment or generates from signature.
func ExtractPurpose(fn *ast.FuncDecl, doc string) (purpose string, source output.PurposeSource) {
	// Priority 1: Doc comment first sentence
	if doc != "" {
		purpose = extractFirstSentence(doc)
		if purpose != "" {
			return purpose, output.PurposeFromDoc
		}
	}

	// Priority 2: Generate template from signature
	purpose = generatePurposeTemplate(fn)
	if purpose != "" {
		return purpose, output.PurposeFromTemplate
	}

	return "", output.PurposeMissing
}

// extractFirstSentence extracts the first sentence from a doc comment.
func extractFirstSentence(doc string) string {
	// Remove leading slashes and stars from comment markers
	doc = strings.TrimSpace(doc)

	// Find first sentence (ends with . ! or ? followed by space or end)
	for i := 0; i < len(doc); i++ {
		if doc[i] == '.' || doc[i] == '!' || doc[i] == '?' {
			if i+1 >= len(doc) || doc[i+1] == ' ' || doc[i+1] == '\n' {
				sentence := strings.TrimSpace(doc[:i+1])
				// Cap length
				if len(sentence) > 200 {
					sentence = sentence[:197] + "..."
				}
				return sentence
			}
		}
	}

	// No sentence end found, take first line
	if idx := strings.Index(doc, "\n"); idx > 0 {
		return strings.TrimSpace(doc[:idx])
	}

	if len(doc) > 200 {
		return doc[:197] + "..."
	}
	return doc
}

// generatePurposeTemplate creates a purpose string from function signature patterns.
func generatePurposeTemplate(fn *ast.FuncDecl) string {
	name := fn.Name.Name

	// Check for common prefixes
	prefixes := map[string]string{
		"New":      "Creates a new ",
		"Make":     "Creates ",
		"Create":   "Creates ",
		"Get":      "Retrieves ",
		"Set":      "Sets ",
		"Update":   "Updates ",
		"Delete":   "Deletes ",
		"Remove":   "Removes ",
		"Add":      "Adds ",
		"Is":       "Checks if ",
		"Has":      "Checks if has ",
		"Can":      "Checks if can ",
		"Should":   "Determines if should ",
		"Must":     "Ensures ",
		"Validate": "Validates ",
		"Check":    "Checks ",
		"Parse":    "Parses ",
		"Format":   "Formats ",
		"Convert":  "Converts ",
		"Open":     "Opens ",
		"Close":    "Closes ",
		"Read":     "Reads ",
		"Write":    "Writes ",
		"Load":     "Loads ",
		"Save":     "Saves ",
		"Find":     "Finds ",
		"Search":   "Searches for ",
		"Init":     "Initializes ",
		"Start":    "Starts ",
		"Stop":     "Stops ",
		"Run":      "Runs ",
		"Execute":  "Executes ",
		"Handle":   "Handles ",
		"Process":  "Processes ",
	}

	for prefix, verb := range prefixes {
		if strings.HasPrefix(name, prefix) {
			rest := name[len(prefix):]
			if rest == "" {
				rest = "operation"
			} else {
				rest = camelToWords(rest)
			}
			return verb + rest + "."
		}
	}

	// Check for method receiver context
	if fn.Recv != nil && len(fn.Recv.List) > 0 {
		return "Method on " + extractTypeName(fn.Recv.List[0].Type) + "."
	}

	return ""
}

// camelToWords converts CamelCase to "camel case".
func camelToWords(s string) string {
	var result strings.Builder
	for i, r := range s {
		if i > 0 && r >= 'A' && r <= 'Z' {
			result.WriteRune(' ')
		}
		result.WriteRune(r)
	}
	return strings.ToLower(result.String())
}

// extractTypeName extracts the type name from an expression.
func extractTypeName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return "*" + extractTypeName(t.X)
	case *ast.SelectorExpr:
		return extractTypeName(t.X) + "." + t.Sel.Name
	}
	return "receiver"
}
