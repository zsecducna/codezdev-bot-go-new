// Product meta files + product (folder) CRUD — ports the config surface of
// accounts.py: type / notice.txt / delivery.txt / config, read_text_file /
// write_text_file, and create/rename/remove product (accounts.py:138-244,
// 1432-1588).
package stock

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// GetAccountType returns "token"/"kiroapi" if this product stores that
// account kind, else "password" — Python `get_account_type`
// (accounts.py:138-158). Reads accounts/<product>/type (a one-line file).
// Absent or any other content -> "password" (backward-compatible default).
// "kiroapi" is recognized (not collapsed to "password") so admin menus and
// delivery logic can distinguish Kiro API key stock from password stock.
func (s *Store) GetAccountType(product string) string {
	raw := strings.ToLower(strings.TrimSpace(readWholeFile(filepath.Join(s.productDir(product), "type"))))
	switch raw {
	case "token":
		return "token"
	case "kiroapi":
		return "kiroapi"
	}
	return "password"
}

// SetAccountType writes accounts/<product>/type — Python `set_account_type`
// (accounts.py:161-179). Takes the same lock pair as Finalize so a type
// change can't interleave with an in-flight delivery's credential reads.
// actype is normalized: anything but "token"/"kiroapi" falls back to
// "password" (matches GetAccountType's default).
func (s *Store) SetAccountType(product, actype string) error {
	if actype != "token" && actype != "kiroapi" {
		actype = "password"
	}
	unlock, err := s.lockProduct(product)
	if err != nil {
		return err
	}
	defer unlock()
	// Atomic tmp+rename write (accounts.py:176-179).
	return writeFileAtomic(filepath.Join(s.productDir(product), "type"), actype, credentialFileMode)
}

// ProductNotice — the admin-set BUY NOTICE for a product ("" if none), shown
// to the customer right after they pick the product — Python `product_notice`
// (accounts.py:182-191). Stored as accounts/<product>/notice.txt.
func (s *Store) ProductNotice(product string) string {
	return readWholeFile(filepath.Join(s.productDir(product), "notice.txt"))
}

// SetProductNotice sets (or CLEARS, when text is blank) a product's buy
// notice — Python `set_product_notice` (accounts.py:194-211). Atomic write
// under the same locks as the other per-product config so it can't interleave
// with a delivery. Clearing removes the file so ProductNotice returns "".
func (s *Store) SetProductNotice(product, text string) error {
	return s.setMetaText(product, "notice.txt", text)
}

// ProductDelivery — the admin-set DELIVERY NOTE for a product ("" if none),
// appended below the built-in purchase-confirmation message at delivery time
// — Python `product_delivery` (accounts.py:214-224). Stored as
// accounts/<product>/delivery.txt (mirror of notice.txt).
func (s *Store) ProductDelivery(product string) string {
	return readWholeFile(filepath.Join(s.productDir(product), "delivery.txt"))
}

// SetProductDelivery sets (or CLEARS) a product's delivery note — Python
// `set_product_delivery` (accounts.py:227-244).
func (s *Store) SetProductDelivery(product, text string) error {
	return s.setMetaText(product, "delivery.txt", text)
}

// setMetaText is the shared set-or-clear body of SetProductNotice /
// SetProductDelivery (accounts.py:198-211 == 231-244): blank text removes the
// file, non-blank is written atomically, all under the lock pair.
func (s *Store) setMetaText(product, filename, text string) error {
	path := filepath.Join(s.productDir(product), filename)
	unlock, err := s.lockProduct(product)
	if err != nil {
		return err
	}
	defer unlock()
	text = strings.TrimSpace(text)
	if text == "" {
		// Clearing removes the file; a missing file is fine (accounts.py:202-207).
		err := os.Remove(path)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return writeFileAtomic(path, text, credentialFileMode)
}

// validProduct — a product name maps directly to a folder under ./accounts,
// so it must be a safe single path segment: letters, digits, space, dot,
// dash, underscore. No slashes / "." / ".." (path traversal) and no leading
// dot (hidden folders are excluded from product listings)
// (accounts.py:1434-1438).
var validProduct = regexp.MustCompile(`^[A-Za-z0-9 ._-]+$`)

// ValidProductName reports whether name is a safe product folder name —
// Python `valid_product_name` (accounts.py:1441-1446).
func ValidProductName(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." || strings.HasPrefix(name, ".") {
		return false
	}
	return validProduct.MatchString(name)
}

// ProductExists reports whether a product folder with this exact name exists
// — Python `product_exists` (accounts.py:1449-1451).
func (s *Store) ProductExists(name string) bool {
	fi, err := os.Stat(filepath.Join(s.AccountsDir, name))
	return err == nil && fi.IsDir()
}

// CreateProduct creates an empty product folder (with active/) — Python
// `create_product` (accounts.py:1490-1503). Returns nil on success; error
// text mirrors the Python (ok, msg) tuples. Only the process mutex is taken
// (the folder doesn't exist yet, so there is no lock file to flock).
func (s *Store) CreateProduct(name string) error {
	if !ValidProductName(name) {
		return fmt.Errorf("invalid name (use letters, digits, space . _ - only)")
	}
	name = strings.TrimSpace(name)
	folder := filepath.Join(s.AccountsDir, name)
	mu.Lock()
	defer mu.Unlock()
	if fi, err := os.Stat(folder); err == nil && fi.IsDir() {
		return fmt.Errorf("a product with that name already exists")
	}
	if err := os.MkdirAll(filepath.Join(folder, "active"), credentialDirMode); err != nil {
		return fmt.Errorf("could not create folder: %v", err)
	}
	return nil
}

// RenameProduct renames a product folder old -> new — Python `rename_product`
// (accounts.py:1506-1533). Refuses if new is invalid/taken, or if old has
// in-flight reserved accounts (a paid order must never point at a moved
// folder). Sold history and DB order rows still reference the OLD name —
// callers should warn the admin.
func (s *Store) RenameProduct(old, new string) error {
	if !s.ProductExists(old) {
		return fmt.Errorf("source product not found")
	}
	if !ValidProductName(new) {
		return fmt.Errorf("invalid new name (use letters, digits, space . _ - only)")
	}
	new = strings.TrimSpace(new)
	if new == old {
		return fmt.Errorf("new name is the same as the old name")
	}
	src := filepath.Join(s.AccountsDir, old)
	dst := filepath.Join(s.AccountsDir, new)
	// Lock the SOURCE folder (accounts.py:1522) — the flock fd stays valid
	// across the rename on POSIX.
	unlock, err := s.lockProduct(old)
	if err != nil {
		return err
	}
	defer unlock()
	// Re-check reserved INSIDE the lock so a Reserve can't land between the
	// check and the move (which would strand a paid order's stock)
	// (accounts.py:1523-1526).
	if s.countReservedLocked(old) > 0 {
		return fmt.Errorf("product has reserved (in-flight) orders — try again after they settle")
	}
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("a product with the new name already exists")
	}
	if err := os.Rename(src, dst); err != nil {
		return fmt.Errorf("rename failed: %v", err)
	}
	return nil
}

// RemoveProduct deletes a product folder and everything in it — Python
// `remove_product` (accounts.py:1536-1555). Refuses while there are reserved
// (in-flight) orders. Sold accounts and DB order rows are NOT removed
// (history is preserved in ./orders), but the product's own sold/ folder is
// deleted with it — callers should warn.
func (s *Store) RemoveProduct(name string) error {
	if !s.ProductExists(name) {
		return fmt.Errorf("product not found")
	}
	folder := filepath.Join(s.AccountsDir, name)
	unlock, err := s.lockProduct(name)
	if err != nil {
		return err
	}
	defer unlock()
	// Re-check reserved INSIDE the lock (accounts.py:1548-1550).
	if s.countReservedLocked(name) > 0 {
		return fmt.Errorf("product has reserved (in-flight) orders — try again after they settle")
	}
	// shutil.rmtree equivalent; the held flock fd survives the unlink.
	if err := os.RemoveAll(folder); err != nil {
		return fmt.Errorf("delete failed: %v", err)
	}
	return nil
}

// ReadTextFile reads accounts/<product>/<filename> raw text, or "" if absent
// — Python `read_text_file` (accounts.py:1558-1569). Used by the admin
// config/.env editors so current contents show before the admin replaces them.
func (s *Store) ReadTextFile(product, filename string) string {
	b, err := os.ReadFile(filepath.Join(s.productDir(product), filename))
	if err != nil {
		return ""
	}
	return string(b)
}

// WriteTextFile atomically writes text to accounts/<product>/<filename> —
// Python `write_text_file` (accounts.py:1572-1588). Only the fixed
// config/.env filenames are passed by the bot, so there is no path-traversal
// surface from user input.
func (s *Store) WriteTextFile(product, filename, content string) error {
	if !s.ProductExists(product) {
		return fmt.Errorf("product not found")
	}
	path := filepath.Join(s.productDir(product), filename)
	if err := writeFileAtomic(path, content, credentialFileMode); err != nil {
		return fmt.Errorf("write failed: %v", err)
	}
	return nil
}
