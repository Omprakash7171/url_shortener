package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"urlshortener/internal/analytics"
	"urlshortener/internal/middleware"
	"urlshortener/internal/service"
)

type URLHandler struct {
	svc    *service.URLService
	logger *slog.Logger
	rec    *analytics.Recorder
}

func NewURLHandler(svc *service.URLService, logger *slog.Logger, rec *analytics.Recorder) *URLHandler {
	return &URLHandler{svc: svc, logger: logger, rec: rec}
}

type createURLRequest struct {
	URL        string `json:"url"`
	CustomCode string `json:"customCode"`
	ExpiresIn  string `json:"expiresIn"`
}

func (h *URLHandler) Create(w http.ResponseWriter, r *http.Request) {
	reqID := middleware.RequestIDFrom(r.Context())

	var req createURLRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, service.MaxBodyBytes)).Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "request body too large", reqID)
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_json", "request body must be valid JSON", reqID)
		return
	}

	idempotencyKey := r.Header.Get("Idempotency-Key")

	res, err := h.svc.Create(r.Context(), service.CreateParams{
		URL:            req.URL,
		CustomCode:     req.CustomCode,
		ExpiresIn:      req.ExpiresIn,
		IdempotencyKey: idempotencyKey,
	})

	if err != nil {
		h.writeCreateError(w, r, err)
		return
	}

	status := http.StatusCreated
	replay := "false"
	if res.Replayed {
		status = http.StatusOK
		replay = "true"
	}
	w.Header().Set("Idempotency-Replay", replay)
	writeJSON(w, status, map[string]any{
		"url":       res.URL.LongURL,
		"shortCode": res.URL.Code,
		"shortUrl":  res.ShortURL,
		"createdAt": res.URL.CreatedAt.UTC().Format(time.RFC3339),
		"expiresAt": formatExpiry(res.URL.ExpiresAt),
	})
}

func (h *URLHandler) writeCreateError(w http.ResponseWriter, r *http.Request, err error) {
	reqID := middleware.RequestIDFrom(r.Context())
	switch {
	case errors.Is(err, service.ErrInvalidURL):
		writeError(w, http.StatusBadRequest, "url_invalid", err.Error(), reqID)
	case errors.Is(err, service.ErrURLTooLong):
		writeError(w, http.StatusBadRequest, "url_too_long", err.Error(), reqID)
	case errors.Is(err, service.ErrSelfReference):
		writeError(w, http.StatusBadRequest, "self_reference", err.Error(), reqID)
	case errors.Is(err, service.ErrInvalidCode):
		writeError(w, http.StatusBadRequest, "custom_code_invalid", err.Error(), reqID)
	case errors.Is(err, service.ErrInvalidExpiry):
		writeError(w, http.StatusBadRequest, "expires_in_invalid", err.Error(), reqID)
	case errors.Is(err, service.ErrIdemKeyTooLong):
		writeError(w, http.StatusBadRequest, "idempotency_key_too_long", err.Error(), reqID)
	case errors.Is(err, service.ErrInFlight):
		writeError(w, http.StatusConflict, "idempotency_in_progress", err.Error(), reqID)
	case errors.Is(err, service.ErrCodeTaken):
		writeError(w, http.StatusConflict, "custom_code_taken", err.Error(), reqID)
	default:
		h.logger.Error("create url failed", "request_id", reqID, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error", reqID)
	}
}

func (h *URLHandler) Get(w http.ResponseWriter, r *http.Request) {
	code := chi.URLParam(r, "code")
	u, err := h.svc.Resolve(r.Context(), code)
	if err != nil {
		h.writeResolveError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"url":       u.LongURL,
		"shortCode": u.Code,
		"shortUrl":  h.svc.ShortURLFor(u.Code),
		"createdAt": u.CreatedAt.UTC().Format(time.RFC3339),
		"expiresAt": formatExpiry(u.ExpiresAt),
	})
}

func (h *URLHandler) Delete(w http.ResponseWriter, r *http.Request) {
	code := chi.URLParam(r, "code")
	u, err := h.svc.Resolve(r.Context(), code)
	if err != nil {
		h.writeResolveError(w, r, err)
		return
	}
	if err := h.svc.Delete(r.Context(), u.Code); err != nil {
		reqID := middleware.RequestIDFrom(r.Context())
		if errors.Is(err, service.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "short code not found", reqID)
			return
		}
		h.logger.Error("delete url failed", "request_id", reqID, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error", reqID)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *URLHandler) Stats(w http.ResponseWriter, r *http.Request) {
	code := chi.URLParam(r, "code")
	stats, err := h.svc.GetStats(r.Context(), code)
	if err != nil {
		h.writeResolveError(w, r, err)
		return
	}

	daily := make([]map[string]any, 0, len(stats.Daily))
	for _, d := range stats.Daily {
		daily = append(daily, map[string]any{
			"day":    d.Day.UTC().Format("2006-01-02"),
			"clicks": d.Clicks,
		})
	}
	referrers := make([]map[string]any, 0, len(stats.TopReferrers))
	for _, ref := range stats.TopReferrers {
		referrers = append(referrers, map[string]any{
			"referrer": ref.Referrer,
			"clicks":   ref.Clicks,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"totalClicks":  stats.TotalClicks,
		"daily":        daily,
		"topReferrers": referrers,
	})
}

func (h *URLHandler) Redirect(w http.ResponseWriter, r *http.Request) {
	code := chi.URLParam(r, "code")
	u, err := h.svc.Resolve(r.Context(), code)
	if err != nil {
		h.writeResolveError(w, r, err)
		return
	}
	if h.rec != nil {
		h.rec.Push(analytics.Event{
			Code:     u.Code,
			Referrer: r.Referer(),
			TS:       time.Now(),
		})
	}
	http.Redirect(w, r, u.LongURL, http.StatusMovedPermanently)
}

func (h *URLHandler) writeResolveError(w http.ResponseWriter, r *http.Request, err error) {
	reqID := middleware.RequestIDFrom(r.Context())
	switch {
	case errors.Is(err, service.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "short code not found", reqID)
	case errors.Is(err, service.ErrExpired):
		writeError(w, http.StatusGone, "link_expired", "link has expired", reqID)
	default:
		h.logger.Error("resolve url failed", "request_id", reqID, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error", reqID)
	}
}

func formatExpiry(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}