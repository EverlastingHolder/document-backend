package httpapi

import (
	"context"
	"net/http"
)

func (h *handler) serveHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path == readyPath && h.config.ReadinessCheck != nil {
		ctx, cancel := context.WithTimeout(r.Context(), h.config.ReadinessTimeout)
		defer cancel()
		if err := h.config.ReadinessCheck(ctx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}
