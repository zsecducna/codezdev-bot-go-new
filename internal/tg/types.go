// Package tg ports the inlined Telegram Bot API helpers of telegram_bot.py
// (API base + call() helper at telegram_bot.py:69, 6725-6761; multipart
// sendDocument at telegram_bot.py:6795-6869; the getUpdates long-poll loop at
// telegram_bot.py:19019-19039) into a small, hand-rolled, stdlib-only client.
//
// This file holds the wire types: only the fields the Python bot actually
// reads off `update["message"]` / `update["callback_query"]` (surveyed via
// grep for `msg["..."]` / `cb["..."]` / `update["..."]` patterns across
// telegram_bot.py) plus the request-side keyboard/markup shapes it sends back
// (`{"inline_keyboard": [...]}`, `{"keyboard": [...]}` literals throughout the
// file, e.g. telegram_bot.py:1729, 1769). JSON tags match the Bot API field
// names exactly so encoding/json needs no custom (un)marshalers.
package tg

import "encoding/json"

// Update is one element of the getUpdates result array. The Python poll loop
// only branches on "callback_query" vs "message" (process(), telegram_bot.py
// 16916-16923) and advances offset from update_id (telegram_bot.py:19033), so
// those are the fields this port needs; other update kinds (e.g. edited
// messages) are never read by the Python bot and are intentionally omitted.
type Update struct {
	UpdateID      int64          `json:"update_id"`
	Message       *Message       `json:"message,omitempty"`
	CallbackQuery *CallbackQuery `json:"callback_query,omitempty"`
}

// User mirrors the Bot API User object. Fields used by the Python bot: id
// (chat id when message.from), username (@handle capture, telegram_bot.py
// e.g. the "capture the tapper's @username" comment near handle_callback),
// first_name/last_name (display fallback), language_code (lang_for()
// fallback, telegram_bot.py:12237-12238 `cb.get("from") or {}).get
// ("language_code", "en")`).
type User struct {
	ID           int64  `json:"id"`
	IsBot        bool   `json:"is_bot,omitempty"`
	FirstName    string `json:"first_name,omitempty"`
	LastName     string `json:"last_name,omitempty"`
	Username     string `json:"username,omitempty"`
	LanguageCode string `json:"language_code,omitempty"`
}

// Chat mirrors the Bot API Chat object. Python reads msg["chat"]["id"] and
// msg["chat"].get("type", "private") (telegram_bot.py: process(), the
// private-chat-only gate around _autodel_track).
type Chat struct {
	ID       int64  `json:"id"`
	Type     string `json:"type,omitempty"`
	Username string `json:"username,omitempty"`
	Title    string `json:"title,omitempty"`
}

// Document mirrors the Bot API Document object. Python only ever reads
// msg["document"]["file_id"] (upstock/provpool/reset upload paths,
// telegram_bot.py process()), so FileID is the field that matters; the rest
// round-trip for completeness.
type Document struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id,omitempty"`
	FileName     string `json:"file_name,omitempty"`
	MimeType     string `json:"mime_type,omitempty"`
	FileSize     int64  `json:"file_size,omitempty"`
}

// PhotoSize mirrors one entry of the Bot API PhotoSize array
// (msg["photo"], telegram_bot.py process()/broadcast-image path). Telegram
// sends several sizes per photo message; callers pick the largest (last
// element) same as the Python bot does implicitly by taking photo[-1].
type PhotoSize struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id,omitempty"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	FileSize     int64  `json:"file_size,omitempty"`
}

// Message mirrors the Bot API Message object, restricted to fields the
// Python bot reads: message_id (edits/deletes keyed off it, telegram_bot.py
// e.g. 4243, 6062), chat, from (sender), text (command/text routing), date,
// document/photo (upload paths above), caption (broadcast image caption),
// reply_markup (edit-in-place keyboards, msg["reply_markup"] reused when
// re-rendering a screen).
type Message struct {
	MessageID   int64                 `json:"message_id"`
	From        *User                 `json:"from,omitempty"`
	Chat        Chat                  `json:"chat"`
	Date        int64                 `json:"date,omitempty"`
	Text        string                `json:"text,omitempty"`
	Caption     string                `json:"caption,omitempty"`
	Document    *Document             `json:"document,omitempty"`
	Photo       []PhotoSize           `json:"photo,omitempty"`
	ReplyMarkup *InlineKeyboardMarkup `json:"reply_markup,omitempty"`
}

// CallbackQuery mirrors the Bot API CallbackQuery object. Python reads
// cb["id"] (answerCallbackQuery), cb.get("data", ""), cb["message"]["chat"]
// ["id"], cb.get("from") (handle_callback, telegram_bot.py:12221-12240).
type CallbackQuery struct {
	ID      string   `json:"id"`
	From    User     `json:"from"`
	Message *Message `json:"message,omitempty"`
	Data    string   `json:"data,omitempty"`
}

// InlineKeyboardButton mirrors one button of an inline keyboard row. The
// Python bot builds these as plain dict literals, e.g.
// {"text": "...", "callback_data": "..."} (telegram_bot.py:1729-1732).
type InlineKeyboardButton struct {
	Text string `json:"text"`
	// CallbackData must be 1-64 BYTES (Bot API limit) — Telegram rejects the
	// whole sendMessage/editMessageText with a 400 BUTTON_DATA_INVALID
	// otherwise. The Python bot never validates this either (it keeps its
	// hand-built callback strings short by convention, e.g. "buy:<prod>:<n>"),
	// so handler authors composing data from user/product input must budget
	// bytes, not runes.
	CallbackData string `json:"callback_data,omitempty"`
	URL          string `json:"url,omitempty"`
	// Style is the Bot API 9.4+ button colour ("success"/"danger"/"primary")
	// used to tint buy/pay buttons (telegram_bot.py:11720-11727, 10192-10196).
	// omitempty so an unstyled button sends the identical payload the Python bot
	// sends when it omits the field; older clients ignore it (progressive
	// enhancement — no behaviour change).
	Style string `json:"style,omitempty"`
}

// InlineKeyboardMarkup mirrors {"inline_keyboard": [[...]]} literals used
// throughout telegram_bot.py (e.g. 1729, 1769) as the reply_markup payload
// for sendMessage/editMessageText.
type InlineKeyboardMarkup struct {
	InlineKeyboard [][]InlineKeyboardButton `json:"inline_keyboard"`
}

// KeyboardButton / ReplyKeyboardMarkup mirror the Bot API's non-inline
// (custom) reply keyboard shape — the persistent main keyboard the handlers
// send from mainKeyboard (port of main_keyboard, telegram_bot.py:6962-6990).
// SendMessage's reply_markup can carry either keyboard kind, and
// ReplyKeyboardRemove is the standard way to clear one.
type KeyboardButton struct {
	Text string `json:"text"`
}

type ReplyKeyboardMarkup struct {
	Keyboard        [][]KeyboardButton `json:"keyboard"`
	ResizeKeyboard  bool               `json:"resize_keyboard,omitempty"`
	OneTimeKeyboard bool               `json:"one_time_keyboard,omitempty"`
	// IsPersistent keeps the custom keyboard shown even after a tap
	// (telegram_bot.py:6989 main_keyboard `"is_persistent": True`); without it
	// some clients collapse the keyboard, hiding the shop's main buttons.
	IsPersistent bool `json:"is_persistent,omitempty"`
}

// ReplyKeyboardRemove clears a previously-set custom reply keyboard.
type ReplyKeyboardRemove struct {
	RemoveKeyboard bool `json:"remove_keyboard"`
}

// BotCommand mirrors one entry of the setMyCommands "commands" array
// (telegram_bot.py:7375-7379: {"command": "admin", "description": "..."}).
type BotCommand struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

// BotCommandScope mirrors setMyCommands' "scope" object. Python only uses
// the per-chat scope: {"type": "chat", "chat_id": chat_id}
// (telegram_bot.py:7381-7383) to change one user's "/" menu without
// affecting anyone else.
type BotCommandScope struct {
	Type   string `json:"type"`
	ChatID int64  `json:"chat_id,omitempty"`
}

// ResponseParameters carries the retry_after (429) / migrate_to_chat_id
// hints the Bot API attaches to some error responses
// (body.get("parameters") in Python's send_document, telegram_bot.py:6853).
type ResponseParameters struct {
	MigrateToChatID int64 `json:"migrate_to_chat_id,omitempty"`
	RetryAfter      int   `json:"retry_after,omitempty"`
}

// apiResponse is the Bot API's envelope: {"ok": bool, "result": ..., ...}.
// Mirrors what Python's call() inspects at telegram_bot.py:6754-6759
// (body.get("ok"), body["result"], body.get("error_code"),
// body.get("description")) plus the "parameters" object used for 429s.
type apiResponse struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result,omitempty"` // raw; unmarshaled per-method by callers
	ErrorCode   int             `json:"error_code,omitempty"`
	Description string          `json:"description,omitempty"`
	// (no omitempty: apiResponse is decode-only, and omitempty is a no-op on
	// nested structs anyway — flagged by go vet's omitzero check.)
	Parameters ResponseParameters `json:"parameters"`
}
