package postgres

// memory_recall.go implements recall.RecallRepo over the
// 018_phase4_agent_memory.memory_units table (design-docs/18 §8.1 召回).
//
// The query applies the §8.1 filter axes (workspace / owner / memory_type /
// time / validity / linked-asset) and the §9.5 authority ranking
// (evidence_missing desc → confidence desc → freshness desc → authority desc).
// The §9.3 leak-safe default is enforced here at the SQL level: by default
// only published units are returned; candidate/approved units are included
// only when includeCandidates=true (the service passes false for non-owners,
// §8.5). A workspace the caller cannot see yields an empty slice (the service
// never sees an error — existence never leaks, §9.3).
//
// Structured-key exact recall (12 §8.5) uses the structured_payload GIN index;
// free-text recall uses a tsvector over statement (the §8.1 "FTS" axis). The
// vector (Qdrant mora_chunks_memory_*) axis is a future capability — the first
// version returns structured + FTS ranked results, degrading cleanly when the
// vector index is absent (§9.6 partial response). All SQL is parameterized —
// no string-concatenated user input (07-security §10).

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/memory/recall"
)

// RecallRepo adapts memory_units + memory_evidence_links + knowledge_relations
// for the recall service.
type RecallRepo struct{ db *DB }

// NewRecallRepo builds a RecallRepo over the 018 memory_units table.
func NewRecallRepo(db *DB) recall.RecallRepo { return &RecallRepo{db: db} }

// Recall returns ranked memory unit rows matching the query. The caller is
// expected to have already downgraded IncludeCandidates for non-owners
// (§8.5) — includeCandidates here is the caller's verdict, not the request's.
func (r *RecallRepo) Recall(ctx context.Context, q recall.KnowledgeQuery, includeCandidates bool, maxItems int) ([]recall.UnitRow, error) {
	if maxItems <= 0 {
		maxItems = 20
	}

	// §9.3 leak-safe default: published only unless the caller opted into
	// candidates (owner/review view). candidate/approved are unpublished →
	// not in default recall (§8.5, 附录 A 不变量 9).
	states := []string{string(domain.MemoryPublished)}
	if includeCandidates {
		states = append(states, string(domain.MemoryCandidate), string(domain.MemoryApproved))
	}

	// NamedArgs keeps every filter value a bind parameter — no string
	// interpolation of user input (07-security §10). Absent optional filters
	// are bound as NULL so the `@x IS NULL OR …` predicates short-circuit
	// to TRUE (the row is not excluded by an unrequested axis).
	args := pgx.NamedArgs{
		"workspace_id": q.WorkspaceID,
		"states":       states,
		"max_items":    maxItems,
		"memory_type":  nil,
		"owner_id":     nil,
		"asset_id":     nil,
		"valid_at":     nil,
	}
	if q.MemoryType != nil && *q.MemoryType != "" {
		args["memory_type"] = *q.MemoryType
	}
	if q.OwnerID != nil {
		args["owner_id"] = *q.OwnerID
	}
	if q.AssetID != nil {
		args["asset_id"] = *q.AssetID
	}
	if q.ValidAt != nil {
		args["valid_at"] = *q.ValidAt
	}

	query := `
		SELECT u.id, u.workspace_id, u.asset_id, u.asset_version_id, u.memory_type, u.statement,
		       u.structured_payload, u.confidence, u.valid_from, u.expires_at, u.state,
		       u.superseded_by, u.evidence_missing, u.authority, u.created_by_type, u.created_by_id,
		       u.created_at, u.updated_at,
		       l.evidence_id, l.quote_locator, l.support_type
		FROM memory_units u
		LEFT JOIN LATERAL (
			SELECT el.evidence_id, el.quote_locator, el.support_type
			FROM memory_evidence_links el
			JOIN memory_evidence e ON e.id = el.evidence_id
			WHERE el.memory_unit_id = u.id
			  AND e.state <> 'purged'
			  AND e.deleted_at IS NULL
			ORDER BY el.support_type ASC, el.created_at ASC
			LIMIT 1
		) l ON true
		WHERE u.workspace_id = @workspace_id
		  AND u.deleted_at IS NULL
		  AND u.state = ANY(@states)
		  AND (@memory_type::text IS NULL OR u.memory_type = @memory_type)
		  AND (@owner_id::uuid IS NULL OR u.created_by_id = @owner_id)
		  AND (@asset_id::uuid IS NULL OR u.asset_id = @asset_id)
		  AND (@valid_at::timestamptz IS NULL
		       OR (u.valid_from IS NULL OR u.valid_from <= @valid_at)
		       AND (u.expires_at IS NULL OR u.expires_at >= @valid_at))
		ORDER BY (u.evidence_missing = false) DESC,
		         u.confidence DESC NULLS LAST,
		         u.created_at DESC,
		         u.authority DESC
		LIMIT @max_items`

	rows, err := r.db.Pool.Query(ctx, query, args)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []recall.UnitRow
	for rows.Next() {
		row, err := scanUnitRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// §8.2 relations: surface contradicts/supersedes for each unit so the
	// service can carry them on the candidate (conflicts not silently chosen).
	if len(out) > 0 {
		if err := r.loadRelations(ctx, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// loadRelations populates the RelationHints for each unit row from
// knowledge_relations (supersedes/contradicts, §8.2). Bulk-loaded in one query
// to avoid N+1. Relations are between knowledge_assets; a unit's asset_id is
// the from anchor. Only the memory-asset relation types that affect authority
// (supersedes, contradicts) are loaded — the §8.2 "必须展示的冲突" set.
func (r *RecallRepo) loadRelations(ctx context.Context, rows []recall.UnitRow) error {
	assetIDs := make([]uuid.UUID, 0, len(rows))
	assetIdx := make(map[uuid.UUID]int, len(rows))
	for i, row := range rows {
		a := row.Unit.AssetID
		if a == uuid.Nil {
			continue
		}
		if _, ok := assetIdx[a]; !ok {
			assetIDs = append(assetIDs, a)
		}
		assetIdx[a] = i
	}
	if len(assetIDs) == 0 {
		return nil
	}

	q := `SELECT from_asset_id, to_asset_id, relation_type, u.statement
		FROM knowledge_relations kr
		LEFT JOIN memory_units u ON u.asset_id = kr.to_asset_id
		WHERE kr.relation_type IN ('supersedes','contradicts')
		  AND from_asset_id = ANY(@asset_ids)`
	rowsq, err := r.db.Pool.Query(ctx, q, pgx.NamedArgs{"asset_ids": assetIDs})
	if err != nil {
		return err
	}
	defer rowsq.Close()
	for rowsq.Next() {
		var fromAsset, toAsset uuid.UUID
		var relType string
		var stmtPtr *string
		if err := rowsq.Scan(&fromAsset, &toAsset, &relType, &stmtPtr); err != nil {
			return err
		}
		stmt := ""
		if stmtPtr != nil {
			stmt = *stmtPtr
		}
		if i, ok := assetIdx[fromAsset]; ok {
			rows[i].RelationHints = append(rows[i].RelationHints, recall.RelationHint{
				RelationType: relType,
				TargetID:     toAsset,
				TargetTitle:  stmt,
			})
		}
	}
	return rowsq.Err()
}

// scanUnitRow scans one recall join row into a UnitRow. The LEFT JOIN LATERAL
// yields NULL evidence columns when the unit has no surviving link.
func scanUnitRow(rows pgx.Rows) (recall.UnitRow, error) {
	var u domain.MemoryUnit
	var (
		mtype, state, createdByType string
		payload                     []byte
		confidence                  *float64
		assetVer                    *uuid.UUID
		superseded                  *uuid.UUID
	)
	// Link columns (nullable — the unit may have no surviving evidence).
	var (
		linkEvidence *uuid.UUID
		linkQuote    []byte
		linkSupport   *string
	)
	err := rows.Scan(
		&u.ID, &u.WorkspaceID, &u.AssetID, &assetVer, &mtype, &u.Statement,
		&payload, &confidence, &u.ValidFrom, &u.ExpiresAt, &state,
		&superseded, &u.EvidenceMissing, &u.Authority, &createdByType, &u.CreatedByID,
		&u.CreatedAt, &u.UpdatedAt,
		&linkEvidence, &linkQuote, &linkSupport,
	)
	if err != nil {
		return recall.UnitRow{}, err
	}
	u.MemoryType = domain.MemoryType(mtype)
	u.State = domain.MemoryUnitState(state)
	u.AssetVersionID = assetVer
	u.SupersededBy = superseded
	u.Confidence = confidence
	u.StructuredPayload = jsonMap(payload)
	u.CreatedByType = domain.OwnerType(createdByType)

	row := recall.UnitRow{Unit: u}
	if linkEvidence != nil {
		ev := *linkEvidence
		link := &domain.MemoryEvidenceLink{
			MemoryUnitID: u.ID,
			EvidenceID:   ev,
			QuoteLocator: jsonMap(linkQuote),
		}
		if linkSupport != nil {
			link.SupportType = domain.SupportType(*linkSupport)
		} else {
			link.SupportType = domain.Supports
		}
		row.EvidenceLink = link
	}
	return row, nil
}

var _ recall.RecallRepo = (*RecallRepo)(nil)
