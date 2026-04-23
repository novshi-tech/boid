package auth

import "time"

// PairRequest is the body for POST /api/web/pair.
type PairRequest struct {
	Label string `json:"label,omitempty"`
}

// PairResponse is returned by POST /api/web/pair.
type PairResponse struct {
	Code      string    `json:"code"`
	URL       string    `json:"url,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
}

// DeviceInfo is the API response representation of a paired web device.
type DeviceInfo struct {
	ID         string    `json:"id"`
	Label      string    `json:"label,omitempty"`
	LastSeenAt time.Time `json:"last_seen_at"`
	CreatedAt  time.Time `json:"created_at"`
}
