package main

// context_wiring.go assembles the Phase 6 Context Broker production wiring
// (design-docs/19 §3.3). It composes the typed query ports the Broker
// orchestrates:
//
//   - MemoryQuery  → context.MemoryAdapter over recall.RecallService (§3.3
//     reuse, not re-implement). The recall service already enforces the
//     leak-safe ranked recall (§8.1/§9.3) + the §4.3 Evidence ACL chain.
//   - DocumentQuery → context.DocumentAdapter over rag/search.HybridSearcher
//     via DocumentHybridSearchFunc (the adapter accepts an exported function
//     port so the wiring layer does not import the context package's
//     unexported docHybridPort).
//   - CodeQuery / SkillQuery → unwired in this stage. An unwired port
//     degrades as provider_error (§9.6 — CodeGraph sidecar absent is normal in
//     dev; the Broker's fan-out records it and does NOT abort the other
//     ports). The YS-209 metadata flags pending_code_port_shape: the
//     codegraph.Service does not yet satisfy codeAdapterPort without a wiring
//     shim (ActiveCodeGraph + Search param contravariance); that shim lands
//     with the code wiring in a follow-up. Skill delivery wiring reuses the
//     skill.DeliveryService already constructed in main.go.
//
// The Broker's AuthzService is the platform/authz.Service (Authorize pre-fetch
// + VisibleAssets post-check, §3.2 two-pass). The PrincipalResolver maps the
// HTTP AuthState → context.Principal (handler.go's authStatePrincipal).
//
// This wiring is fail-closed: a nil port degrades, a nil authz service is
// refused at construction (the Broker cannot serve without the §3.2 gate).

import (
	"context"

	knowledgectx "github.com/lynn901/mora/internal/module/knowledge/context"
	ragsearch "github.com/lynn901/mora/internal/module/rag/search"
	"github.com/lynn901/mora/internal/module/memory/recall"
)

// ragDocumentSearchFunc adapts rag/search.HybridSearcher to the context
// package's exported DocumentHybridSearchFunc (§3.3). The Broker's
// DocumentAdapter calls it; the result maps onto the unified candidate.
//
// RBAC: the rag searcher's own visible_to hard filter is NOT used here — the
// Broker authorizes once at workspace scope (step 2) and post-checks the
// merged candidate IDs (step 5, §3.2 two-pass). Passing IsAdmin=false + empty
// UserID means the rag searcher returns its raw fused ranking; the post-check
// drops unauthorized candidates. A future wiring passes the resolved principal
// so the rag searcher's hard filter runs pre-fetch too (the
// pending_authz_pushdown metadata note, YS-202).
func ragDocumentSearchFunc(searcher *ragsearch.HybridSearcher) knowledgectx.DocumentHybridSearchFunc {
	if searcher == nil {
		return nil
	}
	return func(ctx context.Context, query, workspaceID string, topN int) (*knowledgectx.DocumentHybridResult, error) {
		res, err := searcher.Search(ctx, ragsearch.SearchRequest{
			Query:       query,
			WorkspaceID: workspaceID,
			TopN:        topN,
		})
		if err != nil {
			return nil, err
		}
		out := &knowledgectx.DocumentHybridResult{Total: res.Total}
		if res.Items == nil {
			return out, nil
		}
		out.Items = make([]knowledgectx.DocumentHybridHit, 0, len(res.Items))
		for _, h := range res.Items {
			out.Items = append(out.Items, knowledgectx.DocumentHybridHit{
				DocumentID:  h.DocumentID,
				Title:       h.Title,
				ChunkText:   h.ChunkText,
				ChunkIndex:  h.ChunkIndex,
				SectionPath: h.SectionPath,
				Score:       float64(h.Score),
			})
		}
		return out, nil
	}
}

// newContextBroker builds the production Broker over the wired ports + the
// authz service + the policy repo. Code/Skill ports are nil (degraded as
// provider_error; §9.6). The audit sink records the §9.3 query summary.
func newContextBroker(
	recallSvc *recall.RecallService,
	ragSearcher *ragsearch.HybridSearcher,
	policyRepo knowledgectx.PolicyRepo,
	authzSvc knowledgectx.AuthzService,
	auditSink knowledgectx.AuditSink,
) *knowledgectx.Broker {
	memPort := knowledgectx.NewMemoryAdapter(recallSvc)
	docPort := knowledgectx.NewDocumentAdapterFromFunc(ragDocumentSearchFunc(ragSearcher))
	policyLoader := knowledgectx.DefaultPolicyLoader(policyRepo)
	opts := []knowledgectx.BrokerOption{
		knowledgectx.WithDocumentPort(docPort),
	}
	if auditSink != nil {
		opts = append(opts, knowledgectx.WithAuditSink(auditSink))
	}
	return knowledgectx.NewBroker(
		memPort,
		knowledgectx.NewIntentRouter(nil),
		authzSvc,
		policyLoader,
		knowledgectx.NewBudgeter(),
		knowledgectx.NewCitationBuilder(nil),
		knowledgectx.DefaultBudgetDefaults,
		opts...,
	)
}

// keep the context import referenced when the broker builder is factored out.
var _ context.Context
