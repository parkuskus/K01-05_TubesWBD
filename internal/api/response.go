package api

import (
	"encoding/json"
	"net/http"
)

type errorDetail struct {
	Code        string            `json:"code"`
	Message     string            `json:"message"`
	FieldErrors map[string]string `json:"field_errors,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func WriteError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, struct {
		Error errorDetail `json:"error"`
	}{Error: errorDetail{Code: code, Message: message}})
}

func writeData(w http.ResponseWriter, data any) {
	writeJSON(w, http.StatusOK, struct {
		Data any `json:"data"`
	}{Data: data})
}
