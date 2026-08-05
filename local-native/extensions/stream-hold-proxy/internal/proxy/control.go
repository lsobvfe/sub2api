package proxy

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

//go:embed web/index.html
var controlPage []byte

func (p *Proxy) serveControl(w http.ResponseWriter, request *http.Request) {
	if !isLoopback(request.RemoteAddr) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	switch {
	case (request.URL.Path == controlPrefix || request.URL.Path == controlPrefix+"/") && request.Method == http.MethodGet:
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(controlPage)
	case request.URL.Path == controlPrefix+"/health" && request.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{
			"status":  "ok",
			"enabled": p.controller.Enabled(),
		})
	case request.URL.Path == controlPrefix+"/api/state" && request.Method == http.MethodGet:
		active := p.registry.list()
		writeJSON(w, http.StatusOK, map[string]any{
			"enabled":      p.controller.Enabled(),
			"active_count": len(active),
			"active":       active,
			"timestamp":    time.Now().UTC(),
		})
	case request.URL.Path == controlPrefix+"/api/enabled" && request.Method == http.MethodPut:
		var payload struct {
			Enabled bool `json:"enabled"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, request.Body, 1024))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&payload); err != nil {
			http.Error(w, "Invalid JSON body", http.StatusBadRequest)
			return
		}
		if err := p.controller.Set(payload.Enabled); err != nil {
			p.logger.Error("hold_setting_update_failed", "error", err)
			http.Error(w, "Failed to persist setting", http.StatusInternalServerError)
			return
		}
		p.logger.Info("hold_setting_updated", "enabled", payload.Enabled)
		writeJSON(w, http.StatusOK, map[string]any{"enabled": payload.Enabled})
	default:
		if strings.HasPrefix(request.URL.Path, controlPrefix+"/api/") {
			http.Error(w, "Not Found", http.StatusNotFound)
			return
		}
		http.NotFound(w, request)
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
