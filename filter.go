package main

// Name filter for level titles and author names (Predator04/Soldat-Reborn#288).
//
// A small built-in list, matched after leetspeak normalisation ("Sh1t",
// "f.u.c.k", "FUUUCK"). Offending words are MASKED with asterisks rather than
// rejected, so a title like "Fuck Valley" is stored as "**** Valley" and the
// level still uploads. The game runs the same rules before publishing
// (scripts/name_filter.gd) and asks for another title instead — keep the two
// lists in step.
//
// Two kinds of entries:
//   - filterStrong: also matched INSIDE a word ("fuckface", "bullshit").
//   - filterTokens: whole words only (plus a plural / -ed ending), because
//     they hide inside innocent words: "cockpit", "grape", "title", "Dickens",
//     "spice", "fire retardant", "class".

import (
	"strings"
	"unicode"
)

var filterStrong = []string{
	"fuck", "shit", "cunt", "nigger", "nigga", "faggot", "whore", "slut",
	"bitch", "porn", "hitler", "pussy", "asshole", "motherf", "dickhead",
}

var filterTokens = []string{
	"ass", "fag", "cum", "tit", "tits", "cock", "dick", "rape", "rapist",
	"retard", "spic", "chink", "kike", "wank", "twat", "nazi", "sex", "jizz",
	"kkk", "hoe", "penis", "vagina", "dildo", "anal", "bastard", "boob",
	"boobs", "cocks", "dicks",
}

var leet = map[rune]rune{
	'0': 'o', '1': 'i', '3': 'e', '4': 'a', '5': 's', '7': 't', '8': 'b',
	'9': 'g', '@': 'a', '$': 's', '!': 'i', '|': 'i', '+': 't', '€': 'e',
	'¡': 'i', 'à': 'a', 'á': 'a', 'â': 'a', 'ä': 'a', 'å': 'a', 'è': 'e',
	'é': 'e', 'ê': 'e', 'ë': 'e', 'ì': 'i', 'í': 'i', 'î': 'i', 'ï': 'i',
	'ò': 'o', 'ó': 'o', 'ô': 'o', 'ö': 'o', 'ù': 'u', 'ú': 'u', 'û': 'u',
	'ü': 'u', 'ñ': 'n', 'ç': 'c', 'ß': 's',
}

// filterNorm lower-cases s, maps leetspeak / accents to letters and drops
// everything that isn't a-z.
func filterNorm(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if m, ok := leet[r]; ok {
			r = m
		}
		if r >= 'a' && r <= 'z' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

type letterRun struct {
	r byte
	n int
}

func runsOf(w string) []letterRun {
	var out []letterRun
	for i := 0; i < len(w); i++ {
		if len(out) > 0 && out[len(out)-1].r == w[i] {
			out[len(out)-1].n++
		} else {
			out = append(out, letterRun{w[i], 1})
		}
	}
	return out
}

// stretchAt matches w at n[i:], letting every letter repeat ("fuuuck" matches
// "fuck", "boooob" matches "boob" but "bob" does not). Returns the end index
// or -1.
func stretchAt(n string, i int, rs []letterRun) int {
	for _, r := range rs {
		c := 0
		for i < len(n) && n[i] == r.r {
			c++
			i++
		}
		if c < r.n {
			return -1
		}
	}
	return i
}

func containsStretched(n, w string) bool {
	rs := runsOf(w)
	for i := 0; i < len(n); i++ {
		if n[i] == w[0] && stretchAt(n, i, rs) >= 0 {
			return true
		}
	}
	return false
}

func wholeStretched(n, w string) bool {
	return stretchAt(n, 0, runsOf(w)) == len(n)
}

// wordIsBad reports whether one normalised word should be masked.
func wordIsBad(n string) bool {
	if n == "" {
		return false
	}
	for _, w := range filterStrong {
		if containsStretched(n, w) {
			return true
		}
	}
	for _, w := range filterTokens {
		for _, suf := range []string{"", "s", "es", "ed", "er"} {
			if wholeStretched(n, w+suf) {
				return true
			}
		}
	}
	return false
}

func isTokenRune(r rune) bool {
	if unicode.IsLetter(r) || unicode.IsDigit(r) {
		return true
	}
	_, ok := leet[r]
	return ok
}

type filterSpan struct{ start, end int } // rune offsets, end exclusive

// filterText masks offensive words in s. changed = something was masked.
func filterText(s string) (out string, changed bool) {
	rs := []rune(s)
	var spans []filterSpan
	for i := 0; i < len(rs); {
		if !isTokenRune(rs[i]) {
			i++
			continue
		}
		j := i
		for j < len(rs) && isTokenRune(rs[j]) {
			j++
		}
		spans = append(spans, filterSpan{i, j})
		i = j
	}
	bad := make([]bool, len(spans))
	for k, sp := range spans {
		bad[k] = wordIsBad(filterNorm(string(rs[sp.start:sp.end])))
	}
	// Spelled-out words: "f u c k", "s.h.i.t" — runs of one-letter tokens
	// are joined and checked as one word.
	for k := 0; k < len(spans); {
		if spans[k].end-spans[k].start != 1 {
			k++
			continue
		}
		m := k
		joined := ""
		for m < len(spans) && spans[m].end-spans[m].start == 1 {
			joined += string(rs[spans[m].start])
			m++
		}
		if m-k >= 3 && wordIsBad(filterNorm(joined)) {
			for x := k; x < m; x++ {
				bad[x] = true
			}
		}
		k = m
	}
	for k, sp := range spans {
		if !bad[k] {
			continue
		}
		changed = true
		for x := sp.start; x < sp.end; x++ {
			rs[x] = '*'
		}
	}
	return string(rs), changed
}
