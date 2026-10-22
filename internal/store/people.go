package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash/fnv"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// A Person is someone who uses this server: the identity artifacts see through the
// `user` capability, and the owner of their private `data/users/<id>/` documents. There
// are no accounts: on a trusted network people say who they are, so this names people
// and keeps their data apart; it is not a security boundary.
type Person struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Color     string    `json:"color"`
	CreatedAt time.Time `json:"created_at"`
}

// Contrast-safe in light and dark, like Claude's shell palette.
var personColors = []string{"#b5523b", "#5db8a6", "#6a8fd8", "#c26aa6", "#d4a017", "#5db872", "#9a7bd1", "#d0694d", "#4aa3c4", "#8c9a3a"}

func (s *Store) peoplePath() string { return filepath.Join(s.dir, "meta", "people.json") }

func (s *Store) loadPeople() error {
	s.people = map[string]Person{}
	b, err := os.ReadFile(s.peoplePath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var list []Person
	if err = json.Unmarshal(b, &list); err != nil {
		return errors.New("read people: " + err.Error())
	}
	for _, p := range list {
		s.people[p.ID] = p
	}
	return nil
}

// People lists everyone, oldest first.
func (s *Store) People() []Person {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Person, 0, len(s.people))
	for _, p := range s.people {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

func (s *Store) Person(id string) (Person, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.people[id]
	return p, ok
}

// AddPerson adds someone by name, or returns the person already using that name.
func (s *Store) AddPerson(name string) (Person, error) {
	name = strings.Join(strings.Fields(name), " ")
	if name == "" || len(name) > 60 {
		return Person{}, errors.New("name must contain 1–60 bytes")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.people {
		if strings.EqualFold(p.Name, name) {
			return p, nil
		}
	}
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return Person{}, err
	}
	id := "u_" + hex.EncodeToString(b)
	h := fnv.New32a()
	h.Write([]byte(id))
	p := Person{ID: id, Name: name, Color: personColors[h.Sum32()%uint32(len(personColors))], CreatedAt: time.Now().UTC()}
	s.people[id] = p
	list := make([]Person, 0, len(s.people))
	for _, q := range s.people {
		list = append(list, q)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt.Before(list[j].CreatedAt) })
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return Person{}, err
	}
	if err = os.MkdirAll(filepath.Dir(s.peoplePath()), 0700); err != nil {
		return Person{}, err
	}
	if err = writeAtomic(filepath.Dir(s.peoplePath()), s.peoplePath(), data); err != nil {
		delete(s.people, id)
		return Person{}, err
	}
	return p, nil
}
