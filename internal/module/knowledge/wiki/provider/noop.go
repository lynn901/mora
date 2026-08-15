// Package provider — default provider implementation. The production path will
// wrap an external model (§4.1 "外部模型实现可替换"); this NoopProvider is the
// deterministic fallback the pipeline runs against so the CAS / proposal /
// index path is observable before the model adapter lands. It never invents
// content — it returns empty patch sets (no-op runs) and echoes the lint
// cursor unchanged, so a run produces zero candidates and reaches 'applied'
// with proposal_count=0. That keeps the contract honest: no content is
// synthesized without a real provider behind it.
package provider

import "context"

// NoopProvider is the deterministic fallback WikiMaintenanceProvider. It
// returns empty patch lists and a clean health check. It is the default
// wiring in mora-api / knowledge-worker; swap it for the model adapter when
// that lands (§4.1).
type NoopProvider struct{}

// NewNoopProvider builds the deterministic fallback provider.
func NewNoopProvider() *NoopProvider { return &NoopProvider{} }

// ProposeIngest returns no candidate patches — the run finishes applied with
// zero proposals. The affected-page / source-version inputs are still
// validated by the caller, so an empty result is a well-formed no-op.
func (NoopProvider) ProposeIngest(ctx context.Context, _ Capability, _ WikiIngestRequest) ([]PagePatch, error) {
	return nil, ctx.Err()
}

// ProposeAnswer returns no candidate patch for the explicit settle-answer
// request (no model yet → no synthesized page).
func (NoopProvider) ProposeAnswer(ctx context.Context, _ Capability, _ WikiAnswerRequest) ([]PagePatch, error) {
	return nil, ctx.Err()
}

// Lint returns an empty report (no findings). A real lint implementation
// lives in the wiki/lint package and is invoked by the service's lint
// handler; this provider's Lint is the pass-through path when no model is
// configured.
func (NoopProvider) Lint(ctx context.Context, _ Capability, _ WikiLintRequest) (WikiLintReport, error) {
	return WikiLintReport{}, ctx.Err()
}

// Health reports the provider as healthy (it is a local no-op, always up).
func (NoopProvider) Health(ctx context.Context) error { return ctx.Err() }

// Compile-time check.
var _ WikiMaintenanceProvider = (*NoopProvider)(nil)
