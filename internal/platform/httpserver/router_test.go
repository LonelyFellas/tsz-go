package httpserver

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/darwish/tsz-go/internal/auth"
	"github.com/darwish/tsz-go/internal/user"
)

// newTestRouter builds the real router. The user handler is backed by a nil
// service, which is fine for the routes exercised here (healthz, unknown routes,
// and the auth middleware) because none of them reach into the service.
func newTestRouter() http.Handler {
	return NewRouter(Deps{
		TokenManager: auth.NewTokenManager("secret", time.Hour),
		UserHandler:  user.NewHandler(nil, user.CookieConfig{}),
	})
}

func TestRouter_Healthz(t *testing.T) {
	w := httptest.NewRecorder()
	newTestRouter().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if want := `{"status":"ok"}`; w.Body.String() != want {
		t.Errorf("body = %s, want %s", w.Body.String(), want)
	}
}

func TestRouter_UnknownRoute(t *testing.T) {
	w := httptest.NewRecorder()
	newTestRouter().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/does-not-exist", nil))

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestRouter_ProtectedRouteRequiresAuth(t *testing.T) {
	// /me is registered behind AuthRequired; with no token the middleware must
	// reject before the (nil) handler is ever reached.
	w := httptest.NewRecorder()
	newTestRouter().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/me", nil))

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}
