package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	cookieName   = "boid_session"
	cookieMaxAge = 7776000 // 90 days
)

var ErrInvalidSession = errors.New("invalid session")

type SessionSigner struct {
	secret []byte
	store  *Store
}

func NewSessionSigner(secret []byte, store *Store) *SessionSigner {
	return &SessionSigner{secret: secret, store: store}
}

func (s *SessionSigner) mac(deviceID string, epochHour int64) string {
	h := hmac.New(sha256.New, s.secret)
	h.Write([]byte(deviceID))
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], uint64(epochHour))
	h.Write(buf[:])
	return hex.EncodeToString(h.Sum(nil))
}

func (s *SessionSigner) Issue(w http.ResponseWriter, deviceID string) error {
	epochHour := time.Now().Unix() / 3600
	sig := s.mac(deviceID, epochHour)
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    deviceID + "." + sig,
		Path:     "/",
		MaxAge:   cookieMaxAge,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
	return nil
}

func (s *SessionSigner) Verify(r *http.Request) (string, error) {
	cookie, err := r.Cookie(cookieName)
	if err != nil {
		return "", ErrInvalidSession
	}

	idx := strings.LastIndex(cookie.Value, ".")
	if idx < 0 {
		return "", ErrInvalidSession
	}
	deviceID, sig := cookie.Value[:idx], cookie.Value[idx+1:]

	epochHour := time.Now().Unix() / 3600
	valid := false
	for _, offset := range []int64{-1, 0, 1} {
		if hmac.Equal([]byte(s.mac(deviceID, epochHour+offset)), []byte(sig)) {
			valid = true
			break
		}
	}
	if !valid {
		return "", ErrInvalidSession
	}

	device, err := s.store.GetDevice(r.Context(), deviceID)
	if err != nil {
		return "", fmt.Errorf("get device: %w", err)
	}
	if device == nil {
		return "", ErrInvalidSession
	}

	if err := s.store.UpdateDeviceLastSeen(r.Context(), deviceID, time.Now()); err != nil {
		return "", fmt.Errorf("update last seen: %w", err)
	}

	return deviceID, nil
}

func (s *SessionSigner) Clear(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}
