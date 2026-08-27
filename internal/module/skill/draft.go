package skill

// draft.go is the §6.3 skill_propose archive builder. When an agent submits a
// candidate proposal (a draft SKILL.md body, not a full tar.gz package), the
// internal API handler wraps the draft into a minimal valid tar.gz archive so
// the ProposalSink can store it verbatim as the candidate version's archive
// original. A human reviewer later promotes the candidate via the management
// import path (§6.1), which re-parses + validates the archive.
//
// SECURITY (§4.4 — script-execution count = 0): BuildDraftArchive only writes
// a SKILL.md text entry with mode 0o644. It NEVER sets an executable bit, and
// the sink stores the bytes verbatim. No script is executed on the proposal
// path.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
)

// BuildDraftArchive wraps a draft SKILL.md body into a minimal tar.gz archive
// the ProposalSink stores verbatim. The single entry is named "SKILL.md" with
// mode 0o644 (no exec bit — §4.4). name is the skill name (used only for a
// non-empty guard); body is the draft SKILL.md content. An empty name or body
// returns ErrInvalidProposal.
func BuildDraftArchive(name, body string) ([]byte, error) {
	if name == "" || body == "" {
		return nil, ErrInvalidProposal
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{
		Name: "SKILL.md",
		Mode: 0o644,
		Size: int64(len(body)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return nil, fmt.Errorf("skill: write draft header: %w", err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		return nil, fmt.Errorf("skill: write draft body: %w", err)
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("skill: close draft tar: %w", err)
	}
	if err := gz.Close(); err != nil {
		return nil, fmt.Errorf("skill: close draft gzip: %w", err)
	}
	return buf.Bytes(), nil
}
