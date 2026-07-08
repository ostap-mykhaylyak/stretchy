// Package analysis implements the text analysis chain used for "text"
// fields: unicode-aware tokenization, lowercasing and Latin accent
// folding, so that "Caffè" matches "caffe".
package analysis

import (
	"strings"
	"unicode"
)

type Token struct {
	Term string
	Pos  int
}

var accentFold = map[rune]rune{
	'à': 'a', 'á': 'a', 'â': 'a', 'ã': 'a', 'ä': 'a', 'å': 'a',
	'è': 'e', 'é': 'e', 'ê': 'e', 'ë': 'e',
	'ì': 'i', 'í': 'i', 'î': 'i', 'ï': 'i',
	'ò': 'o', 'ó': 'o', 'ô': 'o', 'õ': 'o', 'ö': 'o', 'ø': 'o',
	'ù': 'u', 'ú': 'u', 'û': 'u', 'ü': 'u',
	'ý': 'y', 'ÿ': 'y', 'ç': 'c', 'ñ': 'n', 'ß': 's',
}

func foldRune(r rune) rune {
	r = unicode.ToLower(r)
	if f, ok := accentFold[r]; ok {
		return f
	}
	return r
}

// Normalize lowercases and folds accents without tokenizing.
// Used for keyword-style comparisons in case-insensitive contexts.
func Normalize(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		b.WriteRune(foldRune(r))
	}
	return b.String()
}

// Analyze splits text into lowercase, accent-folded alphanumeric terms
// with their positions.
func Analyze(text string) []Token {
	var tokens []Token
	var cur strings.Builder
	pos := 0
	flush := func() {
		if cur.Len() > 0 {
			tokens = append(tokens, Token{Term: cur.String(), Pos: pos})
			pos++
			cur.Reset()
		}
	}
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			cur.WriteRune(foldRune(r))
		} else {
			flush()
		}
	}
	flush()
	return tokens
}

// Terms returns just the term strings of Analyze.
func Terms(text string) []string {
	toks := Analyze(text)
	out := make([]string, len(toks))
	for i, t := range toks {
		out[i] = t.Term
	}
	return out
}
