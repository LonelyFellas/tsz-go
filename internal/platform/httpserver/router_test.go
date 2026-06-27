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
		TokenManager: auth.NewTokenManager("secret", time.Hour, auth.RealmWeb),
		UserHandler:  user.NewHandler(nil, user.CookieConfig{}, 15*time.Minute, 720*time.Hour),
	})
}

// /readyz and /metrics must be mounted when their deps are supplied.
func TestRouter_ObservabilityEndpoints(t *testing.T) {
	router := NewRouter(Deps{
		TokenManager: auth.NewTokenManager("secret", time.Hour, auth.RealmWeb),
		UserHandler:  user.NewHandler(nil, user.CookieConfig{}, 15*time.Minute, 720*time.Hour),
		DB:           fakePinger{},
		Metrics:      NewMetrics(),
		ServiceName:  "test",
	})

	for _, path := range []string{"/readyz", "/metrics"} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK {
			t.Errorf("%s status = %d, want 200", path, w.Code)
		}
	}
}

// With no Metrics dep, /metrics is not registered.
func TestRouter_MetricsDisabled(t *testing.T) {
	w := httptest.NewRecorder()
	newTestRouter().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 when metrics are off", w.Code)
	}
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

// The rate limiter, when supplied, must be mounted on the public /auth/* routes
// and only there. A burst-0 limiter rejects every request before the handler is
// reached, so a 429 proves the middleware is wired in; /healthz staying 200
// proves it's scoped to the auth group and not applied globally.
func TestRouter_RateLimiterScopedToAuth(t *testing.T) {
	router := NewRouter(Deps{
		TokenManager:    auth.NewTokenManager("secret", time.Hour, auth.RealmWeb),
		UserHandler:     user.NewHandler(nil, user.CookieConfig{}, 15*time.Minute, 720*time.Hour),
		AuthRateLimiter: NewIPRateLimiter(60, 0, time.Minute),
	})

	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil))
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("/auth/login status = %d, want 429 (limiter must be mounted)", w.Code)
	}

	w = httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Code != http.StatusOK {
		t.Errorf("/healthz status = %d, want 200 (limiter must not be global)", w.Code)
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
