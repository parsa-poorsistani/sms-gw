package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/parsa-poorsistani/sms-gw/internal/database"
)

type Server struct {
	repo *database.Repository
	log  *slog.Logger
}

func New(repo *database.Repository, log *slog.Logger) *Server {
	return &Server{repo: repo, log: log}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /users", s.createUser)
	mux.HandleFunc("GET /users/{id}", s.getUser)
	mux.HandleFunc("POST /users/{id}/credit", s.incBalance)
	mux.HandleFunc("POST /users/{id}/messages", s.sendMessage)
	mux.HandleFunc("GET /users/{id}/messages", s.listMessages)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := s.repo.Ping(ctx); err != nil {
			httpError(w, http.StatusServiceUnavailable, "database unreachable")
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	return metricsMiddleware(mux)
}
