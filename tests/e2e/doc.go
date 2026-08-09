//go:build e2e

// Package e2e contains black-box end-to-end tests that drive the running
// platform (mora-api + mcp-server + rag-worker + PG/Valkey/Qdrant/TEI) over
// HTTP only. They are skipped unless E2E_BASE_URL is set:
//
//	E2E_BASE_URL=... go test -tags=e2e -v ./tests/e2e/...
//
// The suite covers document workflows, RBAC cross-layer consistency, MCP,
// RAG, and boundary scenarios. See README.md in this package.
package e2e
