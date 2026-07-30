package domain

import "github.com/google/uuid"

// ragNamespace is the fixed UUID namespace used to derive deterministic Qdrant
// point ids from (document_id, version_no, chunk_index). Keeping it constant
// makes PointID stable across processes so repeated upserts are idempotent.
var ragNamespace = uuid.MustParse("6ba7b810-9dad-11d1-80b4-00c04fd430c8") // RFC4122 DNS namespace

// uuid5 returns a version-5 (SHA-1) UUID derived from name.
func uuid5(namespace, name string) uuid.UUID {
	return uuid.NewSHA1(ragNamespace, []byte(name))
}

// RAGNamespace returns the configured point-id namespace (mainly for tests).
func RAGNamespace() uuid.UUID { return ragNamespace }
