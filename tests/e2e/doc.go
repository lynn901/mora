//go:build e2e

// Package e2e contains black-box end-to-end tests that drive the running
// platform (mora-api + mcp-server + rag-worker + PG/Valkey/Qdrant/TEI) over
// HTTP only. They are skipped unless E2E_BASE_URL is set:
//
//	E2E_BASE_URL=... go test -tags=e2e -v ./tests/e2e/...
//
// These tests cover PRD (YS-4) acceptance criteria AC-1~19, the core closed
// loop (PRD §5.1), RBAC cross-layer consistency, and PRD §7 boundary scenarios.
// They are written ahead of infra readiness (YS-12) and executed verbatim by
// YS-10 for final closed-loop verification. See VERIFICATION_MATRIX.md and
// README.md in this package.
package e2e
