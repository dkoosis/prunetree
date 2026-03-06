package context

import (
	"database/sql"
	"strings"
)

// flowNode represents a symbol in the call flow graph.
type flowNode struct {
	id       string
	name     string
	receiver string
}

// ExtractPrimaryFlows traces call paths from entry points and returns flow strings.
// It finds main, Execute, RunE functions and traces their call paths.
// Returns flow strings like "main -> Execute -> Store.Open -> WriteIndex"
// Performance: Uses batch queries - no queries in loops.
func ExtractPrimaryFlows(db *sql.DB, repoRoot string, maxDepth int) ([]string, error) {
	if maxDepth <= 0 {
		maxDepth = 5
	}

	// Query 1: Get all entry points (main, Execute, RunE, init)
	entryPoints, err := queryEntryPointSymbols(db, repoRoot)
	if err != nil {
		return nil, err
	}

	if len(entryPoints) == 0 {
		return nil, nil
	}

	// Query 2: Pre-fetch call graph data for all symbols up to maxDepth
	// We batch-load the entire relevant portion of the call graph
	callGraph, err := queryCallGraphBatch(db, repoRoot)
	if err != nil {
		return nil, err
	}

	// Query 3: Pre-fetch symbol names for all symbols in the call graph
	symbolNames, err := querySymbolNamesBatch(db, repoRoot)
	if err != nil {
		return nil, err
	}

	// Build flows in memory (no more DB queries)
	var flows []string
	maxFlows := 10 // Limit total flows for performance

	for _, ep := range entryPoints {
		if len(flows) >= maxFlows {
			break
		}

		// Build flow from this entry point
		flow := buildFlowPath(ep.id, ep.name, ep.receiver, callGraph, symbolNames, maxDepth)
		if flow != "" {
			flows = append(flows, flow)
		}
	}

	return flows, nil
}

// queryEntryPointSymbols returns symbols that are entry points.
// Includes main, Execute, and cobra RunE handlers (runX functions in cmd/).
func queryEntryPointSymbols(db *sql.DB, repoRoot string) ([]flowNode, error) {
	rows, err := db.Query(`
		SELECT id, name, COALESCE(receiver, '') as receiver
		FROM symbols
		WHERE file_path LIKE ? || '/%'
		  AND kind IN ('func', 'method')
		  AND (
		      name = 'main'
		      OR name = 'Execute'
		      OR (name LIKE 'run%' AND file_path LIKE '%/cmd/%' AND name GLOB 'run[A-Z]*')
		  )
		ORDER BY
		  CASE
		    WHEN name = 'main' THEN 1
		    WHEN name = 'Execute' THEN 2
		    ELSE 3
		  END,
		  name
	`, repoRoot)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var nodes []flowNode
	for rows.Next() {
		var n flowNode
		if err := rows.Scan(&n.id, &n.name, &n.receiver); err != nil {
			continue
		}
		nodes = append(nodes, n)
	}
	return nodes, rows.Err()
}

// queryCallGraphBatch fetches the entire call graph for the repo in one query.
// Returns map[caller_id][]callee_id
func queryCallGraphBatch(db *sql.DB, repoRoot string) (map[string][]string, error) {
	rows, err := db.Query(`
		SELECT cg.caller_id, cg.callee_id
		FROM call_graph cg
		JOIN symbols caller ON cg.caller_id = caller.id
		JOIN symbols callee ON cg.callee_id = callee.id
		WHERE caller.file_path LIKE ? || '/%'
		  AND callee.file_path LIKE ? || '/%'
	`, repoRoot, repoRoot)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	graph := make(map[string][]string)
	for rows.Next() {
		var callerID, calleeID string
		if err := rows.Scan(&callerID, &calleeID); err != nil {
			continue
		}
		graph[callerID] = append(graph[callerID], calleeID)
	}
	return graph, rows.Err()
}

// querySymbolNamesBatch fetches all symbol names in one query.
// Returns map[symbol_id]displayName where displayName is "Receiver.Name" or just "Name"
func querySymbolNamesBatch(db *sql.DB, repoRoot string) (map[string]string, error) {
	rows, err := db.Query(`
		SELECT id, name, COALESCE(receiver, '') as receiver
		FROM symbols
		WHERE file_path LIKE ? || '/%'
		  AND kind IN ('func', 'method')
	`, repoRoot)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	names := make(map[string]string)
	for rows.Next() {
		var id, name, receiver string
		if err := rows.Scan(&id, &name, &receiver); err != nil {
			continue
		}
		if receiver != "" {
			// Extract type name from receiver like "*Store" -> "Store"
			typeName := strings.TrimPrefix(receiver, "*")
			names[id] = typeName + "." + name
		} else {
			names[id] = name
		}
	}
	return names, rows.Err()
}

// buildFlowPath constructs a flow string from an entry point using pre-fetched data.
func buildFlowPath(startID, startName, startReceiver string, callGraph map[string][]string, symbolNames map[string]string, maxDepth int) string {
	var path []string

	// Add entry point
	displayName := startName
	if startReceiver != "" {
		typeName := strings.TrimPrefix(startReceiver, "*")
		displayName = typeName + "." + startName
	}
	path = append(path, displayName)

	// Traverse call graph, preferring architecturally significant callees at each level
	visited := make(map[string]bool)
	visited[startID] = true
	currentID := startID

	for depth := 1; depth < maxDepth; depth++ {
		callees := callGraph[currentID]
		if len(callees) == 0 {
			break
		}

		nextID := pickBestCallee(callees, visited, symbolNames)
		if nextID == "" {
			break
		}

		visited[nextID] = true
		name := symbolNames[nextID]
		if name != "" {
			path = append(path, name)
		}
		currentID = nextID
	}

	// Only return flows with at least 2 nodes
	if len(path) < 2 {
		return ""
	}

	return strings.Join(path, " -> ")
}

// GetChangeBoundaries identifies key packages by concern and returns top exported symbols.
// Categories: persistence (store), cli (cmd), output (output), query (query)
// Performance: Uses batch query with GROUP BY instead of per-package queries.
func GetChangeBoundaries(db *sql.DB, repoRoot string) (map[string][]string, error) {
	// Single query that groups symbols by concern based on package path
	// and returns top 5 exported symbols per concern
	rows, err := db.Query(`
		WITH ConcernSymbols AS (
			SELECT
				s.name,
				s.pkg_path,
				CASE
					WHEN s.pkg_path LIKE '%/store%' OR s.pkg_path LIKE '%store' THEN 'persistence'
					WHEN s.pkg_path LIKE '%/cmd%' OR s.pkg_path LIKE '%cmd' THEN 'cli'
					WHEN s.pkg_path LIKE '%/output%' OR s.pkg_path LIKE '%output' THEN 'output'
					WHEN s.pkg_path LIKE '%/query%' OR s.pkg_path LIKE '%query' THEN 'query'
					ELSE NULL
				END as concern,
				COUNT(r.id) as ref_count
			FROM symbols s
			LEFT JOIN refs r ON s.id = r.symbol_id
			WHERE s.file_path LIKE ? || '/%'
			  AND s.kind IN ('func', 'method', 'type', 'interface', 'struct')
			  AND s.name GLOB '[A-Z]*'
			GROUP BY s.id
		),
		RankedSymbols AS (
			SELECT
				name,
				concern,
				ref_count,
				ROW_NUMBER() OVER (PARTITION BY concern ORDER BY ref_count DESC) as rank
			FROM ConcernSymbols
			WHERE concern IS NOT NULL
		)
		SELECT concern, name
		FROM RankedSymbols
		WHERE rank <= 5
		ORDER BY concern, rank
	`, repoRoot)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	boundaries := make(map[string][]string)
	for rows.Next() {
		var concern, name string
		if err := rows.Scan(&concern, &name); err != nil {
			continue
		}
		boundaries[concern] = append(boundaries[concern], name)
	}

	return boundaries, rows.Err()
}

// GetEntryPointDetails returns entry points with their doc comment and immediate callees.
// Uses the EntryPointRef type from types.go.
// Performance: Uses 2 batch queries (entry points, then callees) instead of N+1.
func GetEntryPointDetails(db *sql.DB, repoRoot string) ([]EntryPointRef, error) {
	// Query 1: Get entry points with their details
	rows, err := db.Query(`
		SELECT
			s.id,
			s.name,
			COALESCE(s.receiver, '') as receiver,
			s.file_path,
			s.line_start,
			COALESCE(s.doc, '') as doc
		FROM symbols s
		WHERE s.file_path LIKE ? || '/%'
		  AND s.kind IN ('func', 'method')
		  AND (
		      s.name = 'main'
		      OR s.name = 'Execute'
		      OR (s.name LIKE 'run%' AND s.file_path LIKE '%/cmd/%' AND s.name GLOB 'run[A-Z]*')
		  )
		ORDER BY
		  CASE
		    WHEN s.name = 'main' THEN 1
		    WHEN s.name = 'Execute' THEN 2
		    ELSE 3
		  END,
		  s.name
	`, repoRoot)
	if err != nil {
		return nil, err
	}

	// Collect entry points and their IDs for callee lookup
	type epWithID struct {
		id  string
		ref EntryPointRef
	}
	var entryPoints []epWithID

	for rows.Next() {
		var ep epWithID
		var receiver, doc string
		if err := rows.Scan(&ep.id, &ep.ref.Name, &receiver, &ep.ref.File, &ep.ref.Line, &doc); err != nil {
			continue
		}

		// Format display name with receiver
		if receiver != "" {
			typeName := strings.TrimPrefix(receiver, "*")
			ep.ref.Name = typeName + "." + ep.ref.Name
		}

		// Make file path relative
		ep.ref.File = strings.TrimPrefix(ep.ref.File, repoRoot+"/")

		// Extract first sentence of doc comment as purpose
		if doc != "" {
			ep.ref.Purpose = ExtractFirstSentence(doc)
		}

		entryPoints = append(entryPoints, ep)
	}
	_ = rows.Close()

	if len(entryPoints) == 0 {
		return nil, nil
	}

	// Query 2: Batch fetch callees for all entry points
	// Build a single query with IN clause for all entry point IDs
	ids := make([]string, len(entryPoints))
	for i, ep := range entryPoints {
		ids[i] = ep.id
	}

	// Use a CTE to rank callees and limit to 5 per caller
	calleeQuery := `
		WITH RankedCallees AS (
			SELECT
				cg.caller_id,
				callee.name,
				COALESCE(callee.receiver, '') as receiver,
				ROW_NUMBER() OVER (PARTITION BY cg.caller_id ORDER BY cg.line, cg.col) as rank
			FROM call_graph cg
			JOIN symbols callee ON cg.callee_id = callee.id
			WHERE cg.caller_id IN (` + placeholders(len(ids)) + `)
			  AND callee.file_path LIKE ? || '/%'
		)
		SELECT caller_id, name, receiver
		FROM RankedCallees
		WHERE rank <= 5
		ORDER BY caller_id, rank
	`

	// Build args: IDs first, then repoRoot for LIKE clause
	args := make([]interface{}, len(ids)+1)
	for i, id := range ids {
		args[i] = id
	}
	args[len(ids)] = repoRoot

	calleeRows, err := db.Query(calleeQuery, args...)
	if err != nil {
		return nil, err
	}
	defer calleeRows.Close()

	// Build callee map
	calleeMap := make(map[string][]string)
	for calleeRows.Next() {
		var callerID, name, receiver string
		if err := calleeRows.Scan(&callerID, &name, &receiver); err != nil {
			continue
		}
		displayName := name
		if receiver != "" {
			typeName := strings.TrimPrefix(receiver, "*")
			displayName = typeName + "." + name
		}
		calleeMap[callerID] = append(calleeMap[callerID], displayName)
	}

	// Build final result
	var results []EntryPointRef
	for _, ep := range entryPoints {
		ep.ref.Callees = calleeMap[ep.id]
		results = append(results, ep.ref)
	}

	return results, nil
}

// pickBestCallee selects the most architecturally significant callee from candidates.
// Prefers cross-package method calls (Type.Method) and exported symbols over
// internal helpers like isX/hasX.
func pickBestCallee(callees []string, visited map[string]bool, symbolNames map[string]string) string {
	var bestID string
	var bestScore int

	for _, calleeID := range callees {
		if visited[calleeID] {
			continue
		}
		name := symbolNames[calleeID]
		if name == "" {
			continue
		}
		// Skip stdlib utility calls
		if strings.HasPrefix(name, "fmt.") || strings.HasPrefix(name, "log.") || strings.HasPrefix(name, "strings.") {
			continue
		}

		score := 1
		// Prefer method calls (Type.Method pattern — cross-package significance)
		if strings.Contains(name, ".") {
			score += 3
		}
		// Prefer exported names (uppercase first char of the function/method name)
		parts := strings.SplitN(name, ".", 2)
		checkName := parts[len(parts)-1]
		if len(checkName) > 0 && checkName[0] >= 'A' && checkName[0] <= 'Z' {
			score += 2
		}
		// Deprioritize boolean helpers (isX, hasX)
		lower := strings.ToLower(checkName)
		if strings.HasPrefix(lower, "is") || strings.HasPrefix(lower, "has") {
			score -= 2
		}

		if score > bestScore {
			bestScore = score
			bestID = calleeID
		}
	}

	return bestID
}

// placeholders generates SQL placeholders like "?,?,?" for IN clauses.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("?,", n-1) + "?"
}
