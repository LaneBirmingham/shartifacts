package server

import (
	"errors"
	"net/http"
)

// storageOp is one window.storage call. The preview bridge, the HTTP API and MCP share it,
// and results keep the shapes Claude's window.storage returns.
type storageOp struct {
	Op     string  `json:"op" jsonschema:"get, set, delete or list"`
	Key    string  `json:"key,omitempty" jsonschema:"Key for get, set and delete"`
	Value  *string `json:"value,omitempty" jsonschema:"String value for set (JSON.stringify objects)"`
	Shared bool    `json:"shared,omitempty" jsonschema:"Use the shared scope instead of the personal one"`
	Prefix string  `json:"prefix,omitempty" jsonschema:"Key prefix for list"`
}

// runStorage returns nil for a get of a missing key, as window.storage.get resolves null.
func (s *Server) runStorage(id string, in storageOp) (map[string]any, error) {
	switch in.Op {
	case "get":
		v, ok, e := s.Store.StorageGet(id, in.Key, in.Shared)
		if e != nil || !ok {
			return nil, e
		}
		return map[string]any{"key": in.Key, "value": v, "shared": in.Shared}, nil
	case "set":
		if in.Value == nil {
			return nil, errors.New("set needs a string value")
		}
		if e := s.Store.StorageSet(id, in.Key, *in.Value, in.Shared); e != nil {
			return nil, e
		}
		return map[string]any{"key": in.Key, "value": *in.Value, "shared": in.Shared}, nil
	case "delete":
		deleted, e := s.Store.StorageDelete(id, in.Key, in.Shared)
		if e != nil {
			return nil, e
		}
		return map[string]any{"key": in.Key, "deleted": deleted, "shared": in.Shared}, nil
	case "list":
		keys, e := s.Store.StorageList(id, in.Prefix, in.Shared)
		if e != nil {
			return nil, e
		}
		return map[string]any{"keys": keys, "prefix": in.Prefix, "shared": in.Shared}, nil
	}
	return nil, errors.New("op must be get, set, delete or list")
}

func (s *Server) storage(w http.ResponseWriter, r *http.Request) {
	var in storageOp
	if !decode(w, r, &in) {
		return
	}
	result, e := s.runStorage(r.PathValue("id"), in)
	if e == nil && result == nil {
		writeJSON(w, 200, nil)
		return
	}
	respond(w, 200, result, e)
}
