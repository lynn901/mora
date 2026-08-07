package pipeline

import (
	"strings"
)

// Strategy selects the chunking algorithm (10 §2.2). The default StrategyFixed
// keeps the pre-parsing behavior; new strategies are opt-in via Config.Strategy.
type Strategy string

const (
	StrategyFixed         Strategy = "fixed"          // 现状默认：固定长度 + 重叠 + 标题边界
	StrategyAdaptive3Tier Strategy = "adaptive_3tier" // 自适应三层：section→merge→split
	StrategyParentChild   Strategy = "parent_child"   // 父子块：大粒度父 + 小粒度子
)

// ChunkRole marks a chunk's role in a parent-child strategy (10 §2.3). The
// standalone role (zero value) is used by fixed/adaptive strategies so the
// field is backward-compatible with existing chunks.
type ChunkRole string

const (
	RoleStandalone ChunkRole = ""       // 默认：独立块（现状）
	RoleParent     ChunkRole = "parent" // 父块：大粒度上下文召回
	RoleChild      ChunkRole = "child"  // 子块：精准匹配，指向 parent
)

// ChunkRef is one chunk produced by the chunker (before embedding).
type ChunkRef struct {
	Text        string
	ChunkIndex  int
	SectionPath string
	TokenCount  int
	// parent-child fields (10 §2.3). For fixed/adaptive chunks these stay
	// zero/empty so the payload stays backward-compatible.
	Role             ChunkRole
	ParentChunkID    string // child 指向 parent 的 point id；parent/standalone 为空
	ParentChunkIndex int    // child 指向 parent 的 chunk_index；便于回溯
}

// Tokenizer counts tokens. Production wires the embedding model's tokenizer
// (TEI honors max tokens server-side); the default WordTokenizer is a stable
// approximation sufficient for sizing/overlap and for mock-first tests.
type Tokenizer interface {
	Count(text string) int
}

// WordTokenizer counts whitespace tokens. tokensPerWord (default 1.0) lets ops
// calibrate to a real tokenizer (e.g. 1.3 for many BPE models).
type WordTokenizer struct{ TokensPerWord float64 }

func NewWordTokenizer() WordTokenizer { return WordTokenizer{TokensPerWord: 1.0} }

func (w WordTokenizer) Count(text string) int {
	n := 0
	inWord := false
	for _, r := range text {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			inWord = false
		} else {
			if !inWord {
				n++
				inWord = true
			}
		}
	}
	if w.TokensPerWord <= 0 || w.TokensPerWord == 1 {
		return n
	}
	return int(float64(n)*w.TokensPerWord + 0.5)
}

// Chunk splits structured text (with Markdown heading markers) into chunks.
// Algorithm (05 §3.3): section by headings → size with overlap, never cutting a
// sentence; section_path records the heading hierarchy for each chunk.
//
// The strategy is selected by cfg.Strategy (10 §2.2): StrategyFixed (default,
// unchanged), StrategyAdaptive3Tier (merge small sections, split large), and
// StrategyParentChild (emit parent + child chunks linked by parent_chunk_id).
func Chunk(text string, cfg Config, tok Tokenizer) []ChunkRef {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	switch cfg.Strategy {
	case StrategyAdaptive3Tier:
		return chunkAdaptive3Tier(text, cfg, tok)
	case StrategyParentChild:
		return chunkParentChild(text, cfg, tok)
	default:
		return chunkFixed(text, cfg, tok)
	}
}

// chunkFixed is the pre-parsing algorithm (05 §3.3): section by headings →
// size with overlap, never cutting a sentence. Renamed from the original
// Chunk so the dispatch can route to it. Behavior is unchanged.
func chunkFixed(text string, cfg Config, tok Tokenizer) []ChunkRef {
	chunkSize := cfg.ChunkSize
	if chunkSize <= 0 {
		chunkSize = 512
	}
	overlap := cfg.ChunkOverlap
	if overlap < 0 {
		overlap = 0
	}
	maxSize := cfg.MaxChunkSize
	if maxSize <= 0 {
		maxSize = chunkSize
	}
	if maxSize < chunkSize {
		maxSize = chunkSize
	}

	var chunks []ChunkRef
	idx := 0
	for _, sec := range splitSections(text, cfg.RespectHeadingBoundary) {
		sectionChunks := chunkSection(sec.Body, chunkSize, overlap, maxSize, cfg.RespectHeadingBoundary, tok)
		for _, c := range sectionChunks {
			chunks = append(chunks, ChunkRef{
				Text:        c,
				ChunkIndex:  idx,
				SectionPath: sec.Path,
				TokenCount:  tok.Count(c),
			})
			idx++
		}
	}
	// A document with no headings still produces chunks (section path "").
	if len(chunks) == 0 && strings.TrimSpace(text) != "" {
		for _, c := range chunkSection(text, chunkSize, overlap, maxSize, false, tok) {
			chunks = append(chunks, ChunkRef{Text: c, ChunkIndex: idx, TokenCount: tok.Count(c)})
			idx++
		}
	}
	return chunks
}

// chunkAdaptive3Tier is the adaptive three-tier strategy (10 §2.2):
//
//	Tier 1 (section): split by headings (h1-h3) into sections.
//	Tier 2 (merge):  merge adjacent small sections whose combined tokens ≤
//	                 chunk_size and each < chunk_size/2 (avoid fragment chunks).
//	Tier 3 (split):  sections still over chunk_size are split with overlap
//	                 (reusing chunkSection, the fixed splitter).
//
// The merge step is the only delta from the fixed strategy; merged chunks carry
// a section_path that is the union of the merged sections ("3.1 > 3.2").
func chunkAdaptive3Tier(text string, cfg Config, tok Tokenizer) []ChunkRef {
	chunkSize := cfg.ChunkSize
	if chunkSize <= 0 {
		chunkSize = 512
	}
	overlap := cfg.ChunkOverlap
	if overlap < 0 {
		overlap = 0
	}
	maxSize := cfg.MaxChunkSize
	if maxSize <= 0 {
		maxSize = chunkSize
	}
	if maxSize < chunkSize {
		maxSize = chunkSize
	}
	halfChunk := chunkSize / 2

	secs := splitSections(text, cfg.RespectHeadingBoundary)
	if len(secs) == 0 {
		return chunkFixed(text, cfg, tok)
	}

	var chunks []ChunkRef
	idx := 0
	// Tier 2: merge run of adjacent small sections.
	i := 0
	for i < len(secs) {
		cur := secs[i]
		curBody := strings.TrimSpace(cur.Body)
		curTokens := tok.Count(curBody)
		mergedPath := cur.Path
		// try to merge forward while combined tokens ≤ chunkSize AND each < halfChunk
		for j := i + 1; j < len(secs); j++ {
			next := secs[j]
			nextBody := strings.TrimSpace(next.Body)
			nextTokens := tok.Count(nextBody)
			if curTokens+nextTokens > chunkSize {
				break
			}
			if curTokens >= halfChunk || nextTokens >= halfChunk {
				break
			}
			curBody = curBody + "\n\n" + nextBody
			curTokens += nextTokens
			mergedPath = mergePath(mergedPath, next.Path)
			i = j // advance past the merged section
		}
		// Tier 3: split the (possibly merged) section body.
		for _, c := range chunkSection(curBody, chunkSize, overlap, maxSize, cfg.RespectHeadingBoundary, tok) {
			chunks = append(chunks, ChunkRef{
				Text:        c,
				ChunkIndex:  idx,
				SectionPath: mergedPath,
				TokenCount:  tok.Count(c),
			})
			idx++
		}
		i++
	}
	if len(chunks) == 0 && strings.TrimSpace(text) != "" {
		// fall back to plain fixed chunking of the whole text
		for _, c := range chunkSection(text, chunkSize, overlap, maxSize, false, tok) {
			chunks = append(chunks, ChunkRef{Text: c, ChunkIndex: idx, TokenCount: tok.Count(c)})
			idx++
		}
	}
	return chunks
}

// mergePath joins two section paths so a merged chunk records both ("3.1 > 3.2").
func mergePath(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	return a + " > " + b
}

// chunkParentChild emits parent + child chunks (10 §2.3):
//   - A parent chunk is the whole section body (may exceed chunk_size) — used
//     for context recall.
//   - Child chunks are the section body split to chunk_size — used for precise
//     match. Each child records its parent's chunk index so retrieval can walk
//     child → parent to supplement context.
//
// Very small sections (< chunk_size) emit a single standalone chunk (no
// parent/child split) to avoid pointless single-child parents.
func chunkParentChild(text string, cfg Config, tok Tokenizer) []ChunkRef {
	chunkSize := cfg.ChunkSize
	if chunkSize <= 0 {
		chunkSize = 512
	}
	overlap := cfg.ChunkOverlap
	if overlap < 0 {
		overlap = 0
	}
	maxSize := cfg.MaxChunkSize
	if maxSize <= 0 {
		maxSize = chunkSize
	}
	if maxSize < chunkSize {
		maxSize = chunkSize
	}

	secs := splitSections(text, cfg.RespectHeadingBoundary)
	var chunks []ChunkRef
	idx := 0
	// no headings → no parent/child structure; behave like fixed
	if len(secs) == 0 {
		return chunkFixed(text, cfg, tok)
	}
	for _, sec := range secs {
		body := strings.TrimSpace(sec.Body)
		if body == "" {
			continue
		}
		bodyTokens := tok.Count(body)
		// small section: one standalone chunk, no parent/child overhead
		if bodyTokens <= chunkSize {
			chunks = append(chunks, ChunkRef{
				Text:        body,
				ChunkIndex:  idx,
				SectionPath: sec.Path,
				TokenCount:  bodyTokens,
				Role:        RoleStandalone,
			})
			idx++
			continue
		}
		// parent: whole section (context recall candidate)
		parentIdx := idx
		chunks = append(chunks, ChunkRef{
			Text:        body,
			ChunkIndex:  idx,
			SectionPath: sec.Path,
			TokenCount:  bodyTokens,
			Role:        RoleParent,
		})
		idx++
		// children: section split to chunk_size (precise match candidates)
		childChunks := chunkSection(body, chunkSize, overlap, maxSize, cfg.RespectHeadingBoundary, tok)
		for _, c := range childChunks {
			chunks = append(chunks, ChunkRef{
				Text:             c,
				ChunkIndex:       idx,
				SectionPath:      sec.Path,
				TokenCount:       tok.Count(c),
				Role:             RoleChild,
				ParentChunkIndex: parentIdx,
			})
			idx++
		}
	}
	if len(chunks) == 0 && strings.TrimSpace(text) != "" {
		return chunkFixed(text, cfg, tok)
	}
	return chunks
}

// section is a contiguous run of text under a heading path.
type section struct {
	Path string
	Body string
}

// splitSections parses heading markers into a hierarchy and groups body text.
// When respect is false, heading markers are ignored (whole text is one section).
func splitSections(text string, respect bool) []section {
	var sections []section
	var cur section
	var stack []string // heading texts by level
	push := func(level int, title string) {
		// pop deeper/equal levels
		for len(stack) >= level {
			stack = stack[:len(stack)-1]
		}
		stack = append(stack, title)
	}
	flush := func() {
		if strings.TrimSpace(cur.Body) != "" {
			sections = append(sections, cur)
		}
		cur = section{Path: joinPath(stack)}
	}
	cur = section{Path: ""}
	for _, line := range strings.Split(text, "\n") {
		trim := strings.TrimSpace(line)
		if isCodeFence(line) {
			// keep code fence lines verbatim in the current section body
			cur.Body += line + "\n"
			continue
		}
		if respect {
			if level, title := headingLevel(trim); level > 0 {
				flush()
				push(level, title)
				cur.Path = joinPath(stack)
				continue
			}
		}
		cur.Body += line + "\n"
	}
	flush()
	return sections
}

func joinPath(stack []string) string {
	return strings.Join(stack, " > ")
}

func isCodeFence(line string) bool {
	t := strings.TrimSpace(line)
	return strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~")
}

// headingLevel returns the Markdown heading level (1-6) and title, or 0.
// Recognises leading `#` markers (including the `code:\n` blocks' non-headings).
func headingLevel(line string) (int, string) {
	if len(line) < 2 || line[0] != '#' {
		return 0, ""
	}
	level := 0
	for level < len(line) && line[level] == '#' {
		level++
	}
	if level > 6 {
		return 0, ""
	}
	if level >= len(line) || line[level] != ' ' {
		return 0, ""
	}
	title := strings.TrimSpace(line[level:])
	return level, title
}

// chunkSection splits one section body into chunk-sized pieces with overlap,
// never cutting inside a sentence.
func chunkSection(body string, chunkSize, overlap, maxSize int, _ bool, tok Tokenizer) []string {
	body = strings.TrimSpace(body)
	if body == "" {
		return nil
	}
	if tok.Count(body) <= chunkSize {
		return []string{body}
	}
	sents := splitSentences(body)
	var chunks []string
	var cur []string
	curTokens := 0
	for i := 0; i < len(sents); i++ {
		s := sents[i]
		st := tok.Count(s)
		if st >= maxSize {
			// single sentence longer than max: hard-split by words
			if curTokens > 0 {
				chunks = append(chunks, strings.Join(cur, " "))
				cur, curTokens = cur[:0], 0
			}
			chunks = append(chunks, hardSplitWords(s, chunkSize, overlap, tok)...)
			continue
		}
		if curTokens+st > chunkSize && curTokens > 0 {
			chunks = append(chunks, strings.Join(cur, " "))
			// overlap: carry trailing sentences up to `overlap` tokens
			var ovTokens int
			cur, ovTokens = takeOverlap(cur, overlap, tok)
			cur = append(cur, s)
			curTokens = ovTokens + st
			continue
		}
		cur = append(cur, s)
		curTokens += st
	}
	if len(cur) > 0 {
		chunks = append(chunks, strings.Join(cur, " "))
	}
	return chunks
}

// takeOverlap returns the trailing sentences whose total tokens ≤ overlap,
// along with their total token count.
func takeOverlap(sents []string, overlap int, tok Tokenizer) ([]string, int) {
	if overlap <= 0 || len(sents) == 0 {
		return nil, 0
	}
	var keep []string
	tot := 0
	for i := len(sents) - 1; i >= 0; i-- {
		st := tok.Count(sents[i])
		if tot+st > overlap {
			break
		}
		keep = append([]string{sents[i]}, keep...)
		tot += st
	}
	return keep, tot
}

// splitSentences splits on common sentence terminators while keeping them.
func splitSentences(text string) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	var out []string
	var b strings.Builder
	flush := func() {
		if s := strings.TrimSpace(b.String()); s != "" {
			out = append(out, s)
		}
		b.Reset()
	}
	lines := strings.Split(text, "\n")
	for li, line := range lines {
		if li > 0 {
			b.WriteByte('\n')
		}
		for _, r := range line {
			b.WriteRune(r)
			if r == '.' || r == '。' || r == '!' || r == '！' || r == '?' || r == '？' || r == ';' || r == '；' {
				flush()
			}
		}
	}
	flush()
	if len(out) == 0 {
		out = []string{strings.TrimSpace(text)}
	}
	return out
}

// hardSplitWords splits a too-long sentence (no terminators) into word-grouped
// chunks of size tokens with overlap, so a single runaway sentence is still
// bounded by MaxChunkSize.
func hardSplitWords(s string, size, overlap int, tok Tokenizer) []string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return nil
	}
	var out []string
	step := size - overlap
	if step <= 0 {
		step = size
	}
	for i := 0; i < len(words); i += step {
		end := i + size
		if end > len(words) {
			end = len(words)
		}
		out = append(out, strings.Join(words[i:end], " "))
		if end >= len(words) {
			break
		}
	}
	return out
}
