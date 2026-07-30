package provider

import (
	"math"
	"strings"
	"unicode"
)

// splitWords tokenizes text into lowercase word tokens (unicode letters/digits).
// Shared with the FakeReranker; production sparse vectors come from Qdrant/PG.
func splitWords(text string) []string {
	var out []string
	var b strings.Builder
	flush := func() {
		if b.Len() > 0 {
			out = append(out, strings.ToLower(b.String()))
			b.Reset()
		}
	}
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return out
}

func wordSet(text string) map[string]struct{} {
	s := make(map[string]struct{})
	for _, w := range splitWords(text) {
		s[w] = struct{}{}
	}
	return s
}

func jaccard(a, b map[string]struct{}) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 0
	}
	inter, union := 0, 0
	for w := range a {
		union++
		if _, ok := b[w]; ok {
			inter++
		}
	}
	union += len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

func wordBucket(word string, dim int) int {
	var h uint32 = 2166136261
	for i := 0; i < len(word); i++ {
		h ^= uint32(word[i])
		h *= 16777619
	}
	return int(h % uint32(dim))
}

// clamp guards against NaN/Inf in scores before marshalling to JSON.
func clamp(f float32) float32 {
	if math.IsNaN(float64(f)) || math.IsInf(float64(f), 0) {
		return 0
	}
	return f
}
