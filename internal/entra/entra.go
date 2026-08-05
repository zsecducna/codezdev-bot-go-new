// Package entra is the lower layer for the ☁️ ENTRA / KIRO SSO admin
// category (.omc/specs/py_entra_spec.md) — the Go twin of entra_auth.py,
// entra_enterprise_app.py and entra_create_users.py. It never imports
// internal/handlers or internal/tg (architecture-go.md §6 applied to this
// feature, mirrored from internal/awsidc's own package doc): this package
// only builds argv/env, runs child processes via internal/subproc, parses
// their JSON output, and — for the device-code login itself — speaks the
// Microsoft identity platform's OAuth2 device-authorization flow directly
// over HTTP.
//
// Device-code login is the ONE piece of this feature that is NOT run as a
// vendored Python subprocess in this port, unlike the Kiro/AWS IDC browser
// logins (internal/provision) or the Graph-touching scripts below. The
// device-code flow needs no browser automation in THIS process at all — the
// admin completes sign-in on their OWN device by visiting a URL — so there
// is nothing here that architecture-go.md §6 requires staying in a child
// process. See session.go's package doc for the one deliberately-flagged
// compatibility gap this choice leaves open (the on-disk session cache is
// NOT byte-compatible with MSAL's own cache format).
//
// Two families of exported functions:
//
//  1. Session (session.go, cache.go): StartDeviceCode/CompleteDeviceCode
//     (device-code login), SessionStatus/GetAccessToken (silent refresh +
//     expiry detection), SignOut, SetTenant/GetTenant — pure Go, no
//     subprocess, mirroring entra_auth.py's IN-PROCESS design in the Python
//     original (telegram_bot.py calls entra_auth functions directly, never
//     via subprocess — the shared MSAL cache file is what lets the three
//     child scripts below reuse that same session).
//  2. Script runners (enterpriseapp.go, users.go): one function per Graph
//     operation the two Python child scripts perform, run via
//     internal/subproc exactly like internal/awsidc's Discover/Provision —
//     ListDomains/CreateApp/ListApps/SAMLSetup/ConfigureProvisioning/
//     DeleteApp (entra_enterprise_app.py) and ListGroups/TenantInfo/
//     CreateUsersNow/MakeCSV/VerifyCSV/ResetPassword/
//     PreviewDeleteGroupUsers/DeleteGroupUsers/CleanupDeleteByUPN/
//     SoftDeleteByUPN (entra_create_users.py).
//
// Never logs, prints, or forwards a credential (subproc's own rule, package
// doc contract 1): every runner redacts secret VALUES from any error string
// built out of a child's stderr (redact.go, copied from internal/awsidc's
// approach — see that file's doc for why VALUE-based redaction beats a fixed
// key list), and the session functions never place a raw access/refresh
// token in a returned error.
//
// DELIBERATELY OUT OF SCOPE (left for the handler layer / a follow-up):
// the AWS IAM Identity Center credential-connect sub-flow reached only after
// a successful source="aws" SCIM provisioning (aws_idc.py --verify, the
// `aws login --remote` subprocess, and the IAM-access-keys fallback —
// py_entra_spec.md §7 "AWS credential connect sub-flow"). None of those are
// among the 11 ☁️ ENTRA menu callbacks themselves, and they need a
// materially different subprocess pattern (interactive stdout URL-scraping,
// feeding an authorization code via Stdin) than the clean
// argv-in/JSON-on-stdout contract every function below follows.
package entra

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ctxOrBackground defends against a nil context reaching subproc.Run or an
// http.Client call. No exported function here expects to be called with
// one, but the guard is cheap and mirrors internal/awsidc's identical
// helper (awsidc.go:99-104).
func ctxOrBackground(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// ScriptDir mirrors internal/handlers/otp.go's unexported scriptDir()
// helper: KIRO_SCRIPT_DIR, read LAZILY — because the bot loads .env into the
// process environment AFTER this code is linked in, the same reasoning
// provision.LoginPythonFromEnv documents for its own env read — else the
// executable's own directory. Kept as a local COPY rather than an import of
// internal/handlers, which would be a dependency inversion (internal/entra
// must not import internal/handlers, per this package's brief).
//
// Every function in this package takes scriptDir/cacheDir explicitly
// instead of resolving a default internally (mirroring internal/awsidc's
// Discover/Provision signatures) — ScriptDir is offered only as a
// convenience callers MAY use to build those arguments; nothing in this
// package calls it itself.
func ScriptDir() string {
	if v := strings.TrimSpace(os.Getenv("KIRO_SCRIPT_DIR")); v != "" {
		return v
	}
	if exe, err := os.Executable(); err == nil {
		return filepath.Dir(exe)
	}
	return "."
}

// CacheDir returns the conventional session-cache directory beside the
// vendored scripts. entra_auth.py's CACHE_DIR is
// os.path.dirname(os.path.abspath(__file__)) + "/.entra-auth"
// (entra_auth.py:50) — always beside entra_auth.py itself, which lives in
// scriptDir alongside entra_enterprise_app.py / entra_create_users.py.
// Passing CacheDir(ScriptDir()) keeps this Go port's session cache in the
// SAME directory a vendored entra_auth.py would use, even though the file
// FORMAT differs (see session.go's package doc for that gap).
func CacheDir(scriptDir string) string {
	return filepath.Join(scriptDir, ".entra-auth")
}

// orDefault returns d when it is positive, else def — shared by every
// runner function that accepts a caller-overridable timeout (some
// entra_create_users.py operations run under different timeouts at
// different call sites in the Python original: py_entra_spec.md §4 step 6
// gives --tenant-info a 45s budget from the "Generate users" domain picker,
// while §9 step 3 uses the general 900s default for the SAME --tenant-info
// call from "Clean Up Orders" — a single fixed per-function constant cannot
// express that, so the timeout travels as an explicit parameter instead).
func orDefault(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}
