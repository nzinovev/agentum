package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSPADirectRoutes(t *testing.T) {
	for _, path := range []string{"/", "/projects", "/projects/project-id", "/projects/project-id/new-run", "/runs/run-id"} {
		t.Run(path, func(t *testing.T) {
			response := httptest.NewRecorder()
			serveSPA(response, httptest.NewRequest(http.MethodGet, path, nil))
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "<div id=\"root\"></div>") {
				t.Fatalf("direct route %s: status %d, body %q", path, response.Code, response.Body.String())
			}
		})
	}
}

func TestSPAUnknownAPIPathReturnsNotFound(t *testing.T) {
	response := httptest.NewRecorder()
	serveSPA(response, httptest.NewRequest(http.MethodGet, "/api/v1/missing", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("unknown API path: status %d", response.Code)
	}
}
