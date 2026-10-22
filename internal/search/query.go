package search

import (
	"strconv"
	"strings"
	"time"
)

// Query is a parsed search: words to rank by and filters every result must pass.
//
// Plain words match anywhere, forgiving typos and word forms. "Quoted words" must appear
// together. -word excludes. Filters: kind:react (type:), tag:finance or #finance,
// is:pinned, is:archived, has:comments|open|files|video|chart|…, updated:today|week|month|
// year|30d|2026-03|>2026-01-01 (also created:), before: and after: dates. Archived
// artifacts are left out unless is:archived (only them) or archived:include asks.
type Query struct {
	Words   []string `json:"words,omitempty"`
	Phrases []string `json:"phrases,omitempty"`
	Exclude []string `json:"exclude,omitempty"`
	Kinds   []string `json:"kinds,omitempty"`
	Tags    []string `json:"tags,omitempty"`
	Has     []string `json:"has,omitempty"`
	Pinned  bool     `json:"pinned,omitempty"`
	// Archived is "" to leave archived artifacts out, "only" for just them, or "include".
	Archived string `json:"archived,omitempty"`
	// Updated and Created bound when the artifact last changed and was made; zero is open.
	UpdatedAfter  time.Time `json:"updated_after,omitzero"`
	UpdatedBefore time.Time `json:"updated_before,omitzero"`
	CreatedAfter  time.Time `json:"created_after,omitzero"`
	CreatedBefore time.Time `json:"created_before,omitzero"`
}

// Empty reports whether the query neither ranks nor filters.
func (q Query) Empty() bool {
	return len(q.Words) == 0 && len(q.Phrases) == 0 && len(q.Exclude) == 0 && !q.filtered()
}

func (q Query) filtered() bool {
	return len(q.Kinds) > 0 || len(q.Tags) > 0 || len(q.Has) > 0 || q.Pinned || q.Archived == "only" ||
		!q.UpdatedAfter.IsZero() || !q.UpdatedBefore.IsZero() || !q.CreatedAfter.IsZero() || !q.CreatedBefore.IsZero()
}

// Parse reads a search box query; dates are relative to now.
func Parse(s string, now time.Time) Query {
	var q Query
	for _, tok := range split(s) {
		if tok.quoted {
			if phrase := strings.Join(strings.Fields(tok.text), " "); phrase != "" {
				q.Phrases = append(q.Phrases, phrase)
				q.Words = append(q.Words, plainWords(phrase)...)
			}
			continue
		}
		t := tok.text
		switch {
		case len(t) > 1 && t[0] == '-':
			q.Exclude = append(q.Exclude, plainWords(t[1:])...)
			continue
		case len(t) > 1 && t[0] == '#':
			q.Tags = append(q.Tags, Tag(t[1:]))
			continue
		}
		if key, value, ok := strings.Cut(t, ":"); ok && value != "" && q.Filter(strings.ToLower(key), value, now) {
			continue
		}
		q.Words = append(q.Words, plainWords(t)...)
	}
	return q
}

// Filter applies one key:value filter, reporting false for a key it does not know.
func (q *Query) Filter(key, value string, now time.Time) bool {
	v := strings.ToLower(value)
	switch key {
	case "kind", "type":
		q.Kinds = append(q.Kinds, v)
	case "tag", "tags":
		for _, t := range strings.Split(v, ",") {
			if t = Tag(t); t != "" {
				q.Tags = append(q.Tags, t)
			}
		}
	case "is":
		switch v {
		case "pinned", "pin":
			q.Pinned = true
		case "archived", "archive":
			q.Archived = "only"
		default:
			return false
		}
	case "archived":
		switch v {
		case "exclude", "no", "false":
			q.Archived = ""
		case "only", "yes", "true":
			q.Archived = "only"
		case "include", "any", "all":
			q.Archived = "include"
		default:
			return false
		}
	case "has":
		q.Has = append(q.Has, strings.TrimSuffix(v, "s"))
	case "updated", "modified", "edited", "changed", "created", "made":
		after, before, ok := when(v, now)
		if !ok {
			return false
		}
		if key == "created" || key == "made" {
			q.CreatedAfter, q.CreatedBefore = after, before
		} else {
			q.UpdatedAfter, q.UpdatedBefore = after, before
		}
	case "after", "since", "before", "until":
		// The span's start is the boundary: after:2026-03 is from March on, before:week
		// is more than a week ago.
		at, _, ok := when(strings.TrimLeft(v, "<>="), now)
		if !ok {
			return false
		}
		if key == "after" || key == "since" {
			q.UpdatedAfter = at
		} else {
			q.UpdatedBefore = at
		}
	default:
		return false
	}
	return true
}

// Tag is the stored form of a tag: lowercase, without a leading #, spaces as hyphens.
func Tag(s string) string {
	s = strings.TrimLeft(strings.TrimSpace(s), "#")
	return strings.Join(strings.Fields(strings.ToLower(s)), "-")
}

func plainWords(s string) []string {
	var out []string
	words(s, func(w string, _, _ int) {
		if term(w) != "" {
			out = append(out, w)
		}
	})
	return out
}

type token struct {
	text   string
	quoted bool
}

func split(s string) []token {
	var out []token
	for s = strings.TrimSpace(s); s != ""; s = strings.TrimSpace(s) {
		if s[0] == '"' {
			end := strings.IndexByte(s[1:], '"')
			if end < 0 {
				out = append(out, token{s[1:], true})
				break
			}
			out = append(out, token{s[1 : end+1], true})
			s = s[end+2:]
			continue
		}
		end := strings.IndexAny(s, " \t\n")
		if end < 0 {
			end = len(s)
		}
		out = append(out, token{s[:end], false})
		s = s[end:]
	}
	return out
}

func relative(v string) (time.Duration, bool) {
	if len(v) < 2 {
		return 0, false
	}
	n, err := strconv.Atoi(v[:len(v)-1])
	if err != nil || n < 0 || n > 10000 {
		return 0, false
	}
	day := 24 * time.Hour
	unit := map[byte]time.Duration{'h': time.Hour, 'd': day, 'w': 7 * day, 'm': 30 * day, 'y': 365 * day}[v[len(v)-1]]
	return time.Duration(n) * unit, unit != 0
}

// when reads a time filter value as the span [after, before); either may be zero.
func when(v string, now time.Time) (after, before time.Time, ok bool) {
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	switch v {
	case "today":
		return today, time.Time{}, true
	case "yesterday":
		return today.AddDate(0, 0, -1), today, true
	case "week":
		return now.AddDate(0, 0, -7), time.Time{}, true
	case "month":
		return now.AddDate(0, -1, 0), time.Time{}, true
	case "year":
		return now.AddDate(-1, 0, 0), time.Time{}, true
	}
	if d, ok := relative(v); ok {
		return now.Add(-d), time.Time{}, true
	}
	if v == "" {
		return time.Time{}, time.Time{}, false
	}
	op := v[0]
	if op == '<' || op == '>' {
		v = strings.TrimPrefix(v[1:], "=")
	}
	start, end, ok := period(v, now.Location())
	if !ok {
		return time.Time{}, time.Time{}, false
	}
	switch op {
	case '>':
		return start, time.Time{}, true
	case '<':
		return time.Time{}, start, true
	}
	return start, end, true
}

// period reads 2026, 2026-03 or 2026-03-14 as the span it names.
func period(v string, loc *time.Location) (start, end time.Time, ok bool) {
	for _, f := range []struct {
		layout string
		years  int
		months int
		days   int
	}{{"2006-01-02", 0, 0, 1}, {"2006-01", 0, 1, 0}, {"2006", 1, 0, 0}} {
		if t, err := time.ParseInLocation(f.layout, v, loc); err == nil {
			return t, t.AddDate(f.years, f.months, f.days), true
		}
	}
	return time.Time{}, time.Time{}, false
}
