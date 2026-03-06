package context

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Nugget represents an Orca knowledge graph nugget for save_nug.
type Nugget struct {
	ID   string   `yaml:"id"`
	K    string   `yaml:"k"`
	B    string   `yaml:"b"`
	Tags []string `yaml:"tags,omitempty"`
}

// ToNuggets converts BootContext to Orca nuggets.
func (b *BootContext) ToNuggets() []Nugget {
	projectSlug := slugify(b.Project)

	// Build the body content
	var body strings.Builder
	body.WriteString(fmt.Sprintf("lang: %s\n", b.Lang))
	body.WriteString(fmt.Sprintf("build: %s\n", b.Build))
	body.WriteString(fmt.Sprintf("test: %s\n", b.Test))

	if len(b.EntryPoints) > 0 {
		body.WriteString("entry_points:\n")
		for _, ep := range b.EntryPoints {
			body.WriteString(fmt.Sprintf("  - %s\n", ep))
		}
	}

	if len(b.KeySymbols) > 0 {
		body.WriteString("key_symbols:\n")
		for _, sym := range b.KeySymbols {
			body.WriteString(fmt.Sprintf("  - %s (%s:%d)\n", sym.Name, sym.File, sym.Line))
		}
	}

	if b.Commit != "" {
		body.WriteString(fmt.Sprintf("commit: %s\n", b.Commit))
	}

	return []Nugget{
		{
			ID:   fmt.Sprintf("n:project:%s", projectSlug),
			K:    "project",
			B:    body.String(),
			Tags: []string{projectSlug, "boot-context"},
		},
	}
}

// ToNuggets converts ProjectContext to Orca nuggets.
func (p *ProjectContext) ToNuggets() []Nugget {
	projectSlug := slugify(p.Project.Name)
	var nugs []Nugget

	// Project overview nugget
	var projBody strings.Builder
	projBody.WriteString(fmt.Sprintf("name: %s\n", p.Project.Name))
	projBody.WriteString(fmt.Sprintf("root: %s\n", p.Project.Root))
	if len(p.Project.Lang) > 0 {
		projBody.WriteString(fmt.Sprintf("lang: %s\n", strings.Join(p.Project.Lang, ", ")))
	}
	if p.Project.Build != "" {
		projBody.WriteString(fmt.Sprintf("build: %s\n", p.Project.Build))
	}
	if p.Project.Test != "" {
		projBody.WriteString(fmt.Sprintf("test: %s\n", p.Project.Test))
	}

	nugs = append(nugs, Nugget{
		ID:   fmt.Sprintf("n:project:%s", projectSlug),
		K:    "project",
		B:    projBody.String(),
		Tags: []string{projectSlug},
	})

	// Architecture nugget
	if len(p.Architecture.Components) > 0 {
		var archBody strings.Builder
		archBody.WriteString("components:\n")
		for _, comp := range p.Architecture.Components {
			archBody.WriteString(fmt.Sprintf("  %s:\n", comp.Name))
			archBody.WriteString(fmt.Sprintf("    purpose: %s\n", comp.Purpose))
			if comp.Entry != "" {
				archBody.WriteString(fmt.Sprintf("    entry: %s\n", comp.Entry))
			}
			if len(comp.KeyFiles) > 0 {
				archBody.WriteString("    key_files:\n")
				for _, f := range comp.KeyFiles {
					archBody.WriteString(fmt.Sprintf("      - %s\n", f))
				}
			}
		}

		if len(p.Architecture.DataFlows) > 0 {
			archBody.WriteString("data_flows:\n")
			for _, df := range p.Architecture.DataFlows {
				archBody.WriteString(fmt.Sprintf("  - %s -> %s", df.From, df.To))
				if df.Via != "" {
					archBody.WriteString(fmt.Sprintf(" (%s)", df.Via))
				}
				archBody.WriteString("\n")
			}
		}

		if len(p.Architecture.Boundaries) > 0 {
			archBody.WriteString("boundaries:\n")
			for _, b := range p.Architecture.Boundaries {
				archBody.WriteString(fmt.Sprintf("  %s:\n", b.Package))
				if len(b.Owns) > 0 {
					archBody.WriteString(fmt.Sprintf("    owns: [%s]\n", strings.Join(b.Owns, ", ")))
				}
				if len(b.Exports) > 0 {
					archBody.WriteString(fmt.Sprintf("    exports: [%s]\n", strings.Join(b.Exports, ", ")))
				}
			}
		}

		nugs = append(nugs, Nugget{
			ID:   fmt.Sprintf("n:map:%s-arch", projectSlug),
			K:    "map",
			B:    archBody.String(),
			Tags: []string{projectSlug, "architecture"},
		})
	}

	// Key symbols nugget
	if len(p.Symbols.Types) > 0 || len(p.Symbols.Functions) > 0 {
		var symBody strings.Builder
		if len(p.Symbols.Types) > 0 {
			symBody.WriteString("types:\n")
			for _, t := range p.Symbols.Types {
				symBody.WriteString(fmt.Sprintf("  - %s (%s:%d)\n", t.Name, t.File, t.Line))
			}
		}
		if len(p.Symbols.Functions) > 0 {
			symBody.WriteString("functions:\n")
			for _, f := range p.Symbols.Functions {
				symBody.WriteString(fmt.Sprintf("  - %s (%s:%d)\n", f.Name, f.File, f.Line))
			}
		}

		nugs = append(nugs, Nugget{
			ID:   fmt.Sprintf("n:map:%s-symbols", projectSlug),
			K:    "map",
			B:    symBody.String(),
			Tags: []string{projectSlug, "symbols"},
		})
	}

	return nugs
}

// slugify converts a string to a slug suitable for nugget IDs.
func slugify(s string) string {
	// Get base name if it's a path
	s = filepath.Base(s)
	// Convert to lowercase
	s = strings.ToLower(s)
	// Replace spaces and underscores with hyphens
	s = strings.ReplaceAll(s, " ", "-")
	s = strings.ReplaceAll(s, "_", "-")
	// Remove any characters that aren't alphanumeric or hyphens
	var result strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			result.WriteRune(r)
		}
	}
	return result.String()
}
