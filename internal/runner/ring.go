package runner

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"time"
)

// ring keeps a process's most recent output lines.
type ring struct {
	mu    sync.Mutex
	max   int
	items []LogLine
}

func newRing(max int) *ring { return &ring{max: max} }

func (r *ring) add(stream, text string) {
	if len(text) > 4000 {
		text = text[:4000] + "…"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.items = append(r.items, LogLine{At: time.Now().UTC(), Stream: stream, Text: text})
	if len(r.items) > r.max {
		r.items = append([]LogLine(nil), r.items[len(r.items)-r.max:]...)
	}
}

func (r *ring) lines() []LogLine {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]LogLine{}, r.items...)
}

// tail is the last n lines as text, for error messages.
func (r *ring) tail(n int) string {
	lines := r.lines()
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	var b strings.Builder
	for _, l := range lines {
		if l.Stream != "system" {
			b.WriteString(l.Text + "\n")
		}
	}
	if b.Len() == 0 {
		return "(no output)"
	}
	return b.String()
}

// writer splits a stream into lines.
func (r *ring) writer(stream string) io.Writer { return &lineWriter{r: r, stream: stream} }

type lineWriter struct {
	r      *ring
	stream string
	buf    []byte
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.r.add(w.stream, strings.TrimRight(string(w.buf[:i]), "\r"))
		w.buf = w.buf[i+1:]
	}
	if len(w.buf) > 4000 {
		w.r.add(w.stream, string(w.buf))
		w.buf = nil
	}
	return len(p), nil
}
