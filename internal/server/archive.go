package server

import (
	"errors"
	"fmt"
	"net/http"
)

// maxBulk is how many artifacts one bulk action names: enough to clear a long-neglected
// library in a few calls, few enough that one call stays quick.
const maxBulk = 200

// bulkFailure is one artifact a bulk action left as it was, and why.
type bulkFailure struct {
	ID    string `json:"id"`
	Error string `json:"error"`
}

// bulkResult reports a bulk action artifact by artifact, since one stale or missing ID
// should not undo the rest.
type bulkResult struct {
	Op     string        `json:"op"`
	Done   []string      `json:"done"`
	Failed []bulkFailure `json:"failed"`
}

// bulk archives, restores or permanently deletes artifacts. Delete takes only archived
// artifacts: archiving first is what makes deleting many at once without asking safe.
func (s *Server) bulk(op string, ids []string) (bulkResult, error) {
	var act func(string) error
	switch op {
	case "archive", "restore":
		act = func(id string) error {
			_, err := s.Store.SetArchived(id, op == "archive")
			return err
		}
	case "delete":
		act = s.Store.DeleteArchived
	default:
		return bulkResult{}, errors.New("op must be archive, restore or delete")
	}
	if len(ids) == 0 {
		return bulkResult{}, errors.New("name at least one artifact in ids")
	}
	if len(ids) > maxBulk {
		return bulkResult{}, fmt.Errorf("at most %d artifacts at a time", maxBulk)
	}
	out := bulkResult{Op: op, Done: []string{}, Failed: []bulkFailure{}}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		if err := act(id); err != nil {
			out.Failed = append(out.Failed, bulkFailure{id, err.Error()})
		} else {
			out.Done = append(out.Done, id)
		}
	}
	return out, nil
}

func (s *Server) archiveRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/artifacts/bulk", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Op  string   `json:"op"`
			IDs []string `json:"ids"`
		}
		if !decode(w, r, &in) {
			return
		}
		out, e := s.bulk(in.Op, in.IDs)
		respond(w, 200, out, e)
	})
}
