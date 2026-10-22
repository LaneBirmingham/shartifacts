// Package search finds artifacts by what they are about: their titles, tags, descriptions,
// rendered text, comments and notes. It forgives typos and word forms, expands a few
// everyday synonyms, filters by type, tag, contents and date, and relates artifacts that
// share their vocabulary. Everything runs in memory with no external service.
package search

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// words calls fn for each run of letters and digits in s with its byte offsets,
// splitting camelCase identifiers (budgetTracker is budget and tracker).
func words(s string, fn func(w string, start, end int)) {
	start := -1
	var prev rune
	for i, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.Is(unicode.Mn, r) {
			if start >= 0 && unicode.IsUpper(r) && unicode.IsLower(prev) {
				fn(s[start:i], start, i)
				start = i
			}
			if start < 0 {
				start = i
			}
		} else if start >= 0 {
			fn(s[start:i], start, i)
			start = -1
		}
		prev = r
	}
	if start >= 0 {
		fn(s[start:], start, len(s))
	}
}

var folds = strings.NewReplacer(
	"à", "a", "á", "a", "â", "a", "ä", "a", "ã", "a", "å", "a", "æ", "ae", "ç", "c",
	"è", "e", "é", "e", "ê", "e", "ë", "e", "ì", "i", "í", "i", "î", "i", "ï", "i",
	"ñ", "n", "ò", "o", "ó", "o", "ô", "o", "ö", "o", "õ", "o", "ø", "o", "ù", "u",
	"ú", "u", "û", "u", "ü", "u", "ý", "y", "ÿ", "y", "ß", "ss", "œ", "oe",
)

// normal lowercases a word and folds common accents, so Pokémon matches pokemon.
func normal(w string) string {
	w = strings.ToLower(w)
	for _, r := range w {
		if r >= utf8.RuneSelf {
			return strings.Map(func(r rune) rune {
				if unicode.Is(unicode.Mn, r) {
					return -1
				}
				return r
			}, folds.Replace(w))
		}
	}
	return w
}

// term is the indexed form of a word, or "" for one too short, too long or too common
// to search by.
func term(w string) string {
	if len(w) > 40 {
		return ""
	}
	w = normal(w)
	if utf8.RuneCountInString(w) < 2 || stopwords[w] {
		return ""
	}
	return stem(w)
}

// stem strips common English endings so plurals and tenses meet: charts and chart,
// planning and planned, recipes and recipe, stories and story.
func stem(w string) string {
	if len(w) <= 3 || !isLetters(w) {
		return w
	}
	switch {
	case strings.HasSuffix(w, "ies") && len(w) > 4:
		w = w[:len(w)-3] + "y"
	case strings.HasSuffix(w, "sses"):
		w = w[:len(w)-2]
	case strings.HasSuffix(w, "ss"), strings.HasSuffix(w, "us"), strings.HasSuffix(w, "is"):
	case strings.HasSuffix(w, "s"):
		w = w[:len(w)-1]
	}
	for _, suffix := range []string{"ing", "ed"} {
		base := strings.TrimSuffix(w, suffix)
		if base != w && len(base) >= 3 && hasVowel(base) {
			w = base
			// running → runn → run; calling and missed keep their double letter.
			if n := len(w); w[n-1] == w[n-2] && !strings.ContainsRune("lsz", rune(w[n-1])) {
				w = w[:n-1]
			}
			break
		}
	}
	if strings.HasSuffix(w, "ly") && len(w) > 5 {
		w = w[:len(w)-2]
	}
	if n := len(w); n > 3 && w[n-1] == 'e' {
		w = w[:n-1]
	}
	if n := len(w); n > 3 && w[n-1] == 'y' && !strings.ContainsRune("aeiou", rune(w[n-2])) {
		w = w[:n-1] + "i"
	}
	return w
}

func isLetters(w string) bool {
	for i := 0; i < len(w); i++ {
		if w[i] < 'a' || w[i] > 'z' {
			return false
		}
	}
	return true
}

func hasVowel(w string) bool { return strings.ContainsAny(w, "aeiouy") }

// distance is the optimal string alignment distance between a and b (an adjacent swap
// counts as one edit), or max+1 once it must exceed max.
func distance(a, b string, max int) int {
	x, y := []rune(a), []rune(b)
	if abs(len(x)-len(y)) > max {
		return max + 1
	}
	prev2 := make([]int, len(y)+1)
	prev := make([]int, len(y)+1)
	row := make([]int, len(y)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(x); i++ {
		row[0] = i
		least := i
		for j := 1; j <= len(y); j++ {
			cost := 1
			if x[i-1] == y[j-1] {
				cost = 0
			}
			row[j] = min(prev[j]+1, row[j-1]+1, prev[j-1]+cost)
			if i > 1 && j > 1 && x[i-1] == y[j-2] && x[i-2] == y[j-1] {
				row[j] = min(row[j], prev2[j-2]+1)
			}
			least = min(least, row[j])
		}
		if least > max {
			return max + 1
		}
		prev2, prev, row = prev, row, prev2
	}
	return prev[len(y)]
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

var stopwords = map[string]bool{}

// codeWords are language keywords and markup that every source shares; in the source
// field they would match nearly anything, so it skips them.
var codeWords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`a an and are as at be but by for from has have he her his i if in into is it
		its me my no not of on or our she so than that the their them then there these they this to too
		us was we were what when where which who why will with you your yours about all also am any been can
		could did do does each had how just more most other own same should some such very would`) {
		stopwords[w] = true
	}
	for _, w := range strings.Fields(`import export default from function const let var return if else for while
		switch case break continue new this class extends super async await try catch finally throw typeof
		instanceof null undefined true false void delete yield of in do static get set constructor props
		state react usestate useeffect useref usememo usecallback div span button input label section main
		header footer nav ul li ol img svg path rect circle style script html head body meta link px em rem
		rgb rgba hsl calc flex grid block none auto center left right top bottom width height margin padding
		border color background font size weight display position absolute relative fixed classname onclick
		onchange value key id src href type name map filter reduce length push console log document window
		element event target preventdefault math max min floor round json stringify parse string number
		boolean object array promise then text items gap rounded shadow hover lg sm md xl`) {
		codeWords[term(w)] = true
	}
	for _, group := range synonymGroups {
		var stems []string
		for _, w := range strings.Fields(group) {
			stems = append(stems, term(w))
		}
		for _, s := range stems {
			for _, other := range stems {
				if other != s && !contains(synonyms[s], other) {
					synonyms[s] = append(synonyms[s], other)
				}
			}
		}
	}
}

// synonymGroups are everyday words people use interchangeably when looking for something.
var synonymGroups = []string{
	"chart graph plot visualization visualisation viz",
	"diagram flowchart",
	"todo task checklist",
	"budget finance money expense spending cost",
	"price cost",
	"mortgage loan",
	"calendar schedule planner agenda",
	"event meeting appointment",
	"recipe cooking meal food",
	"photo image picture pic",
	"video movie film clip",
	"music audio song sound",
	"map location place",
	"note notes memo",
	"document doc",
	"dashboard report metrics stats statistics analytics kpi",
	"timer countdown stopwatch clock",
	"calculator calc",
	"form survey questionnaire",
	"quiz trivia",
	"slides deck presentation",
	"game puzzle",
	"workout exercise fitness gym training",
	"travel trip holiday vacation itinerary",
	"house home",
	"kid child children",
	"tv television series",
	"car vehicle",
	"bug issue defect",
	"guide tutorial howto",
	"list inventory catalog catalogue",
}

var synonyms = map[string][]string{}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
