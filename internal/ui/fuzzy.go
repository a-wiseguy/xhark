package ui

import "strings"

type scoredIdx struct {
	idx   int
	score int
}

// fuzzyMatchScore returns (score, ok). Lower score is better.
// Matching is a simple case-insensitive subsequence match.
func fuzzyMatchScore(needle, haystack string) (int, bool) {
	needle = strings.ToLower(needle)
	haystack = strings.ToLower(haystack)
	if needle == "" {
		return 0, true
	}

	score := 0
	j := 0
	for i := 0; i < len(haystack) && j < len(needle); i++ {
		if haystack[i] == needle[j] {
			score += i
			j++
		}
	}
	if j != len(needle) {
		return 0, false
	}
	return score, true
}
