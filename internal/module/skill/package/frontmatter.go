package skillpkg

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/goccy/go-yaml"
)

// Frontmatter delimiters per the agentskills.io spec: SKILL.md begins with a
// YAML frontmatter block fenced by "---" lines. Everything before the closing
// fence is the parsed frontmatter; everything after is the skill body.
const (
	fmFence = "---"
)

// Known frontmatter field keys Mora understands semantically (§2.2). Fields
// outside this set are "unknown legal fields": preserved verbatim in
// OriginalFrontmatter (§2.3 lossless), reported as opaque_fields in the
// compatibility report, and never dropped.
var knownFrontmatterKeys = map[string]bool{
	"name":          true,
	"description":   true,
	"version":       true,
	"format":        true, // declared format_id family, e.g. agentskills.io/v1.0
	"schema_version": true,
	"license":       true,
	"author":        true,
	"capabilities":  true, // declared tools/skills/resources
}

// parseSkillMDFrontmatter extracts the YAML frontmatter from SKILL.md and
// returns the full parsed map (lossless — every field, known or unknown, is
// kept), plus the declared format_id, schema_version, and a capability
// summary. A missing frontmatter block is not an error (the package is still
// saveable; the validator records a structure finding); malformed YAML is
// returned as an error so the validator can surface it.
//
// The returned map is the authoritative OriginalFrontmatter: it is written
// verbatim to skill_packages.original_frontmatter so export reproduces the
// import exactly (§2.3 roundtrip).
func parseSkillMDFrontmatter(skillMD []byte) (fm map[string]any, formatID, schemaVer string, caps map[string]any, err error) {
	body, frontmatter, ok := splitFrontmatter(skillMD)
	_ = body
	if !ok {
		// No frontmatter — return an empty map; not an error (validator will
		// record a structure finding).
		return map[string]any{}, "", "", nil, nil
	}
	if err := yaml.Unmarshal(frontmatter, &fm); err != nil {
		return map[string]any{}, "", "", nil, fmt.Errorf("skill: malformed SKILL.md frontmatter: %w", err)
	}
	if fm == nil {
		fm = map[string]any{}
	}

	formatID, _ = fm["format"].(string)
	schemaVer, _ = fm["schema_version"].(string)
	if schemaVer == "" {
		if v, ok := fm["version"].(string); ok {
			schemaVer = v
		}
	}
	if c, ok := fm["capabilities"].(map[string]any); ok {
		caps = c
	} else if c, ok := fm["capabilities"].([]any); ok {
		caps = map[string]any{"capabilities": c}
	} else {
		caps = map[string]any{}
	}
	return fm, formatID, schemaVer, caps, nil
}

// splitFrontmatter separates the YAML frontmatter block from the body. The
// block is delimited by a leading "---\n" and a closing "\n---" line. Returns
// (body, frontmatterBytes, ok). ok=false means no frontmatter present.
func splitFrontmatter(b []byte) (body, frontmatter []byte, ok bool) {
	// Tolerate a leading UTF-8 BOM (EF BB BF).
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	// Must start with the fence on its own line.
	if !bytes.HasPrefix(b, []byte(fmFence+"\n")) && !bytes.HasPrefix(b, []byte(fmFence+"\r\n")) {
		return b, nil, false
	}
	// Skip the opening fence line.
	rest := b
	if bytes.HasPrefix(rest, []byte(fmFence+"\r\n")) {
		rest = rest[len(fmFence)+2:]
	} else {
		rest = rest[len(fmFence)+1:]
	}
	// Find the closing fence: a line that is exactly "---".
	idx := bytes.Index(rest, []byte("\n"+fmFence+"\n"))
	if idx >= 0 {
		fm := rest[:idx+1]
		tail := rest[idx+1+len(fmFence)+1:]
		// Skip the closing fence's trailing newline.
		tail = bytes.TrimPrefix(tail, []byte("\n"))
		return tail, fm, true
	}
	// Closing fence at EOF (no trailing content).
	if bytes.HasSuffix(rest, []byte("\n"+fmFence)) || bytes.HasSuffix(rest, []byte("\r\n"+fmFence)) {
		fm := rest
		fm = bytes.TrimSuffix(fm, []byte("\r\n"+fmFence))
		fm = bytes.TrimSuffix(fm, []byte("\n"+fmFence))
		return nil, fm, true
	}
	return b, nil, false
}

// opaqueFields returns the frontmatter field paths that are NOT in the known
// set — the values Mora preserves but does not semantically understand. Used
// by the compatibility report's opaque_fields list (§4.3). Order is sorted
// for deterministic output.
func opaqueFields(fm map[string]any) []string {
	var out []string
	for k := range fm {
		if !knownFrontmatterKeys[strings.ToLower(k)] {
			out = append(out, k)
		}
	}
	// sort for determinism
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}
