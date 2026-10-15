package note

import "time"

type NoteType string

const (
	NoteTypeNote      NoteType = "note"
	NoteTypeTodo      NoteType = "todo"
	NoteTypeChecklist NoteType = "checklist"
)

type User struct {
	ID          string    `db:"id"`
	LineUserID  string    `db:"line_user_id"`
	DisplayName string    `db:"display_name"`
	CreatedAt   time.Time `db:"created_at"`
}

type Note struct {
	ID        string    `db:"id"`
	UserID    string    `db:"user_id"`
	Content   *string   `db:"content"` // pointer เพราะ nullable
	Type      NoteType  `db:"type"`
	IsDone    bool      `db:"is_done"`
	DueDate   *time.Time `db:"due_date"`
	ImageURL  *string   `db:"image_url"`
	CreatedAt time.Time `db:"created_at"`
	UpdatedAt time.Time `db:"updated_at"`
}

type NoteItem struct {
	ID       string `db:"id"`
	NoteID   string `db:"note_id"`
	Text     string `db:"text"`
	IsDone   bool   `db:"is_done"`
	Position int    `db:"position"`
}

type CommandResult struct {
	Action      string
	Note        *Note
	Notes       []*Note
	NoteItems   map[string][]*NoteItem
	Date        time.Time
	Query       string
	Message     string
	ItemMatches []*ItemMatch
	ChoiceDone  bool
}

// ItemMatch คือผลลัพธ์การค้นหา item แบบ fuzzy พร้อมข้อมูล note แม่ติดมาด้วย
// (ใช้โชว์ให้ user เลือกตอนเจอหลายอันคล้ายกัน เช่น "น้ำหกแพ็ค · 25/8")
type ItemMatch struct {
	Item         *NoteItem
	NoteContent  string
	NoteCreated  time.Time
	Similarity   float64
}
