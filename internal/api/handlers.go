package api

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/parsa-poorsistani/sms-gw/internal/store"
)

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := decode(r, &req); err != nil || req.Name == "" {
		httpError(w, http.StatusBadRequest, "name is required")
		return
	}
	u, err := s.repo.CreateUser(r.Context(), req.Name)
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, u)
}

func (s *Server) getUser(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	u, err := s.repo.GetUser(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "user not found")
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, u)
}

func (s *Server) incBalance(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	var req struct {
		Amount int64 `json:"amount"`
	}
	if err := decode(r, &req); err != nil || req.Amount <= 0 {
		httpError(w, http.StatusBadRequest, "amount must be a positive integer")
		return
	}
	u, err := s.repo.IncBalance(r.Context(), id, req.Amount)
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "user not found")
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, u)
}

func (s *Server) sendMessage(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	var req struct {
		Phone   string `json:"phone"`
		Body    string `json:"body"`
		Express bool   `json:"express"`
	}
	if err := decode(r, &req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if !validPhone(req.Phone) {
		httpError(w, http.StatusBadRequest, "invalid phone number")
		return
	}
	if req.Body == "" || len([]rune(req.Body)) > 70 {
		httpError(w, http.StatusBadRequest, "body must be 1-70 characters (single page)")
		return
	}

	m, err := s.repo.EnqueueMessage(r.Context(), id, req.Phone, req.Body, req.Express)
	switch {
	case errors.Is(err, store.ErrNotFound):
		httpError(w, http.StatusNotFound, "user not found")
	case errors.Is(err, store.ErrInsufficientBalance):
		httpError(w, http.StatusPaymentRequired, "insufficient balance")
	case err != nil:
		s.internal(w, err)
	default:
		writeJSON(w, http.StatusAccepted, m)
	}
}

func (s *Server) listMessages(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			httpError(w, http.StatusBadRequest, "limit must be 1-200")
			return
		}
		limit = n
	}
	before := time.Now().Add(time.Hour)
	if v := r.URL.Query().Get("before"); v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			httpError(w, http.StatusBadRequest, "before must be RFC3339")
			return
		}
		before = t
	}

	if _, err := s.repo.GetUser(r.Context(), id); errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "user not found")
		return
	} else if err != nil {
		s.internal(w, err)
		return
	}

	msgs, err := s.repo.ListMessages(r.Context(), id, before, limit)
	if err != nil {
		s.internal(w, err)
		return
	}
	resp := map[string]any{"messages": msgs}
	if len(msgs) == limit {
		resp["next_before"] = msgs[len(msgs)-1].CreatedAt.Format(time.RFC3339Nano)
	}
	writeJSON(w, http.StatusOK, resp)
}
