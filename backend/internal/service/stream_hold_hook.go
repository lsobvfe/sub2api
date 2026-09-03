package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// StreamHoldHook is the only core integration point for the local-native proxy.
type StreamHoldHook interface {
	Set(ctx context.Context, key string, enabled bool) error
	Sync(ctx context.Context, keys []string) error
}

type localStreamHoldHook struct {
	baseURL string
	client  *http.Client
}

func NewLocalStreamHoldHook() StreamHoldHook {
	baseURL := strings.TrimRight(strings.TrimSpace(os.Getenv("STREAM_HOLD_PROXY_URL")), "/")
	if baseURL == "" {
		return nil
	}
	return &localStreamHoldHook{
		baseURL: baseURL,
		client:  &http.Client{Timeout: 3 * time.Second},
	}
}

func (h *localStreamHoldHook) Set(ctx context.Context, key string, enabled bool) error {
	return h.put(ctx, "/_stream-hold/api/key", struct {
		KeyHash string `json:"key_hash"`
		Enabled bool   `json:"enabled"`
	}{keyHash(key), enabled})
}

func (h *localStreamHoldHook) Sync(ctx context.Context, keys []string) error {
	hashes := make([]string, 0, len(keys))
	for _, key := range keys {
		hashes = append(hashes, keyHash(key))
	}
	return h.put(ctx, "/_stream-hold/api/keys", struct {
		KeyHashes []string `json:"key_hashes"`
	}{hashes})
}

func (h *localStreamHoldHook) put(ctx context.Context, path string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, h.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := h.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("stream hold proxy returned %s", response.Status)
	}
	return nil
}

func keyHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}
