// The notes backend: a Go HTTP server on 127.0.0.1:$PORT that keeps its data in $DATA_DIR.
// shartifacts strips the app's base URL, so routes start at /api/.
//
// Several versions of an app can run at once against the same DATA_DIR (the live one, a
// preview, the one it just replaced), so the notes live on disk, not in memory: every
// request reads the file, and writes hold a file lock.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Note struct {
	ID        string    `json:"id"`
	Text      string    `json:"text"`
	Author    string    `json:"author,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type notes struct {
	mu   sync.Mutex
	path string
}

func (n *notes) load() []Note {
	list := []Note{}
	if b, err := os.ReadFile(n.path); err == nil {
		json.Unmarshal(b, &list)
	}
	return list
}

// update changes the list under a lock shared with every other process using the file.
func (n *notes) update(change func([]Note) ([]Note, error)) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	lock, err := os.OpenFile(n.path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	list, err := change(n.load())
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := n.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, n.path)
}

var errNoNote = errors.New("no such note")

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// viewer is who shartifacts says is using the app (empty for share-link visitors).
func viewer(r *http.Request) string {
	var u struct {
		Name string `json:"name"`
	}
	json.Unmarshal([]byte(r.Header.Get("X-Shartifacts-User")), &u)
	return u.Name
}

func main() {
	dir := os.Getenv("DATA_DIR")
	if dir == "" {
		dir = "."
	}
	store := &notes{path: filepath.Join(dir, "notes.json")}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true, "version": os.Getenv("APP_VERSION")})
	})
	mux.HandleFunc("GET /api/me", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"name": viewer(r), "access": r.Header.Get("X-Shartifacts-Access")})
	})
	mux.HandleFunc("GET /api/notes", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, store.load())
	})
	mux.HandleFunc("POST /api/notes", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil || strings.TrimSpace(in.Text) == "" {
			writeJSON(w, 400, map[string]string{"error": "send {\"text\": \"…\"}"})
			return
		}
		b := make([]byte, 6)
		rand.Read(b)
		note := Note{ID: hex.EncodeToString(b), Text: strings.TrimSpace(in.Text), Author: viewer(r), CreatedAt: time.Now().UTC()}
		err := store.update(func(list []Note) ([]Note, error) { return append([]Note{note}, list...), nil })
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 201, note)
	})
	mux.HandleFunc("DELETE /api/notes/{id}", func(w http.ResponseWriter, r *http.Request) {
		err := store.update(func(list []Note) ([]Note, error) {
			for i, n := range list {
				if n.ID == r.PathValue("id") {
					return append(list[:i], list[i+1:]...), nil
				}
			}
			return nil, errNoNote
		})
		switch {
		case errors.Is(err, errNoNote):
			writeJSON(w, 404, map[string]string{"error": err.Error()})
		case err != nil:
			writeJSON(w, 500, map[string]string{"error": err.Error()})
		default:
			writeJSON(w, 200, map[string]bool{"deleted": true})
		}
	})

	host, port := os.Getenv("HOST"), os.Getenv("PORT")
	if port == "" {
		host, port = "127.0.0.1", "8081"
	}
	srv := &http.Server{Addr: net.JoinHostPort(host, port), Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	log.Printf("notes listening on %s, data in %s", srv.Addr, dir)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
