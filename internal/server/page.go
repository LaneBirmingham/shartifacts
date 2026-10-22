package server

import (
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Agents list artifacts and media a page at a time: a library of thousands, returned whole,
// would fill an agent's context before it found anything.
const (
	defaultPage = 50
	maxPage     = 200
)

var errCursor = errors.New("cursor is not a next_cursor this list returned; start again without one")

// pageOf returns up to limit items of a list sorted newest first (time descending, then id
// ascending) that come after cursor, and the cursor for the page after it, "" at the end.
// The cursor holds the last item's place rather than a count, so items added since do not
// shift the next page.
func pageOf[T any](items []T, key func(T) (time.Time, string), cursor string, limit int) ([]T, string, error) {
	if limit <= 0 {
		limit = defaultPage
	}
	limit = min(limit, maxPage)
	start := 0
	if cursor != "" {
		at, id, err := decodeCursor(cursor)
		if err != nil {
			return nil, "", err
		}
		for start < len(items) {
			t, i := key(items[start])
			if t.Before(at) || t.Equal(at) && i > id {
				break
			}
			start++
		}
	}
	end := min(start+limit, len(items))
	next := ""
	if end < len(items) {
		next = encodeCursor(key(items[end-1]))
	}
	return items[start:end], next, nil
}

func encodeCursor(at time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(at.UnixNano(), 10) + "/" + id))
}

func decodeCursor(c string) (time.Time, string, error) {
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return time.Time{}, "", errCursor
	}
	ns, id, ok := strings.Cut(string(b), "/")
	n, err := strconv.ParseInt(ns, 10, 64)
	if !ok || err != nil {
		return time.Time{}, "", errCursor
	}
	return time.Unix(0, n), id, nil
}
