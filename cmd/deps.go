package cmd

import (
	"database/sql"
	"os"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/dkoosis/snipe/internal/output"
	"github.com/dkoosis/snipe/internal/query"
)

var depsTreeFlag bool

var depsCmd = &cobra.Command{
	Use:     "deps [package]",
	Short:   "Show dependency topology for a package or the full project",
	GroupID: "advanced",
	Long: `Shows what a package depends on and what depends on it.

Without arguments, shows dependencies for the current directory's package.
With --tree, shows the full internal dependency graph with cycle detection.

Examples:
  snipe deps internal/store    # Bidirectional deps for store
  snipe deps store             # Short name resolution
  snipe deps --tree            # Full project dependency graph`,
	Args: cobra.MaximumNArgs(1),
	RunE: runDeps,
}

func init() {
	depsCmd.Flags().BoolVar(&depsTreeFlag, "tree", false, "Show full dependency graph")
	rootCmd.AddCommand(depsCmd)
}

func runDeps(cmd *cobra.Command, args []string) error {
	start := time.Now()

	compact, _, _, _, _, _ := GetOutputConfig()
	w := output.NewWriter(os.Stdout, compact)

	s, dir, err := OpenStore(w, "deps")
	if err != nil {
		return err
	}
	defer s.Close()

	modulePath := query.DetectModulePath(s.DB())
	if modulePath == "" {
		return w.WriteError("deps", &output.Error{
			Code:    output.ErrInternal,
			Message: "could not detect module path from index",
		})
	}

	if depsTreeFlag {
		return runDepsTree(w, s.DB(), modulePath, dir, start)
	}

	// Resolve package argument
	repoRoot, _ := s.GetMeta("repo_root")
	arg := "."
	if len(args) > 0 {
		arg = args[0]
	}
	pkgPath := query.ResolvePkgPattern(s.DB(), arg, dir, repoRoot)

	// Ensure we have the full package path as stored in the imports table.
	pkgPath = query.ResolveFullPkgPath(s.DB(), pkgPath, modulePath)

	return runDepsSingle(w, s.DB(), pkgPath, modulePath, dir, start)
}

func runDepsSingle(w *output.Writer, db *sql.DB, pkgPath, modulePath, dir string, start time.Time) error {
	deps, err := query.FindPackageDeps(db, pkgPath, modulePath)
	if err != nil {
		return w.WriteError("deps", &output.Error{
			Code:    output.ErrInternal,
			Message: err.Error(),
		})
	}

	result := output.DepsResult{
		Package:      pkgPath,
		Dependencies: deps.Dependencies,
		Dependents:   deps.Dependents,
	}

	resp := output.Response[output.DepsResult]{
		Protocol: output.ProtocolVersion,
		Ok:       true,
		Results:  []output.DepsResult{result},
		Meta:     depsMeta(db, dir, start, map[string]string{"package": pkgPath}, len(deps.Dependencies)+len(deps.Dependents)),
	}

	return w.WriteResponse(resp)
}

func runDepsTree(w *output.Writer, db *sql.DB, modulePath, dir string, start time.Time) error {
	graph, err := query.FindDepGraph(db, modulePath)
	if err != nil {
		return w.WriteError("deps", &output.Error{
			Code:    output.ErrInternal,
			Message: err.Error(),
		})
	}

	edges := make([]output.DepTreeEdge, len(graph.Edges))
	for i, e := range graph.Edges {
		edges[i] = output.DepTreeEdge{From: e.From, To: e.To, FileCount: e.FileCount}
	}

	sort.Strings(graph.Packages)

	result := output.DepTreeResult{
		Packages: graph.Packages,
		Edges:    edges,
		Cycles:   graph.Cycles,
	}

	resp := output.Response[output.DepTreeResult]{
		Protocol: output.ProtocolVersion,
		Ok:       true,
		Results:  []output.DepTreeResult{result},
		Meta:     depsMeta(db, dir, start, map[string]string{"mode": "tree"}, len(graph.Packages)),
	}

	return w.WriteResponse(resp)
}

func depsMeta(db *sql.DB, dir string, start time.Time, q map[string]string, total int) output.Meta {
	return output.Meta{
		Command:    "deps",
		Query:      q,
		RepoRoot:   dir,
		IndexState: query.CheckIndexState(db, dir, Version),
		Ms:         time.Since(start).Milliseconds(),
		Total:      total,
	}
}
