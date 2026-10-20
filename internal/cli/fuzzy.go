// Fuzzy matching for the command palette, the dialogs and the autocomplete:
// a subsequence match scored like fuzzysort does in OpenCode — consecutive
// letters and word starts weigh more, and a prefix match doubles the score.
package cli

import (
	"sort"
	"strings"
)

// fuzzyScore rates how well query matches target; 0 means no match. An empty
// query matches everything with the same score, so the original order holds.
func fuzzyScore(query, target string) int {
	if query == "" {
		return 1
	}
	q := []rune(strings.ToLower(query))
	s := []rune(strings.ToLower(target))
	score, qi, prev := 0, 0, -2
	for i, r := range s {
		if qi == len(q) {
			break
		}
		if r != q[qi] {
			continue
		}
		pts := 1
		if i == prev+1 {
			pts += 3 // consecutive letters
		}
		if i == 0 || !isWordRune(s[i-1]) {
			pts += 2 // start of a word
		}
		score += pts
		prev = i
		qi++
	}
	if qi < len(q) {
		return 0
	}
	if strings.HasPrefix(string(s), string(q)) {
		score *= 2
	}
	return score
}

// fuzzyRank returns the indexes of the candidates that match query, best
// first; ties keep the candidates' own order. key gives the text of candidate
// i to match against.
func fuzzyRank(n int, query string, key func(i int) string) []int {
	type hit struct{ i, score int }
	var hits []hit
	for i := 0; i < n; i++ {
		if sc := fuzzyScore(query, key(i)); sc > 0 {
			hits = append(hits, hit{i, sc})
		}
	}
	sort.SliceStable(hits, func(a, b int) bool { return hits[a].score > hits[b].score })
	out := make([]int, len(hits))
	for i, h := range hits {
		out[i] = h.i
	}
	return out
}
