package handlers

// info.go — the /info Customer Info card (balance + referral) and the shared
// time/status formatters the history views reuse. Ports:
//   - the cmd=="info" branch @11767,
//   - referral_code_for @10099 + gen_unique_code @10090,
//   - fmt_when @6786 + accounts.localize/tz_label @54-88,
//   - STATUS_LABEL @721 lookup.

import (
	"context"
	"crypto/rand"
	"fmt"
	"log"
	"time"

	"codezdev-bot/internal/i18n"
)

// referralPct is REFERRAL_PCT (telegram_bot.py:1574): both the referral discount
// and the referrer reward are 10%.
const referralPct = 10

// cmdInfo renders the Customer Info card — the cmd=="info" branch (telegram_bot.py:
// 11767-11775): the balance card, plus the referral section when the referral
// program is enabled.
func (b *Bot) cmdInfo(ctx context.Context, chatID int64, lang i18n.Lang) {
	bal, _ := b.getBalance(chatID)
	name := b.nameFor(chatID)
	if name == "" {
		name = "—" // chat_names.get(chat_id, "—") @11769
	}
	text := i18n.F(i18n.C().Info.Balance.T(lang),
		"name", name, "chat_id", fmtInt(chatID), "balance", b.usdtDisp(bal))
	// Show the referral code only while the referral program is enabled (@11772).
	if b.referralEnabled() {
		if ref := b.referralCodeFor(chatID); ref != "" {
			text += "\n\n" + i18n.F(i18n.C().Info.ReferralInfo.T(lang),
				"code", ref, "pct", itoa(referralPct))
		}
	}
	b.send(ctx, chatID, text, nil)
}

// referralEnabled reports whether the customer referral program is on — the
// referral_enabled gate (telegram_bot.py:5129). False when no config is wired.
func (b *Bot) referralEnabled() bool {
	if b.cfg == nil {
		return false
	}
	return b.cfg.ReferralEnabled()
}

// referralCodeFor returns this chat's persistent referral code, creating it on
// first access — referral_code_for (telegram_bot.py:10099-10104). "" on a store
// error (the card then simply omits the referral line).
func (b *Bot) referralCodeFor(chatID int64) string {
	if b.store == nil {
		return ""
	}
	code, err := b.store.GetReferralCode(fmtInt(chatID))
	if err != nil {
		log.Println("get_referral_code failed:", err)
		return ""
	}
	if code != "" {
		return code
	}
	stored, err := b.store.SetReferralCode(fmtInt(chatID), b.genUniqueCode())
	if err != nil {
		log.Println("set_referral_code failed:", err)
		return ""
	}
	return stored
}

// referralCodeAlphabet is the code character set — [A-Z0-9], the shape a bank
// transfer content survives (matches the referral/promo code style).
const referralCodeAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// genUniqueCode returns a fresh 8-char code not already used by a promo OR a
// referral — gen_unique_code (telegram_bot.py:10090-10096). Tries up to 50 times,
// then accepts the last (astronomically-unlikely collision, same as Python).
func (b *Bot) genUniqueCode() string {
	var last string
	for range 50 {
		last = randCode(8)
		if b.store == nil {
			return last
		}
		used, err := b.store.CodeInUse(last)
		if err != nil {
			log.Println("code_in_use check failed:", err)
			return last
		}
		if !used {
			return last
		}
	}
	return last
}

// randCode builds an n-char code from referralCodeAlphabet using crypto/rand
// (no seeding, safe for concurrent workers — Date/rand-free unlike a math/rand
// global). Modulo bias over a 36-char set on a byte is negligible for a
// non-security display code.
func randCode(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand should never fail; degrade to a fixed filler rather than
		// panic in the money-adjacent /info path.
		for i := range buf {
			buf[i] = 0
		}
	}
	out := make([]byte, n)
	for i, x := range buf {
		out[i] = referralCodeAlphabet[int(x)%len(referralCodeAlphabet)]
	}
	return string(out)
}

// ---- time + status formatters (shared with history) ------------------------

// langTZ maps a customer language to its display timezone — LANG_TZ
// (accounts.py). vi -> Vietnam (+07), zh -> China (+08); everything else falls
// back to the server's local zone.
var langTZ = map[i18n.Lang]string{
	i18n.VI: "Asia/Ho_Chi_Minh",
	i18n.ZH: "Asia/Shanghai",
}

// localizeTime converts an epoch to the customer's timezone by language —
// accounts.localize (accounts.py:54-69). Falls back to server-local when the tz
// database lacks the zone (a slim image), never erroring in the delivery path.
func localizeTime(ts float64, lang i18n.Lang) time.Time {
	if name, ok := langTZ[lang]; ok {
		if loc, err := time.LoadLocation(name); err == nil {
			return time.Unix(int64(ts), 0).In(loc)
		}
	}
	return time.Unix(int64(ts), 0).Local()
}

// fmtWhen formats an epoch in the customer's timezone with a zone label — fmt_when
// (telegram_bot.py:6786-6798) + tz_label (accounts.py:72-88). Remapped zones
// (vi/zh) show the numeric offset "(GMT +7)"; server/English shows the zone
// abbreviation (e.g. CEST).
func fmtWhen(ts float64, lang i18n.Lang) string {
	dt := localizeTime(ts, lang)
	out := dt.Format("2006-01-02 15:04")
	if _, remapped := langTZ[lang]; remapped {
		_, offSec := dt.Zone()
		sign := "+"
		if offSec < 0 {
			sign, offSec = "-", -offSec
		}
		hh, mm := offSec/3600, (offSec%3600)/60
		body := fmt.Sprintf("%s%d", sign, hh)
		if mm != 0 {
			body += fmt.Sprintf(":%02d", mm)
		}
		return out + " (GMT " + body + ")"
	}
	if name, _ := dt.Zone(); name != "" {
		return out + " " + name
	}
	return out
}

// statusLabel resolves an order status to its localized history-line prefix —
// STATUS_LABEL.get(status, status) (telegram_bot.py:721, 16246). Unknown statuses
// show the raw value.
func statusLabel(status string, lang i18n.Lang) string {
	if s, ok := i18n.C().History.StatusLabel[status]; ok {
		return s.T(lang)
	}
	return status
}
