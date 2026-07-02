package word

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/darwish/tsz-go/internal/auth"
)

// Handler adapts HTTP requests to the word Service. All routes sit behind
// AdminAuthRequired (mounted in the router), so the admin id is always in
// context.
type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

func internalError(c *gin.Context, err error) {
	_ = c.Error(err)
	c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
}

// respondErr maps domain errors onto the API's error body. Publish
// completeness failures carry a details array on top of the usual {"error"}.
func respondErr(c *gin.Context, err error) {
	var ie *IncompleteError
	var ve *ValidationError
	switch {
	case errors.As(err, &ie):
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": ie.Error(), "details": ie.Details})
	case errors.As(err, &ve):
		c.JSON(http.StatusBadRequest, gin.H{"error": ve.Error()})
	case errors.Is(err, ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "word not found"})
	case errors.Is(err, ErrHeadwordTaken):
		c.JSON(http.StatusConflict, gin.H{"error": "word already exists"})
	case errors.Is(err, ErrStale):
		c.JSON(http.StatusConflict, gin.H{"error": "word was modified by others"})
	case errors.Is(err, ErrIDConflict):
		c.JSON(http.StatusBadRequest, gin.H{"error": ErrIDConflict.Error()})
	case errors.Is(err, ErrBadTargetRef):
		c.JSON(http.StatusBadRequest, gin.H{"error": ErrBadTargetRef.Error()})
	default:
		internalError(c, err)
	}
}

func wordID(c *gin.Context) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.Param("wordId"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid word id"})
		return uuid.Nil, false
	}
	return id, true
}

type createRequest struct {
	Headword string `json:"headword" binding:"required"`
	Kind     Kind   `json:"kind"` // defaults to word
}

// Create is step one of the form (基本信息): register the headword, get a
// draft shell back.
func (h *Handler) Create(c *gin.Context) {
	var req createRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	adminID := c.MustGet(auth.ContextAdminIDKey).(uuid.UUID)
	w, err := h.svc.CreateShell(c.Request.Context(), adminID, req.Headword, req.Kind)
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"word": w})
}

// Get returns the whole tree; the edit page loads once and switches tabs
// locally.
func (h *Handler) Get(c *gin.Context) {
	id, ok := wordID(c)
	if !ok {
		return
	}
	w, err := h.svc.Get(c.Request.Context(), id)
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"word": w})
}

// maxSaveBodyBytes caps the tree-save request body; validateTree's node and
// text bounds cover the parsed shape, this covers the raw bytes before parsing.
const maxSaveBodyBytes = 4 << 20 // 4 MiB

// SaveContent is the 保存 button: full-tree replace (see SaveInput).
func (h *Handler) SaveContent(c *gin.Context) {
	id, ok := wordID(c)
	if !ok {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxSaveBodyBytes)
	var in SaveInput
	if err := c.ShouldBindJSON(&in); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request body too large (max 4 MiB)"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	w, err := h.svc.Save(c.Request.Context(), id, &in)
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"word": w})
}

// Publish is the 提交 button: completeness check (V1–V10) then draft →
// published; 422 lists every violation.
func (h *Handler) Publish(c *gin.Context) {
	id, ok := wordID(c)
	if !ok {
		return
	}
	w, err := h.svc.Publish(c.Request.Context(), id)
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"word": w})
}

// List backs the list page: pagination plus the search row's filters.
func (h *Handler) List(c *gin.Context) {
	page := clampInt(parseInt(c.Query("page"), 1), 1, 1<<31)
	pageSize := clampInt(parseInt(c.Query("page_size"), 20), 1, 100)

	f := ListFilter{
		Query:  c.Query("q"),
		Gloss:  c.Query("gloss"),
		Kind:   Kind(c.Query("kind")),
		POS:    c.Query("pos"),
		Level:  Level(c.Query("level")),
		Status: Status(c.Query("status")),
		Limit:  pageSize,
		Offset: (page - 1) * pageSize,
	}
	for q, dst := range map[string]*time.Time{"created_from": &f.CreatedFrom, "created_to": &f.CreatedTo} {
		if v := c.Query(q); v != "" {
			ts, err := time.Parse(time.RFC3339, v)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": q + " must be RFC3339"})
				return
			}
			*dst = ts
		}
	}

	items, total, err := h.svc.List(c.Request.Context(), f)
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"words": items,
		"page":  gin.H{"page": page, "page_size": pageSize, "total": total},
	})
}

// Stats backs the list-page header counters (累计/今日/本月创编).
func (h *Handler) Stats(c *gin.Context) {
	st, err := h.svc.Stats(c.Request.Context())
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, st)
}

// Delete removes one entry with its whole tree.
func (h *Handler) Delete(c *gin.Context) {
	id, ok := wordID(c)
	if !ok {
		return
	}
	if err := h.svc.Delete(c.Request.Context(), id); err != nil {
		respondErr(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

type batchDeleteRequest struct {
	IDs []uuid.UUID `json:"ids" binding:"required"`
}

// BatchDelete serves the list page's checkbox delete. Unknown ids are skipped;
// the response reports how many entries actually existed.
func (h *Handler) BatchDelete(c *gin.Context) {
	var req batchDeleteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	n, err := h.svc.BatchDelete(c.Request.Context(), req.IDs)
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": n})
}

// RelatedSearch backs the 添加关联词 dialog: find entries by headword, list
// their senses for picking.
func (h *Handler) RelatedSearch(c *gin.Context) {
	results, err := h.svc.RelatedSearch(c.Request.Context(),
		c.Query("q"), Kind(c.Query("kind")), parseInt(c.Query("limit"), 20))
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"results": results})
}

func parseInt(s string, def int) int {
	if s == "" {
		return def
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return def
		}
		n = n*10 + int(r-'0')
		if n > 1<<30 {
			return def
		}
	}
	return n
}

func clampInt(n, lo, hi int) int {
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}
