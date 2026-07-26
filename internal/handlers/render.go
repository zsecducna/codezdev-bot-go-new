package handlers

import (
	"context"
	"log"
	"strings"

	"codezdev-bot/internal/i18n"
	"codezdev-bot/internal/tg"
)

// KeyboardVersion is the reply-keyboard layout version — KEYBOARD_VERSION
// (telegram_bot.py:6932). Telegram caches the persistent keyboard per chat, so
// a customer with an OLD keyboard never sees newly-added buttons until
// ensureKeyboard re-pushes. BUMP THIS whenever mainKeyboard's layout changes so
// the new keyboard reaches everyone without a manual /start.
const KeyboardVersion = "5"

// mainKeyboard builds the persistent reply keyboard — port of main_keyboard
// (telegram_bot.py:6962-6990). Three rows: business commands (buy + deposit),
// the Sell-AWS entry on its own row (wide label), then the Self-Service button
// beside the language button, with the Admin entry appended for admins only.
// Button labels come from the SAME i18n source as labelToCmd, so a tap always
// resolves (handlers-go.md §6). resize_keyboard shrinks it to fit and
// is_persistent keeps it shown after a tap (@6988-6989).
func mainKeyboard(lang i18n.Lang, admin bool) *tg.ReplyKeyboardMarkup {
	c := i18n.C()
	ssRow := []tg.KeyboardButton{
		{Text: c.SelfServiceBtn.T(lang)},
		{Text: i18n.LangButton},
	}
	if admin {
		ssRow = append(ssRow, tg.KeyboardButton{Text: i18n.AdminButton})
	}
	return &tg.ReplyKeyboardMarkup{
		Keyboard: [][]tg.KeyboardButton{
			{{Text: c.DescFor("buy").T(lang)}, {Text: c.DescFor("deposit").T(lang)}},
			{{Text: c.SellBtn.T(lang)}},
			ssRow,
		},
		ResizeKeyboard: true, // shrink to fit the buttons (@6988)
		IsPersistent:   true, // keep the keyboard shown (@6989)
	}
}

// languageKeyboard builds the inline language picker — port of
// language_keyboard (telegram_bot.py:6909-6911): one button per supported
// language, in LANGS order, each labeled in its own language.
func languageKeyboard() *tg.InlineKeyboardMarkup {
	rows := make([][]tg.InlineKeyboardButton, 0, len(i18n.LANGS))
	c := i18n.C()
	for _, l := range i18n.LANGS {
		rows = append(rows, []tg.InlineKeyboardButton{
			{Text: c.LangLabel.T(l), CallbackData: "setlang:" + string(l)},
		})
	}
	return &tg.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// labelMap is the reverse map (button label -> command), built once from the
// same i18n descriptions used to render the keyboard so labels and routing can
// never drift — port of LABEL_TO_CMD (telegram_bot.py:7000-7026). The labels
// contain no support @handle, so a static build at init is safe across
// white-label rewrites.
var labelMap = buildLabelMap()

func buildLabelMap() map[string]string {
	m := map[string]string{}
	c := i18n.C()
	// Business command descriptions in every language (@7000-7004).
	for _, cm := range c.Commands {
		m[cm.Desc.EN] = cm.Cmd
		m[cm.Desc.VI] = cm.Cmd
		m[cm.Desc.ZH] = cm.Cmd
	}
	// Fixed multilingual buttons (@7008-7015).
	m[i18n.LangButton] = "language"
	m[i18n.SupportButton] = "support"
	m[i18n.CodeBtn] = "getcode"
	// Self-Service in every language + the legacy v4 label (@7020-7022).
	m[c.SelfServiceBtn.EN] = "selfservice"
	m[c.SelfServiceBtn.VI] = "selfservice"
	m[c.SelfServiceBtn.ZH] = "selfservice"
	m[i18n.SelfServiceBtnLegacy] = "selfservice"
	// Sell button in every language (@7025-7026).
	m[c.SellBtn.EN] = "sellaws"
	m[c.SellBtn.VI] = "sellaws"
	m[c.SellBtn.ZH] = "sellaws"
	return m
}

// labelToCmd resolves a tapped button label to its command, or "" — port of
// label_to_cmd (telegram_bot.py:7029-7031). Trimmed exact match.
func labelToCmd(text string) string {
	return labelMap[strings.TrimSpace(text)]
}

// ensureKeyboard re-pushes the current reply keyboard the first time a chat
// sends text after KeyboardVersion changed — port of _ensure_keyboard
// (telegram_bot.py:6945-6959). No-op once the chat has the current version.
//
// The version flag is set BEFORE the send so a flood of messages can't
// double-push; on a send error it is rolled back to "" so the chat retries on
// its next message. The flag is never held across the network call (§8).
func (b *Bot) ensureKeyboard(ctx context.Context, chatID int64, lang i18n.Lang) {
	st := b.states.Get(chatID)

	st.mu.Lock()
	if st.KBVersionSent == KeyboardVersion {
		st.mu.Unlock()
		return
	}
	st.KBVersionSent = KeyboardVersion // set first so a flood doesn't double-push
	st.mu.Unlock()

	_, err := b.tg.SendMessage(ctx, tg.SendMessageParams{
		ChatID:      chatID,
		Text:        i18n.C().KBRefreshNote.T(lang),
		ReplyMarkup: mainKeyboard(lang, b.isAdmin(chatID)),
	})
	if err != nil {
		// Let it retry next message (Python pops the flag @6959).
		st.mu.Lock()
		st.KBVersionSent = ""
		st.mu.Unlock()
		log.Println("ensure_keyboard push failed:", err)
	}
}
