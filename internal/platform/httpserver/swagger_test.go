package httpserver

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/darwish/tsz-go/internal/auth"
	"github.com/darwish/tsz-go/internal/user"
)

func newDocsRouter(spec []byte, enable bool) http.Handler {
	return NewRouter(Deps{
		TokenManager: auth.NewTokenManager("secret", time.Hour),
		UserHandler:  user.NewHandler(nil, user.CookieConfig{}, 15*time.Minute, 720*time.Hour),
		OpenAPISpec:  spec,
		EnableDocs:   enable,
	})
}

func TestRegisterDocs_ServesUIAndSpec(t *testing.T) {
	spec := []byte("openapi: 3.0.3\n")
	router := newDocsRouter(spec, true)

	t.Run("ui", func(t *testing.T) {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/docs", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		if !strings.Contains(w.Body.String(), "swagger-ui") {
			t.Errorf("body missing swagger-ui markup: %s", w.Body.String())
		}
	})

	t.Run("spec", func(t *testing.T) {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/docs/openapi.yaml", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		if !bytes.Equal(w.Body.Bytes(), spec) {
			t.Errorf("spec body = %q, want %q", w.Body.Bytes(), spec)
		}
	})

	// The bundled assets must be served locally so the UI renders offline.
	for _, asset := range []string{"/docs/static/swagger-ui.css", "/docs/static/swagger-ui-bundle.js"} {
		t.Run(asset, func(t *testing.T) {
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, asset, nil))
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", w.Code)
			}
			if w.Body.Len() == 0 {
				t.Error("asset body is empty")
			}
		})
	}
}

func TestRegisterDocs_DisabledByDefault(t *testing.T) {
	// Docs off → /docs must not be registered (404).
	w := httptest.NewRecorder()
	newDocsRouter([]byte("openapi: 3.0.3\n"), false).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/docs", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestRegisterDocs_EnabledButNoSpec(t *testing.T) {
	// EnableDocs true but an empty spec must be a no-op, not a panic or a route
	// serving an empty document.
	w := httptest.NewRecorder()
	newDocsRouter(nil, true).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/docs", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}
