package entra

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// entraCreateUsersScript is the vendored child script this file drives —
// bulk user create/verify/reset/delete via Graph. Not yet present in this
// Go repo; the reference source is
// /Users/z/Desktop/Kiro/entra_create_users.py.
const entraCreateUsersScript = "entra_create_users.py"

// defaultUsersTimeout is entra_create_users.py's general subprocess budget
// (py_entra_spec.md "Subprocess contract (general)": "_run_entra_users
// ... timeout defaults to 900s"). Every function below accepts an explicit
// timeout override (0 = this default) because the SAME operation
// (--tenant-info) runs under a SHORTER 45s budget at one call site
// (py_entra_spec.md §4 step 6) and this default at another (§9 step 3) — a
// single fixed constant per function cannot express that.
const defaultUsersTimeout = 900 * time.Second

// bulkDeleteTimeout is the fixed 3600s budget the Python bot ALWAYS uses for
// the two truly destructive bulk operations — confirmed delete-group-users
// and delete-by-upn (cleanup) — because both can touch thousands of
// throttled Graph calls (py_entra_spec.md "Subprocess contract (general)").
const bulkDeleteTimeout = 3600 * time.Second

// GroupInfo is one entry of do_list_groups' "groups" array
// (entra_create_users.py:507-514).
type GroupInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// GroupsResult is do_list_groups' JSON shape, feeding the group pickers
// used by "Generate users" and "Delete users in group"
// (py_entra_spec.md §4/§8).
type GroupsResult struct {
	OK     bool        `json:"ok"`
	Groups []GroupInfo `json:"groups"`
}

func listGroupsArgs() []string { return []string{"--list-groups"} }

// ListGroups lists the tenant's existing groups (do_list_groups,
// entra_create_users.py:507-514).
func ListGroups(ctx context.Context, scriptDir, python string, timeout time.Duration) (GroupsResult, error) {
	raw, err := runScript(ctx, python, filepath.Join(scriptDir, entraCreateUsersScript),
		listGroupsArgs(), nil, orDefault(timeout, defaultUsersTimeout), nil)
	if err != nil {
		return GroupsResult{}, err
	}
	return decodeResult[GroupsResult](raw, entraCreateUsersScript)
}

// TenantInfoResult is do_tenant_info's JSON shape
// (entra_create_users.py:210-227): the CURRENT signed-in tenant's id and
// ALL its verified domains — used to scope both the multi-domain UPN picker
// in "Generate users" and the tenant-membership check in "Clean Up Orders"
// (py_entra_spec.md §4 step 6, §9 step 3).
type TenantInfoResult struct {
	OK            bool     `json:"ok"`
	TenantID      string   `json:"tenant_id"`
	PrimaryDomain string   `json:"primary_domain"`
	Domains       []string `json:"domains"`
}

func tenantInfoArgs() []string { return []string{"--tenant-info"} }

// TenantInfo reads the signed-in tenant's id + verified domains
// (do_tenant_info, entra_create_users.py:210-227). Pass a short timeout
// (e.g. 45*time.Second) at the "Generate users" domain-picker call site so
// a hang never stalls the admin with no Back button
// (py_entra_spec.md §4 step 6); 0 uses the general 900s default.
func TenantInfo(ctx context.Context, scriptDir, python string, timeout time.Duration) (TenantInfoResult, error) {
	raw, err := runScript(ctx, python, filepath.Join(scriptDir, entraCreateUsersScript),
		tenantInfoArgs(), nil, orDefault(timeout, defaultUsersTimeout), nil)
	if err != nil {
		return TenantInfoResult{}, err
	}
	return decodeResult[TenantInfoResult](raw, entraCreateUsersScript)
}

// CreateUsersNowResult is do_generate's JSON shape
// (entra_create_users.py:413-497) for the LIVE-create path ("⚡ Create now").
// ErrorCount is do_generate's own "error" key — an INT count of per-user
// create failures on a successful run; it is unrelated to (and never
// confused with, since decodeResult only reaches this struct when the
// envelope's "ok" is true) the STRING "error" key on the failure envelope.
type CreateUsersNowResult struct {
	OK             bool   `json:"ok"`
	Created        int    `json:"created"`
	Existing       int    `json:"existing"`
	ErrorCount     int    `json:"error"`
	Assigned       int    `json:"assigned"`
	MFAOff         int    `json:"mfa_off"`
	MFAUnavailable bool   `json:"mfa_unavailable"`
	ErrorSample    string `json:"error_sample"`
	GroupID        string `json:"group_id"`
	Output         string `json:"output"`
	GroupAssigned  bool   `json:"group_assigned"`
	Note           string `json:"note"`
}

// createUsersNowArgs mirrors the Python bot's exact command
// (py_entra_spec.md §4 "_entra_run_create"): license fills BOTH --subname
// and --group (the license name IS the group name IS the store subfolder),
// --assign is always "group" for the live-create path, and --domain is
// omitted when "" (the child then falls back to the tenant's own primary
// verified domain).
func createUsersNowArgs(license string, count int, spID, output, domain string) []string {
	args := []string{
		"--subname", license, "--group", license,
		"--count", strconv.Itoa(count),
		"--sp-id", spID, "--assign", "group",
		"--output", output,
	}
	if domain != "" {
		args = append(args, "--domain", domain)
	}
	return args
}

// CreateUsersNow creates `count` cloud-only users (do_generate,
// entra_create_users.py:413-497), assigns the license GROUP (not
// per-user) to the app, and writes a per-user password CSV to output — the
// "⚡ Create now" path of "Generate users" (py_entra_spec.md §4). The CSV at
// Output contains plaintext passwords; it is host-local and this package
// never reads it back.
func CreateUsersNow(ctx context.Context, scriptDir, python, license string, count int, spID, output, domain string, timeout time.Duration) (CreateUsersNowResult, error) {
	raw, err := runScript(ctx, python, filepath.Join(scriptDir, entraCreateUsersScript),
		createUsersNowArgs(license, count, spID, output, domain), nil, orDefault(timeout, defaultUsersTimeout), nil)
	if err != nil {
		return CreateUsersNowResult{}, err
	}
	return decodeResult[CreateUsersNowResult](raw, entraCreateUsersScript)
}

// MakeCSVResult is make_csv's JSON shape (entra_create_users.py:676-711) for
// the "📄 Generate CSV" bulk-create console path.
type MakeCSVResult struct {
	OK            bool   `json:"ok"`
	Count         int    `json:"count"`
	Domain        string `json:"domain"`
	Output        string `json:"output"`
	MembersOutput string `json:"members_output"`
	Group         string `json:"group"`
	GroupID       string `json:"group_id"`
	GroupAssigned bool   `json:"group_assigned"`
}

func makeCSVArgs(license string, count int, spID, output, membersOutput, domain string) []string {
	args := []string{
		"--make-csv",
		"--subname", license, "--group", license,
		"--count", strconv.Itoa(count),
		"--sp-id", spID,
		"--output", output, "--members-output", membersOutput,
	}
	if domain != "" {
		args = append(args, "--domain", domain)
	}
	return args
}

// MakeCSV creates + assigns the license group UP FRONT (no users yet) and
// writes the Entra portal's bulk-create template plus a group
// members-import CSV (make_csv, entra_create_users.py:676-711) — the "📄
// Generate CSV" path of "Generate users" (py_entra_spec.md §4). Both output
// and membersOutput are host-local paths the caller sends as documents.
func MakeCSV(ctx context.Context, scriptDir, python, license string, count int, spID, output, membersOutput, domain string, timeout time.Duration) (MakeCSVResult, error) {
	raw, err := runScript(ctx, python, filepath.Join(scriptDir, entraCreateUsersScript),
		makeCSVArgs(license, count, spID, output, membersOutput, domain), nil, orDefault(timeout, defaultUsersTimeout), nil)
	if err != nil {
		return MakeCSVResult{}, err
	}
	return decodeResult[MakeCSVResult](raw, entraCreateUsersScript)
}

// VerifySample is one entry of do_verify's random spot-check
// (entra_create_users.py:943-1000). Enabled is nil when the user lookup
// itself failed (Exists is then also false) — the same None-vs-bool
// distinction the Python dict carries.
type VerifySample struct {
	UPN         string `json:"upn"`
	Exists      bool   `json:"exists"`
	DisplayName string `json:"displayName"`
	Enabled     *bool  `json:"enabled"`
	InGroup     bool   `json:"in_group"`
}

// VerifyCSVResult is do_verify's JSON shape (entra_create_users.py:943-1000)
// — the CSV → store verification pipeline shared by "🔁 Verify CSV → store"
// and the "✅ Done — verify" button (py_entra_spec.md §5).
type VerifyCSVResult struct {
	OK               bool           `json:"ok"`
	Planned          int            `json:"planned"`
	InGroup          int            `json:"in_group"`
	NotInGroup       int            `json:"not_in_group"`
	NotInGroupSample []string       `json:"not_in_group_sample"`
	InGroupUPNs      []string       `json:"in_group_upns"`
	Sample           []VerifySample `json:"sample"`
	GroupID          string         `json:"group_id"`
	Forced           int            `json:"forced"`
	ForcedUPNs       []string       `json:"forced_upns"`
	ForceErrors      []string       `json:"force_errors"`
	ForceFailed      int            `json:"force_failed"`
}

func verifyCSVArgs(license, spID, output string, sampleSize int, skipForceUPNsFile string) []string {
	args := []string{
		"--verify", "--group", license, "--sp-id", spID,
		"--output", output, "--sample-size", strconv.Itoa(sampleSize),
	}
	if skipForceUPNsFile != "" {
		args = append(args, "--skip-force-upns-file", skipForceUPNsFile)
	}
	return args
}

// VerifyCSV confirms which of the CSV's planned users landed in the license
// group, force-changes their password at next sign-in (skipping any UPN
// already flipped on a prior verify — one-shot per user), spot-checks a
// random sample live, and reports counts (do_verify,
// entra_create_users.py:943-1000). skipForceUPNs is the caller's growing
// "already forced" set (persisted in the .meta.json sidecar — a
// handler-layer concern); when non-empty it is written to a temp file this
// function owns and cleans up, since --skip-force-upns-file takes a PATH,
// not a list on argv.
func VerifyCSV(ctx context.Context, scriptDir, python, license, spID, output string, sampleSize int, skipForceUPNs []string, timeout time.Duration) (VerifyCSVResult, error) {
	var skipFile string
	if len(skipForceUPNs) > 0 {
		f, cleanup, err := writeLinesFile("entra-skip-force-*.txt", skipForceUPNs)
		if err != nil {
			return VerifyCSVResult{}, err
		}
		defer cleanup()
		skipFile = f
	}
	raw, err := runScript(ctx, python, filepath.Join(scriptDir, entraCreateUsersScript),
		verifyCSVArgs(license, spID, output, sampleSize, skipFile), nil, orDefault(timeout, defaultUsersTimeout), nil)
	if err != nil {
		return VerifyCSVResult{}, err
	}
	return decodeResult[VerifyCSVResult](raw, entraCreateUsersScript)
}

// ResetPasswordResult is do_reset_password's JSON shape
// (entra_create_users.py:548-563). Found is false when no matching user
// exists in the tenant — decodeResult surfaces that case as a returned
// error (the opError envelope's "error" carries the exact "user '...' not
// found in this tenant" message), same treatment as every other Graph
// failure in this package.
type ResetPasswordResult struct {
	OK       bool   `json:"ok"`
	Found    bool   `json:"found"`
	UserID   string `json:"user_id"`
	Username string `json:"username"`
	UPN      string `json:"upn"`
	Password string `json:"password"`
}

func resetPasswordArgs(ident string) []string {
	return []string{"--reset-password", "--user", ident}
}

// ResetPassword resets one tenant user's password to a fresh strong value
// WITHOUT forcing a change at next sign-in, so the saved credential works
// immediately (do_reset_password, entra_create_users.py:548-563) — the
// explicit contrast with CreateUsersNow's forced-change default
// (py_entra_spec.md §6).
func ResetPassword(ctx context.Context, scriptDir, python, ident string, timeout time.Duration) (ResetPasswordResult, error) {
	raw, err := runScript(ctx, python, filepath.Join(scriptDir, entraCreateUsersScript),
		resetPasswordArgs(ident), nil, orDefault(timeout, defaultUsersTimeout), nil)
	if err != nil {
		return ResetPasswordResult{}, err
	}
	return decodeResult[ResetPasswordResult](raw, entraCreateUsersScript)
}

// DeleteGroupUsersPreview is do_delete_group_users' PREVIEW shape
// (entra_create_users.py:1048-1132, the `not args.confirm` branch) — count +
// sample only, no writes.
type DeleteGroupUsersPreview struct {
	OK            bool     `json:"ok"`
	Preview       bool     `json:"preview"`
	Group         string   `json:"group"`
	GroupID       string   `json:"group_id"`
	Count         int      `json:"count"`
	SkippedAdmins int      `json:"skipped_admins"`
	Sample        []string `json:"sample"`
}

func previewDeleteGroupUsersArgs(group string) []string {
	return []string{"--delete-group-users", "--group", group}
}

// PreviewDeleteGroupUsers previews (does NOT delete) the user members of
// group that would be removed by DeleteGroupUsers (do_delete_group_users,
// entra_create_users.py:1048-1132, no --confirm). A group name matching MORE
// THAN ONE tenant group is refused ("ambiguous; rename or delete by a unique
// group name") rather than guessed — surfaced as a returned error.
func PreviewDeleteGroupUsers(ctx context.Context, scriptDir, python, group string, timeout time.Duration) (DeleteGroupUsersPreview, error) {
	raw, err := runScript(ctx, python, filepath.Join(scriptDir, entraCreateUsersScript),
		previewDeleteGroupUsersArgs(group), nil, orDefault(timeout, defaultUsersTimeout), nil)
	if err != nil {
		return DeleteGroupUsersPreview{}, err
	}
	return decodeResult[DeleteGroupUsersPreview](raw, entraCreateUsersScript)
}

// DeleteGroupUsersResult is do_delete_group_users' CONFIRMED-delete shape
// (entra_create_users.py:1048-1132, the args.confirm branch) — a DIFFERENT
// field set than the preview (no "preview"/"count"/"sample"; "total" instead
// of "count", plus per-outcome tallies). GroupDeleted is true only when
// EVERY targeted user was cleanly removed AND no admin/role-holder was
// skipped (py_entra_spec.md §8's "batch-cleanliness rule") — a partial
// failure always keeps the group for retry.
type DeleteGroupUsersResult struct {
	OK                 bool   `json:"ok"`
	Group              string `json:"group"`
	GroupID            string `json:"group_id"`
	Total              int    `json:"total"`
	SkippedAdmins      int    `json:"skipped_admins"`
	Deleted            int    `json:"deleted"`
	Purged             int    `json:"purged"`
	Failed             int    `json:"failed"`
	PurgeFailed        int    `json:"purge_failed"`
	GroupDeleted       bool   `json:"group_deleted"`
	GroupPurged        bool   `json:"group_purged"`
	GroupDeleteError   string `json:"group_delete_error"`
	GroupDeleteSkipped string `json:"group_delete_skipped"`
}

func deleteGroupUsersArgs(group string) []string {
	return []string{"--delete-group-users", "--group", group, "--confirm", "--purge"}
}

// DeleteGroupUsers PERMANENTLY deletes (hard-purges, IRREVERSIBLE) every
// non-protected user member of group (do_delete_group_users,
// entra_create_users.py:1048-1132, --confirm --purge). The signed-in admin
// and every directory-role holder are never deleted. timeout<=0 uses the
// fixed 3600s bulk-delete budget the Python bot always passes here
// (py_entra_spec.md "Subprocess contract (general)").
func DeleteGroupUsers(ctx context.Context, scriptDir, python, group string, timeout time.Duration) (DeleteGroupUsersResult, error) {
	raw, err := runScript(ctx, python, filepath.Join(scriptDir, entraCreateUsersScript),
		deleteGroupUsersArgs(group), nil, orDefault(timeout, bulkDeleteTimeout), nil)
	if err != nil {
		return DeleteGroupUsersResult{}, err
	}
	return decodeResult[DeleteGroupUsersResult](raw, entraCreateUsersScript)
}

// CleanupResult is do_delete_by_upn's JSON shape
// (entra_create_users.py:1135-1193) — the order-driven "Clean Up Orders"
// deletion, distinct from DeleteGroupUsers' group-driven deletion. A UPN not
// found in the tenant (already gone, or never a tenant account) is counted
// in NotFound, not Failed — the whole operation is idempotent, safely
// re-runnable.
type CleanupResult struct {
	OK          bool     `json:"ok"`
	Targeted    int      `json:"targeted"`
	Deleted     int      `json:"deleted"`
	Purged      int      `json:"purged"`
	NotFound    int      `json:"not_found"`
	Protected   int      `json:"protected"`
	Failed      int      `json:"failed"`
	DeletedUPNs []string `json:"deleted_upns"`
}

func cleanupDeleteByUPNArgs(upnsFile string) []string {
	return []string{"--delete-by-upn", "--upns-file", upnsFile, "--purge"}
}

// CleanupDeleteByUPN PERMANENTLY deletes (hard-purges, IRREVERSIBLE) the
// tenant users named by upns (do_delete_by_upn, entra_create_users.py:
// 1135-1193, --delete-by-upn --upns-file --purge) — powers "🧼 Clean Up
// Orders" (py_entra_spec.md §9). upns is written to a temp file this
// function owns and cleans up, since --upns-file takes a PATH. timeout<=0
// uses the fixed 3600s bulk-delete budget the Python bot always passes here.
func CleanupDeleteByUPN(ctx context.Context, scriptDir, python string, upns []string, timeout time.Duration) (CleanupResult, error) {
	upnsFile, cleanup, err := writeLinesFile("entra-cleanup-upns-*.txt", upns)
	if err != nil {
		return CleanupResult{}, err
	}
	defer cleanup()
	raw, err := runScript(ctx, python, filepath.Join(scriptDir, entraCreateUsersScript),
		cleanupDeleteByUPNArgs(upnsFile), nil, orDefault(timeout, bulkDeleteTimeout), nil)
	if err != nil {
		return CleanupResult{}, err
	}
	return decodeResult[CleanupResult](raw, entraCreateUsersScript)
}

// DeleteByUPNResult is do_delete_by_upn's JSON shape (entra_create_users.py:
// 1135-1193) — the SAME wire shape CleanupResult decodes (that function's
// child call is this exact operation with --purge hardcoded on), field names
// matched on purpose so a caller switching from CleanupDeleteByUPN to
// SoftDeleteByUPN only has to change the function name, not any field
// access. Purged is always 0 on the soft path, since do_delete_by_upn only
// attempts a directory purge when its own purge argument is true
// (entra_create_users.py:1175-1181). Kept as its own named type rather than
// reusing CleanupResult because that name is tied to the ALWAYS-purge "Clean
// Up Orders" contract — the orders refund-delete sweep should not have to
// reason about a result type whose name promises a purge it never asks for.
type DeleteByUPNResult struct {
	OK          bool     `json:"ok"`
	Targeted    int      `json:"targeted"`
	Deleted     int      `json:"deleted"`
	Purged      int      `json:"purged"`
	NotFound    int      `json:"not_found"`
	Protected   int      `json:"protected"`
	Failed      int      `json:"failed"`
	DeletedUPNs []string `json:"deleted_upns"`
}

// Clean reports the exact "batch-cleanliness" rule the orders slice's
// admrefdel/refdelone refund-delete sweep depends on to decide whether it
// may mark any order processed (py_orders_spec.md "admrefdel": "clean =
// failed==0 and protected==0" — the helper returns only BATCH-level counts,
// never a per-UPN outcome, so a caller cannot know WHICH upn failed/was
// skipped, only whether the whole batch came back clean). When Clean() is
// false the caller must mark NOTHING — a re-run is always safe, since Graph
// reports an already-deleted account as not_found rather than failed.
func (r DeleteByUPNResult) Clean() bool {
	return r.Failed == 0 && r.Protected == 0
}

func softDeleteByUPNArgs(upnsFile string) []string {
	return []string{"--delete-by-upn", "--upns-file", upnsFile}
}

// SoftDeleteByUPN deletes the tenant users named by upns via Entra's normal
// ~30-day RECOVERABLE soft-delete (do_delete_by_upn, entra_create_users.py:
// 1135-1193, --delete-by-upn --upns-file, deliberately WITHOUT --purge) —
// the primitive the orders slice's "🗑️ Delete Refunded Accounts"
// (admrefdel/refdelone) sweep needs (py_orders_spec.md "admrefdel": "Soft-
// delete only, no --purge ... recoverable in Entra for ~30 days if this was
// a mistake or the refund is reversed"). This function deliberately takes NO
// purge parameter — CleanupDeleteByUPN already owns the one caller that
// legitimately wants --purge ("🧼 Clean Up Orders", py_entra_spec.md §9,
// confirmed permanent/purge by that spec's own report line "♻️ purged
// (quota freed now)" and confirm-copy "Permanent: deletes + purges") — so
// there is no bool here a caller could flip by accident into an irreversible
// purge. upns is written to a temp file this function owns and cleans up,
// mirroring CleanupDeleteByUPN's own --upns-file mechanism (UPNs are not
// secret, but a large list still does not belong on argv). timeout<=0 uses
// the fixed 3600s bulk-delete budget the Python bot always passes for this
// operation.
func SoftDeleteByUPN(ctx context.Context, scriptDir, python string, upns []string, timeout time.Duration) (DeleteByUPNResult, error) {
	upnsFile, cleanup, err := writeLinesFile("entra-softdelete-upns-*.txt", upns)
	if err != nil {
		return DeleteByUPNResult{}, err
	}
	defer cleanup()
	raw, err := runScript(ctx, python, filepath.Join(scriptDir, entraCreateUsersScript),
		softDeleteByUPNArgs(upnsFile), nil, orDefault(timeout, bulkDeleteTimeout), nil)
	if err != nil {
		return DeleteByUPNResult{}, err
	}
	return decodeResult[DeleteByUPNResult](raw, entraCreateUsersScript)
}

// writeLinesFile writes one entry per line to a fresh 0600 temp file (never
// wider than the credential-bearing tree this package sits next to — even
// though UPNs/usernames are not secret) and returns a cleanup func that
// removes it. Mirrors internal/awsidc/provision.go's writeUsersFile.
func writeLinesFile(pattern string, lines []string) (path string, cleanup func(), err error) {
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", func() {}, err
	}
	path = f.Name()
	cleanup = func() { os.Remove(path) }
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		cleanup()
		return "", func() {}, err
	}
	for _, ln := range lines {
		if _, err := f.WriteString(ln + "\n"); err != nil {
			f.Close()
			cleanup()
			return "", func() {}, err
		}
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return path, cleanup, nil
}
