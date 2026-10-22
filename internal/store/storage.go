package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Artifact storage mirrors Claude's window.storage: string values under short keys, in a
// personal and a shared scope per artifact. This server has one user, so both scopes are
// simply kept; the distinction keeps artifacts written for Claude working unchanged.
const (
	MaxStorageKey   = 200
	MaxStorageValue = 5 << 20
	MaxStorageTotal = 20 << 20
)

var ErrStorageFull = errors.New("artifact storage is full (20 MiB); delete keys first")

type kv struct {
	Personal map[string]string `json:"personal"`
	Shared   map[string]string `json:"shared"`
}

func (k *kv) scope(shared bool) map[string]string {
	if shared {
		return k.Shared
	}
	return k.Personal
}
func (k *kv) size() int {
	n := 0
	for _, m := range []map[string]string{k.Personal, k.Shared} {
		for key, v := range m {
			n += len(key) + len(v)
		}
	}
	return n
}

func validKey(key string) error {
	if key == "" || len(key) > MaxStorageKey {
		return fmt.Errorf("key must contain 1–%d bytes", MaxStorageKey)
	}
	if strings.ContainsAny(key, " \t\r\n/\\'\"`") {
		return errors.New("key cannot contain whitespace, slashes or quotes")
	}
	return nil
}

func (s *Store) storagePath(id string) string { return filepath.Join(s.dir, "storage", id+".json") }

// loadKV returns the artifact's storage, reading it from disk on first use. s.kvMu is held.
func (s *Store) loadKV(id string) (*kv, error) {
	s.mu.Lock()
	_, exists := s.items[id]
	s.mu.Unlock()
	if !exists {
		return nil, ErrNotFound
	}
	if k, ok := s.kv[id]; ok {
		return k, nil
	}
	k := &kv{Personal: map[string]string{}, Shared: map[string]string{}}
	b, err := os.ReadFile(s.storagePath(id))
	if err == nil {
		if err = json.Unmarshal(b, k); err != nil {
			return nil, fmt.Errorf("read storage for %s: %w", id, err)
		}
		if k.Personal == nil {
			k.Personal = map[string]string{}
		}
		if k.Shared == nil {
			k.Shared = map[string]string{}
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if s.kv == nil {
		s.kv = map[string]*kv{}
	}
	s.kv[id] = k
	return k, nil
}

func (s *Store) saveKV(id string, k *kv) error {
	dir := filepath.Join(s.dir, "storage")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	b, err := json.Marshal(k)
	if err != nil {
		return err
	}
	return writeAtomic(dir, s.storagePath(id), b)
}

func (s *Store) StorageGet(id, key string, shared bool) (string, bool, error) {
	if err := validKey(key); err != nil {
		return "", false, err
	}
	s.kvMu.Lock()
	defer s.kvMu.Unlock()
	k, err := s.loadKV(id)
	if err != nil {
		return "", false, err
	}
	v, ok := k.scope(shared)[key]
	return v, ok, nil
}

func (s *Store) StorageSet(id, key, value string, shared bool) error {
	if err := validKey(key); err != nil {
		return err
	}
	if len(value) > MaxStorageValue {
		return errors.New("value exceeds 5 MiB")
	}
	s.kvMu.Lock()
	defer s.kvMu.Unlock()
	k, err := s.loadKV(id)
	if err != nil {
		return err
	}
	m := k.scope(shared)
	old, had := m[key]
	next := k.size() + len(value)
	if had {
		next -= len(old)
	} else {
		next += len(key)
	}
	if next > MaxStorageTotal {
		return ErrStorageFull
	}
	m[key] = value
	if err = s.saveKV(id, k); err != nil {
		if had {
			m[key] = old
		} else {
			delete(m, key)
		}
		return err
	}
	return nil
}

func (s *Store) StorageDelete(id, key string, shared bool) (bool, error) {
	if err := validKey(key); err != nil {
		return false, err
	}
	s.kvMu.Lock()
	defer s.kvMu.Unlock()
	k, err := s.loadKV(id)
	if err != nil {
		return false, err
	}
	m := k.scope(shared)
	old, had := m[key]
	if !had {
		return false, nil
	}
	delete(m, key)
	if err = s.saveKV(id, k); err != nil {
		m[key] = old
		return false, err
	}
	return true, nil
}

func (s *Store) StorageList(id, prefix string, shared bool) ([]string, error) {
	s.kvMu.Lock()
	defer s.kvMu.Unlock()
	k, err := s.loadKV(id)
	if err != nil {
		return nil, err
	}
	keys := []string{}
	for key := range k.scope(shared) {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys, nil
}

// dropStorage removes an artifact's storage with the artifact itself.
func (s *Store) dropStorage(id string) error {
	s.kvMu.Lock()
	defer s.kvMu.Unlock()
	delete(s.kv, id)
	if err := os.Remove(s.storagePath(id)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
