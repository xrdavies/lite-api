package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExcludedGovernanceRoutes(t *testing.T) {
	a := &App{mux: http.NewServeMux()}
	a.routes()
	for _, route := range []string{
		"GET /api/v1/admin/prompt-audit/config",
		"PUT /api/v1/admin/prompt-audit/config",
		"POST /api/v1/admin/prompt-audit/endpoints/probe",
		"GET /api/v1/admin/prompt-audit/runtime",
		"GET /api/v1/admin/prompt-audit/events",
		"GET /api/v1/admin/prompt-audit/events/1",
		"DELETE /api/v1/admin/prompt-audit/events/1",
		"POST /api/v1/admin/prompt-audit/events/batch-delete",
		"POST /api/v1/admin/prompt-audit/events/delete-preview",
		"POST /api/v1/admin/prompt-audit/events/delete-by-filter",
		"GET /api/v1/admin/compliance",
		"POST /api/v1/admin/compliance/accept",
		"GET /api/v1/admin/risk-control/config",
		"PUT /api/v1/admin/risk-control/config",
		"POST /api/v1/admin/risk-control/api-keys/test",
		"GET /api/v1/admin/risk-control/status",
		"GET /api/v1/admin/risk-control/logs",
		"POST /api/v1/admin/risk-control/users/1/unban",
		"DELETE /api/v1/admin/risk-control/hashes",
		"DELETE /api/v1/admin/risk-control/hashes/all",
		"GET /api/v1/admin/grok/runtime-sanity",
	} {
		method, path, _ := strings.Cut(route, " ")
		r := httptest.NewRequest(method, path, nil)
		if _, pattern := a.mux.Handler(r); pattern != "" {
			t.Fatalf("excluded route %s matches %s", route, pattern)
		}
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Fatalf("excluded route %s returned %d", route, w.Code)
		}
	}
}
