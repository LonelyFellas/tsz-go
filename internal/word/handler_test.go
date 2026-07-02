package word

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/darwish/tsz-go/internal/auth"
)

// newWordRouter mounts the handler the way the real router does, with a stub
// middleware playing AdminAuthRequired (seeding the admin id).
func newWordRouter(svc *Service, adminID uuid.UUID) *gin.Engine {
	gin.SetMode(gin.TestMode)
	h := NewHandler(svc)
	r := gin.New()
	g := r.Group("/admin/words")
	g.Use(func(c *gin.Context) { c.Set(auth.ContextAdminIDKey, adminID) })
	g.POST("", h.Create)
	g.GET("", h.List)
	g.GET("/stats", h.Stats)
	g.POST("/batch-delete", h.BatchDelete)
	g.GET("/related-search", h.RelatedSearch)
	g.GET("/:wordId", h.Get)
	g.PUT("/:wordId/content", h.SaveContent)
	g.POST("/:wordId/publish", h.Publish)
	g.DELETE("/:wordId", h.Delete)
	return r
}

type handlerEnv struct {
	svc     *Service
	fake    *fakeStore
	router  *gin.Engine
	adminID uuid.UUID
}

func newHandlerEnv() handlerEnv {
	svc, fake, _ := newTestService()
	adminID := uuid.New()
	fake.RegisterAdmin(adminID, "测试管理员")
	return handlerEnv{svc: svc, fake: fake, router: newWordRouter(svc, adminID), adminID: adminID}
}

func (e handlerEnv) do(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	return w
}

// seedWord creates a shell and returns the decoded word from the response.
func seedWord(t *testing.T, e handlerEnv) *Word {
	t.Helper()
	res := e.do(t, http.MethodPost, "/admin/words", `{"headword":"`+ctHeadword()+`"}`)
	if res.Code != http.StatusCreated {
		t.Fatalf("create shell: %d %s", res.Code, res.Body.String())
	}
	var out struct {
		Word *Word `json:"word"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out.Word
}

// TestHandler_StatusMapping pins the error→HTTP wiring; the business rules
// themselves are covered by the service and contract tests.
func TestHandler_StatusMapping(t *testing.T) {
	t.Run("create: 201, bad body 400, duplicate 409", func(t *testing.T) {
		e := newHandlerEnv()
		w := seedWord(t, e)
		if w.Status != StatusDraft || w.Kind != KindWord {
			t.Errorf("shell = %+v", w)
		}
		if res := e.do(t, http.MethodPost, "/admin/words", `{}`); res.Code != http.StatusBadRequest {
			t.Errorf("missing headword: %d", res.Code)
		}
		if res := e.do(t, http.MethodPost, "/admin/words", `{"headword":"`+w.Headword+`"}`); res.Code != http.StatusConflict {
			t.Errorf("duplicate: %d", res.Code)
		}
	})

	t.Run("get: 200, malformed id 400, unknown 404", func(t *testing.T) {
		e := newHandlerEnv()
		w := seedWord(t, e)
		if res := e.do(t, http.MethodGet, "/admin/words/"+w.ID.String(), ""); res.Code != http.StatusOK {
			t.Errorf("get: %d", res.Code)
		}
		if res := e.do(t, http.MethodGet, "/admin/words/not-a-uuid", ""); res.Code != http.StatusBadRequest {
			t.Errorf("bad id: %d", res.Code)
		}
		if res := e.do(t, http.MethodGet, "/admin/words/"+uuid.NewString(), ""); res.Code != http.StatusNotFound {
			t.Errorf("unknown: %d", res.Code)
		}
	})

	t.Run("save: 200, stale 409, validation 400", func(t *testing.T) {
		e := newHandlerEnv()
		w := seedWord(t, e)
		body := func(base string) string {
			return fmt.Sprintf(`{"base_updated_at":%q,"frequency":"0.023134","dialect_mode":"unified",
				"dialects":[],"sense_groups":[],"pos":[]}`, base)
		}
		ok := e.do(t, http.MethodPut, "/admin/words/"+w.ID.String()+"/content",
			body(w.UpdatedAt.Format("2006-01-02T15:04:05.999999999Z07:00")))
		if ok.Code != http.StatusOK {
			t.Fatalf("save: %d %s", ok.Code, ok.Body.String())
		}
		stale := e.do(t, http.MethodPut, "/admin/words/"+w.ID.String()+"/content",
			body(w.UpdatedAt.Format("2006-01-02T15:04:05.999999999Z07:00")))
		if stale.Code != http.StatusConflict {
			t.Errorf("stale save: %d %s", stale.Code, stale.Body.String())
		}
		bad := e.do(t, http.MethodPut, "/admin/words/"+w.ID.String()+"/content",
			`{"base_updated_at":"2026-01-01T00:00:00Z","dialect_mode":"both","dialects":[],"sense_groups":[],"pos":[]}`)
		if bad.Code != http.StatusBadRequest {
			t.Errorf("invalid mode: %d %s", bad.Code, bad.Body.String())
		}
	})

	t.Run("publish: 422 with details, then 200", func(t *testing.T) {
		e := newHandlerEnv()
		w := seedWord(t, e)
		res := e.do(t, http.MethodPost, "/admin/words/"+w.ID.String()+"/publish", "")
		if res.Code != http.StatusUnprocessableEntity {
			t.Fatalf("incomplete publish: %d %s", res.Code, res.Body.String())
		}
		var out struct {
			Error   string   `json:"error"`
			Details []string `json:"details"`
		}
		if err := json.Unmarshal(res.Body.Bytes(), &out); err != nil || len(out.Details) == 0 {
			t.Fatalf("details missing: %s", res.Body.String())
		}

		// Complete the entry through the service, then publish over HTTP.
		if _, err := e.svc.Save(t.Context(), w.ID, minTree(w.UpdatedAt)); err != nil {
			t.Fatalf("Save: %v", err)
		}
		res = e.do(t, http.MethodPost, "/admin/words/"+w.ID.String()+"/publish", "")
		if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"status":"published"`) {
			t.Errorf("publish: %d %s", res.Code, res.Body.String())
		}
	})

	t.Run("list: 200 with page envelope, invalid filter 400", func(t *testing.T) {
		e := newHandlerEnv()
		seedWord(t, e)
		res := e.do(t, http.MethodGet, "/admin/words?page=1&page_size=10&q=测试管理员", "")
		if res.Code != http.StatusOK {
			t.Fatalf("list: %d %s", res.Code, res.Body.String())
		}
		var out struct {
			Words []ListItem `json:"words"`
			Page  struct {
				Page     int   `json:"page"`
				PageSize int   `json:"page_size"`
				Total    int64 `json:"total"`
			} `json:"page"`
		}
		if err := json.Unmarshal(res.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if out.Page.Total != 1 || len(out.Words) != 1 || out.Words[0].CreatedByName != "测试管理员" {
			t.Errorf("list body: %+v", out)
		}
		if res := e.do(t, http.MethodGet, "/admin/words?kind=idiom", ""); res.Code != http.StatusBadRequest {
			t.Errorf("bad kind: %d", res.Code)
		}
		if res := e.do(t, http.MethodGet, "/admin/words?created_from=yesterday", ""); res.Code != http.StatusBadRequest {
			t.Errorf("bad time: %d", res.Code)
		}
	})

	t.Run("stats: 200", func(t *testing.T) {
		e := newHandlerEnv()
		seedWord(t, e)
		res := e.do(t, http.MethodGet, "/admin/words/stats", "")
		if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"total":1`) {
			t.Errorf("stats: %d %s", res.Code, res.Body.String())
		}
	})

	t.Run("delete: 204, unknown 404; batch reports count", func(t *testing.T) {
		e := newHandlerEnv()
		w := seedWord(t, e)
		if res := e.do(t, http.MethodDelete, "/admin/words/"+w.ID.String(), ""); res.Code != http.StatusNoContent {
			t.Errorf("delete: %d", res.Code)
		}
		if res := e.do(t, http.MethodDelete, "/admin/words/"+w.ID.String(), ""); res.Code != http.StatusNotFound {
			t.Errorf("re-delete: %d", res.Code)
		}
		w2 := seedWord(t, e)
		res := e.do(t, http.MethodPost, "/admin/words/batch-delete",
			`{"ids":["`+w2.ID.String()+`","`+uuid.NewString()+`"]}`)
		if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"deleted":1`) {
			t.Errorf("batch delete: %d %s", res.Code, res.Body.String())
		}
		if res := e.do(t, http.MethodPost, "/admin/words/batch-delete", `{"ids":[]}`); res.Code != http.StatusBadRequest {
			t.Errorf("empty batch: %d %s", res.Code, res.Body.String())
		}
	})

	t.Run("related-search: 200", func(t *testing.T) {
		e := newHandlerEnv()
		w := seedWord(t, e)
		res := e.do(t, http.MethodGet, "/admin/words/related-search?q="+w.Headword, "")
		if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), w.ID.String()) {
			t.Errorf("related search: %d %s", res.Code, res.Body.String())
		}
	})
}

// TestSaveContent_BodyTooLarge pins the 4 MiB request cap: an oversized body
// must come back as 413 before parsing, not be mistaken for malformed JSON.
func TestSaveContent_BodyTooLarge(t *testing.T) {
	e := newHandlerEnv()
	body := `{"frequency":"` + strings.Repeat("9", maxSaveBodyBytes+1) + `"}`
	res := e.do(t, http.MethodPut, "/admin/words/"+uuid.NewString()+"/content", body)
	if res.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (body %s)", res.Code, res.Body.String())
	}
}
