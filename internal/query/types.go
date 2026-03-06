package query

import (
	"database/sql"
	"fmt"

	"github.com/dkoosis/snipe/internal/output"
)

// TypeInfo contains type relationship information.
type TypeInfo struct {
	Symbol  SymbolRow
	Methods []MethodInfo
	Embeds  []EmbedInfo
	Fields  []FieldInfo
	// Implements is marked as partial - full interface satisfaction
	// requires type-checker analysis which is a v2 feature.
	Implements ImplementsInfo
}

// MethodInfo describes a method on a type.
type MethodInfo struct {
	ID        string
	Name      string
	Signature string
	Receiver  string
	File      string
	Line      int
	Doc       string
}

// EmbedInfo describes an embedded type.
type EmbedInfo struct {
	ID        string
	TypeName  string
	FieldName string // Anonymous if empty
	File      string
	Line      int
}

// FieldInfo describes a struct field.
type FieldInfo struct {
	Name     string
	TypeExpr string
	Tag      string
	Line     int
}

// ImplementsInfo tracks interface implementation status.
// V1: This is always partial as we don't have full type-checking.
type ImplementsInfo struct {
	Status     string   `json:"status"` // "partial" or "unknown"
	Interfaces []string `json:"interfaces,omitempty"`
	Note       string   `json:"note,omitempty"`
}

// TypeResult is the output format for the types command.
type TypeResult struct {
	Symbol     string            `json:"symbol"`
	Kind       string            `json:"kind"`
	File       string            `json:"file"`
	Signature  string            `json:"signature,omitempty"`
	Doc        string            `json:"doc,omitempty"`
	Methods    []TypeMethodOut   `json:"methods,omitempty"`
	Embeds     []TypeEmbedOut    `json:"embeds,omitempty"`
	Fields     []TypeFieldOut    `json:"fields,omitempty"`
	Implements ImplementsInfoOut `json:"implements"`
}

// TypeMethodOut is the output format for a method.
type TypeMethodOut struct {
	Name      string `json:"name"`
	Signature string `json:"signature"`
	File      string `json:"file"`
	Line      int    `json:"line"`
}

// TypeEmbedOut is the output format for an embedded type.
type TypeEmbedOut struct {
	TypeName  string `json:"type_name"`
	FieldName string `json:"field_name,omitempty"`
	File      string `json:"file,omitempty"`
}

// TypeFieldOut is the output format for a field.
type TypeFieldOut struct {
	Name     string `json:"name"`
	TypeExpr string `json:"type"`
	Tag      string `json:"tag,omitempty"`
}

// ImplementsInfoOut is the output format for implements info.
type ImplementsInfoOut struct {
	Status string `json:"status"`
	Note   string `json:"note,omitempty"`
}

// GetTypeInfo retrieves type information for a symbol.
func GetTypeInfo(db *sql.DB, symbolID string) (*TypeInfo, error) {
	sym, err := LookupByID(db, symbolID)
	if err != nil {
		return nil, err
	}
	if sym == nil {
		return nil, fmt.Errorf("symbol not found: %s", symbolID)
	}

	// Only struct, interface, type alias kinds are valid
	if sym.Kind != "struct" && sym.Kind != "interface" && sym.Kind != "type" {
		return nil, fmt.Errorf("types command requires struct/interface/type, got %s", sym.Kind)
	}

	info := &TypeInfo{
		Symbol: *sym,
		Implements: ImplementsInfo{
			Status: "partial",
			Note:   "Full interface satisfaction requires type-checker (v2 feature)",
		},
	}

	// Get methods
	methods, err := GetMethodsForType(db, sym.Name, sym.PkgPath)
	if err == nil {
		info.Methods = methods
	}

	// Get embeds (from refs where the reference is in this type's definition)
	embeds, err := getEmbedsForType(db, symbolID, sym.FilePath, sym.LineStart, sym.LineEnd)
	if err == nil {
		info.Embeds = embeds
	}

	// Get fields (from symbols with kind=field in this type)
	fields, err := getFieldsForType(db, symbolID, sym.FilePath, sym.LineStart, sym.LineEnd)
	if err == nil {
		info.Fields = fields
	}

	return info, nil
}

// GetMethodsForType finds all methods for a type by receiver matching.
func GetMethodsForType(db *sql.DB, typeName, _ string) ([]MethodInfo, error) {
	// Match both value and pointer receivers
	valueRecv := "(" + typeName + ")"
	ptrRecv := "(*" + typeName + ")"

	rows, err := db.Query(`
		SELECT id, name, signature, receiver, file_path_rel, line_start, doc
		FROM symbols
		WHERE kind = 'method'
		  AND (receiver = ? OR receiver = ?)
		ORDER BY name
	`, valueRecv, ptrRecv)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var methods []MethodInfo
	for rows.Next() {
		var m MethodInfo
		var sig, recv, doc sql.NullString
		if err := rows.Scan(&m.ID, &m.Name, &sig, &recv, &m.File, &m.Line, &doc); err != nil {
			return nil, err
		}
		m.Signature = sig.String
		m.Receiver = recv.String
		m.Doc = doc.String
		methods = append(methods, m)
	}
	return methods, rows.Err()
}

// getEmbedsForType finds embedded types within a struct definition.
// This uses a heuristic: look for type references within the struct's line range
// that are at field positions (based on column position patterns).
func getEmbedsForType(db *sql.DB, _, filePath string, lineStart, lineEnd int) ([]EmbedInfo, error) {
	// Query refs to other types within this type's definition
	rows, err := db.Query(`
		SELECT DISTINCT r.symbol_id, s.name, s.kind, r.file_path_rel, r.line
		FROM refs r
		JOIN symbols s ON r.symbol_id = s.id
		WHERE r.file_path = ?
		  AND r.line > ? AND r.line < ?
		  AND s.kind IN ('struct', 'interface', 'type')
		ORDER BY r.line
	`, filePath, lineStart, lineEnd)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var embeds []EmbedInfo
	for rows.Next() {
		var e EmbedInfo
		var kind string
		if err := rows.Scan(&e.ID, &e.TypeName, &kind, &e.File, &e.Line); err != nil {
			return nil, err
		}
		embeds = append(embeds, e)
	}
	return embeds, rows.Err()
}

// getFieldsForType finds fields within a struct definition.
func getFieldsForType(db *sql.DB, _, filePath string, lineStart, lineEnd int) ([]FieldInfo, error) {
	// Query field symbols within the struct's line range
	rows, err := db.Query(`
		SELECT name, signature, line_start
		FROM symbols
		WHERE file_path = ?
		  AND kind = 'field'
		  AND line_start > ? AND line_start < ?
		ORDER BY line_start
	`, filePath, lineStart, lineEnd)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var fields []FieldInfo
	for rows.Next() {
		var f FieldInfo
		var sig sql.NullString
		if err := rows.Scan(&f.Name, &sig, &f.Line); err != nil {
			return nil, err
		}
		f.TypeExpr = sig.String
		fields = append(fields, f)
	}
	return fields, rows.Err()
}

// ToTypeResult converts TypeInfo to output format.
func (ti *TypeInfo) ToTypeResult() output.Result {
	// Build a custom result for type info
	filePath := ti.Symbol.FilePathRel
	if filePath == "" {
		filePath = ti.Symbol.FilePath
	}

	return output.Result{
		ID:    ti.Symbol.ID,
		Name:  ti.Symbol.Name,
		Kind:  ti.Symbol.Kind,
		File:  filePath,
		Match: ti.Symbol.Signature.String,
		Range: output.Range{
			Start: output.Position{Line: ti.Symbol.LineStart, Col: ti.Symbol.ColStart},
			End:   output.Position{Line: ti.Symbol.LineEnd, Col: ti.Symbol.ColEnd},
		},
	}
}
