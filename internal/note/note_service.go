package note

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"jodjod/internal/user"
)

const (
	deleteKeyword = "ลบ"
	editPrefix    = "แก้ "

	viewKeyword   = "ดู"
	viewTodayWord = "ดูวันนี้"
	viewPrefix    = "ดู "

	viewChecklistKeyword   = "ดูลิส"
	viewChecklistTodayWord = "ดูลิสวันนี้"
	viewChecklistPrefix    = "ดูลิส "

	viewNoteKeyword   = "ดูโน้ต"
	viewNoteTodayWord = "ดูโน้ตวันนี้"
	viewNotePrefix    = "ดูโน้ต "

	searchPrefix       = "หา "
	completeItemPrefix = "เสร็จ "

	fuzzyDoneSuffix   = " เสร็จ"
	fuzzyUndoneSuffix = " ยกเลิก"

	minContentLength = 1
	maxContentLength = 2000

	fuzzyConfidentGap = 0.15
	fuzzyMaxChoices   = 5

	recentFeedLimit = 50
)

var (
	errEmptyContent   = errors.New("content is empty")
	errContentTooLong = errors.New("content too long")

	bangkok = mustLoadBangkok()
)

func mustLoadBangkok() *time.Location {
	loc, err := time.LoadLocation("Asia/Bangkok")
	if err != nil {
		return time.FixedZone("Asia/Bangkok", 7*60*60)
	}
	return loc
}

type Service struct {
	noteRepo *Repository
	userRepo *user.Repository

	// lineUsers จำ LINE user ID → user ID ไว้ชั่วคราว ลด query ไป DB (อยู่ไกล ~0.4 วิ/query)
	// หมดอายุเร็วเพราะการเชื่อมบัญชีบนเว็บเปลี่ยน mapping ได้
	lineUsers sync.Map // string → cachedUser
}

type cachedUser struct {
	id      string
	expires time.Time
}

const lineUserCacheTTL = 10 * time.Minute

func NewService(noteRepo *Repository, userRepo *user.Repository) *Service {
	return &Service{noteRepo: noteRepo, userRepo: userRepo}
}

// IsCommand บอกว่าข้อความเป็นคำสั่งแบบเดิมที่ CaptureFromLine ตอบได้ทันที (fast path ไม่ต้องใช้ AI)
// ข้อความอื่นทั้งหมด (โน้ตอิสระ, ลิสต์, เตือน, คำถาม) ให้ AI จัดการ
// "ดู <x>" ที่ <x> ไม่ใช่วันที่ (เช่น "ดู หนังพรุ่งนี้") ไม่ถือเป็นคำสั่ง
func IsCommand(text string) bool {
	t := strings.TrimSpace(text)
	switch {
	case t == deleteKeyword,
		t == viewKeyword, t == viewTodayWord,
		t == viewChecklistKeyword, t == viewChecklistTodayWord,
		t == viewNoteKeyword, t == viewNoteTodayWord:
		return true
	case strings.HasPrefix(t, editPrefix),
		strings.HasPrefix(t, searchPrefix):
		return true
	case strings.HasPrefix(t, completeItemPrefix):
		_, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(t, completeItemPrefix)))
		return err == nil
	case strings.HasSuffix(t, fuzzyDoneSuffix) && t != fuzzyDoneSuffix,
		strings.HasSuffix(t, fuzzyUndoneSuffix) && t != fuzzyUndoneSuffix:
		return true
	}
	// ลำดับสำคัญ: เช็ค prefix ที่ยาวกว่าก่อน ("ดูลิส " ก่อน "ดู ")
	for _, prefix := range []string{viewChecklistPrefix, viewNotePrefix, viewPrefix} {
		if strings.HasPrefix(t, prefix) {
			_, err := parseThaiDate(strings.TrimSpace(strings.TrimPrefix(t, prefix)))
			return err == nil
		}
	}
	return false
}

// ResolveLineUser คืน user ID ในระบบของ LINE user (สร้างใหม่ถ้ายังไม่มี)
func (s *Service) ResolveLineUser(ctx context.Context, lineUserID string) (string, error) {
	if v, ok := s.lineUsers.Load(lineUserID); ok {
		if c := v.(cachedUser); time.Now().Before(c.expires) {
			return c.id, nil
		}
	}
	u, err := s.userRepo.FindOrCreateByLineID(ctx, lineUserID, "")
	if err != nil {
		return "", fmt.Errorf("find or create user: %w", err)
	}
	s.lineUsers.Store(lineUserID, cachedUser{id: u.ID, expires: time.Now().Add(lineUserCacheTTL)})
	return u.ID, nil
}

func (s *Service) CaptureFromLine(ctx context.Context, lineUserID, displayName, text string) (*CommandResult, error) {
	u, err := s.userRepo.FindOrCreateByLineID(ctx, lineUserID, displayName)
	if err != nil {
		return nil, fmt.Errorf("find or create user: %w", err)
	}

	trimmed := strings.TrimSpace(text)

	switch {
	case trimmed == deleteKeyword:
		return s.deleteLatest(ctx, u.ID)

	case strings.HasPrefix(trimmed, editPrefix):
		newContent := strings.TrimSpace(strings.TrimPrefix(trimmed, editPrefix))
		if result := validateOrResult(newContent); result != nil {
			return result, nil
		}
		return s.editLatest(ctx, u.ID, newContent)

	case strings.HasPrefix(trimmed, completeItemPrefix):
		posStr := strings.TrimSpace(strings.TrimPrefix(trimmed, completeItemPrefix))
		pos, err := strconv.Atoi(posStr)
		if err != nil || pos < 1 {
			return &CommandResult{Action: "invalid_content", Message: `พิมพ์ "เสร็จ" ตามด้วยเลขลำดับ เช่น "เสร็จ 2"`}, nil
		}
		return s.completeLatestChecklistItem(ctx, u.ID, pos)

	case strings.HasSuffix(trimmed, fuzzyDoneSuffix) && trimmed != fuzzyDoneSuffix:
		query := strings.TrimSpace(strings.TrimSuffix(trimmed, fuzzyDoneSuffix))
		return s.fuzzyToggleItem(ctx, u.ID, query, true)

	case strings.HasSuffix(trimmed, fuzzyUndoneSuffix) && trimmed != fuzzyUndoneSuffix:
		query := strings.TrimSpace(strings.TrimSuffix(trimmed, fuzzyUndoneSuffix))
		return s.fuzzyToggleItem(ctx, u.ID, query, false)

	case strings.HasPrefix(trimmed, searchPrefix):
		query := strings.TrimSpace(strings.TrimPrefix(trimmed, searchPrefix))
		if result := validateOrResult(query); result != nil {
			return result, nil
		}
		return s.searchNotes(ctx, u.ID, query)

	case trimmed == viewChecklistKeyword || trimmed == viewChecklistTodayWord:
		t := NoteTypeChecklist
		return s.viewByDate(ctx, u.ID, time.Now().In(bangkok), &t)

	case strings.HasPrefix(trimmed, viewChecklistPrefix):
		dateStr := strings.TrimSpace(strings.TrimPrefix(trimmed, viewChecklistPrefix))
		date, err := parseThaiDate(dateStr)
		if err != nil {
			return &CommandResult{Action: "invalid_date"}, nil
		}
		t := NoteTypeChecklist
		return s.viewByDate(ctx, u.ID, date, &t)

	case trimmed == viewNoteKeyword || trimmed == viewNoteTodayWord:
		t := NoteTypeNote
		return s.viewByDate(ctx, u.ID, time.Now().In(bangkok), &t)

	case strings.HasPrefix(trimmed, viewNotePrefix):
		dateStr := strings.TrimSpace(strings.TrimPrefix(trimmed, viewNotePrefix))
		date, err := parseThaiDate(dateStr)
		if err != nil {
			return &CommandResult{Action: "invalid_date"}, nil
		}
		t := NoteTypeNote
		return s.viewByDate(ctx, u.ID, date, &t)

	case trimmed == viewKeyword || trimmed == viewTodayWord:
		return s.viewByDate(ctx, u.ID, time.Now().In(bangkok), nil)

	case strings.HasPrefix(trimmed, viewPrefix):
		dateStr := strings.TrimSpace(strings.TrimPrefix(trimmed, viewPrefix))
		date, err := parseThaiDate(dateStr)
		if err != nil {
			return &CommandResult{Action: "invalid_date"}, nil
		}
		return s.viewByDate(ctx, u.ID, date, nil)

	case isChecklist(trimmed):
		title, items := splitChecklist(trimmed)
		if result := validateOrResult(title); result != nil {
			return result, nil
		}
		n, err := s.noteRepo.SaveChecklist(ctx, u.ID, &title, items, NoteSourceLine)
		if err != nil {
			return nil, fmt.Errorf("save checklist: %w", err)
		}
		return &CommandResult{Action: "created", Note: n}, nil

	default:
		if result := validateOrResult(trimmed); result != nil {
			return result, nil
		}
		n := &Note{UserID: u.ID, Content: &trimmed, Type: NoteTypeNote, Source: NoteSourceLine}
		if err := s.noteRepo.SaveNote(ctx, n); err != nil {
			return nil, fmt.Errorf("save note: %w", err)
		}
		return &CommandResult{Action: "created", Note: n}, nil
	}
}

func (s *Service) deleteLatest(ctx context.Context, userID string) (*CommandResult, error) {
	n, err := s.noteRepo.FindLatestByUser(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("find latest note: %w", err)
	}
	if err := s.noteRepo.Delete(ctx, n.ID); err != nil {
		return nil, fmt.Errorf("delete note: %w", err)
	}
	return &CommandResult{Action: "deleted"}, nil
}

func (s *Service) editLatest(ctx context.Context, userID, newContent string) (*CommandResult, error) {
	n, err := s.noteRepo.FindLatestByUser(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("find latest note: %w", err)
	}
	if err := s.noteRepo.UpdateContent(ctx, n.ID, newContent); err != nil {
		return nil, fmt.Errorf("update note: %w", err)
	}
	n.Content = &newContent
	return &CommandResult{Action: "updated", Note: n}, nil
}

func (s *Service) completeLatestChecklistItem(ctx context.Context, userID string, position int) (*CommandResult, error) {
	n, err := s.noteRepo.FindLatestByUser(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("find latest note: %w", err)
	}
	if n.Type != NoteTypeChecklist {
		return &CommandResult{Action: "invalid_content", Message: "โน้ตล่าสุดไม่ใช่ checklist ครับ"}, nil
	}
	item, err := s.noteRepo.FindItemByPosition(ctx, n.ID, position)
	if err != nil {
		return &CommandResult{Action: "invalid_content", Message: "ไม่พบรายการลำดับนั้นครับ"}, nil
	}
	if err := s.noteRepo.SetItemDone(ctx, userID, item.ID, true); err != nil {
		return nil, fmt.Errorf("complete item: %w", err)
	}
	return &CommandResult{Action: "item_toggled", Message: item.Text}, nil
}

func (s *Service) fuzzyToggleItem(ctx context.Context, userID, query string, done bool) (*CommandResult, error) {
	if query == "" {
		return &CommandResult{Action: "invalid_content", Message: "พิมพ์ชื่อรายการก่อนคำว่า เสร็จ/ยกเลิก ด้วยครับ"}, nil
	}

	matches, err := s.noteRepo.FindItemsBySimilarText(ctx, userID, query, fuzzyMaxChoices)
	if err != nil {
		return nil, fmt.Errorf("fuzzy find items: %w", err)
	}
	if len(matches) == 0 {
		return &CommandResult{Action: "invalid_content", Message: fmt.Sprintf(`ไม่เจอรายการที่คล้าย "%s" เลยครับ`, query)}, nil
	}

	confident := len(matches) == 1
	if len(matches) >= 2 && (matches[0].Similarity-matches[1].Similarity) >= fuzzyConfidentGap {
		confident = true
	}

	if confident {
		best := matches[0]
		if err := s.noteRepo.SetItemDone(ctx, userID, best.Item.ID, done); err != nil {
			return nil, fmt.Errorf("set item done: %w", err)
		}
		return &CommandResult{Action: "item_toggled", Message: best.Item.Text}, nil
	}

	return &CommandResult{Action: "item_choice", ItemMatches: matches, ChoiceDone: done}, nil
}

func (s *Service) searchNotes(ctx context.Context, userID, query string) (*CommandResult, error) {
	notes, err := s.noteRepo.SearchByUser(ctx, userID, query)
	if err != nil {
		return nil, fmt.Errorf("search notes: %w", err)
	}
	return &CommandResult{Action: "searched", Notes: notes, NoteItems: s.attachItems(ctx, notes), Query: query}, nil
}

func (s *Service) viewByDate(ctx context.Context, userID string, date time.Time, filterType *NoteType) (*CommandResult, error) {
	notes, err := s.noteRepo.FindByUserAndDate(ctx, userID, date, filterType, false)
	if err != nil {
		return nil, fmt.Errorf("view notes: %w", err)
	}
	return &CommandResult{Action: "listed", Notes: notes, NoteItems: s.attachItems(ctx, notes), Date: date}, nil
}

func (s *Service) attachItems(ctx context.Context, notes []*Note) map[string][]*NoteItem {
	itemsByNote := make(map[string][]*NoteItem)
	for _, n := range notes {
		if n.Type != NoteTypeChecklist {
			continue
		}
		items, err := s.noteRepo.FindItemsByNoteID(ctx, n.ID)
		if err != nil {
			continue
		}
		itemsByNote[n.ID] = items
	}
	return itemsByNote
}

func isChecklist(text string) bool { return strings.Contains(text, ",") }

func splitChecklist(text string) (title string, items []string) {
	trimmed := strings.TrimSpace(text)
	idx := strings.IndexAny(trimmed, " \t")
	rest := ""
	if idx == -1 {
		title = trimmed
	} else {
		title = strings.TrimSpace(trimmed[:idx])
		rest = trimmed[idx+1:]
	}
	for _, p := range strings.Split(rest, ",") {
		t := strings.TrimSpace(p)
		if t != "" {
			items = append(items, t)
		}
	}
	return title, items
}

func parseThaiDate(s string) (time.Time, error) {
	if t, err := time.ParseInLocation("2/1/2006", s, bangkok); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation("2/1", s, bangkok); err == nil {
		now := time.Now().In(bangkok)
		return time.Date(now.Year(), t.Month(), t.Day(), 0, 0, 0, 0, bangkok), nil
	}
	return time.Time{}, fmt.Errorf("invalid date format: %s", s)
}

func validateContentLength(text string) error {
	length := utf8.RuneCountInString(text)
	if length < minContentLength {
		return errEmptyContent
	}
	if length > maxContentLength {
		return errContentTooLong
	}
	return nil
}

func validateOrResult(text string) *CommandResult {
	err := validateContentLength(text)
	if err == nil {
		return nil
	}
	msg := "ข้อความสั้นเกินไปครับ ลองพิมพ์ใหม่อีกครั้ง"
	if errors.Is(err, errContentTooLong) {
		msg = fmt.Sprintf("ข้อความยาวเกินไปครับ (จำกัดไม่เกิน %d ตัวอักษร)", maxContentLength)
	}
	return &CommandResult{Action: "invalid_content", Message: msg}
}

// ==========================================================================
// ฝั่ง LIFF และ Web API
// ==========================================================================

func (s *Service) resolveUserID(ctx context.Context, userID, lineUserID, displayName string) (string, error) {
	if userID != "" {
		return userID, nil
	}
	if lineUserID != "" {
		u, err := s.userRepo.FindOrCreateByLineID(ctx, lineUserID, displayName)
		if err != nil {
			return "", err
		}
		return u.ID, nil
	}
	return "", errors.New("missing user identity")
}

func (s *Service) CreateFromAPI(ctx context.Context, userID, lineUserID, displayName, content string, items []string) (*Note, []*NoteItem, error) {
	uid, err := s.resolveUserID(ctx, userID, lineUserID, displayName)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve user: %w", err)
	}
	trimmed := strings.TrimSpace(content)
	if err := validateContentLength(trimmed); err != nil {
		return nil, nil, err
	}
	cleanItems := make([]string, 0, len(items))
	for _, it := range items {
		it = strings.TrimSpace(it)
		if it != "" {
			cleanItems = append(cleanItems, it)
		}
	}
	if len(cleanItems) > 0 {
		n, err := s.noteRepo.SaveChecklist(ctx, uid, &trimmed, cleanItems, NoteSourceWeb)
		if err != nil {
			return nil, nil, err
		}
		noteItems, err := s.noteRepo.FindItemsByNoteID(ctx, n.ID)
		if err != nil {
			return n, nil, nil
		}
		return n, noteItems, nil
	}
	n := &Note{UserID: uid, Content: &trimmed, Type: NoteTypeNote, Source: NoteSourceWeb}
	if err := s.noteRepo.SaveNote(ctx, n); err != nil {
		return nil, nil, fmt.Errorf("save note: %w", err)
	}
	return n, nil, nil
}

type FeedParams struct {
	TypeFilter *NoteType
	Search     string
	After      *time.Time
	Desc       bool
	Limit      int
	PinnedOnly bool
}

func (s *Service) ListFeedFromAPI(ctx context.Context, userID, lineUserID, displayName string, p FeedParams) ([]*Note, map[string][]*NoteItem, error) {
	uid, err := s.resolveUserID(ctx, userID, lineUserID, displayName)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve user: %w", err)
	}
	notes, err := s.noteRepo.FindFeed(ctx, FeedQuery{
		UserID: uid, TypeFilter: p.TypeFilter, Search: p.Search,
		After: p.After, Desc: p.Desc, Limit: p.Limit, PinnedOnly: p.PinnedOnly,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("list feed: %w", err)
	}
	return notes, s.attachItems(ctx, notes), nil
}

func (s *Service) GetNoteCountFromAPI(ctx context.Context, userID, lineUserID, displayName string) (int, error) {
	uid, err := s.resolveUserID(ctx, userID, lineUserID, displayName)
	if err != nil {
		return 0, fmt.Errorf("resolve user: %w", err)
	}
	return s.noteRepo.CountByUser(ctx, uid)
}

func (s *Service) DeleteFromAPI(ctx context.Context, userID, lineUserID, displayName, noteID string) error {
	uid, err := s.resolveUserID(ctx, userID, lineUserID, displayName)
	if err != nil {
		return fmt.Errorf("resolve user: %w", err)
	}
	return s.noteRepo.DeleteByUserAndID(ctx, uid, noteID)
}

func (s *Service) UpdateContentFromAPI(ctx context.Context, userID, lineUserID, displayName, noteID, content string) error {
	uid, err := s.resolveUserID(ctx, userID, lineUserID, displayName)
	if err != nil {
		return fmt.Errorf("resolve user: %w", err)
	}
	trimmed := strings.TrimSpace(content)
	if err := validateContentLength(trimmed); err != nil {
		return err
	}
	return s.noteRepo.UpdateContentByUser(ctx, uid, noteID, trimmed)
}

func (s *Service) SetNoteDoneFromAPI(ctx context.Context, userID, lineUserID, displayName, noteID string, done bool) error {
	uid, err := s.resolveUserID(ctx, userID, lineUserID, displayName)
	if err != nil {
		return fmt.Errorf("resolve user: %w", err)
	}
	return s.noteRepo.SetNoteDone(ctx, uid, noteID, done)
}

func (s *Service) SetPinnedFromAPI(ctx context.Context, userID, lineUserID, displayName, noteID string, pinned bool) error {
	uid, err := s.resolveUserID(ctx, userID, lineUserID, displayName)
	if err != nil {
		return fmt.Errorf("resolve user: %w", err)
	}
	return s.noteRepo.SetPinned(ctx, uid, noteID, pinned)
}

func (s *Service) SetItemDoneFromAPI(ctx context.Context, userID, lineUserID, displayName, itemID string, done bool) error {
	uid, err := s.resolveUserID(ctx, userID, lineUserID, displayName)
	if err != nil {
		return fmt.Errorf("resolve user: %w", err)
	}
	return s.noteRepo.SetItemDone(ctx, uid, itemID, done)
}

// MarkEventProcessed คืน true ถ้า webhook event นี้ยังไม่เคยถูกประมวลผล (และ mark ไว้แล้ว)
func (s *Service) MarkEventProcessed(ctx context.Context, webhookEventID string) (bool, error) {
	return s.noteRepo.TryMarkEventProcessed(ctx, webhookEventID)
}

// ToggleItemFromPostback ใช้ตอนรับ Postback จาก Quick Reply ที่ user กดเลือก (กรณี
// fuzzyToggleItem เจอกำกวมแล้วส่งตัวเลือกไปก่อนหน้า)
func (s *Service) ToggleItemFromPostback(ctx context.Context, lineUserID, itemID string, done bool) (*CommandResult, error) {
	u, err := s.userRepo.FindOrCreateByLineID(ctx, lineUserID, "")
	if err != nil {
		return nil, fmt.Errorf("find or create user: %w", err)
	}
	if err := s.noteRepo.SetItemDone(ctx, u.ID, itemID, done); err != nil {
		return nil, fmt.Errorf("toggle item from postback: %w", err)
	}
	return &CommandResult{Action: "item_toggled"}, nil
}