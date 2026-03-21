package httputil

import (
	"encoding/json"
	"net/http"
	"strings"
)

// SecurityHeaders wraps an http.Handler adding standard security headers.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		next.ServeHTTP(w, r)
	})
}

// JSONResponse writes a JSON response with 200 OK.
func JSONResponse(rw http.ResponseWriter, data any) {
	rw.Header().Set("Content-Type", "application/json")
	json.NewEncoder(rw).Encode(data)
}

// JSONError writes a JSON error response with the given status code.
func JSONError(rw http.ResponseWriter, message string, statusCode int) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(statusCode)
	json.NewEncoder(rw).Encode(map[string]string{"error": message})
}

// JSONCreated writes a JSON response with 201 Created status.
func JSONCreated(rw http.ResponseWriter, data any) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(201)
	json.NewEncoder(rw).Encode(data)
}

// RequireJSON returns true if the request has a JSON Content-Type, otherwise sends 415.
func RequireJSON(rw http.ResponseWriter, r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	if ct != "" && !strings.HasPrefix(ct, "application/json") {
		JSONError(rw, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return false
	}
	return true
}
