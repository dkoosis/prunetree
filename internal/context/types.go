// Package context generates Claude-optimized project context from snipe index.
package context

// ProjectContext is the top-level output structure for snipe context command.
type ProjectContext struct {
	Project      Project      `json:"project" yaml:"project"`
	Architecture Architecture `json:"architecture" yaml:"architecture"`
	Files        Files        `json:"files,omitempty" yaml:"files,omitempty"`
	Symbols      Symbols      `json:"symbols" yaml:"symbols"`
	Meta         Meta         `json:"meta" yaml:"meta"`
}

// BootContext is a minimal context for LLM boot sequences (~2000 tokens).
type BootContext struct {
	Project     string      `json:"project" yaml:"project"`
	Lang        string      `json:"lang" yaml:"lang"`
	Build       string      `json:"build" yaml:"build"`
	Test        string      `json:"test" yaml:"test"`
	BuildInfo   *BuildInfo  `json:"build_info,omitempty" yaml:"build_info,omitempty"`
	EntryPoints []string    `json:"entry_points" yaml:"entry_points"`
	KeySymbols  []SymbolRef `json:"key_symbols" yaml:"key_symbols"`
	ActiveWork  *ActiveWork `json:"active_work,omitempty" yaml:"active_work,omitempty"`
	Commit      string      `json:"commit" yaml:"commit"`

	// Enhanced fields (Phase 2)
	BootViews *BootViews   `json:"boot_views,omitempty" yaml:"boot_views,omitempty"`
	Packages  []PackageRef `json:"packages,omitempty" yaml:"packages,omitempty"`

	// Phase 4: Architecture summary
	ArchSummary *ArchSummary `json:"arch_summary,omitempty" yaml:"arch_summary,omitempty"`

	// Conventions detected from index
	Conventions *Conventions `json:"conventions,omitempty" yaml:"conventions,omitempty"`
}

// BootViews contains the three orientation views for LLM boot sequences.
type BootViews struct {
	EntryPointDetails []EntryPointRef     `json:"entry_point_details,omitempty" yaml:"entry_point_details,omitempty"`
	PrimaryFlows      []string            `json:"primary_flows,omitempty" yaml:"primary_flows,omitempty"`
	ChangeBoundaries  map[string][]string `json:"change_boundaries,omitempty" yaml:"change_boundaries,omitempty"`
	InterfaceMap      []InterfaceEntry    `json:"interface_map,omitempty" yaml:"interface_map,omitempty"`
}

// InterfaceEntry describes a key interface and its known implementors in the repo.
type InterfaceEntry struct {
	Interface    string           `json:"interface" yaml:"interface"`
	File         string           `json:"file" yaml:"file"`
	Line         int              `json:"line" yaml:"line"`
	Methods      []string         `json:"methods" yaml:"methods"`
	Implementors []ImplementorRef `json:"implementors" yaml:"implementors"`
}

// ImplementorRef is a lightweight reference to a type implementing an interface.
type ImplementorRef struct {
	Name string `json:"name" yaml:"name"`
	File string `json:"file" yaml:"file"`
	Line int    `json:"line" yaml:"line"`
}

// ArchSummary provides a high-level architecture overview grounded in call graph data.
// This is used for Phase 4 context enrichment to help LLMs understand codebase structure.
type ArchSummary struct {
	// Spine contains primary call flows from entry points (from static analysis)
	Spine []string `json:"spine" yaml:"spine"`
	// Components lists packages with their purposes
	Components []PackagePurpose `json:"components" yaml:"components"`
	// Edges contains top cross-package call relationships with counts
	Edges []CrossPackageEdge `json:"edges" yaml:"edges"`
	// Description is LLM-generated prose describing the architecture (placeholder for now)
	Description string `json:"description" yaml:"description"`
}

// PackagePurpose describes a package and its inferred purpose.
type PackagePurpose struct {
	Name    string `json:"name" yaml:"name"`
	Purpose string `json:"purpose" yaml:"purpose"`
}

// CrossPackageEdge represents a cross-package call relationship.
type CrossPackageEdge struct {
	From  string `json:"from" yaml:"from"`
	To    string `json:"to" yaml:"to"`
	Count int    `json:"count" yaml:"count"`
}

// EntryPointRef describes an entry point with its purpose and immediate callees.
type EntryPointRef struct {
	Name    string   `json:"name" yaml:"name"`
	File    string   `json:"file" yaml:"file"`
	Line    int      `json:"line" yaml:"line"`
	Purpose string   `json:"purpose,omitempty" yaml:"purpose,omitempty"`
	Callees []string `json:"callees,omitempty" yaml:"callees,omitempty"`
}

// PackageRef describes a package with its purpose and role counts.
type PackageRef struct {
	Name       string         `json:"name" yaml:"name"`
	Purpose    string         `json:"purpose" yaml:"purpose"`
	Roles      map[string]int `json:"roles,omitempty" yaml:"roles,omitempty"`
	TopSymbols []SymbolRef    `json:"top_symbols,omitempty" yaml:"top_symbols,omitempty"`
}

// Project contains basic project information.
type Project struct {
	Name  string   `json:"name" yaml:"name"`
	Root  string   `json:"root" yaml:"root"`
	Lang  []string `json:"lang" yaml:"lang"`
	Build string   `json:"build,omitempty" yaml:"build,omitempty"`
	Test  string   `json:"test,omitempty" yaml:"test,omitempty"`
}

// Architecture describes high-level code structure.
type Architecture struct {
	Components []Component `json:"components" yaml:"components"`
	DataFlows  []DataFlow  `json:"data_flows,omitempty" yaml:"data_flows,omitempty"`
	Boundaries []Boundary  `json:"boundaries,omitempty" yaml:"boundaries,omitempty"`
}

// DataFlow describes a data path between components.
type DataFlow struct {
	From   string `json:"from" yaml:"from"`
	To     string `json:"to" yaml:"to"`
	Via    string `json:"via,omitempty" yaml:"via,omitempty"` // Mechanism: "import", "call", "channel"
	Weight int    `json:"weight,omitempty" yaml:"weight,omitempty"`
}

// Boundary describes ownership of a package/directory.
type Boundary struct {
	Package string   `json:"package" yaml:"package"`
	Owns    []string `json:"owns" yaml:"owns"`                           // What this package is responsible for
	Exports []string `json:"exports,omitempty" yaml:"exports,omitempty"` // Key exported symbols
}

// Component represents a logical grouping of code.
type Component struct {
	Name     string   `json:"name" yaml:"name"`
	Purpose  string   `json:"purpose" yaml:"purpose"`
	Entry    string   `json:"entry,omitempty" yaml:"entry,omitempty"`
	KeyFiles []string `json:"key_files,omitempty" yaml:"key_files,omitempty"`
}

// Files organizes files by concern/purpose.
type Files struct {
	ByConcern map[string][]FileInfo `json:"by_concern" yaml:"by_concern"`
}

// FileInfo describes a file with its purpose and key exports.
type FileInfo struct {
	Path        string   `json:"path" yaml:"path"`
	Description string   `json:"description" yaml:"description"`
	Source      string   `json:"source,omitempty" yaml:"source,omitempty"` // "doc" or "inferred"
	Exports     []string `json:"exports,omitempty" yaml:"exports,omitempty"`
}

// Symbols lists key types and functions.
type Symbols struct {
	Types           []SymbolRef      `json:"types,omitempty" yaml:"types,omitempty"`
	Functions       []SymbolRef      `json:"functions,omitempty" yaml:"functions,omitempty"`
	ExtensionPoints []ExtensionPoint `json:"extension_points,omitempty" yaml:"extension_points,omitempty"`
}

// ExtensionPoint describes a high-centrality symbol suitable for extension.
type ExtensionPoint struct {
	Name        string `json:"name" yaml:"name"`
	Kind        string `json:"kind" yaml:"kind"` // interface, func, type
	File        string `json:"file" yaml:"file"`
	Line        int    `json:"line" yaml:"line"`
	RefCount    int    `json:"ref_count" yaml:"ref_count"`                           // How often it's referenced
	CallerCount int    `json:"caller_count,omitempty" yaml:"caller_count,omitempty"` // For funcs: how often called
	Purpose     string `json:"purpose,omitempty" yaml:"purpose,omitempty"`           // From doc comment
}

// SymbolRef is a lightweight reference to a symbol.
type SymbolRef struct {
	Name       string `json:"name" yaml:"name"`
	File       string `json:"file" yaml:"file"`
	Line       int    `json:"line" yaml:"line"`
	Role       string `json:"role,omitempty" yaml:"role,omitempty"`
	Visibility string `json:"visibility,omitempty" yaml:"visibility,omitempty"`
	Purpose    string `json:"purpose,omitempty" yaml:"purpose,omitempty"`
}

// BuildInfo describes the detected build/task system and CI configuration.
type BuildInfo struct {
	System     string   `json:"system" yaml:"system"`                               // "mage", "make", "task", "just", "go"
	Build      string   `json:"build" yaml:"build"`                                 // Primary build command
	Test       string   `json:"test" yaml:"test"`                                   // Primary test command
	Targets    []string `json:"targets,omitempty" yaml:"targets,omitempty"`         // Available targets
	CI         []CIInfo `json:"ci,omitempty" yaml:"ci,omitempty"`                   // CI configs found
	GoGenerate bool     `json:"go_generate,omitempty" yaml:"go_generate,omitempty"` // true if //go:generate found
}

// CIInfo describes a detected CI configuration file.
type CIInfo struct {
	System string `json:"system" yaml:"system"` // "github-actions", "gitlab-ci", "circleci", "jenkins"
	File   string `json:"file" yaml:"file"`     // Relative path
}

// Meta contains generation metadata.
type Meta struct {
	GeneratedAt      string `json:"generated_at" yaml:"generated_at"`
	GitCommit        string `json:"git_commit,omitempty" yaml:"git_commit,omitempty"`
	IndexFingerprint string `json:"index_fingerprint,omitempty" yaml:"index_fingerprint,omitempty"`
}

// Conventions holds detected coding conventions for a project.
type Conventions struct {
	Constructors  *ConstructorConvention `json:"constructors,omitempty" yaml:"constructors,omitempty"`
	Receivers     *ReceiverConvention    `json:"receivers,omitempty" yaml:"receivers,omitempty"`
	Testing       *TestConvention        `json:"testing,omitempty" yaml:"testing,omitempty"`
	Interfaces    *InterfaceConvention   `json:"interfaces,omitempty" yaml:"interfaces,omitempty"`
	ErrorHandling *ErrorConvention       `json:"errors,omitempty" yaml:"errors,omitempty"`
	FileOrg       *FileOrgConvention     `json:"file_organization,omitempty" yaml:"file_organization,omitempty"`
}

// ConstructorConvention describes New* function patterns.
type ConstructorConvention struct {
	Pattern    string `json:"pattern" yaml:"pattern"`
	Confidence string `json:"confidence" yaml:"confidence"`
	Total      int    `json:"total" yaml:"total"`
	WithError  int    `json:"with_error" yaml:"with_error"`
	WithoutErr int    `json:"without_error" yaml:"without_error"`
}

// ReceiverConvention describes method receiver naming patterns.
type ReceiverConvention struct {
	Pattern      string  `json:"pattern" yaml:"pattern"`
	Confidence   string  `json:"confidence" yaml:"confidence"`
	Total        int     `json:"total" yaml:"total"`
	SingleLetter int     `json:"single_letter" yaml:"single_letter"`
	Descriptive  int     `json:"descriptive" yaml:"descriptive"`
	PointerPct   float64 `json:"pointer_pct" yaml:"pointer_pct"`
}

// TestConvention describes testing patterns.
type TestConvention struct {
	Pattern    string `json:"pattern" yaml:"pattern"`
	Confidence string `json:"confidence" yaml:"confidence"`
	TestFiles  int    `json:"test_files" yaml:"test_files"`
	Colocated  int    `json:"colocated" yaml:"colocated"`
	Separate   int    `json:"separate" yaml:"separate"`
	Helpers    int    `json:"helpers" yaml:"helpers"`
}

// InterfaceConvention describes interface naming and sizing patterns.
type InterfaceConvention struct {
	Pattern    string `json:"pattern" yaml:"pattern"`
	Confidence string `json:"confidence" yaml:"confidence"`
	Total      int    `json:"total" yaml:"total"`
	ErSuffix   int    `json:"er_suffix" yaml:"er_suffix"`
}

// ErrorConvention describes error handling patterns.
type ErrorConvention struct {
	Pattern    string `json:"pattern" yaml:"pattern"`
	Confidence string `json:"confidence" yaml:"confidence"`
	Sentinels  int    `json:"sentinels" yaml:"sentinels"`
	ErrorFuncs int    `json:"error_returning_funcs" yaml:"error_returning_funcs"`
}

// FileOrgConvention describes file organization patterns.
type FileOrgConvention struct {
	Pattern      string  `json:"pattern" yaml:"pattern"`
	Confidence   string  `json:"confidence" yaml:"confidence"`
	AvgTypesFile float64 `json:"avg_types_per_file" yaml:"avg_types_per_file"`
	SingleType   int     `json:"single_type_files" yaml:"single_type_files"`
	MultiType    int     `json:"multi_type_files" yaml:"multi_type_files"`
}
