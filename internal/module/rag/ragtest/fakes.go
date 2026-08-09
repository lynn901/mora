// Package ragtest provides in-memory fakes for every RAG port, so the pipeline,
// search engine, worker and handlers can be fully exercised without Qdrant,
// Valkey, PostgreSQL, or TEI.
package ragtest

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/rag"
	"github.com/lynn901/mora/internal/module/rag/provider"
)

// ---------------------------------------------------------------------------
// Model store + provider factory
// ---------------------------------------------------------------------------

// FakeModelStore holds embedding model configs; exactly one is active.
type FakeModelStore struct {
	mu     sync.Mutex
	models map[string]domain.EmbeddingModel
	active string
	autoID int
}

func NewFakeModelStore(active domain.EmbeddingModel) *FakeModelStore {
	m := &FakeModelStore{models: map[string]domain.EmbeddingModel{}}
	if active.ID == "" {
		active.ID = "model-1"
	}
	active.Status = "active"
	m.models[active.ID] = active
	m.active = active.ID
	return m
}

func (m *FakeModelStore) GetActive(ctx context.Context) (domain.EmbeddingModel, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.models[m.active], nil
}

// GetActiveOr is a test convenience returning the active model, ignoring errors.
func (m *FakeModelStore) GetActiveOr(ctx context.Context) domain.EmbeddingModel {
	mo, _ := m.GetActive(ctx)
	return mo
}
func (m *FakeModelStore) GetByID(ctx context.Context, id string) (domain.EmbeddingModel, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mo, ok := m.models[id]
	if !ok {
		return mo, errNotFound("model " + id)
	}
	return mo, nil
}
func (m *FakeModelStore) List(ctx context.Context) ([]domain.EmbeddingModel, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]domain.EmbeddingModel, 0, len(m.models))
	for _, mo := range m.models {
		out = append(out, mo)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
func (m *FakeModelStore) Upsert(ctx context.Context, mo domain.EmbeddingModel) (domain.EmbeddingModel, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if mo.ID == "" {
		m.autoID++
		mo.ID = "model-" + itoa(m.autoID)
	}
	mo.Status = "active"
	m.models[mo.ID] = mo
	if m.active == "" {
		m.active = mo.ID
	}
	return mo, nil
}
func (m *FakeModelStore) SetActive(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.models[id]; !ok {
		return errNotFound("model " + id)
	}
	for k := range m.models {
		copy := m.models[k]
		copy.Status = ""
		if k == id {
			copy.Status = "active"
		}
		m.models[k] = copy
	}
	m.active = id
	return nil
}

// FakeProviderFactory returns a configurable FakeProvider/Reranker. Flip
// EmbedFail/RerankFail or Unavailable to simulate outages (AC-13, degradation).
type FakeProviderFactory struct {
	Dim         int
	EmbedFail   bool
	Unavailable bool
	RerankFail  bool
}

func (f *FakeProviderFactory) For(ctx context.Context, model domain.EmbeddingModel) (provider.EmbeddingProvider, error) {
	dim := f.Dim
	if dim == 0 {
		dim = model.Dimension
	}
	fp := provider.NewFakeProvider(dim)
	fp.Unavailable = f.Unavailable || f.EmbedFail
	return fp, nil
}
func (f *FakeProviderFactory) Reranker(ctx context.Context) (provider.RerankerProvider, error) {
	return &provider.FakeReranker{Unavailable: f.RerankFail}, nil
}

// ---------------------------------------------------------------------------
// Document store + RBAC
// ---------------------------------------------------------------------------

type FakeDocumentStore struct {
	mu     sync.Mutex
	snaps  map[string]DocumentSnapshot // key: docID|version
	pubIDs []string
}

type DocumentSnapshot = rag.DocumentSnapshot

func NewFakeDocumentStore() *FakeDocumentStore {
	return &FakeDocumentStore{snaps: map[string]DocumentSnapshot{}}
}

func (d *FakeDocumentStore) Put(s DocumentSnapshot) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.snaps[s.DocumentID+"|"+itoa(s.VersionNo)] = s
	found := false
	for _, id := range d.pubIDs {
		if id == s.DocumentID {
			found = true
			break
		}
	}
	if !found && s.Status == domain.DocPublished {
		d.pubIDs = append(d.pubIDs, s.DocumentID)
	}
}

func (d *FakeDocumentStore) GetSnapshot(ctx context.Context, docID string, version int) (DocumentSnapshot, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s, ok := d.snaps[docID+"|"+itoa(version)]
	if !ok {
		return s, errNotFound("snapshot " + docID)
	}
	return s, nil
}

func (d *FakeDocumentStore) PublishedDocumentIDs(ctx context.Context, cursor string, limit int) ([]string, string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	sort.Strings(d.pubIDs)
	start := 0
	if cursor != "" {
		for i, id := range d.pubIDs {
			if id > cursor {
				start = i
				break
			}
		}
	}
	end := start + limit
	if end > len(d.pubIDs) {
		end = len(d.pubIDs)
	}
	out := d.pubIDs[start:end]
	next := ""
	if end < len(d.pubIDs) {
		next = d.pubIDs[end-1]
	}
	return out, next, nil
}

// FakeRBACResolver maps document → visible subjects and user → group membership.
type FakeRBACResolver struct {
	mu      sync.Mutex
	readers map[string][]string // docID → subjects
	groups  map[string][]string // userID → group ids
}

func NewFakeRBACResolver() *FakeRBACResolver {
	return &FakeRBACResolver{readers: map[string][]string{}, groups: map[string][]string{}}
}

// SetReaders sets the visible_to subjects for a document.
func (r *FakeRBACResolver) SetReaders(docID string, subjects []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.readers[docID] = append([]string(nil), subjects...)
}

// SetGroups sets the groups a user belongs to (for search-time scope).
func (r *FakeRBACResolver) SetGroups(userID string, groupIDs []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.groups[userID] = append([]string(nil), groupIDs...)
}

func (r *FakeRBACResolver) ResolveReaders(ctx context.Context, docID string) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.readers[docID]...), nil
}

func (r *FakeRBACResolver) ViewerScope(ctx context.Context, userID string) (rag.ViewerScope, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	subs := []string{domain.UserSubject(userID)}
	for _, g := range r.groups[userID] {
		subs = append(subs, domain.GroupSubject(g))
	}
	return rag.ViewerScope{UserID: userID, SubjectIDs: subs}, nil
}

// ---------------------------------------------------------------------------
// Vector store (Fake Qdrant): cosine search + RBAC payload hard filter
// ---------------------------------------------------------------------------

type fakePoint struct {
	pointID string
	vector  []float32
	payload domain.ChunkMetadata
}

type FakeVectorStore struct {
	mu         sync.Mutex
	colls      map[string][]fakePoint
	ensured    map[string]int
	FailUpsert bool // force UpsertChunks to fail (AC-13 retry/dead-letter test)
}

func NewFakeVectorStore() *FakeVectorStore {
	return &FakeVectorStore{colls: map[string][]fakePoint{}, ensured: map[string]int{}}
}

func (v *FakeVectorStore) EnsureCollection(ctx context.Context, name string, dim int) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, ok := v.colls[name]; !ok {
		v.colls[name] = []fakePoint{}
	}
	v.ensured[name] = dim
	return nil
}

func (v *FakeVectorStore) UpsertChunks(ctx context.Context, coll string, points []rag.VectorPoint) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.FailUpsert {
		return errUnavailable("qdrant upsert forced failure")
	}
	cur := v.colls[coll]
	for _, p := range points {
		found := false
		for i, e := range cur {
			if e.pointID == p.PointID {
				cur[i] = fakePoint{p.PointID, p.Vector, p.Payload}
				found = true
				break
			}
		}
		if !found {
			cur = append(cur, fakePoint{p.PointID, p.Vector, p.Payload})
		}
	}
	v.colls[coll] = cur
	return nil
}

func (v *FakeVectorStore) DeleteByDocument(ctx context.Context, coll, docID string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	keep := v.colls[coll][:0]
	for _, p := range v.colls[coll] {
		if p.payload.DocumentID != docID {
			keep = append(keep, p)
		}
	}
	v.colls[coll] = keep
	return nil
}

func (v *FakeVectorStore) DeleteByDocumentVersion(ctx context.Context, coll, docID string, version int) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	keep := v.colls[coll][:0]
	for _, p := range v.colls[coll] {
		if !(p.payload.DocumentID == docID && p.payload.VersionNo == version) {
			keep = append(keep, p)
		}
	}
	v.colls[coll] = keep
	return nil
}

func (v *FakeVectorStore) SetVisibleTo(ctx context.Context, coll, docID string, vis []string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	for i, p := range v.colls[coll] {
		if p.payload.DocumentID == docID {
			v.colls[coll][i].payload.VisibleTo = append([]string(nil), vis...)
		}
	}
	return nil
}

func (v *FakeVectorStore) SearchDense(ctx context.Context, req rag.VectorSearchRequest) ([]rag.VectorHit, error) {
	v.mu.Lock()
	points := append([]fakePoint(nil), v.colls[req.CollectionName]...)
	v.mu.Unlock()
	subjSet := toSet(req.VisibleTo)
	var hits []rag.VectorHit
	for _, p := range points {
		if !intersects(p.payload.VisibleTo, subjSet) {
			continue // RBAC hard filter
		}
		if req.WorkspaceID != "" && p.payload.WorkspaceID != req.WorkspaceID {
			continue
		}
		if req.DirectoryID != "" && p.payload.DirectoryID != req.DirectoryID {
			continue
		}
		if p.payload.Status != "" && p.payload.Status != string(domain.DocPublished) {
			continue
		}
		if len(req.Tags) > 0 && !hasAllTags(p.payload.Tags, req.Tags) {
			continue
		}
		hits = append(hits, rag.VectorHit{PointID: p.pointID, Score: cosine(req.Vector, p.vector), Payload: p.payload})
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if len(hits) > req.TopK {
		hits = hits[:req.TopK]
	}
	return hits, nil
}

// Count returns the number of points in a collection (test helper).
func (v *FakeVectorStore) Count(coll string) int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.colls[coll])
}

// PointsFor returns the points of a document as exported VectorPoints (test helper).
func (v *FakeVectorStore) PointsFor(coll, docID string) []rag.VectorPoint {
	v.mu.Lock()
	defer v.mu.Unlock()
	var out []rag.VectorPoint
	for _, p := range v.colls[coll] {
		if p.payload.DocumentID == docID {
			out = append(out, rag.VectorPoint{PointID: p.pointID, Vector: p.vector, Payload: p.payload})
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// FTS store (Fake BM25): term-overlap scoring + RBAC subject filter
// ---------------------------------------------------------------------------

type FTSDoc struct {
	Title      string
	Text       string
	ChunkIndex int
	VisibleTo  []string
	Workspace  string
	Directory  string
}

type FakeFTSStore struct {
	mu   sync.Mutex
	docs map[string]FTSDoc // key docID|chunkIndex
}

func NewFakeFTSStore() *FakeFTSStore { return &FakeFTSStore{docs: map[string]FTSDoc{}} }

func (f *FakeFTSStore) Index(docID string, d FTSDoc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.docs[docID+"|"+itoa(d.ChunkIndex)] = d
}

func (f *FakeFTSStore) SearchBM25(ctx context.Context, req rag.FTSRequest) ([]rag.FTSHit, error) {
	f.mu.Lock()
	docs := make(map[string]FTSDoc, len(f.docs))
	for k, v := range f.docs {
		docs[k] = v
	}
	f.mu.Unlock()
	qterms := toSet(splitWords(req.Query))
	subjSet := toSet(req.VisibleTo)
	var hits []rag.FTSHit
	for key, d := range docs {
		if !intersects(d.VisibleTo, subjSet) {
			continue
		}
		if req.WorkspaceID != "" && d.Workspace != req.WorkspaceID {
			continue
		}
		if req.DirectoryID != "" && d.Directory != req.DirectoryID {
			continue
		}
		score := float32(0)
		dterms := toSet(splitWords(d.Title + " " + d.Text))
		for t := range qterms {
			if _, ok := dterms[t]; ok {
				score += 1.0
			}
		}
		if score == 0 {
			continue
		}
		docID, chunkIdx := splitKey(key)
		hits = append(hits, rag.FTSHit{
			DocumentID: docID, Title: d.Title, ChunkText: d.Text,
			ChunkIndex: chunkIdx, Score: score, WorkspaceID: d.Workspace,
		})
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if len(hits) > req.TopK {
		hits = hits[:req.TopK]
	}
	return hits, nil
}

// ---------------------------------------------------------------------------
// Index status store (Fake PG): tasks + document badge + chunk meta
// ---------------------------------------------------------------------------

type FakeIndexStatusStore struct {
	mu           sync.Mutex
	tasks        map[string]domain.IndexingTask // taskID → task
	tasksByEvent map[string]string              // docID|eventID → taskID
	docs         map[string]rag.IndexStatusInfo
	chunks       map[string]map[int][]domain.Chunk // docID → version → chunks
}

func NewFakeIndexStatusStore() *FakeIndexStatusStore {
	return &FakeIndexStatusStore{
		tasks:        map[string]domain.IndexingTask{},
		tasksByEvent: map[string]string{},
		docs:         map[string]rag.IndexStatusInfo{},
		chunks:       map[string]map[int][]domain.Chunk{},
	}
}

func (s *FakeIndexStatusStore) UpsertTask(ctx context.Context, task domain.IndexingTask) (domain.IndexingTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := task.DocumentID + "|" + task.EventID
	if id, ok := s.tasksByEvent[key]; ok {
		return s.tasks[id], nil
	}
	if task.ID == "" {
		task.ID = "task-" + itoa(len(s.tasks)+1)
	}
	task.CreatedAt = time.Now()
	task.UpdatedAt = time.Now()
	if task.MaxAttempt == 0 {
		task.MaxAttempt = 3
	}
	s.tasks[task.ID] = task
	s.tasksByEvent[key] = task.ID
	return task, nil
}

func (s *FakeIndexStatusStore) UpdateTaskStatus(ctx context.Context, taskID string, status domain.IndexingTaskStatus, attempt int, errMsg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[taskID]
	if !ok {
		return errNotFound("task " + taskID)
	}
	t.Status = status
	t.Attempt = attempt
	t.ErrorMessage = errMsg
	t.UpdatedAt = time.Now()
	s.tasks[taskID] = t
	return nil
}

func (s *FakeIndexStatusStore) GetTask(ctx context.Context, taskID string) (domain.IndexingTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[taskID]
	if !ok {
		return t, errNotFound("task " + taskID)
	}
	return t, nil
}

func (s *FakeIndexStatusStore) SetDocumentIndexStatus(ctx context.Context, docID string, status domain.IndexStatus, errMsg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	info := s.docs[docID]
	info.IndexStatus = status
	info.LastError = errMsg
	if status == domain.IndexIndexed {
		now := time.Now()
		info.LastIndexedAt = &now
	}
	s.docs[docID] = info
	return nil
}

func (s *FakeIndexStatusStore) GetDocumentIndexStatus(ctx context.Context, docID string) (rag.IndexStatusInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	info := s.docs[docID]
	if info.IndexStatus == "" {
		info.IndexStatus = domain.IndexPending
	}
	if vs, ok := s.chunks[docID]; ok {
		info.ChunkCount = 0
		for _, cs := range vs {
			info.ChunkCount += len(cs)
		}
	}
	return info, nil
}

func (s *FakeIndexStatusStore) RecordChunks(ctx context.Context, docID string, version int, modelID string, chunks []domain.Chunk) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.chunks[docID] == nil {
		s.chunks[docID] = map[int][]domain.Chunk{}
	}
	// idempotent upsert by chunk_index
	cur := s.chunks[docID][version]
	for _, c := range chunks {
		found := false
		for i, e := range cur {
			if e.ChunkIndex == c.ChunkIndex {
				cur[i] = c
				found = true
				break
			}
		}
		if !found {
			cur = append(cur, c)
		}
	}
	s.chunks[docID][version] = cur
	return nil
}

func (s *FakeIndexStatusStore) DeleteChunkMeta(ctx context.Context, docID string, version int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.chunks[docID] != nil {
		delete(s.chunks[docID], version)
	}
	return nil
}

func (s *FakeIndexStatusStore) DeleteAllChunkMeta(ctx context.Context, docID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.chunks, docID)
	return nil
}

func (s *FakeIndexStatusStore) PendingTasks(ctx context.Context, cutoff time.Time, limit int) ([]domain.IndexingTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []domain.IndexingTask
	for _, t := range s.tasks {
		if (t.Status == domain.TaskPending || t.Status == domain.TaskFailed) && t.UpdatedAt.Before(cutoff) {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.Before(out[j].UpdatedAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ChunksOf returns recorded chunk meta for a (doc,version) (test helper).
func (s *FakeIndexStatusStore) ChunksOf(docID string, version int) []domain.Chunk {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.chunks[docID][version]
}

// ---------------------------------------------------------------------------
// Idempotency + event queue
// ---------------------------------------------------------------------------

type FakeIdempotencyStore struct {
	mu   sync.Mutex
	seen map[string]struct{}
}

func NewFakeIdempotencyStore() *FakeIdempotencyStore {
	return &FakeIdempotencyStore{seen: map[string]struct{}{}}
}

func (s *FakeIdempotencyStore) MarkSeen(ctx context.Context, eventID string, ttl time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.seen[eventID]; ok {
		return true, nil
	}
	s.seen[eventID] = struct{}{}
	return false, nil
}

// FakeEventQueue is an in-memory Valkey Streams stand-in with ACK/dead-letter.
type FakeEventQueue struct {
	mu        sync.Mutex
	stream    []entry
	pending   map[string]bool // entryID → delivered (un-ACKed)
	dead      []deadEntry
	idCounter int64
}

type entry struct {
	id string
	ev domain.DocEvent
	ts time.Time
}
type deadEntry struct {
	ev     domain.DocEvent
	reason string
}

func NewFakeEventQueue() *FakeEventQueue {
	return &FakeEventQueue{pending: map[string]bool{}}
}

func (q *FakeEventQueue) Publish(ctx context.Context, ev domain.DocEvent) (string, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.idCounter++
	id := itoa64(q.idCounter) + "-0"
	q.stream = append(q.stream, entry{id: id, ev: ev, ts: time.Now()})
	return id, nil
}

func (q *FakeEventQueue) ReadGroup(ctx context.Context, consumer string, count int64, block time.Duration) ([]rag.QueueMessage, error) {
	q.mu.Lock()
	var out []rag.QueueMessage
	for _, e := range q.stream {
		if q.pending[e.id] {
			continue
		}
		q.pending[e.id] = true
		out = append(out, rag.QueueMessage{Stream: "doc_events", ID: e.id, DocEvent: e.ev})
		if int64(len(out)) >= count {
			break
		}
	}
	q.mu.Unlock()
	return out, nil
}

func (q *FakeEventQueue) Ack(ctx context.Context, msg rag.QueueMessage) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.pending, msg.ID)
	return nil
}

func (q *FakeEventQueue) MoveToDeadLetter(ctx context.Context, msg rag.QueueMessage, reason string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.dead = append(q.dead, deadEntry{ev: msg.DocEvent, reason: reason})
	return nil
}

func (q *FakeEventQueue) Claim(ctx context.Context, consumer string, minIdle time.Duration, count int64) ([]rag.QueueMessage, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []rag.QueueMessage
	now := time.Now()
	for _, e := range q.stream {
		if q.pending[e.id] && now.Sub(e.ts) > minIdle {
			out = append(out, rag.QueueMessage{Stream: "doc_events", ID: e.id, DocEvent: e.ev})
			if int64(len(out)) >= count {
				break
			}
		}
	}
	return out, nil
}

// Dead returns dead-lettered events (test helper).
func (q *FakeEventQueue) Dead() []deadEntry {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]deadEntry(nil), q.dead...)
}

// --- helpers ---

type errNotFound string

func (e errNotFound) Error() string { return "not found: " + string(e) }

type errUnavailable string

func (e errUnavailable) Error() string { return "unavailable: " + string(e) }

func toSet(s []string) map[string]struct{} {
	m := make(map[string]struct{}, len(s))
	for _, x := range s {
		m[x] = struct{}{}
	}
	return m
}

func intersects(visibleTo []string, subjects map[string]struct{}) bool {
	for _, s := range visibleTo {
		if _, ok := subjects[s]; ok {
			return true
		}
	}
	return false
}

func hasAllTags(have, want []string) bool {
	hs := toSet(have)
	for _, t := range want {
		if _, ok := hs[t]; !ok {
			return false
		}
	}
	return true
}

func cosine(a, b []float32) float32 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return float32(dot / (sqrt64(na) * sqrt64(nb)))
}

func splitWords(s string) []string {
	var out []string
	var b strings.Builder
	flush := func() {
		if b.Len() > 0 {
			out = append(out, strings.ToLower(b.String()))
			b.Reset()
		}
	}
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == ',' || r == '.' || r == '。' {
			flush()
		} else {
			b.WriteRune(r)
		}
	}
	flush()
	return out
}

func splitKey(key string) (string, int) {
	idx := strings.LastIndex(key, "|")
	if idx < 0 {
		return key, 0
	}
	n := 0
	for i := idx + 1; i < len(key); i++ {
		if key[i] < '0' || key[i] > '9' {
			return key, 0
		}
		n = n*10 + int(key[i]-'0')
	}
	return key[:idx], n
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [21]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func sqrt64(x float64) float64 {
	// stdlib math.Sqrt without importing math (keeps the fake dependency-free)
	if x <= 0 {
		return 0
	}
	z := x
	for i := 0; i < 40; i++ {
		z = z - (z*z-x)/(2*z)
	}
	return z
}
