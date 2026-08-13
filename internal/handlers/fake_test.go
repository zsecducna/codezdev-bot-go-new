package handlers

import (
	"context"
	"sync"

	"codezdev-bot/internal/tg"
)

// fakeTG is the recording Telegram double the handler tests drive
// (handlers-go.md §7: the handler doc owns the fake-Telegram harness). It
// captures every outbound call and can be told to fail SendMessage so the
// ensureKeyboard rollback path is testable. Safe for concurrent use — Process
// may run on multiple goroutines in production, and tests assert on the record
// after the call returns.
type fakeTG struct {
	mu sync.Mutex

	Sends       []tg.SendMessageParams
	Edits       []tg.EditMessageTextParams
	Answers     []tg.AnswerCallbackQueryParams
	Deletes     [][2]int64
	SetCommands []setCommandsCall
	Docs        []docSend   // sendDocument calls (order-file delivery)
	Photos      []photoSend // sendPhotoURL calls (SePay QR image links)

	sendErr error // when non-nil, SendMessage returns it (rollback tests)

	// Files/fileErr back GetFileBytes for adminstock.go's upload-intercept
	// tests: Files maps a file_id to the bytes that "download" should return;
	// fileErr, when non-nil, makes every GetFileBytes call fail instead
	// (simulating a getFile/CDN error).
	Files   map[string][]byte
	fileErr error
}

// docSend records one sendDocument call (chat + local file path).
type docSend struct {
	ChatID   int64
	FilePath string
	Caption  string
}

// photoSend records one sendPhoto call. PhotoURL carries the hosted image URL
// (SendPhotoURLOpts) or the LOCAL file path (SendPhoto), so a test can assert
// on either QR path; Markup is the inline keyboard attached to the photo, if
// any (the USDT deposit screen no longer attaches one).
type photoSend struct {
	ChatID   int64
	PhotoURL string
	Caption  string
	Markup   any
	Local    bool // true when the image came from a local file upload
}

type setCommandsCall struct {
	Commands []tg.BotCommand
	Scope    *tg.BotCommandScope
}

func (f *fakeTG) SendMessage(_ context.Context, p tg.SendMessageParams) (*tg.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return nil, f.sendErr
	}
	f.Sends = append(f.Sends, p)
	return &tg.Message{MessageID: int64(len(f.Sends)), Chat: tg.Chat{ID: p.ChatID}}, nil
}

func (f *fakeTG) EditMessageText(_ context.Context, p tg.EditMessageTextParams) (*tg.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Edits = append(f.Edits, p)
	return &tg.Message{MessageID: p.MessageID, Chat: tg.Chat{ID: p.ChatID}}, nil
}

func (f *fakeTG) AnswerCallbackQuery(_ context.Context, p tg.AnswerCallbackQueryParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Answers = append(f.Answers, p)
	return nil
}

func (f *fakeTG) DeleteMessage(_ context.Context, chatID, messageID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Deletes = append(f.Deletes, [2]int64{chatID, messageID})
	return nil
}

func (f *fakeTG) SetMyCommands(_ context.Context, commands []tg.BotCommand, scope *tg.BotCommandScope) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.SetCommands = append(f.SetCommands, setCommandsCall{Commands: commands, Scope: scope})
	return nil
}

func (f *fakeTG) GetChat(_ context.Context, chatID int64) (*tg.Chat, error) {
	return &tg.Chat{ID: chatID, Type: "private"}, nil
}

func (f *fakeTG) SendDocument(_ context.Context, chatID int64, filePath, caption string) (*tg.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Docs = append(f.Docs, docSend{ChatID: chatID, FilePath: filePath, Caption: caption})
	return &tg.Message{MessageID: 1, Chat: tg.Chat{ID: chatID}}, nil
}

func (f *fakeTG) SendPhotoURLOpts(_ context.Context, p tg.SendPhotoURLParams) (*tg.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Photos = append(f.Photos, photoSend{
		ChatID: p.ChatID, PhotoURL: p.Photo, Caption: p.Caption, Markup: p.ReplyMarkup,
	})
	return &tg.Message{MessageID: 1, Chat: tg.Chat{ID: p.ChatID}}, nil
}

func (f *fakeTG) SendPhoto(_ context.Context, chatID int64, filePath, caption string) (*tg.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Photos = append(f.Photos, photoSend{
		ChatID: chatID, PhotoURL: filePath, Caption: caption, Local: true,
	})
	return &tg.Message{MessageID: 1, Chat: tg.Chat{ID: chatID}}, nil
}

func (f *fakeTG) GetFileBytes(_ context.Context, fileID string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fileErr != nil {
		return nil, f.fileErr
	}
	return f.Files[fileID], nil
}

func (f *fakeTG) sendCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.Sends)
}

// ---- update builders ----------------------------------------------------

func textUpdate(chatID int64, text string) tg.Update {
	return tg.Update{Message: &tg.Message{
		MessageID: 1,
		Chat:      tg.Chat{ID: chatID, Type: "private"},
		From:      &tg.User{ID: chatID},
		Text:      text,
	}}
}

func textUpdateChatType(chatID int64, text, chatType string) tg.Update {
	u := textUpdate(chatID, text)
	u.Message.Chat.Type = chatType
	return u
}

func callbackUpdate(chatID int64, data string) tg.Update {
	return tg.Update{CallbackQuery: &tg.CallbackQuery{
		ID:      "cbid",
		From:    tg.User{ID: chatID},
		Message: &tg.Message{MessageID: 7, Chat: tg.Chat{ID: chatID, Type: "private"}},
		Data:    data,
	}}
}

// newTestBot builds a Bot wired to a fresh fakeTG, with the given admin id and
// banned set. Store/Config are nil (the dispatch/keyboard logic under test
// doesn't need them).
func newTestBot(adminID string, banned map[string]bool) (*Bot, *fakeTG) {
	f := &fakeTG{}
	b := New(Deps{TG: f, AdminID: adminID, Banned: banned})
	return b, f
}
