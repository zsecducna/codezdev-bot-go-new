// Promo codes (admin-issued) + referral codes (per-customer) — port of
// store.py:997-1147.
//
// Codes are stored UPPER-CASE (store.py:1000) and are globally unique across
// promo_codes AND referrals so a redeemed code is unambiguous
// (store.py:95-97, CodeInUse below).
package store

import (
	"database/sql"
	"strings"
)

// Promo mirrors one `promo_codes` row (store.py:90-94). Stack=true adds the
// promo % ON TOP of the volume discount; false (default) replaces it.
// Active=false disables a code without deleting its history.
type Promo struct {
	Code      string
	Percent   float64
	MaxUses   int64
	UsedCount int64
	Stack     bool
	Active    bool
	TS        float64
}

const promoCols = "code, percent, max_uses, used_count, stack, active, ts"

func scanPromo(r rowScanner) (*Promo, error) {
	var (
		p             Promo
		stack, active int64
		ts            sql.NullFloat64
	)
	if err := r.Scan(&p.Code, &p.Percent, &p.MaxUses, &p.UsedCount,
		&stack, &active, &ts); err != nil {
		return nil, err
	}
	p.Stack, p.Active, p.TS = stack != 0, active != 0, ts.Float64
	return &p, nil
}

// CreatePromo inserts a new admin promo code, stored upper-case
// (store.py:999-1005).
func (s *Store) CreatePromo(code string, percent float64, maxUses int64, stack bool) error {
	stackInt := 0
	if stack {
		stackInt = 1
	}
	_, err := s.db.Exec("INSERT INTO promo_codes(code, percent, max_uses, used_count, "+
		"stack, active, ts) VALUES (?,?,?,0,?,1,?)",
		strings.ToUpper(code), percent, maxUses, stackInt, now())
	return err
}

// GetPromo returns a promo code row, or nil. Case-insensitive lookup
// (store.py:1008-1016).
func (s *Store) GetPromo(code string) (*Promo, error) {
	if code == "" {
		return nil, nil
	}
	p, err := scanPromo(s.db.QueryRow(
		"SELECT "+promoCols+" FROM promo_codes WHERE code=?", strings.ToUpper(code)))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return p, err
}

// ListPromos returns all promo codes, newest first — for the admin list view
// (store.py:1019-1024).
func (s *Store) ListPromos() ([]*Promo, error) {
	rows, err := s.db.Query("SELECT " + promoCols + " FROM promo_codes ORDER BY ts DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Promo
	for rows.Next() {
		p, err := scanPromo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PromoReserveUse atomically consumes ONE use of `code` iff it is active and
// uses remain (store.py:1027-1043). Returns the (post-increment) promo if THIS
// call took a slot, else nil (unknown / inactive / exhausted). The
// `used_count < max_uses` guard lives IN the UPDATE so concurrent applies can
// never oversell a code; the whole read-modify-write runs in one
// BEGIN IMMEDIATE transaction (architecture-go.md §5). Pair every success with
// PromoReleaseUse if the order ultimately does not deliver.
func (s *Store) PromoReserveUse(code string) (*Promo, error) {
	u := strings.ToUpper(code)
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	res, err := tx.Exec("UPDATE promo_codes SET used_count = used_count + 1 "+
		"WHERE code=? AND active=1 AND used_count < max_uses", u)
	if err != nil {
		return nil, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n != 1 {
		return nil, tx.Commit()
	}
	p, err := scanPromo(tx.QueryRow(
		"SELECT "+promoCols+" FROM promo_codes WHERE code=?", u))
	if err != nil {
		return nil, err
	}
	return p, tx.Commit()
}

// PromoReleaseUse gives back one reserved use (order expired / replaced / not
// delivered, store.py:1046-1050). MAX(0, ...) floors the counter so a stray
// double-release can never go negative.
func (s *Store) PromoReleaseUse(code string) error {
	_, err := s.db.Exec("UPDATE promo_codes SET used_count = MAX(0, used_count - 1) "+
		"WHERE code=?", strings.ToUpper(code))
	return err
}

// SetPromoActive enables/disables a promo code without deleting it
// (store.py:1053-1057).
func (s *Store) SetPromoActive(code string, active bool) error {
	a := 0
	if active {
		a = 1
	}
	_, err := s.db.Exec("UPDATE promo_codes SET active=? WHERE code=?",
		a, strings.ToUpper(code))
	return err
}

// DeletePromo removes a promo code entirely (store.py:1060-1063).
func (s *Store) DeletePromo(code string) error {
	_, err := s.db.Exec("DELETE FROM promo_codes WHERE code=?", strings.ToUpper(code))
	return err
}

// ---- referral codes (per-customer) ----------------------------------------

// GetReferralCode returns this chat's persistent referral code, or "" if not
// yet created (store.py:1068-1073).
func (s *Store) GetReferralCode(chatID string) (string, error) {
	var code string
	err := s.db.QueryRow("SELECT code FROM referrals WHERE chat_id=?",
		chatID).Scan(&code)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return code, err
}

// SetReferralCode creates this chat's referral code if absent and returns the
// STORED code (store.py:1076-1086). INSERT OR IGNORE keeps the first code
// assigned even under a race, so the returned value is always the canonical
// one for the chat. Insert + read-back run in one transaction.
func (s *Store) SetReferralCode(chatID, code string) (string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("INSERT OR IGNORE INTO referrals(chat_id, code, ts) VALUES (?,?,?)",
		chatID, strings.ToUpper(code), now()); err != nil {
		return "", err
	}
	var stored string
	if err := tx.QueryRow("SELECT code FROM referrals WHERE chat_id=?",
		chatID).Scan(&stored); err != nil {
		return "", err
	}
	return stored, tx.Commit()
}

// CodeInUse reports whether `code` already exists as a promo OR a referral
// code — the cross-table uniqueness check (store.py:1089-1097).
func (s *Store) CodeInUse(code string) (bool, error) {
	u := strings.ToUpper(code)
	var one int
	err := s.db.QueryRow("SELECT 1 FROM promo_codes WHERE code=?", u).Scan(&one)
	if err == nil {
		return true, nil
	}
	if err != sql.ErrNoRows {
		return false, err
	}
	err = s.db.QueryRow("SELECT 1 FROM referrals WHERE code=?", u).Scan(&one)
	if err == nil {
		return true, nil
	}
	if err != sql.ErrNoRows {
		return false, err
	}
	return false, nil
}

// ReferralOwner returns the chat_id that owns referral `code`, or ""
// (store.py:1100-1107).
func (s *Store) ReferralOwner(code string) (string, error) {
	if code == "" {
		return "", nil
	}
	var chatID string
	err := s.db.QueryRow("SELECT chat_id FROM referrals WHERE code=?",
		strings.ToUpper(code)).Scan(&chatID)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return chatID, err
}

// HasRedeemedReferral reports whether this chat has ever (currently-reserved
// or finalized) redeemed a referral code — the one-time-per-customer guard
// reads this (store.py:1110-1115).
func (s *Store) HasRedeemedReferral(chatID string) (bool, error) {
	var one int
	err := s.db.QueryRow("SELECT 1 FROM referral_redemptions WHERE chat_id=?",
		chatID).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

// ReferralRedeemReserve reserves a one-time referral redemption for chatID
// (store.py:1118-1130). Returns true if THIS call took the (lifetime) slot,
// false if the chat had already redeemed one — INSERT OR IGNORE against the
// redeemer-chat_id PK is the atomic guard. Release with ReferralRedeemRelease
// only if the order never delivers.
func (s *Store) ReferralRedeemReserve(chatID, code, referrerChat, des string) (bool, error) {
	res, err := s.db.Exec(
		"INSERT OR IGNORE INTO referral_redemptions(chat_id, code, "+
			"referrer_chat, order_des, ts) VALUES (?,?,?,?,?)",
		chatID, strings.ToUpper(code), referrerChat, des, now())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ReferralRedeemRelease undoes a reserved redemption (order expired / replaced
// / not delivered, store.py:1133-1147). When orderDes is non-nil, only the
// redemption reserved FOR THAT order is removed — so releasing one order can
// never wipe a redemption a different (e.g. delivered) order legitimately
// owns. orderDes=nil deletes any row for the chat (used by code-replace, where
// the row belongs to the same order).
func (s *Store) ReferralRedeemRelease(chatID string, orderDes *string) error {
	if orderDes == nil {
		_, err := s.db.Exec("DELETE FROM referral_redemptions WHERE chat_id=?", chatID)
		return err
	}
	_, err := s.db.Exec("DELETE FROM referral_redemptions WHERE chat_id=? AND order_des=?",
		chatID, *orderDes)
	return err
}
