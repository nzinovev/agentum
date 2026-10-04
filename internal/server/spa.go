package server

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed web
var bundledWeb embed.FS

var webFiles = mustWebFiles()

func mustWebFiles() fs.FS {
	files, err := fs.Sub(bundledWeb, "web")
	if err != nil {
		panic(err)
	}
	return files
}

// serveSPA serves bundled frontend routes and assets. The API owns its own
// namespace so an unknown API route returns 404 instead of the SPA document.
func serveSPA(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if strings.HasPrefix(path, "/api/") || strings.HasPrefix(path, "/api/v1") {
		http.NotFound(w, r)
		return
	}
	if strings.HasPrefix(path, "/assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		http.FileServer(http.FS(webFiles)).ServeHTTP(w, r)
		return
	}
	if !spaRoute(path) {
		http.NotFound(w, r)
		return
	}
	document, err := fs.ReadFile(webFiles, "index.html")
	if err != nil {
		http.Error(w, "frontend unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(document)
}

// spaRoute accepts the paths the client router can restore after a reload.
func spaRoute(path string) bool {
	if path == "/" || path == "/projects" {
		return true
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 2 && ((parts[0] == "projects" && parts[1] != "") || (parts[0] == "runs" && parts[1] != "")) {
		return true
	}
	return len(parts) == 3 && parts[0] == "projects" && parts[1] != "" && parts[2] == "new-run"
}
