package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

type commandRequest struct {
	Command string `json:"command"`
	Track   string `json:"track,omitempty"`
	Enabled *bool  `json:"enabled,omitempty"`
}

type signalRequest struct {
	SDP  string `json:"sdp"`
	Type string `json:"type"`
}

type commandResponse struct {
	Command string `json:"command"`
	Track   string `json:"track,omitempty"`
	Enabled *bool  `json:"enabled,omitempty"`
}

type signalResponse struct {
	SDP  string `json:"sdp"`
	Type string `json:"type"`
}

type senderSession struct {
	mu        sync.Mutex
	commandCh chan commandRequest
	offer     *signalRequest
	answer    *signalRequest
}

type Server struct {
	mu      sync.Mutex
	senders map[string]*senderSession
}

func New() *Server {
	return &Server{
		senders: make(map[string]*senderSession),
	}
}

func (s *Server) RegisterHandlers(mux *http.ServeMux) {
	mux.HandleFunc("/sender/register", s.handleRegisterSender)
	mux.HandleFunc("/sender/", s.senderCommandHandler)
	mux.HandleFunc("/receiver/", s.receiverHandler)
	mux.HandleFunc("/signal/", s.signalHandler)
}

func (s *Server) getOrCreateSender(id string) *senderSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.senders[id]
	if !ok {
		sess = &senderSession{
			commandCh: make(chan commandRequest, 1),
		}
		s.senders[id] = sess
	}
	return sess
}

func (s *Server) handleRegisterSender(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var payload struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if payload.ID == "" {
		http.Error(w, "id is required", http.StatusBadRequest)
		return
	}
	s.getOrCreateSender(payload.ID)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "registered"})
}

func (s *Server) senderCommandHandler(w http.ResponseWriter, r *http.Request) {
	// Expecting /sender/{id}/command
	const prefix = "/sender/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		http.NotFound(w, r)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, prefix), "/")
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	id, action := parts[0], parts[1]

	switch action {
	case "command":
		s.handleSenderCommand(w, r, id)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleSenderCommand(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sess := s.getOrCreateSender(id)

	ctx := r.Context()
	select {
	case cmd := <-sess.commandCh:
		writeJSON(w, commandResponse{Command: cmd.Command, Track: cmd.Track, Enabled: cmd.Enabled})
	case <-time.After(60 * time.Second):
		writeJSON(w, commandResponse{Command: "noop"})
	case <-ctx.Done():
		http.Error(w, ctx.Err().Error(), http.StatusGatewayTimeout)
	}
}

func (s *Server) receiverHandler(w http.ResponseWriter, r *http.Request) {
	// Expecting /receiver/{id}/command
	const prefix = "/receiver/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		http.NotFound(w, r)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, prefix), "/")
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	id, action := parts[0], parts[1]

	switch action {
	case "command":
		s.handleReceiverCommand(w, r, id)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleReceiverCommand(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var payload commandRequest
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if payload.Command == "" {
		http.Error(w, "command is required", http.StatusBadRequest)
		return
	}
	if payload.Track != "" {
		payload.Track = strings.ToLower(strings.TrimSpace(payload.Track))
	}
	sess := s.getOrCreateSender(id)
	select {
	case sess.commandCh <- payload:
	default:
		// drop command if queue full
	}
	writeJSON(w, map[string]string{"status": "queued"})
}

func (s *Server) signalHandler(w http.ResponseWriter, r *http.Request) {
	// Expecting /signal/{id}/offer or /signal/{id}/answer
	const prefix = "/signal/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		http.NotFound(w, r)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, prefix), "/")
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	id, action := parts[0], parts[1]

	sess := s.getOrCreateSender(id)

	switch action {
	case "offer":
		s.handleOffer(w, r, sess)
	case "answer":
		s.handleAnswer(w, r, sess)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleOffer(w http.ResponseWriter, r *http.Request, sess *senderSession) {
	switch r.Method {
	case http.MethodPost:
		var payload signalRequest
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if payload.SDP == "" || payload.Type == "" {
			http.Error(w, "invalid sdp", http.StatusBadRequest)
			return
		}
		sess.mu.Lock()
		sess.offer = &payload
		sess.answer = nil
		sess.mu.Unlock()
		writeJSON(w, map[string]string{"status": "stored"})
	case http.MethodGet:
		offer, err := sess.getOffer()
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		writeJSON(w, signalResponse{SDP: offer.SDP, Type: offer.Type})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleAnswer(w http.ResponseWriter, r *http.Request, sess *senderSession) {
	switch r.Method {
	case http.MethodPost:
		var payload signalRequest
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if payload.SDP == "" || payload.Type == "" {
			http.Error(w, "invalid sdp", http.StatusBadRequest)
			return
		}
		sess.mu.Lock()
		sess.answer = &payload
		sess.mu.Unlock()
		writeJSON(w, map[string]string{"status": "stored"})
	case http.MethodGet:
		answer, err := sess.getAnswer()
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		writeJSON(w, signalResponse{SDP: answer.SDP, Type: answer.Type})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *senderSession) getOffer() (*signalRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.offer == nil {
		return nil, errors.New("offer not available")
	}
	offer := s.offer
	s.offer = nil
	return offer, nil
}

func (s *senderSession) getAnswer() (*signalRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.answer == nil {
		return nil, errors.New("answer not available")
	}
	answer := s.answer
	s.answer = nil
	return answer, nil
}

func writeJSON(w http.ResponseWriter, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		http.Error(w, fmt.Sprintf("failed to encode response: %v", err), http.StatusInternalServerError)
	}
}

// Run launches the HTTP server with graceful shutdown support.
func (s *Server) Run(ctx context.Context, addr string) error {
	mux := http.NewServeMux()
	s.RegisterHandlers(mux)
	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}
