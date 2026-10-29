package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os/exec"
	"sync"
	"time"
)

//go:embed web
var webFS embed.FS

// hub keeps the latest board and pushes it to every open page, which shows
// it only on a refresh but uses it to tell that something changed.
type hub struct {
	mu      sync.Mutex
	board   []byte
	clients map[chan []byte]struct{}
}

func newHub() *hub {
	return &hub{board: []byte("{}"), clients: map[chan []byte]struct{}{}}
}

func (h *hub) publish(board Board) {
	data, err := json.Marshal(board)
	if err != nil {
		log.Printf("encode board: %v", err)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if bytes.Equal(withoutUpdatedAt(data), withoutUpdatedAt(h.board)) {
		return
	}
	h.board = data
	for c := range h.clients {
		select {
		case c <- data:
		default: // a slow page catches up on the next change
		}
	}
}

// withoutUpdatedAt drops the timestamp so an unchanged board is not resent.
func withoutUpdatedAt(data []byte) []byte {
	if i := bytes.LastIndex(data, []byte(`,"updated_at"`)); i >= 0 {
		return data[:i]
	}
	return data
}

func (h *hub) subscribe() (chan []byte, []byte, func()) {
	c := make(chan []byte, 1)
	h.mu.Lock()
	h.clients[c] = struct{}{}
	current := h.board
	h.mu.Unlock()
	return c, current, func() {
		h.mu.Lock()
		delete(h.clients, c)
		h.mu.Unlock()
	}
}

type server struct {
	cfg     Config
	hub     *hub
	refresh func(context.Context) Board
	paths   func() map[string]bool
}

func (s *server) routes() http.Handler {
	static, _ := fs.Sub(webFS, "web")
	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServerFS(static))
	mux.HandleFunc("GET /events", s.events)
	mux.HandleFunc("POST /api/refresh", s.guard(s.handleRefresh))
	mux.HandleFunc("POST /api/open", s.guard(s.open))
	return s.localOnly(mux)
}

// localOnly refuses requests whose Host is not the loopback address the
// server listens on, so a web page cannot reach it through DNS rebinding.
func (s *server) localOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		if host != "localhost" && host != "127.0.0.1" && host != "::1" {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// guard requires a custom header on actions: a cross-site form or fetch
// cannot set it without a CORS preflight, which this server never answers.
func (s *server) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Devboard") != "1" {
			http.Error(w, "missing X-Devboard header", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

func (s *server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")

	updates, current, unsubscribe := s.hub.subscribe()
	defer unsubscribe()

	send := func(data []byte) {
		fmt.Fprintf(w, "event: board\ndata: %s\n\n", data)
		flusher.Flush()
	}
	send(current)

	keepAlive := time.NewTicker(30 * time.Second)
	defer keepAlive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case data := <-updates:
			send(data)
		case <-keepAlive.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

// handleRefresh fetches every source and answers with the board. Leaving
// the page does not cancel the fetch: its result is still worth keeping.
func (s *server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), time.Minute)
	defer cancel()
	board := s.refresh(ctx)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(board)
}

// open shows a worktree in Finder or the editor. Only paths of known
// worktrees are accepted.
func (s *server) open(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
		With string `json:"with"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if !s.paths()[req.Path] {
		http.Error(w, "unknown worktree", http.StatusNotFound)
		return
	}

	var cmd *exec.Cmd
	switch req.With {
	case "finder":
		cmd = exec.Command("open", req.Path)
	case "editor":
		if len(s.cfg.EditorCommand) == 0 {
			http.Error(w, "no editor_command configured", http.StatusBadRequest)
			return
		}
		args := append(append([]string{}, s.cfg.EditorCommand[1:]...), req.Path)
		cmd = exec.Command(s.cfg.EditorCommand[0], args...)
	default:
		http.Error(w, "unknown target", http.StatusBadRequest)
		return
	}
	if err := cmd.Start(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	go cmd.Wait()
	w.WriteHeader(http.StatusNoContent)
}

func serve(ctx context.Context, addr string, h http.Handler) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		// Open event streams end with the server instead of holding up
		// the shutdown.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()
	log.Printf("devboard on http://%s", addr)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}
	return nil
}
