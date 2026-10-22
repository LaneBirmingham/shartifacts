package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// A Share is a read-only link to one artifact for people outside the network, served
// only on the operator's share origin. Version 0 follows the latest version.
type Share struct {
	Token     string    `json:"token"`
	Artifact  string    `json:"artifact"`
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Store) sharesPath() string { return filepath.Join(s.dir, "meta", "shares.json") }

func (s *Store) loadShares() error {
	s.shares = map[string]Share{}
	b, err := os.ReadFile(s.sharesPath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var list []Share
	if err = json.Unmarshal(b, &list); err != nil {
		return errors.New("read shares: " + err.Error())
	}
	for _, sh := range list {
		if _, ok := s.items[sh.Artifact]; ok {
			s.shares[sh.Token] = sh
		}
	}
	return nil
}

// writeShares persists the share list. s.mu is held.
func (s *Store) writeShares() error {
	list := make([]Share, 0, len(s.shares))
	for _, sh := range s.shares {
		list = append(list, sh)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt.Before(list[j].CreatedAt) })
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(s.sharesPath()), 0700); err != nil {
		return err
	}
	return writeAtomic(filepath.Dir(s.sharesPath()), s.sharesPath(), b)
}

// CreateShare makes a link to the artifact at version (0: always the latest).
func (s *Store) CreateShare(artifact string, version int) (Share, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.items[artifact]
	if !ok {
		return Share{}, ErrNotFound
	}
	if version < 0 || version > len(a.Revisions) {
		return Share{}, errors.New("no such version")
	}
	if len(s.shares) >= 1000 {
		return Share{}, errors.New("at most 1000 share links; revoke some first")
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return Share{}, err
	}
	sh := Share{Token: hex.EncodeToString(b), Artifact: artifact, Version: version, CreatedAt: time.Now().UTC()}
	s.shares[sh.Token] = sh
	if err := s.writeShares(); err != nil {
		delete(s.shares, sh.Token)
		return Share{}, err
	}
	return sh, nil
}

func (s *Store) Shares(artifact string) []Share {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Share{}
	for _, sh := range s.shares {
		if sh.Artifact == artifact {
			out = append(out, sh)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// Shared resolves a token to its share and artifact.
func (s *Store) Shared(token string) (Share, Artifact, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sh, ok := s.shares[token]
	if !ok {
		return Share{}, Artifact{}, false
	}
	a, ok := s.items[sh.Artifact]
	if !ok {
		return Share{}, Artifact{}, false
	}
	a.Revisions = append([]Revision(nil), a.Revisions...)
	return sh, a, true
}

// RevokeShare ends a link; false when there was none.
func (s *Store) RevokeShare(token string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.shares[token]
	if !ok {
		return false, nil
	}
	delete(s.shares, token)
	if err := s.writeShares(); err != nil {
		s.shares[token] = old
		return false, err
	}
	return true, nil
}

// dropShares ends an artifact's links with it. s.mu is held.
func (s *Store) dropShares(artifact string) error {
	changed := false
	for t, sh := range s.shares {
		if sh.Artifact == artifact {
			delete(s.shares, t)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return s.writeShares()
}
