// service.go — 装配：端口注入 + authz.Service 注入 + 配置加载（§3.3）。
//
// The wiring layer composes the ContextBroker from the four type-query ports,
// the IntentRouter, the AuthorityPolicy loader, the Budgeter, the
// CitationBuilder, and the authz seam. It is where the platform/authz.Service
// is adapted to the local AuthzSeam (§3.3) — the broker itself never imports
// platform/authz, preserving layering (authz is below the modules).
//
// This file lands the wiring seam + constructor signatures as TODO stubs. The
// real wiring (port adapters, policy loader, authz adaptation) lands in a
// follow-up sub-task.

// New wires a ContextBroker from its ports + the authz seam (§3.3). The authz
// param adapts platform/authz.Service to the local AuthzSeam so the broker's
// two-stage gate (D10) runs through the same decision pipeline the rest of the
// platform uses, without a knowledge/context → platform/authz import. The
// policy loader reads context_authority_policies (is_current=true) at startup;
// policy_version is part of the cache key (§5.3 / 附录 A #21).
//
// TODO: wire the real port adapters + IntentRouter + Budgeter +
// CitationBuilder + policy loader once their implementations land. Returns a
// broker whose Execute is still a skeleton stub.
package contextbroker

func New(doc DocumentQuery, code CodeQuery, mem MemoryQuery, skill SkillQuery, authz AuthzSeam) ContextBroker {
	return &broker{
		doc:   doc,
		code:  code,
		mem:   mem,
		skill: skill,
		authz: authz,
	}
}
