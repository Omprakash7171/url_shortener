package model

import "time"

type URL struct {
	ID        int64
	Code      string
	LongURL   string
	ExpiresAt *time.Time
	CreatedAt time.Time
}