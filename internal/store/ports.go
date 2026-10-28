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
	"time"
)

// A PortClaim records that a service listens on a port of a machine on the network, so an
// agent about to deploy something there can pick a port nothing else uses. shartifacts
// cannot see other machines, so the registry knows only what agents and people claim; it
// is a shared ledger, not a scan. App backends never appear here: the runner gives each a
// free loopback port of its own ($PORT).
type PortClaim struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Protocol string `json:"protocol"`
	Service  string `json:"service"`
	// Owner is who looks after the service: an agent, a person, or a tool such as docker.
	Owner     string    `json:"owner,omitempty"`
	Note      string    `json:"note,omitempty"`
	ClaimedAt time.Time `json:"claimed_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type portKey struct {
	host     string
	port     int
	protocol string
}

func (c PortClaim) key() portKey { return portKey{c.Host, c.Port, c.Protocol} }

// PortTakenError refuses a claim on a port another service holds. It names the holder,
// and the next port up that is free, so an agent can move on without listing everything.
type PortTakenError struct {
	Holder PortClaim
	// Next is the lowest unclaimed port above the one asked for; 0 when there is none.
	Next int
}

func (e *PortTakenError) Error() string {
	msg := fmt.Sprintf("%d/%s on %s is claimed by %q", e.Holder.Port, e.Holder.Protocol, e.Holder.Host, e.Holder.Service)
	if e.Holder.Owner != "" {
		msg += " (" + e.Holder.Owner + ")"
	}
	msg += " since " + e.Holder.ClaimedAt.Format("2006-01-02")
	if e.Next > 0 {
		msg += fmt.Sprintf("; the next free port is %d", e.Next)
	}
	return msg
}

// Unwrap makes a taken port a conflict, which the API answers with 409.
func (e *PortTakenError) Unwrap() error { return ErrConflict }

const (
	// maxPortClaims bounds the registry, which a home or office network stays well within.
	maxPortClaims = 5000
	// DefaultPortFrom is where a claim without a port starts looking: the first port that
	// needs no privileges.
	DefaultPortFrom = 1024
)

// hostName is how a machine is named in the registry: a hostname, an IP address or a
// short alias, lowercased so one machine is not two.
var hostName = regexp.MustCompile(`^[a-z0-9]([a-z0-9._:-]{0,98}[a-z0-9])?$`)

func (s *Store) portsPath() string { return filepath.Join(s.dir, "meta", "ports.json") }

func (s *Store) loadPorts() error {
	s.ports = map[portKey]PortClaim{}
	b, err := os.ReadFile(s.portsPath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var list []PortClaim
	if err = json.Unmarshal(b, &list); err != nil {
		return errors.New("read ports: " + err.Error())
	}
	for _, c := range list {
		s.ports[c.key()] = c
	}
	return nil
}

// writePorts persists the registry. s.mu is held.
func (s *Store) writePorts() error {
	b, err := json.MarshalIndent(s.portList(""), "", "  ")
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(s.portsPath()), 0700); err != nil {
		return err
	}
	return writeAtomic(filepath.Dir(s.portsPath()), s.portsPath(), b)
}

// portList is the registry, or one host's part of it, by host, port and protocol. s.mu is held.
func (s *Store) portList(host string) []PortClaim {
	out := []PortClaim{}
	for _, c := range s.ports {
		if host == "" || c.Host == host {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Host != b.Host {
			return a.Host < b.Host
		}
		if a.Port != b.Port {
			return a.Port < b.Port
		}
		return a.Protocol < b.Protocol
	})
	return out
}

// NormalizePortHost lowercases and checks a host name for the registry.
func NormalizePortHost(host string) (string, error) {
	host = strings.ToLower(strings.TrimSpace(host))
	if !hostName.MatchString(host) {
		return "", errors.New("host must be a hostname, IP address or alias: 1–100 letters, digits, dots, colons, hyphens or underscores")
	}
	return host, nil
}

func normalizeProtocol(p string) (string, error) {
	switch p = strings.ToLower(strings.TrimSpace(p)); p {
	case "":
		return "tcp", nil
	case "tcp", "udp":
		return p, nil
	}
	return "", errors.New("protocol must be tcp or udp")
}

// Ports lists the registry, or one host's claims when host is set.
func (s *Store) Ports(host string) ([]PortClaim, error) {
	if host != "" {
		var err error
		if host, err = NormalizePortHost(host); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.portList(host), nil
}

// PortHosts names every host with a claim, so agents reuse a machine's name rather than
// registering it twice under another.
func (s *Store) PortHosts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	out := []string{}
	for _, c := range s.ports {
		if !seen[c.Host] {
			seen[c.Host] = true
			out = append(out, c.Host)
		}
	}
	sort.Strings(out)
	return out
}

// ClaimPort records that in.Service uses in.Port on in.Host. Port 0 claims the lowest free
// port at or above from (DefaultPortFrom when 0). The check and the claim happen together,
// so two agents asking at once never get the same port. Claiming a port the same service
// already holds updates its owner and note (keeping either when left empty); one another
// service holds is a *PortTakenError.
func (s *Store) ClaimPort(in PortClaim, from int) (PortClaim, bool, error) {
	var err error
	if in.Host, err = NormalizePortHost(in.Host); err != nil {
		return PortClaim{}, false, err
	}
	if in.Protocol, err = normalizeProtocol(in.Protocol); err != nil {
		return PortClaim{}, false, err
	}
	in.Service = strings.Join(strings.Fields(in.Service), " ")
	in.Owner = strings.Join(strings.Fields(in.Owner), " ")
	in.Note = strings.TrimSpace(in.Note)
	switch {
	case in.Service == "" || len(in.Service) > 100:
		return PortClaim{}, false, errors.New("service must name what listens there in 1–100 bytes")
	case len(in.Owner) > 100:
		return PortClaim{}, false, errors.New("owner must be at most 100 bytes")
	case len(in.Note) > 500:
		return PortClaim{}, false, errors.New("note must be at most 500 bytes")
	case in.Port < 0 || in.Port > 65535:
		return PortClaim{}, false, errors.New("port must be 1–65535, or 0 for the next free one")
	case from < 0 || from > 65535:
		return PortClaim{}, false, errors.New("from must be 1–65535")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if in.Port == 0 {
		if from == 0 {
			from = DefaultPortFrom
		}
		if in.Port = s.freePort(in.Host, in.Protocol, from); in.Port == 0 {
			return PortClaim{}, false, fmt.Errorf("no free %s port on %s at or above %d", in.Protocol, in.Host, from)
		}
	}
	now := time.Now().UTC()
	old, held := s.ports[in.key()]
	if held && !strings.EqualFold(old.Service, in.Service) {
		return PortClaim{}, false, &PortTakenError{Holder: old, Next: s.freePort(in.Host, in.Protocol, in.Port+1)}
	}
	in.ClaimedAt, in.UpdatedAt = now, now
	if held {
		// Claiming again is an update, and like other updates it keeps what it leaves out.
		in.ClaimedAt = old.ClaimedAt
		if in.Owner == "" {
			in.Owner = old.Owner
		}
		if in.Note == "" {
			in.Note = old.Note
		}
	} else if len(s.ports) >= maxPortClaims {
		return PortClaim{}, false, fmt.Errorf("at most %d port claims; release some first", maxPortClaims)
	}
	s.ports[in.key()] = in
	if err := s.writePorts(); err != nil {
		if held {
			s.ports[in.key()] = old
		} else {
			delete(s.ports, in.key())
		}
		return PortClaim{}, false, err
	}
	return in, !held, nil
}

// freePort is the lowest unclaimed port at or above from on a host, or 0. s.mu is held.
func (s *Store) freePort(host, protocol string, from int) int {
	for p := max(from, 1); p <= 65535; p++ {
		if _, ok := s.ports[portKey{host, p, protocol}]; !ok {
			return p
		}
	}
	return 0
}

// ReleasePort ends a claim, returning it; false when there was none.
func (s *Store) ReleasePort(host string, port int, protocol string) (PortClaim, bool, error) {
	var err error
	if host, err = NormalizePortHost(host); err != nil {
		return PortClaim{}, false, err
	}
	if protocol, err = normalizeProtocol(protocol); err != nil {
		return PortClaim{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := portKey{host, port, protocol}
	old, ok := s.ports[k]
	if !ok {
		return PortClaim{}, false, nil
	}
	delete(s.ports, k)
	if err := s.writePorts(); err != nil {
		s.ports[k] = old
		return PortClaim{}, false, err
	}
	return old, true, nil
}
