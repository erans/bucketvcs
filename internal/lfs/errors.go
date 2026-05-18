package lfs

import (
	"encoding/json"
	"net/http"
)

// ContentType is the LFS API content type. Per the Git LFS spec, batch
// responses MUST be served with this content type.
const ContentType = "application/vnd.git-lfs+json"

// WriteError writes the LFS error JSON shape with the given HTTP
// status. The body is {"message": msg}.
func WriteError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", ContentType)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Message string `json:"message"`
	}{Message: msg})
}
