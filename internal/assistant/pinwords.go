package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"jodjod/internal/note"
)

// answerPinned ตอบ "ปักอะไรไว้บ้าง" จาก DB ตรงๆ ไม่ผ่านโมเดล (ดู note.IsPinnedQuestion)
// บันทึกเป็น tool call ใน ai_actions เหมือนปกติ และใช้ได้แม้โมเดลล่ม
func (a *Assistant) answerPinned(ctx context.Context, exec *executor, action *Action) (string, error) {
	start := time.Now()
	notes, err := a.store.FindFeed(ctx, note.FeedQuery{UserID: exec.userID, Desc: true, Limit: 50, PinnedOnly: true})
	run := toolRun{Name: "list_pinned", Input: json.RawMessage(`{"kind":"all"}`)}
	if err != nil {
		run.IsError, run.Error = true, "internal error, the action was not completed"
		action.ToolCalls = append(action.ToolCalls, run)
		return fallbackReply, errors.New("list pinned notes: " + err.Error())
	}
	items := map[string][]*note.NoteItem{}
	for _, n := range notes {
		if n.Type != note.NoteTypeChecklist {
			continue
		}
		its, err := a.store.FindItemsByNoteID(ctx, n.ID)
		if err != nil {
			return fallbackReply, errors.New("load pinned list items: " + err.Error())
		}
		items[n.ID] = its
	}
	action.timing.tools += time.Since(start)
	run.Result = map[string]any{"count": len(notes)}
	action.ToolCalls = append(action.ToolCalls, run)
	return note.FormatPinned(notes, items), nil
}
