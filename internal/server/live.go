package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/LaneBirmingham/shartifacts/internal/store"
)

// The live hub carries what happens to an artifact to the pages showing it, as server-sent
// events: document, version, comment and asset changes (from the store), and the `room`
// capability's presence and messages between everyone viewing it right now. It also keeps
// a short history for agents watching for changes (watch_artifacts).

type liveClient struct {
	artifact, peer, viewer string
	// public clients are share-link visitors: they hear changes to documents they may read,
	// deletion, and, unless the link is pinned to one version, new versions.
	public, pinned bool
	send           chan []byte
	// done closes when a reconnect replaces this stream.
	done chan struct{}
}

type roomPeer struct {
	Peer      string         `json:"peer"`
	By        *string        `json:"by"`
	Presence  map[string]any `json:"presence"`
	UpdatedAt int64          `json:"updatedAt"`
}

// LiveEvent is one change in the history agents can watch.
type LiveEvent struct {
	Seq      int64     `json:"seq"`
	Artifact string    `json:"artifact"`
	Kind     string    `json:"kind"`
	Detail   string    `json:"detail,omitempty"`
	At       time.Time `json:"at"`
}

type hub struct {
	mu      sync.Mutex
	clients map[string]map[*liveClient]bool
	// rooms[artifact][room][peer]
	rooms   map[string]map[string]map[string]*roomPeer
	history []LiveEvent
	seq     int64
	wake    chan struct{}
	// visible reports whether a share visitor may read the document at a path.
	visible func(artifact, path string) bool
}

var (
	roomName  = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,47}$`)
	roomTopic = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,47}$`)
	peerLabel = regexp.MustCompile(`^[a-z0-9]{8,32}$`)
)

func newHub() *hub {
	return &hub{clients: map[string]map[*liveClient]bool{}, rooms: map[string]map[string]map[string]*roomPeer{}, wake: make(chan struct{})}
}

func (h *hub) broadcast(artifact string, v any, to func(*liveClient) bool) {
	b, _ := json.Marshal(v)
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients[artifact] {
		if to == nil || to(c) {
			select {
			case c.send <- b:
			default: // a stalled page misses events rather than blocking everyone
			}
		}
	}
}

// changed is the store's change listener.
func (h *hub) changed(artifact, kind, detail string) {
	visible := kind == "deleted" || kind == "version" || kind == "db" && h.visible != nil && h.visible(artifact, detail)
	h.broadcast(artifact, map[string]string{"type": kind, "detail": detail}, func(c *liveClient) bool {
		return !c.public || visible && !(kind == "version" && c.pinned)
	})
	h.mu.Lock()
	h.seq++
	h.history = append(h.history, LiveEvent{h.seq, artifact, kind, detail, time.Now().UTC()})
	if len(h.history) > 2000 {
		h.history = h.history[len(h.history)-2000:]
	}
	close(h.wake)
	h.wake = make(chan struct{})
	h.mu.Unlock()
}

// since returns events after seq (for artifacts in ids, if any), and a channel closed at
// the next event.
func (h *hub) since(seq int64, ids map[string]bool, kinds map[string]bool) ([]LiveEvent, int64, chan struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := []LiveEvent{}
	for _, e := range h.history {
		if e.Seq > seq && (len(ids) == 0 || ids[e.Artifact]) && (len(kinds) == 0 || kinds[e.Kind]) {
			out = append(out, e)
		}
	}
	return out, h.seq, h.wake
}

func (h *hub) peers(artifact, room string) []*roomPeer {
	out := []*roomPeer{}
	for _, p := range h.rooms[artifact][room] {
		out = append(out, p)
	}
	return out
}

// sendPeers tells everyone in a room who is there. h.mu is not held.
func (h *hub) sendPeers(artifact, room string) {
	h.mu.Lock()
	list := h.peers(artifact, room)
	members := map[string]bool{}
	for p := range h.rooms[artifact][room] {
		members[p] = true
	}
	h.mu.Unlock()
	h.broadcast(artifact, map[string]any{"type": "room", "room": room, "kind": "peers", "peers": list}, func(c *liveClient) bool { return !c.public && members[c.peer] })
}

func (h *hub) leaveAll(artifact, peer string) {
	h.mu.Lock()
	left := []string{}
	for name, room := range h.rooms[artifact] {
		if _, ok := room[peer]; ok {
			delete(room, peer)
			left = append(left, name)
		}
	}
	h.mu.Unlock()
	for _, name := range left {
		h.sendPeers(artifact, name)
	}
}

// live streams an artifact's events to one page: GET /api/artifacts/{id}/live?peer=&viewer=
func (s *Server) live(w http.ResponseWriter, r *http.Request) {
	if _, e := s.Store.Get(r.PathValue("id")); e != nil {
		http.NotFound(w, r)
		return
	}
	s.liveFor(w, r, r.PathValue("id"), s.viewerID(r), nil)
}

// liveFor streams one artifact's events, to a share visitor when share is set. Only members
// (not share visitors) may join rooms.
func (s *Server) liveFor(w http.ResponseWriter, r *http.Request, id, viewer string, share *store.Share) {
	peer := r.URL.Query().Get("peer")
	if !peerLabel.MatchString(peer) {
		http.Error(w, "peer must be 8–32 lowercase letters and digits", 400)
		return
	}
	c := &liveClient{artifact: id, peer: peer, viewer: viewer, public: share != nil, pinned: share != nil && share.Version != 0, send: make(chan []byte, 256), done: make(chan struct{})}
	s.hub.mu.Lock()
	if s.hub.clients[id] == nil {
		s.hub.clients[id] = map[*liveClient]bool{}
	}
	for old := range s.hub.clients[id] {
		if old.peer == peer && old.public == c.public {
			delete(s.hub.clients[id], old) // a reconnect replaces the old stream
			close(old.done)
		}
	}
	s.hub.clients[id][c] = true
	s.hub.mu.Unlock()
	defer func() {
		s.hub.mu.Lock()
		_, current := s.hub.clients[id][c]
		delete(s.hub.clients[id], c)
		s.hub.mu.Unlock()
		// A replaced stream leaves the rooms to the stream that replaced it.
		if !c.public && current {
			s.hub.leaveAll(id, peer)
		}
	}()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	rc := http.NewResponseController(w)
	write := func(b []byte) bool {
		rc.SetWriteDeadline(time.Now().Add(time.Minute))
		if _, e := fmt.Fprintf(w, "data: %s\n\n", b); e != nil {
			return false
		}
		return rc.Flush() == nil
	}
	if !write([]byte(`{"type":"hello"}`)) {
		return
	}
	tick := time.NewTicker(25 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-c.done:
			return
		case b := <-c.send:
			if !write(b) {
				return
			}
		case <-tick.C:
			// A revoked link stops hearing about changes.
			if share != nil {
				if _, _, _, ok := s.shared(r); !ok {
					return
				}
			}
			rc.SetWriteDeadline(time.Now().Add(time.Minute))
			if _, e := fmt.Fprint(w, ": keepalive\n\n"); e != nil || rc.Flush() != nil {
				return
			}
		}
	}
}

// room handles the `room` capability's sends: POST /api/artifacts/{id}/room
func (s *Server) room(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Peer  string         `json:"peer"`
		Room  string         `json:"room"`
		Op    string         `json:"op"`
		Topic string         `json:"topic,omitempty"`
		Data  any            `json:"data,omitempty"`
		Patch map[string]any `json:"patch,omitempty"`
	}
	if !decode(w, r, &in) {
		return
	}
	id := r.PathValue("id")
	if in.Room == "" {
		in.Room = "lobby"
	}
	fail := func(code, msg string) { writeJSON(w, 400, map[string]string{"error": msg, "code": code}) }
	if !peerLabel.MatchString(in.Peer) || (in.Room != "lobby" && !roomName.MatchString(in.Room)) {
		fail("invalid_argument", "peer or room name is malformed")
		return
	}
	s.hub.mu.Lock()
	connected := false
	for c := range s.hub.clients[id] {
		connected = connected || (c.peer == in.Peer && !c.public)
	}
	if s.hub.rooms[id] == nil {
		s.hub.rooms[id] = map[string]map[string]*roomPeer{}
	}
	room := s.hub.rooms[id][in.Room]
	me := room[in.Peer]
	s.hub.mu.Unlock()
	if !connected {
		fail("upstream_error", "this page has no live connection; reconnect first")
		return
	}
	var by *string
	if v := s.viewerID(r); v != "" {
		by = &v
	}
	switch in.Op {
	case "join":
		s.hub.mu.Lock()
		if room == nil {
			room = map[string]*roomPeer{}
			s.hub.rooms[id][in.Room] = room
		}
		joined := 0
		for _, rm := range s.hub.rooms[id] {
			if _, ok := rm[in.Peer]; ok {
				joined++
			}
		}
		if me == nil && joined >= 17 {
			s.hub.mu.Unlock()
			fail("limit_reached", "a page joins at most 16 named rooms")
			return
		}
		if me == nil {
			room[in.Peer] = &roomPeer{Peer: in.Peer, By: by, Presence: map[string]any{}, UpdatedAt: time.Now().UnixMilli()}
		}
		s.hub.mu.Unlock()
		s.hub.sendPeers(id, in.Room)
	case "leave":
		s.hub.mu.Lock()
		delete(room, in.Peer)
		s.hub.mu.Unlock()
		s.hub.sendPeers(id, in.Room)
	case "emit":
		if me == nil {
			fail("invalid_argument", "join the room first")
			return
		}
		if !roomTopic.MatchString(in.Topic) {
			fail("invalid_argument", "topic must match ^[a-z][a-z0-9_.-]{0,47}$")
			return
		}
		if b, _ := json.Marshal(in.Data); len(b) > 4096 {
			fail("invalid_argument", "data is at most 4 KiB of JSON")
			return
		}
		members := map[string]bool{}
		s.hub.mu.Lock()
		for p := range room {
			members[p] = true
		}
		s.hub.mu.Unlock()
		s.hub.broadcast(id, map[string]any{"type": "room", "room": in.Room, "kind": "message", "topic": in.Topic, "data": in.Data, "peer": in.Peer, "by": by}, func(c *liveClient) bool { return !c.public && members[c.peer] })
	case "presence":
		if me == nil {
			fail("invalid_argument", "join the room first")
			return
		}
		s.hub.mu.Lock()
		merged := map[string]any{}
		for k, v := range me.Presence {
			merged[k] = v
		}
		for k, v := range in.Patch {
			if v == nil {
				delete(merged, k)
			} else {
				merged[k] = v
			}
		}
		if b, _ := json.Marshal(merged); len(b) > 4096 {
			s.hub.mu.Unlock()
			fail("invalid_argument", "presence is at most 4 KiB of JSON")
			return
		}
		room[in.Peer] = &roomPeer{Peer: in.Peer, By: me.By, Presence: merged, UpdatedAt: time.Now().UnixMilli()}
		s.hub.mu.Unlock()
		s.hub.sendPeers(id, in.Room)
	default:
		fail("invalid_argument", "op must be join, leave, emit or presence")
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
