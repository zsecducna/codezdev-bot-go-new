package handlers

import (
	"context"
	"log"
	"slices"
	"strings"

	"codezdev-bot/internal/i18n"
	"codezdev-bot/internal/tg"
)

// callbackBannedNotice is the fixed (non-localized) alert shown to a banned
// chat that taps an inline button — telegram_bot.py:12230.
const callbackBannedNotice = "🚫 You are banned from using this bot."

// handleCommand dispatches a /command (or a tapped reply-keyboard button) to
// its handler — port of handle_command (telegram_bot.py:11627-11669+). M2.1
// implements the core customer entries (start / language / support /
// selfservice); feature commands (buy, deposit, info, history, getcode,
// sellaws, otp) and the admin slash commands land in their own slices, so an
// unrecognized command is a no-op here for now.
func (b *Bot) handleCommand(ctx context.Context, chatID int64, cmd, clientLang string) {
	lang := b.langFor(chatID, clientLang)
	// Map a legacy Vietnamese command to its current English name (@1632).
	cmd = i18n.ResolveAlias(cmd)

	// Maintenance-mode gate: non-admins get the notice and nothing else runs;
	// admins are exempt (@11635-11637).
	if b.maintenance() && !b.isAdmin(chatID) {
		b.send(ctx, chatID, i18n.C().Maintenance.T(lang), nil)
		return
	}
	// An active login-code session locks the chat: any other command/button must
	// first confirm stopping it, and the confirmation resumes THIS same command
	// afterwards. The first getcode / /otp that OPENS a session isn't blocked — no
	// session exists yet (otp_guard @11647).
	if b.otpGuard(ctx, chatID, lang, cmd) {
		return
	}

	switch cmd {
	case "start":
		// Show the persistent keyboard + welcome. Record the current keyboard
		// version so the next message doesn't trigger a redundant ensureKeyboard
		// re-push (@11652).
		st := b.states.Get(chatID)
		st.mu.Lock()
		st.KBVersionSent = KeyboardVersion
		st.mu.Unlock()
		b.send(ctx, chatID, i18n.C().Welcome.T(lang), mainKeyboard(lang, b.isAdmin(chatID)))
	case "language":
		b.send(ctx, chatID, i18n.C().LangPrompt.T(lang), languageKeyboard())
	case "support":
		// Plain contact card; the button is only shown to customers but this is
		// harmless if an admin triggers it (@11658-11661).
		b.send(ctx, chatID, i18n.C().Support.T(lang), nil)
	case "selfservice":
		b.showSelfService(ctx, chatID, lang)
	case "buy":
		// List ./accounts subfolders as inline buttons (index-based callbacks),
		// reseller rows, visibility gates + the confirm-first pending-order gate
		// (telegram_bot.py:11680-11758).
		b.cmdBuy(ctx, chatID, lang)
	case "deposit":
		// Funding-method picker; QR is wired (M2.3), the rest render per admin
		// toggles (telegram_bot.py:11765-11766 -> start_deposit @10592).
		b.cmdDeposit(ctx, chatID, lang)
	case "info":
		// Customer Info card: balance (+ referral when enabled) — @11767.
		b.cmdInfo(ctx, chatID, lang)
	case "getcode":
		// Ask which purchased account(s) they want a login code for; the next plain
		// text line is one or more usernames (ownership-checked, batched) — @11674.
		b.cmdGetCode(ctx, chatID, lang)
	case "history":
		// Per-product purchase summary with a toggle to the detailed list — @11681.
		b.cmdHistory(ctx, chatID, lang)
	default:
		// Unknown / not-yet-ported command -> no-op. Later slices add cases.
	}
}

// showSelfService is implemented in selfservice.go (M2.6).

// handleCallback handles an inline-button tap — port of handle_callback
// (telegram_bot.py:12221+). Pipeline order (@12221-12285): ban gate -> product-
// click rate limit (buy:/buyx:) -> [std-admin capability gate: M3] -> ack ->
// handle capture -> [OTP guard: M2.6] -> delete the tapped menu message (sync
// LastMenu) -> prefix dispatch. setlang: is handled inline; every other prefix
// routes through the ordered cbRoutes table (handlers-go.md §4).
func (b *Bot) handleCallback(ctx context.Context, cb *tg.CallbackQuery) {
	if cb.Message == nil {
		return
	}
	chatID := cb.Message.Chat.ID
	data := cb.Data
	admin := b.isAdmin(chatID)

	// Banned gate (@12228-12231): alert only (clears the spinner), no chat
	// message, admins exempt.
	if b.isBanned(chatID) && !admin {
		if err := b.tg.AnswerCallbackQuery(ctx, tg.AnswerCallbackQueryParams{
			CallbackQueryID: cb.ID, Text: callbackBannedNotice, ShowAlert: true,
		}); err != nil {
			log.Println("answerCallbackQuery (ban) failed:", err)
		}
		return
	}

	// Anti-spam: cap product-name taps on the "Choose a product" screen — a
	// blocked tap is answered with an alert (NO chat message) BEFORE the ack, so
	// the refusal itself can't be spammed, and the handler goes no further
	// (@12232-12242).
	if strings.HasPrefix(data, "buy:") || strings.HasPrefix(data, "buyx:") {
		st := b.states.Get(chatID)
		if allowed, retry := b.productClickAllowed(st); !allowed {
			lang := b.langFor(chatID, "en")
			if err := b.tg.AnswerCallbackQuery(ctx, tg.AnswerCallbackQueryParams{
				CallbackQueryID: cb.ID,
				Text:            i18n.F(i18n.C().Buy.RateLimitProduct.T(lang), "sec", itoa(retry)),
				ShowAlert:       true,
			}); err != nil {
				log.Println("answerCallbackQuery (rate limit) failed:", err)
			}
			return
		}
	}

	// The standard-admin capability gate (@12251) is M3.1 — no standard admins
	// exist yet, so it is a no-op today and intentionally omitted.

	// Ack first so Telegram clears the button spinner before slower work
	// (@12258).
	if err := b.tg.AnswerCallbackQuery(ctx, tg.AnswerCallbackQueryParams{CallbackQueryID: cb.ID}); err != nil {
		log.Println("answerCallbackQuery failed:", err)
	}

	// Capture the tapper's @username — button-only buyers never send text, so
	// this is the only place their handle is seen (@12259-12265).
	b.captureHandle(chatID, &cb.From)

	// An active login-code session blocks every other inline action too. Checked
	// BEFORE the delete below so the tapped button survives for a re-tap once they
	// stop. The otp-control taps are exempt so the stop/keep confirmation itself
	// works (@12428-12434).
	if data != "otpkeep" && !strings.HasPrefix(data, "otpstop:") {
		clang := b.langFor(chatID, cbClientLang(cb))
		if b.otpGuard(ctx, chatID, clang, "") {
			return
		}
	}

	// Delete the message that held these buttons: once a choice is made the
	// prompt is spent, so removing it keeps the chat from filling with stale menus
	// as the user steps through a flow. Best-effort. Sync LastMenu so send_menu
	// doesn't try to delete it again (@12279-12285).
	st := b.states.Get(chatID)
	mid := cb.Message.MessageID
	st.mu.Lock()
	if st.LastMenu == mid {
		st.LastMenu = 0
	}
	st.mu.Unlock()
	_ = b.tg.DeleteMessage(ctx, chatID, mid)

	// setlang:<lang> — switch this chat's language, swap its slash menu, and
	// re-show the main keyboard relabeled + confirmation in the NEW language
	// (@15024-15031).
	if suffix, ok := strings.CutPrefix(data, "setlang:"); ok {
		lang := i18n.Lang(suffix)
		if supportedLang(lang) {
			b.setLang(chatID, lang)
			b.ApplyMenu(ctx, chatID, lang)
			b.send(ctx, chatID, i18n.C().LangConfirm.T(lang), mainKeyboard(lang, admin))
		}
		return
	}

	// Ordered prefix dispatch (the buy pipeline this slice registers; later
	// slices append their routes). An unmatched callback is a no-op.
	b.dispatchCB(&cbCtx{Ctx: ctx, ChatID: chatID, Data: data, CB: cb, st: st})
}

// supportedLang reports whether lang is one of the LANGS (@15026's `lang in
// LANGS` guard) — a hand-crafted setlang: payload for an unknown code is
// ignored.
func supportedLang(lang i18n.Lang) bool {
	return slices.Contains(i18n.LANGS, lang)
}

// ApplyMenu sets the per-chat slash-command menu to lang — port of apply_menu
// (telegram_bot.py:7364-7384). The menu is per-chat scoped, so an admin chat
// gets the extra admin commands appended; customers never see them. Exported so
// cmd/bot can set the admin menu at boot.
func (b *Bot) ApplyMenu(ctx context.Context, chatID int64, lang i18n.Lang) {
	cmds := MenuCommands(lang, b.isAdmin(chatID))
	scope := &tg.BotCommandScope{Type: "chat", ChatID: chatID}
	if err := b.tg.SetMyCommands(ctx, cmds, scope); err != nil {
		log.Println("apply_menu setMyCommands failed:", err)
	}
}

// MenuCommands builds the setMyCommands "commands" array for lang — the business
// commands from the i18n catalog, plus the admin extras (@7372-7378) when
// admin. Exported so cmd/bot can build the global default menu at boot without
// re-deriving the list.
func MenuCommands(lang i18n.Lang, admin bool) []tg.BotCommand {
	c := i18n.C()
	cmds := make([]tg.BotCommand, 0, len(c.Commands)+6)
	for _, cm := range c.Commands {
		cmds = append(cmds, tg.BotCommand{Command: cm.Cmd, Description: cm.Desc.T(lang)})
	}
	if admin {
		// Admin extras — fixed labels (telegram_bot.py:7372-7378). Not localized
		// in the Python original either.
		cmds = append(cmds,
			tg.BotCommand{Command: "admin", Description: "🔑 Admin panel"},
			tg.BotCommand{Command: "refund", Description: "💸 Refund an order"},
			tg.BotCommand{Command: "getord", Description: "🔎 Look up an order"},
			tg.BotCommand{Command: "reset", Description: "♻️ Reset a user's password"},
			tg.BotCommand{Command: "audit", Description: "📜 Audit log"},
			tg.BotCommand{Command: "pool", Description: "🏊 Pool health"},
		)
	}
	return cmds
}

// maintenance reports whether admin maintenance mode is on (settings key
// "maintenance", @11635). Defaults to false when no config is wired (unit
// tests) or the key is absent/non-bool.
func (b *Bot) maintenance() bool {
	if b.cfg == nil {
		return false
	}
	v, _ := b.cfg.Settings()["maintenance"].(bool)
	return v
}

// send is a small sendMessage wrapper: attaches an optional reply markup and
// logs (never returns) a send failure, matching the Python call sites that
// fire-and-forget their sendMessage.
func (b *Bot) send(ctx context.Context, chatID int64, text string, markup any) {
	p := tg.SendMessageParams{ChatID: chatID, Text: text}
	if markup != nil {
		p.ReplyMarkup = markup
	}
	if _, err := b.tg.SendMessage(ctx, p); err != nil {
		log.Println("sendMessage failed:", err)
	}
}
