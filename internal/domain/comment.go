package domain

import "time"

type Comment struct {
	ID         UUID       `json:"id"`
	DocumentID UUID       `json:"document_id"`
	BlockID    *UUID      `json:"block_id,omitempty"`
	ParentID   *UUID      `json:"parent_id,omitempty"`
	AuthorID   UUID       `json:"author_id"`
	Content    string     `json:"content"`
	Mentions   []UUID     `json:"mentions,omitempty"`
	Resolved   bool       `json:"resolved"`
	ResolvedBy *UUID      `json:"resolved_by,omitempty"`
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}
