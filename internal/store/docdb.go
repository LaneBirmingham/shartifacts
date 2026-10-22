package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// The document database behind Claude's `db` capability: JSON documents at slash paths
// (`tasks/t1`, `boards/b1/columns/c2`) per artifact, shared by its viewers, except that
// each person's `data/users/<id>/` subtree is theirs alone. Declared rules (the artifact's
// capabilities.db.rules) set read/write levels per path as Claude's do. Everyone on this
// network may edit artifacts, so a viewer holds `admin`, and `owner` when they own it.

var (
	ErrInvalidArgument = errors.New("invalid_argument")
	ErrQuotaExceeded   = errors.New("quota_exceeded")
	docSegment         = regexp.MustCompile(`^[A-Za-z0-9_\-.~:@+]+$`)
)

const (
	maxDocBytes  = 256 << 10
	maxDocDepth  = 32
	maxDocs      = 25000
	maxDBBytes   = 64 << 20
	maxQueryRows = 1000
)

type DocRecord struct {
	Data        map[string]any `json:"data"`
	Version     int64          `json:"version"`
	UpdatedAt   time.Time      `json:"updated_at"`
	LeaseHolder string         `json:"lease_holder,omitempty"`
	LeaseUntil  time.Time      `json:"lease_until,omitempty"`
}

// Doc is one document as reads return it.
type Doc struct {
	ID      string         `json:"id"`
	Path    string         `json:"path"`
	Exists  bool           `json:"exists"`
	Data    map[string]any `json:"data,omitempty"`
	Version int64          `json:"version,omitempty"`
}

type DocFilter struct {
	Field string `json:"field"`
	Op    string `json:"op"`
	Value any    `json:"value"`
}

type DocQuery struct {
	Where   []DocFilter `json:"where,omitempty"`
	OrderBy string      `json:"order_by,omitempty"`
	Dir     string      `json:"dir,omitempty"`
	Limit   int         `json:"limit,omitempty"`
}

type AcquireResult struct {
	Acquired  bool   `json:"acquired"`
	Version   int64  `json:"version,omitempty"`
	ExpiresAt string `json:"expiresAt,omitempty"`
	Holder    string `json:"holder,omitempty"`
}

// Viewer is who a db call acts as: their person id ("" for nobody), whether they own the
// artifact, and whether they only view it (someone following a share link).
type Viewer struct {
	ID       string
	Owner    bool
	ReadOnly bool
}

type docDB struct {
	docs map[string]*DocRecord
}

type dbRule struct {
	Path  string `json:"path"`
	Read  string `json:"read,omitempty"`
	Write string `json:"write,omitempty"`
}

var levels = map[string]int{"view": 0, "interact": 1, "admin": 2, "owner": 3}

// SplitDocPath checks a path against Claude's grammar and reports whether it names a
// document (an even number of segments) rather than a collection.
func SplitDocPath(p string) ([]string, bool, error) {
	if p == "" || len(p) > 1000 {
		return nil, false, fmt.Errorf("%w: a path needs 1–1000 bytes", ErrInvalidArgument)
	}
	segs := strings.Split(p, "/")
	if len(segs) > 16 {
		return nil, false, fmt.Errorf("%w: at most 16 segments", ErrInvalidArgument)
	}
	for _, s := range segs {
		if s == "." || s == ".." || len(s) > 200 || !docSegment.MatchString(s) {
			return nil, false, fmt.Errorf("%w: segment %q must be letters, digits or _ - . ~ : @ +", ErrInvalidArgument, s)
		}
	}
	return segs, len(segs)%2 == 0, nil
}

func parseRules(capabilities json.RawMessage) []dbRule {
	var c struct {
		DB struct {
			Rules []dbRule `json:"rules"`
		} `json:"db"`
	}
	json.Unmarshal(capabilities, &c)
	return c.DB.Rules
}

// access decides whether the viewer may read and write a path under the rules.
func access(rules []dbRule, segs []string, v Viewer) (read, write bool) {
	level := levels["admin"]
	if v.Owner {
		level = levels["owner"]
	}
	if v.ReadOnly {
		level = levels["view"]
	}
	// Private subtrees: data/users/{self} always, plus every declared `prefix/{self}`.
	prefixes := [][]string{{"data", "users"}}
	declaredAt := map[string]bool{}
	for _, r := range rules {
		declaredAt[r.Path] = true
		if parts := strings.Split(r.Path, "/"); parts[len(parts)-1] == "{self}" {
			prefixes = append(prefixes, parts[:len(parts)-1])
		}
	}
	for _, p := range prefixes {
		if len(segs) > len(p) && strings.Join(segs[:len(p)], "/") == strings.Join(p, "/") {
			if segs[len(p)] != v.ID || v.ID == "" {
				// Someone else's subtree: hidden unless a rule at the prefix opens it.
				if !declaredAt[strings.Join(p, "/")] {
					return false, false
				}
			}
		}
	}
	readMin, writeMin, depthRead, depthWrite := levels["view"], levels["interact"], -1, -1
	for _, r := range rules {
		parts := []string{}
		if r.Path != "" {
			parts = strings.Split(r.Path, "/")
		}
		if len(parts) > len(segs) {
			continue
		}
		match := true
		for i, part := range parts {
			if part != segs[i] && !(part == "{self}" && segs[i] == v.ID && v.ID != "") {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		if l, ok := levels[r.Read]; ok && len(parts) > depthRead {
			readMin, depthRead = l, len(parts)
		}
		if l, ok := levels[r.Write]; ok && len(parts) > depthWrite {
			writeMin, depthWrite = l, len(parts)
		}
	}
	write = level >= writeMin
	return write || level >= readMin, write
}

func (s *Store) dbPath(id string) string { return filepath.Join(s.dir, "db", id+".json") }

// openDB loads an artifact's database and its rules. s.dbMu is held.
func (s *Store) openDB(artifact string) (*docDB, []dbRule, error) {
	s.mu.Lock()
	a, ok := s.items[artifact]
	s.mu.Unlock()
	if !ok {
		return nil, nil, ErrNotFound
	}
	rules := parseRules(a.Capabilities)
	if d, ok := s.dbs[artifact]; ok {
		return d, rules, nil
	}
	d := &docDB{docs: map[string]*DocRecord{}}
	b, err := os.ReadFile(s.dbPath(artifact))
	if err == nil {
		if err = json.Unmarshal(b, &d.docs); err != nil {
			return nil, nil, fmt.Errorf("read db for %s: %w", artifact, err)
		}
	} else if !os.IsNotExist(err) {
		return nil, nil, err
	}
	if s.dbs == nil {
		s.dbs = map[string]*docDB{}
	}
	s.dbs[artifact] = d
	return d, rules, nil
}

func (s *Store) saveDB(artifact string, d *docDB) error {
	b, err := json.Marshal(d.docs)
	if err != nil {
		return err
	}
	if len(b) > maxDBBytes {
		return fmt.Errorf("%w: this artifact's database is over 64 MiB", ErrQuotaExceeded)
	}
	dir := filepath.Join(s.dir, "db")
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	return writeAtomic(dir, s.dbPath(artifact), b)
}

func checkBody(data map[string]any) error {
	if data == nil {
		return fmt.Errorf("%w: a document body must be a JSON object", ErrInvalidArgument)
	}
	b, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	if len(b) > maxDocBytes {
		return fmt.Errorf("%w: a document is at most 256 KiB", ErrInvalidArgument)
	}
	var depth func(v any, d int) int
	depth = func(v any, d int) int {
		max := d
		switch x := v.(type) {
		case map[string]any:
			for _, c := range x {
				if n := depth(c, d+1); n > max {
					max = n
				}
			}
		case []any:
			for _, c := range x {
				if n := depth(c, d+1); n > max {
					max = n
				}
			}
		}
		return max
	}
	if depth(data, 1) > maxDocDepth {
		return fmt.Errorf("%w: a document nests at most 32 levels", ErrInvalidArgument)
	}
	return nil
}

func docOf(path string, r *DocRecord) Doc {
	segs := strings.Split(path, "/")
	d := Doc{ID: segs[len(segs)-1], Path: path}
	if r != nil {
		d.Exists, d.Data, d.Version = true, r.Data, r.Version
	}
	return d
}

// DocGet reads one document; one the viewer cannot see reads as missing.
func (s *Store) DocGet(artifact, path string, v Viewer) (Doc, error) {
	segs, isDoc, err := SplitDocPath(path)
	if err != nil || !isDoc {
		return Doc{}, cmpErr(err, "a document path has an even number of segments")
	}
	s.dbMu.Lock()
	defer s.dbMu.Unlock()
	d, rules, err := s.openDB(artifact)
	if err != nil {
		return Doc{}, err
	}
	if read, _ := access(rules, segs, v); !read {
		return docOf(path, nil), nil
	}
	return docOf(path, d.docs[path]), nil
}

// DocReadable reports whether v may read the document at path, so that change notices
// reach only those who could read what changed.
func (s *Store) DocReadable(artifact, path string, v Viewer) bool {
	segs, _, err := SplitDocPath(path)
	if err != nil {
		return false
	}
	s.dbMu.Lock()
	defer s.dbMu.Unlock()
	_, rules, err := s.openDB(artifact)
	if err != nil {
		return false
	}
	read, _ := access(rules, segs, v)
	return read
}

func cmpErr(err error, msg string) error {
	if err != nil {
		return err
	}
	return fmt.Errorf("%w: %s", ErrInvalidArgument, msg)
}

// DocWrite sets, merges into ("update", which needs the document to exist) or deletes a
// document, returning the paths that changed.
func (s *Store) DocWrite(artifact, op, path string, data map[string]any, v Viewer) error {
	segs, isDoc, err := SplitDocPath(path)
	if err != nil || !isDoc {
		return cmpErr(err, "a document path has an even number of segments")
	}
	if op != "delete" {
		if err = checkBody(data); err != nil {
			return err
		}
	}
	s.dbMu.Lock()
	d, rules, err := s.openDB(artifact)
	if err != nil {
		s.dbMu.Unlock()
		return err
	}
	if _, write := access(rules, segs, v); !write {
		s.dbMu.Unlock()
		return fmt.Errorf("%w: this viewer may not write %s", ErrInvalidArgument, path)
	}
	old := d.docs[path]
	switch op {
	case "set":
		if old == nil && len(d.docs) >= maxDocs {
			s.dbMu.Unlock()
			return fmt.Errorf("%w: an artifact's database holds at most %d documents", ErrQuotaExceeded, maxDocs)
		}
		d.docs[path] = &DocRecord{Data: data, Version: version(old) + 1, UpdatedAt: time.Now().UTC(), LeaseHolder: lease(old).LeaseHolder, LeaseUntil: lease(old).LeaseUntil}
	case "update":
		if old == nil {
			s.dbMu.Unlock()
			return fmt.Errorf("%w: update needs an existing document; use set to create %s", ErrInvalidArgument, path)
		}
		merged := mergeDoc(old.Data, data)
		if err = checkBody(merged); err != nil {
			s.dbMu.Unlock()
			return err
		}
		d.docs[path] = &DocRecord{Data: merged, Version: old.Version + 1, UpdatedAt: time.Now().UTC(), LeaseHolder: old.LeaseHolder, LeaseUntil: old.LeaseUntil}
	case "delete":
		if old == nil {
			s.dbMu.Unlock()
			return nil
		}
		delete(d.docs, path)
	default:
		s.dbMu.Unlock()
		return fmt.Errorf("%w: unknown op %q", ErrInvalidArgument, op)
	}
	if err = s.saveDB(artifact, d); err != nil {
		if old == nil {
			delete(d.docs, path)
		} else {
			d.docs[path] = old
		}
		s.dbMu.Unlock()
		return err
	}
	s.dbMu.Unlock()
	s.changed(artifact, "db", path)
	return nil
}

func version(r *DocRecord) int64 {
	if r == nil {
		return 0
	}
	return r.Version
}
func lease(r *DocRecord) DocRecord {
	if r == nil {
		return DocRecord{}
	}
	return *r
}

// mergeDoc merges nested objects recursively; anything else replaces the field.
func mergeDoc(base, patch map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range patch {
		if pm, ok := v.(map[string]any); ok {
			if bm, ok := out[k].(map[string]any); ok {
				out[k] = mergeDoc(bm, pm)
				continue
			}
		}
		out[k] = v
	}
	return out
}

// DocQuery reads a collection's documents the viewer can see, filtered and ordered.
func (s *Store) DocQuery(artifact, collection string, q DocQuery, v Viewer) ([]Doc, error) {
	segs, isDoc, err := SplitDocPath(collection)
	if err != nil || isDoc {
		return nil, cmpErr(err, "a collection path has an odd number of segments")
	}
	if len(q.Where) > 10 {
		return nil, fmt.Errorf("%w: at most 10 filters", ErrInvalidArgument)
	}
	if q.Limit < 0 || q.Limit > maxQueryRows {
		return nil, fmt.Errorf("%w: limit must be 1–1000", ErrInvalidArgument)
	}
	for _, f := range q.Where {
		switch f.Op {
		case "==", "!=", "<", "<=", ">", ">=", "array-contains":
		case "in", "not-in":
			if list, ok := f.Value.([]any); !ok || len(list) > 30 {
				return nil, fmt.Errorf("%w: %s takes an array of at most 30 values", ErrInvalidArgument, f.Op)
			}
		default:
			return nil, fmt.Errorf("%w: unknown operator %q", ErrInvalidArgument, f.Op)
		}
	}
	s.dbMu.Lock()
	d, rules, err := s.openDB(artifact)
	if err != nil {
		s.dbMu.Unlock()
		return nil, err
	}
	out := []Doc{}
	prefix := collection + "/"
	for path, r := range d.docs {
		if !strings.HasPrefix(path, prefix) || strings.Contains(path[len(prefix):], "/") {
			continue
		}
		if read, _ := access(rules, append(append([]string{}, segs...), path[len(prefix):]), v); !read || !matches(r.Data, q.Where) {
			continue
		}
		out = append(out, docOf(path, r))
	}
	s.dbMu.Unlock()
	sort.SliceStable(out, func(i, j int) bool {
		if q.OrderBy != "" {
			a, aok := out[i].Data[q.OrderBy]
			b, bok := out[j].Data[q.OrderBy]
			if aok != bok {
				return aok // documents missing the field sort last
			}
			if c := compareValues(a, b); c != 0 {
				if q.Dir == "desc" {
					return c > 0
				}
				return c < 0
			}
		}
		return out[i].ID < out[j].ID
	})
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

func matches(data map[string]any, where []DocFilter) bool {
	for _, f := range where {
		v, ok := data[f.Field]
		switch f.Op {
		case "==":
			if !ok || compareValues(v, f.Value) != 0 {
				return false
			}
		case "!=":
			if !ok || compareValues(v, f.Value) == 0 {
				return false
			}
		case "<", "<=", ">", ">=":
			if !ok || rank(v) != rank(f.Value) {
				return false
			}
			c := compareValues(v, f.Value)
			if (f.Op == "<" && c >= 0) || (f.Op == "<=" && c > 0) || (f.Op == ">" && c <= 0) || (f.Op == ">=" && c < 0) {
				return false
			}
		case "in", "not-in":
			found := false
			for _, x := range f.Value.([]any) {
				if ok && compareValues(v, x) == 0 {
					found = true
				}
			}
			if !ok || found != (f.Op == "in") {
				return false
			}
		case "array-contains":
			list, isList := v.([]any)
			found := false
			for _, x := range list {
				if compareValues(x, f.Value) == 0 {
					found = true
				}
			}
			if !isList || !found {
				return false
			}
		}
	}
	return true
}

func rank(v any) int {
	switch v.(type) {
	case nil:
		return 0
	case bool:
		return 1
	case float64, int, int64:
		return 2
	case string:
		return 3
	case []any:
		return 4
	}
	return 5
}

func compareValues(a, b any) int {
	if ra, rb := rank(a), rank(b); ra != rb {
		return ra - rb
	}
	switch x := a.(type) {
	case bool:
		y := b.(bool)
		if x == y {
			return 0
		} else if !x {
			return -1
		}
		return 1
	case float64:
		y := toFloat(b)
		if x < y {
			return -1
		} else if x > y {
			return 1
		}
		return 0
	case int, int64:
		return compareValues(toFloat(a), b)
	case string:
		return strings.Compare(x, b.(string))
	}
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return strings.Compare(string(ja), string(jb))
}

func toFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	case int64:
		return float64(x)
	}
	return 0
}

// DocAcquire grants a short cooperative lease on a document when nobody else holds one.
func (s *Store) DocAcquire(artifact, path, holder string, ttl time.Duration, data map[string]any, v Viewer) (AcquireResult, error) {
	segs, isDoc, err := SplitDocPath(path)
	if err != nil || !isDoc {
		return AcquireResult{}, cmpErr(err, "a document path has an even number of segments")
	}
	if holder == "" {
		return AcquireResult{}, fmt.Errorf("%w: holder is required", ErrInvalidArgument)
	}
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	ttl = min(max(ttl, time.Second), 10*time.Minute)
	s.dbMu.Lock()
	d, rules, err := s.openDB(artifact)
	if err != nil {
		s.dbMu.Unlock()
		return AcquireResult{}, err
	}
	if _, write := access(rules, segs, v); !write {
		s.dbMu.Unlock()
		return AcquireResult{}, fmt.Errorf("%w: this viewer may not write %s", ErrInvalidArgument, path)
	}
	now := time.Now().UTC()
	old := d.docs[path]
	if old != nil && old.LeaseHolder != "" && old.LeaseHolder != holder && now.Before(old.LeaseUntil) {
		s.dbMu.Unlock()
		return AcquireResult{Acquired: false, ExpiresAt: old.LeaseUntil.Format(time.RFC3339)}, nil
	}
	rec := &DocRecord{Data: map[string]any{}, UpdatedAt: now}
	if old != nil {
		copied := *old
		rec = &copied
	}
	if data != nil {
		rec.Data = mergeDoc(rec.Data, data)
		if err = checkBody(rec.Data); err != nil {
			s.dbMu.Unlock()
			return AcquireResult{}, err
		}
	}
	rec.Version++
	rec.LeaseHolder, rec.LeaseUntil = holder, now.Add(ttl)
	d.docs[path] = rec
	if err = s.saveDB(artifact, d); err != nil {
		if old == nil {
			delete(d.docs, path)
		} else {
			d.docs[path] = old
		}
		s.dbMu.Unlock()
		return AcquireResult{}, err
	}
	s.dbMu.Unlock()
	s.changed(artifact, "db", path)
	return AcquireResult{Acquired: true, Version: rec.Version, ExpiresAt: rec.LeaseUntil.Format(time.RFC3339), Holder: holder}, nil
}

// dropDB removes an artifact's database with the artifact.
func (s *Store) dropDB(id string) error {
	s.dbMu.Lock()
	defer s.dbMu.Unlock()
	delete(s.dbs, id)
	if err := os.Remove(s.dbPath(id)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Changes are announced to whoever listens (the server's live event hub).
type ChangeFunc func(artifact, kind, detail string)

var changeMu sync.Mutex

// OnChange registers the listener for document, version, comment and asset changes.
func (s *Store) OnChange(f ChangeFunc) {
	changeMu.Lock()
	s.onChange = f
	changeMu.Unlock()
}

func (s *Store) changed(artifact, kind, detail string) {
	changeMu.Lock()
	f := s.onChange
	changeMu.Unlock()
	if f != nil {
		f(artifact, kind, detail)
	}
}
