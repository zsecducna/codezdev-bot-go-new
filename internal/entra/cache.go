package entra

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// cacheFileName is DELIBERATELY NOT "msal_cache.bin" (entra_auth.py's own
// name, entra_auth.py:51) — see session.go's package doc for why this
// package's on-disk schema is NOT MSAL's SerializableTokenCache wire format.
// Reusing that exact filename for a differently-shaped file would misname
// it: whoever later inspects .entra-auth/ should see a name that does not
// imply msal-python compatibility that isn't actually there.
const cacheFileName = "entra_session.json"

// tenantFileName matches entra_auth.py's TENANT_FILE (entra_auth.py:57)
// exactly — it is a bare trimmed string, so there is no format gap to flag
// here, unlike the token cache.
const tenantFileName = "tenant.txt"

// sessionCacheSchema is a version tag on this package's own cache file, so a
// future format change can detect (and refuse to misread) an older file
// rather than silently misinterpreting it.
const sessionCacheSchema = "go-entra-session-v1"

// sessionCache is this package's on-disk session representation. See
// session.go's package doc for the explicit compatibility gap this leaves
// with a vendored entra_auth.py.
type sessionCache struct {
	Schema       string    `json:"schema"`
	Username     string    `json:"username"`
	TenantID     string    `json:"tenant_id"`
	TenantDomain string    `json:"tenant_domain"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// loadSessionCache reads cacheDir's session file. A MISSING file is "no
// session yet" (ok=false, err=nil) rather than an error — the common case on
// a fresh host. A CORRUPT/truncated file degrades the same way, mirroring
// entra_auth._load_cache's "corrupt cache -> start empty; the user simply
// logs in again" behavior (entra_auth.py:93-104) — a damaged cache must
// never crash a status check or force-open a false error path. Any OTHER
// read failure (e.g. a permissions error) is returned as a genuine error,
// since that is not a "no session" case and silently swallowing it could
// mask a misconfigured deploy.
func loadSessionCache(cacheDir string) (sessionCache, bool, error) {
	raw, err := os.ReadFile(filepath.Join(cacheDir, cacheFileName))
	if err != nil {
		if os.IsNotExist(err) {
			return sessionCache{}, false, nil
		}
		return sessionCache{}, false, err
	}
	var c sessionCache
	if json.Unmarshal(raw, &c) != nil {
		return sessionCache{}, false, nil
	}
	if c.AccessToken == "" && c.RefreshToken == "" {
		return sessionCache{}, false, nil
	}
	return c, true, nil
}

// saveSessionCache persists c to cacheDir, 0600 in a 0700 dir — the same
// permissions entra_auth._save_cache uses (entra_auth.py:108-121), since
// this file holds a live refresh token that grants Graph admin power for
// the signed-in tenant, same as the Python cache does.
func saveSessionCache(cacheDir string, c sessionCache) error {
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(cacheDir, 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	path := filepath.Join(cacheDir, cacheFileName)
	fd, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer fd.Close()
	// fchmod BEFORE writing so the refresh token is never written while the
	// file is group/world-readable, whether the file is new or pre-existed
	// with looser perms (same reasoning as entra_auth.py:114-119's
	// os.fchmod-before-write comment).
	if err := fd.Chmod(0o600); err != nil {
		return err
	}
	_, err = fd.Write(raw)
	return err
}

// SignOut forgets the cached session by deleting the cache file — a no-op
// if absent (sign_out, entra_auth.py:219-226). Does NOT touch tenant.txt, so
// the next login skips re-typing the tenant unless it's changed manually
// (py_entra_spec.md "adm:entralogout").
func SignOut(cacheDir string) error {
	err := os.Remove(filepath.Join(cacheDir, cacheFileName))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// SetTenant persists the target tenant (id or primary domain) so every
// subsequent token call authenticates against it (set_tenant,
// entra_auth.py:68-81). Changing the tenant drops any cached session for
// the OLD tenant — a token for a different tenant would just 403.
func SetTenant(cacheDir, tenant string) error {
	tenant = strings.TrimSpace(tenant)
	cur, err := GetTenant(cacheDir)
	if err != nil {
		return err
	}
	if cur != tenant {
		if err := SignOut(cacheDir); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(cacheDir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(cacheDir, tenantFileName)
	fd, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer fd.Close()
	if err := fd.Chmod(0o600); err != nil {
		return err
	}
	_, err = fd.WriteString(tenant)
	return err
}

// GetTenant returns the persisted target tenant (id or domain), or "" if
// none was ever set (get_tenant, entra_auth.py:60-65).
func GetTenant(cacheDir string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(cacheDir, tenantFileName))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}
