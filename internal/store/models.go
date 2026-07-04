package store

import (
	"time"

	"github.com/google/uuid"
)

type User struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	Balance   int64     `json:"balance"`
	CreatedAt time.Time `json:"created_at"`
}

type Message struct {
	ID         uuid.UUID  `json:"id"`
	UserID     uuid.UUID  `json:"user_id"`
	Phone      string     `json:"phone"`
	Body       string     `json:"body"`
	Express    bool       `json:"express"`
	Status     string     `json:"status"`
	Attempts   int        `json:"attempts"`
	ProviderID *string    `json:"provider_id,omitempty"`
	Error      *string    `json:"error,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	SentAt     *time.Time `json:"sent_at,omitempty"`
}
