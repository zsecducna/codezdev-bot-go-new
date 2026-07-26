package handlers

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"codezdev-bot/internal/config"
)

// usdtDisp is the customer-facing money string for a stored-VND amount, USDT-only
// — port of usdt_disp -> usdt_str -> paypal.vnd_to_usd (telegram_bot.py:6434-6442,
// 6425-6431; paypal.py:46-55). e.g. 125000 -> "5.00 USDT". Rounds UP to the cent
// (never undercharge). Falls back to "— USDT" if the rate is unset/invalid so a
// render never crashes. Uses the config rate (settings.json usd_to_vnd overriding
// .env USD_TO_VND, else the compiled default) rather than os.Getenv, so display
// is deterministic and independent of the process environment.
func (b *Bot) usdtDisp(vnd int64) string {
	rateStr := config.DefaultUSDToVND
	if b.cfg != nil {
		rateStr = b.cfg.USDToVND()
	}
	rate, err := strconv.ParseFloat(rateStr, 64)
	if err != nil || rate <= 0 {
		return "— USDT"
	}
	usd := float64(vnd) / rate
	// Python: cents = int(usd*100) + (1 if (usd*100) % 1 else 0)  -> ceil to cent.
	cents := math.Ceil(usd*100 - 1e-9)
	return fmt.Sprintf("%.2f USDT", cents/100)
}

// itoa / itoa64 are the terse int->string helpers used to fill i18n {qty}/{max}/
// {stock} placeholders (i18n.F takes string kv pairs).
func itoa(n int) string     { return strconv.Itoa(n) }
func itoa64(n int64) string { return strconv.FormatInt(n, 10) }

// cbData joins callback-data parts with ":" — the single builder for compound
// callback strings (buy:N, tier:N:CODE, cancelord:des:cmd …). Telegram rejects
// any callback_data over 64 BYTES with a 400, so cbDataLen (used by tests)
// checks the budget; product refs stay INDEX-based to keep well under it
// (handlers-go.md §4).
func cbData(parts ...string) string { return strings.Join(parts, ":") }

// cbDataLen reports whether a built callback string is within Telegram's 64-byte
// hard limit — the assertion TestCallbackDataLen runs on every builder.
func cbDataLen(data string) bool { return len(data) <= 64 }
