package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/LaneBirmingham/shartifacts/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Agent tools that mirror what Claude's own Artifact tooling offers beyond create and
// update: the artifact's document store (as ArtifactData), deleting an artifact only once
// the person confirms, and watching for new versions and comments.

type pendingDelete struct {
	id      string
	base    int
	expires time.Time
}

var (
	deleteMu      sync.Mutex
	deleteTokens  = map[string]pendingDelete{}
	errNeedsQuery = errors.New("query needs collection")
)

// dbOp is one artifact_db operation; batch holds several.
type dbOp struct {
	Op         string         `json:"op" jsonschema:"get, list, query, set, update, str_replace or delete"`
	Path       string         `json:"path,omitempty" jsonschema:"Document path (even segments), e.g. tasks/t1"`
	Collection string         `json:"collection,omitempty" jsonschema:"Collection path (odd segments) for list and query"`
	Data       map[string]any `json:"data,omitempty" jsonschema:"Document body for set, fields to merge for update"`
	Where      [][]any        `json:"where,omitempty" jsonschema:"Filters [[field, op, value]]; op is == != < <= > >= in not-in array-contains"`
	OrderBy    string         `json:"order_by,omitempty"`
	Dir        string         `json:"dir,omitempty" jsonschema:"asc or desc"`
	Limit      int            `json:"limit,omitempty"`
	OldStr     string         `json:"old_str,omitempty" jsonschema:"For str_replace: text in the document's JSON, occurring once"`
	NewStr     string         `json:"new_str,omitempty"`
}

func (s *Server) runDBOp(id string, op dbOp) (any, error) {
	// Agents act as the owner with no person id: shared data, never anyone's private subtree.
	v := store.Viewer{Owner: true}
	switch op.Op {
	case "get":
		return s.Store.DocGet(id, op.Path, v)
	case "list", "query":
		if op.Collection == "" {
			return nil, errNeedsQuery
		}
		q := store.DocQuery{OrderBy: op.OrderBy, Dir: op.Dir, Limit: op.Limit}
		for _, w := range op.Where {
			if len(w) != 3 {
				return nil, errors.New("each filter is [field, op, value]")
			}
			field, _ := w[0].(string)
			opName, _ := w[1].(string)
			q.Where = append(q.Where, store.DocFilter{Field: field, Op: opName, Value: w[2]})
		}
		docs, e := s.Store.DocQuery(id, op.Collection, q, v)
		return map[string]any{"docs": docs}, e
	case "set", "update", "delete":
		if e := s.Store.DocWrite(id, op.Op, op.Path, op.Data, v); e != nil {
			return nil, e
		}
		return map[string]any{"ok": true, "path": op.Path}, nil
	case "str_replace":
		d, e := s.Store.DocGet(id, op.Path, v)
		if e != nil {
			return nil, e
		}
		if !d.Exists {
			return nil, fmt.Errorf("%s does not exist", op.Path)
		}
		b, _ := json.Marshal(d.Data)
		text := string(b)
		if n := strings.Count(text, op.OldStr); op.OldStr == "" || n != 1 {
			return nil, fmt.Errorf("old_str must occur exactly once in the document's JSON (found %d)", strings.Count(text, op.OldStr))
		}
		var data map[string]any
		if e = json.Unmarshal([]byte(strings.Replace(text, op.OldStr, op.NewStr, 1)), &data); e != nil {
			return nil, fmt.Errorf("the replacement leaves invalid JSON: %v", e)
		}
		if e = s.Store.DocWrite(id, "set", op.Path, data, v); e != nil {
			return nil, e
		}
		return map[string]any{"ok": true, "path": op.Path}, nil
	}
	return nil, errors.New("op must be get, list, query, set, update, str_replace or delete")
}

func (s *Server) addAgentTools(m *mcp.Server) {
	type DBInput struct {
		ID string `json:"id"`
		dbOp
		Batch []dbOp `json:"batch,omitempty" jsonschema:"Several operations, applied in order (op: batch)"`
	}
	mcp.AddTool(m, &mcp.Tool{Name: "artifact_db", Description: "Read and write the document store an artifact's page uses through claude.use(\"db\") (op get|list|query|set|update|str_replace|delete|batch). Agents see shared data, never people's private data/users/<id>/ documents."}, func(ctx context.Context, req *mcp.CallToolRequest, in DBInput) (*mcp.CallToolResult, any, error) {
		if in.Op != "batch" {
			out, e := s.runDBOp(in.ID, in.dbOp)
			if e != nil {
				return nil, nil, e
			}
			return nil, out, nil
		}
		results := []any{}
		for i, op := range in.Batch {
			out, e := s.runDBOp(in.ID, op)
			if e != nil {
				return nil, nil, fmt.Errorf("batch %d (%s): %w; earlier operations were applied", i+1, op.Op, e)
			}
			results = append(results, out)
		}
		return nil, map[string]any{"results": results}, nil
	})

	type DeleteInput struct {
		ID          string   `json:"id,omitempty"`
		IDs         []string `json:"ids,omitempty" jsonschema:"Archived artifacts to delete together, up to 200 (emptying the archive); no confirmation"`
		BaseVersion int      `json:"base_version,omitempty" jsonschema:"For an artifact still in the library: its latest version from get_artifact"`
		Confirm     string   `json:"confirm,omitempty" jsonschema:"The token the first call returned, after the person confirmed"`
	}
	destructive := true
	mcp.AddTool(m, &mcp.Tool{Name: "delete_artifact", Description: "Permanently delete artifacts with their versions, data, assets, comments and share links. Archived artifacts are deleted at once: pass ids (or id) to empty the archive, and get done and failed by id; one not archived fails rather than being deleted. An artifact still in the library needs id, base_version and the person's say-so: the first call returns a confirmation token to show them, then call again with confirm. To tidy up reversibly, archive with update_artifact instead.", Annotations: &mcp.ToolAnnotations{DestructiveHint: &destructive}}, func(ctx context.Context, req *mcp.CallToolRequest, in DeleteInput) (*mcp.CallToolResult, any, error) {
		if in.IDs != nil {
			if in.ID != "" {
				return nil, nil, errors.New("send id or ids, not both")
			}
			out, e := s.bulk("delete", in.IDs)
			if e != nil {
				return nil, nil, e
			}
			return nil, out, nil
		}
		a, e := s.Store.Get(in.ID)
		if e != nil {
			return nil, nil, e
		}
		// Archiving was the deliberate first step, so an archived artifact needs no token.
		if !a.ArchivedAt.IsZero() {
			if e = s.Store.DeleteArchived(in.ID); e != nil {
				return nil, nil, e
			}
			return nil, map[string]any{"deleted": true, "id": in.ID}, nil
		}
		deleteMu.Lock()
		defer deleteMu.Unlock()
		for k, p := range deleteTokens {
			if time.Now().After(p.expires) {
				delete(deleteTokens, k)
			}
		}
		if in.Confirm == "" {
			if in.BaseVersion != len(a.Revisions) {
				return nil, nil, store.ErrConflict
			}
			b := make([]byte, 8)
			rand.Read(b)
			token := hex.EncodeToString(b)
			deleteTokens[token] = pendingDelete{in.ID, in.BaseVersion, time.Now().Add(10 * time.Minute)}
			r := a.Revisions[len(a.Revisions)-1]
			return nil, map[string]any{"confirm": token, "title": r.Title, "versions": len(a.Revisions), "message": fmt.Sprintf("Ask the person to confirm deleting %q and its %d versions; it cannot be undone. Then call again with this confirm token (valid 10 minutes).", r.Title, len(a.Revisions))}, nil
		}
		p, ok := deleteTokens[in.Confirm]
		if !ok || p.id != in.ID || p.base != in.BaseVersion {
			return nil, nil, errors.New("that confirmation is unknown or expired; call without confirm to ask again")
		}
		delete(deleteTokens, in.Confirm)
		if e = s.Store.Delete(in.ID, in.BaseVersion); e != nil {
			return nil, nil, e
		}
		return nil, map[string]any{"deleted": true, "id": in.ID}, nil
	})

	type WatchInput struct {
		IDs   []string `json:"ids,omitempty" jsonschema:"Artifacts to watch; omit for all"`
		After int64    `json:"after,omitempty" jsonschema:"The cursor from the previous call; 0 returns recent events"`
		Kinds []string `json:"kinds,omitempty" jsonschema:"version, comments, db, assets, archived (detail true or false) or deleted; omit for all"`
		Wait  int      `json:"wait_seconds,omitempty" jsonschema:"Wait up to this long (max 50) for the next event when there is none yet"`
	}
	mcp.AddTool(m, &mcp.Tool{Name: "watch_artifacts", Description: "Learn what changed: new versions (yours or the person's), comments, page data, assets, archiving and deletion. Returns events after the cursor, waiting up to wait_seconds for one; pass the returned cursor next time."}, func(ctx context.Context, req *mcp.CallToolRequest, in WatchInput) (*mcp.CallToolResult, any, error) {
		ids, kinds := map[string]bool{}, map[string]bool{}
		for _, id := range in.IDs {
			ids[id] = true
		}
		for _, k := range in.Kinds {
			kinds[k] = true
		}
		deadline := time.After(time.Duration(min(max(in.Wait, 0), 50)) * time.Second)
		for {
			events, cursor, wake := s.hub.since(in.After, ids, kinds)
			if len(events) > 100 {
				events = events[len(events)-100:]
			}
			if len(events) > 0 || in.Wait <= 0 {
				return nil, map[string]any{"events": events, "cursor": cursor}, nil
			}
			select {
			case <-wake:
			case <-deadline:
				return nil, map[string]any{"events": events, "cursor": cursor}, nil
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
		}
	})
}
