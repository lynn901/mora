package index

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

func TestBuildIndex_Deterministic(t *testing.T) {
	pages := []PublishedPage{
		{PageKey: "b", PageKind: "entity", ContentHash: "h2"},
		{PageKey: "a", PageKind: "summary", ContentHash: "h1"},
	}
	_, hash1, err := BuildIndex(pages)
	if err != nil {
		t.Fatal(err)
	}
	// Reorder input — same hash.
	pages2 := []PublishedPage{
		{PageKey: "a", PageKind: "summary", ContentHash: "h1"},
		{PageKey: "b", PageKind: "entity", ContentHash: "h2"},
	}
	_, hash2, _ := BuildIndex(pages2)
	if hash1 != hash2 {
		t.Fatalf("index hash not deterministic: %s != %s", hash1, hash2)
	}
}

func TestBuildIndex_Sorted(t *testing.T) {
	pages := []PublishedPage{
		{PageKey: "b", PageKind: "entity", ContentHash: "h2"},
		{PageKey: "a", PageKind: "summary", ContentHash: "h1"},
		{PageKey: "a2", PageKind: "summary", ContentHash: "h0"},
	}
	content, _, _ := BuildIndex(pages)
	var body IndexContent
	if err := json.Unmarshal(content, &body); err != nil {
		t.Fatal(err)
	}
	// Deterministic sort is (page_kind, page_key): entity before summary, and
	// within summary "a" < "a2" lexicographically (prefix sorts first).
	want := []string{"b", "a", "a2"}
	if len(body.Pages) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(body.Pages))
	}
	for i, e := range body.Pages {
		if e.PageKey != want[i] {
			t.Errorf("entry %d: want %s, got %s", i, want[i], e.PageKey)
		}
	}
}

func TestBuildIndex_DifferentContentDifferentHash(t *testing.T) {
	pages := []PublishedPage{{PageKey: "a", PageKind: "summary", ContentHash: "h1"}}
	_, h1, _ := BuildIndex(pages)
	pages[0].ContentHash = "h2"
	_, h2, _ := BuildIndex(pages)
	if h1 == h2 {
		t.Fatal("different content should produce different hash")
	}
}

func TestAppendLog_AppendsOnly(t *testing.T) {
	e1 := LogEntry{RunID: uuid.New(), Trigger: "ingest", Timestamp: "t1"}
	content1, _, _ := AppendLog(nil, e1)
	var entries []LogEntry
	if err := json.Unmarshal(content1, &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	// Append a second entry — prior entries must be intact (不可改写).
	e2 := LogEntry{RunID: uuid.New(), Trigger: "lint", Timestamp: "t2"}
	content2, _, _ := AppendLog(content1, e2)
	if err := json.Unmarshal(content2, &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	if entries[0].Trigger != "ingest" {
		t.Errorf("first entry mutated: %s", entries[0].Trigger)
	}
}

func TestHashMatches(t *testing.T) {
	if !HashMatches("a", "a") {
		t.Error("expected match")
	}
	if HashMatches("a", "b") {
		t.Error("expected no match")
	}
	if HashMatches("", "a") {
		t.Error("empty should not match")
	}
}
