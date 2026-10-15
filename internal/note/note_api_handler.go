package note

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

type APIHandler struct {
	service *Service
}

func NewAPIHandler(service *Service) *APIHandler {
	return &APIHandler{service: service}
}

func (h *APIHandler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.POST("/notes", h.create)
	rg.GET("/notes", h.feed)
	rg.GET("/notes/count", h.count)
	rg.PATCH("/notes/:id", h.update)
	rg.DELETE("/notes/:id", h.delete)
	rg.PATCH("/notes/:id/items/:itemId", h.toggleItem)
}

const defaultFeedLimit = 20
const maxFeedLimit = 100

type createRequest struct {
	Content string   `json:"content"`
	Items   []string `json:"items,omitempty"`
}

type updateRequest struct {
	Content *string `json:"content,omitempty"`
	IsDone  *bool   `json:"is_done,omitempty"`
}

type toggleItemRequest struct {
	IsDone bool `json:"is_done"`
}

type noteDTO struct {
	ID        string     `json:"id"`
	Content   string     `json:"content"`
	Type      string     `json:"type"`
	IsDone    bool       `json:"is_done"`
	ImageURL  string     `json:"image_url,omitempty"`
	DueDate   *time.Time `json:"due_date,omitempty"`
	RemindAt  *time.Time `json:"remind_at,omitempty"`
	Source    string     `json:"source"`
	CreatedAt time.Time  `json:"created_at"`
	Items     []itemDTO  `json:"items,omitempty"`
}

type itemDTO struct {
	ID     string `json:"id"`
	Text   string `json:"text"`
	IsDone bool   `json:"is_done"`
}

func toNoteDTO(n *Note, items []*NoteItem) noteDTO {
	dto := noteDTO{
		ID: n.ID, Type: string(n.Type), IsDone: n.IsDone, CreatedAt: n.CreatedAt,
		DueDate: n.DueDate, RemindAt: n.RemindAt, Source: string(n.Source),
	}
	if n.Content != nil {
		dto.Content = *n.Content
	}
	if n.ImageURL != nil {
		dto.ImageURL = *n.ImageURL
	}
	for _, it := range items {
		dto.Items = append(dto.Items, itemDTO{ID: it.ID, Text: it.Text, IsDone: it.IsDone})
	}
	return dto
}

func toNoteDTOList(notes []*Note, itemsByNote map[string][]*NoteItem) []noteDTO {
	dtos := make([]noteDTO, 0, len(notes))
	for _, n := range notes {
		dtos = append(dtos, toNoteDTO(n, itemsByNote[n.ID]))
	}
	return dtos
}

func userFromContext(c *gin.Context) (userID, lineUserID, displayName string) {
	if v, ok := c.Get("user_id"); ok {
		userID, _ = v.(string)
	}
	if v, ok := c.Get("line_user_id"); ok {
		lineUserID, _ = v.(string)
	}
	if v, ok := c.Get("display_name"); ok {
		displayName, _ = v.(string)
	}
	return
}

func parseTypeQuery(c *gin.Context) (*NoteType, bool) {
	raw := c.Query("type")
	if raw == "" {
		return nil, true
	}
	switch NoteType(raw) {
	case NoteTypeNote, NoteTypeTodo, NoteTypeChecklist, NoteTypeReminder:
		t := NoteType(raw)
		return &t, true
	default:
		return nil, false
	}
}

func parseSortQuery(c *gin.Context) (desc bool, ok bool) {
	switch c.Query("sort") {
	case "", "desc":
		return true, true // default เรียงใหม่->เก่า เหมาะกับ feed
	case "asc":
		return false, true
	default:
		return false, false
	}
}

func (h *APIHandler) create(c *gin.Context) {
	userID, lineUserID, displayName := userFromContext(c)

	var req createRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	n, items, err := h.service.CreateFromAPI(c.Request.Context(), userID, lineUserID, displayName, req.Content, req.Items)
	if err != nil {
		if errors.Is(err, errEmptyContent) || errors.Is(err, errContentTooLong) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "create failed"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"note": toNoteDTO(n, items)})
}

// feed: GET /api/notes?type=&q=&sort=&after=&limit=
// รวม list ปกติ + search ไว้ endpoint เดียว (ใส่ q มา = ค้นหา, ไม่ใส่ = list ปกติ)
// pagination แบบ cursor: ส่ง next_cursor ที่ได้ไปเป็น "after" ในการเรียกครั้งถัดไป
// next_cursor เป็น null แปลว่าหมดแล้ว ไม่ต้องโหลดต่อ
func (h *APIHandler) feed(c *gin.Context) {
	userID, lineUserID, displayName := userFromContext(c)

	typeFilter, ok := parseTypeQuery(c)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid type, use note|todo|checklist"})
		return
	}

	desc, ok := parseSortQuery(c)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid sort, use asc|desc"})
		return
	}

	limit := defaultFeedLimit
	if limitStr := c.Query("limit"); limitStr != "" {
		parsed, err := strconv.Atoi(limitStr)
		if err != nil || parsed < 1 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid limit"})
			return
		}
		limit = parsed
		if limit > maxFeedLimit {
			limit = maxFeedLimit
		}
	}

	var after *time.Time
	if afterStr := c.Query("after"); afterStr != "" {
		parsed, err := time.Parse(time.RFC3339Nano, afterStr)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid after cursor"})
			return
		}
		after = &parsed
	}

	notes, itemsByNote, err := h.service.ListFeedFromAPI(c.Request.Context(), userID, lineUserID, displayName, FeedParams{
		TypeFilter: typeFilter,
		Search:     c.Query("q"),
		After:      after,
		Desc:       desc,
		Limit:      limit,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "feed failed"})
		return
	}

	var nextCursor *string
	if len(notes) == limit {
		cursor := notes[len(notes)-1].CreatedAt.Format(time.RFC3339Nano)
		nextCursor = &cursor
	}

	c.JSON(http.StatusOK, gin.H{
		"notes":       toNoteDTOList(notes, itemsByNote),
		"next_cursor": nextCursor,
	})
}

// count: GET /api/notes/count — สถิติรวมตลอดเวลา ไม่เกี่ยวกับ pagination ของ feed
func (h *APIHandler) count(c *gin.Context) {
	userID, lineUserID, displayName := userFromContext(c)

	count, err := h.service.GetNoteCountFromAPI(c.Request.Context(), userID, lineUserID, displayName)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "count failed"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"count": count})
}

func (h *APIHandler) update(c *gin.Context) {
	userID, lineUserID, displayName := userFromContext(c)
	noteID := c.Param("id")

	var req updateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	if req.Content == nil && req.IsDone == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "nothing to update"})
		return
	}

	if req.Content != nil {
		err := h.service.UpdateContentFromAPI(c.Request.Context(), userID, lineUserID, displayName, noteID, *req.Content)
		if err != nil {
			if errors.Is(err, errEmptyContent) || errors.Is(err, errContentTooLong) {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			c.JSON(http.StatusNotFound, gin.H{"error": "note not found"})
			return
		}
	}

	if req.IsDone != nil {
		err := h.service.SetNoteDoneFromAPI(c.Request.Context(), userID, lineUserID, displayName, noteID, *req.IsDone)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "note not found"})
			return
		}
	}

	c.Status(http.StatusNoContent)
}

func (h *APIHandler) delete(c *gin.Context) {
	userID, lineUserID, displayName := userFromContext(c)
	noteID := c.Param("id")

	if err := h.service.DeleteFromAPI(c.Request.Context(), userID, lineUserID, displayName, noteID); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "note not found"})
		return
	}

	c.Status(http.StatusNoContent)
}

func (h *APIHandler) toggleItem(c *gin.Context) {
	userID, lineUserID, displayName := userFromContext(c)
	itemID := c.Param("itemId")

	var req toggleItemRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	err := h.service.SetItemDoneFromAPI(c.Request.Context(), userID, lineUserID, displayName, itemID, req.IsDone)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "item not found"})
		return
	}

	c.Status(http.StatusNoContent)
}