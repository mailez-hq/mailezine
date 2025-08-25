package imap

import (
	"time"

	"mailezine/internal/mailstore"
)

func testMessage(uid uint32, body string) *mailstore.Message {
	return &mailstore.Message{
		UID:          uid,
		Size:         int64(len(body)),
		InternalDate: time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC),
	}
}
