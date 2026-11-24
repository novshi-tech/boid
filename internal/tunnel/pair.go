package tunnel

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Pairing is a started pairing the owner still has to approve.
type Pairing struct {
	Gateway         string    `json:"gateway"`
	Name            string    `json:"name"`
	Code            string    `json:"code"`
	VerificationURI string    `json:"verification_uri"`
	Thumbprint      string    `json:"thumbprint"`
	ExpiresAt       time.Time `json:"expires_at"`
	pairingID       string
	interval        time.Duration
	key             ed25519.PrivateKey
}

// PairingDeniedError reports a pairing the owner refused or let expire.
type PairingDeniedError struct{ Status string }

func (e *PairingDeniedError) Error() string { return "tunnel: pairing " + e.Status }

func postJSON(ctx context.Context, hc *http.Client, url string, in, out any) (int, error) {
	raw, err := json.Marshal(in)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "boid-tunnel")
	resp, err := hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if out != nil {
		_ = json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 1<<20)).Decode(out)
	}
	return resp.StatusCode, nil
}

// StartPairing generates a key and asks the gateway for a pairing code.
func StartPairing(ctx context.Context, hc *http.Client, gateway, name, description string) (*Pairing, error) {
	gateway = strings.TrimRight(gateway, "/")
	key, err := GenerateKey()
	if err != nil {
		return nil, err
	}
	var resp struct {
		Code                    string `json:"code"`
		PairingID               string `json:"pairing_id"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int    `json:"expires_in"`
		Interval                int    `json:"interval"`
		Error                   string `json:"error"`
	}
	body := map[string]any{"name": name, "public_key": publicJWK(key), "mode": "standard"}
	if description != "" {
		body["description"] = description
	}
	status, err := postJSON(ctx, hc, gateway+"/tunnel/pair", body, &resp)
	if err != nil {
		return nil, fmt.Errorf("tunnel: start pairing: %w", err)
	}
	if status != http.StatusCreated {
		return nil, fmt.Errorf("tunnel: pairing refused (%d) %s", status, resp.Error)
	}
	interval := time.Duration(resp.Interval) * time.Second
	if interval <= 0 {
		interval = 3 * time.Second
	}
	return &Pairing{
		Gateway:         gateway,
		Name:            name,
		Code:            resp.Code,
		VerificationURI: resp.VerificationURIComplete,
		Thumbprint:      Thumbprint(key),
		ExpiresAt:       time.Now().Add(time.Duration(resp.ExpiresIn) * time.Second),
		pairingID:       resp.PairingID,
		interval:        interval,
		key:             key,
	}, nil
}

// Wait polls until the owner approves or denies, or the code expires.
func (p *Pairing) Wait(ctx context.Context, hc *http.Client) (*Identity, error) {
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-t.C:
		}
		var resp struct {
			Status    string `json:"status"`
			ServiceID string `json:"service_id"`
			Name      string `json:"name"`
		}
		code, err := postJSON(ctx, hc, p.Gateway+"/tunnel/pair/status", map[string]string{"pairing_id": p.pairingID}, &resp)
		if err != nil {
			if time.Now().After(p.ExpiresAt) {
				return nil, &PairingDeniedError{Status: "expired"}
			}
			continue
		}
		switch {
		case resp.Status == "pending":
			continue
		case resp.Status == "approved":
			return &Identity{Gateway: p.Gateway, ServiceID: resp.ServiceID, Name: resp.Name, PrivateKey: p.key}, nil
		case code == http.StatusNotFound:
			return nil, &PairingDeniedError{Status: "expired"}
		default:
			return nil, &PairingDeniedError{Status: resp.Status}
		}
	}
}
