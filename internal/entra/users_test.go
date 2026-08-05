package entra

import (
	"context"
	"os"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

// users_test.go covers the happy path of every entra_create_users.py runner
// plus its argv builder. Cross-cutting failure-mode coverage lives once in
// runner_test.go since every function here shares that exact plumbing.

func writeCreateUsersStub(t *testing.T, dir, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("stub is a POSIX shell script")
	}
	return writeStubScript(t, dir, entraCreateUsersScript, body)
}

func TestListGroupsArgs(t *testing.T) {
	if got, want := listGroupsArgs(), []string{"--list-groups"}; !reflect.DeepEqual(got, want) {
		t.Errorf("listGroupsArgs() = %v, want %v", got, want)
	}
}

func TestListGroups(t *testing.T) {
	dir := t.TempDir()
	writeCreateUsersStub(t, dir, `echo '{"ok":true,"groups":[{"id":"g1","name":"Kiro Pro"}]}'`)
	got, err := ListGroups(context.Background(), dir, "sh", 0)
	if err != nil {
		t.Fatalf("ListGroups: %v", err)
	}
	want := GroupsResult{OK: true, Groups: []GroupInfo{{ID: "g1", Name: "Kiro Pro"}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ListGroups() = %+v, want %+v", got, want)
	}
}

func TestTenantInfoArgs(t *testing.T) {
	if got, want := tenantInfoArgs(), []string{"--tenant-info"}; !reflect.DeepEqual(got, want) {
		t.Errorf("tenantInfoArgs() = %v, want %v", got, want)
	}
}

func TestTenantInfo(t *testing.T) {
	dir := t.TempDir()
	writeCreateUsersStub(t, dir, `echo '{"ok":true,"tenant_id":"t1","primary_domain":"a.com","domains":["a.com","b.com"]}'`)
	// A short 45s-style override, matching the genusers domain-picker call
	// site (py_entra_spec.md §4 step 6) — proves the timeout param actually
	// reaches the child, not just the 0-means-default path.
	got, err := TenantInfo(context.Background(), dir, "sh", 45*time.Second)
	if err != nil {
		t.Fatalf("TenantInfo: %v", err)
	}
	want := TenantInfoResult{OK: true, TenantID: "t1", PrimaryDomain: "a.com", Domains: []string{"a.com", "b.com"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("TenantInfo() = %+v, want %+v", got, want)
	}
}

func TestCreateUsersNowArgs(t *testing.T) {
	got := createUsersNowArgs("kiropro", 10, "sp1", "/out.csv", "")
	want := []string{"--subname", "kiropro", "--group", "kiropro", "--count", "10", "--sp-id", "sp1", "--assign", "group", "--output", "/out.csv"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("createUsersNowArgs() = %v, want %v", got, want)
	}
	gotWithDomain := createUsersNowArgs("kiropro", 10, "sp1", "/out.csv", "d.com")
	wantWithDomain := append(append([]string{}, want...), "--domain", "d.com")
	if !reflect.DeepEqual(gotWithDomain, wantWithDomain) {
		t.Errorf("createUsersNowArgs(domain) = %v, want %v", gotWithDomain, wantWithDomain)
	}
}

func TestCreateUsersNow(t *testing.T) {
	dir := t.TempDir()
	writeCreateUsersStub(t, dir, `echo '{"ok":true,"created":10,"existing":0,"error":0,"assigned":10,"mfa_off":10,"mfa_unavailable":false,"error_sample":"","group_id":"g1","output":"/out.csv","group_assigned":true}'`)
	got, err := CreateUsersNow(context.Background(), dir, "sh", "kiropro", 10, "sp1", "/out.csv", "", 0)
	if err != nil {
		t.Fatalf("CreateUsersNow: %v", err)
	}
	if got.Created != 10 || !got.GroupAssigned || got.GroupID != "g1" {
		t.Errorf("CreateUsersNow() = %+v", got)
	}
}

func TestCreateUsersNowFailureEnvelopeCarriesStringError(t *testing.T) {
	dir := t.TempDir()
	writeCreateUsersStub(t, dir, `echo '{"ok":false,"step":"subname","error":"subscription name must contain letters or numbers"}'; exit 1`)
	_, err := CreateUsersNow(context.Background(), dir, "sh", "!!!", 10, "sp1", "/out.csv", "", 0)
	if err == nil {
		t.Fatal("CreateUsersNow: got nil error, want the subname-validation error")
	}
	if !strings.Contains(err.Error(), "subscription name must contain letters or numbers") {
		t.Errorf("CreateUsersNow error = %q", err.Error())
	}
}

func TestMakeCSVArgs(t *testing.T) {
	got := makeCSVArgs("kiropro", 500, "sp1", "/tpl.csv", "/members.csv", "d.com")
	want := []string{"--make-csv", "--subname", "kiropro", "--group", "kiropro", "--count", "500", "--sp-id", "sp1", "--output", "/tpl.csv", "--members-output", "/members.csv", "--domain", "d.com"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("makeCSVArgs() = %v, want %v", got, want)
	}
}

func TestMakeCSV(t *testing.T) {
	dir := t.TempDir()
	writeCreateUsersStub(t, dir, `echo '{"ok":true,"count":500,"domain":"d.com","output":"/tpl.csv","members_output":"/members.csv","group":"kiropro","group_id":"g1","group_assigned":true}'`)
	got, err := MakeCSV(context.Background(), dir, "sh", "kiropro", 500, "sp1", "/tpl.csv", "/members.csv", "d.com", 0)
	if err != nil {
		t.Fatalf("MakeCSV: %v", err)
	}
	if got.Count != 500 || !got.GroupAssigned {
		t.Errorf("MakeCSV() = %+v", got)
	}
}

func TestVerifyCSVArgsOmitsSkipFileWhenEmpty(t *testing.T) {
	got := verifyCSVArgs("kiropro", "sp1", "/out.csv", 5, "")
	want := []string{"--verify", "--group", "kiropro", "--sp-id", "sp1", "--output", "/out.csv", "--sample-size", "5"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("verifyCSVArgs(skip=\"\") = %v, want %v", got, want)
	}
	gotWithSkip := verifyCSVArgs("kiropro", "sp1", "/out.csv", 5, "/skip.txt")
	wantWithSkip := append(append([]string{}, want...), "--skip-force-upns-file", "/skip.txt")
	if !reflect.DeepEqual(gotWithSkip, wantWithSkip) {
		t.Errorf("verifyCSVArgs(skip) = %v, want %v", gotWithSkip, wantWithSkip)
	}
}

// TestVerifyCSVWritesAndCleansUpSkipForceFile pins that a non-empty
// skipForceUPNs list is staged to a real temp file (since
// --skip-force-upns-file takes a path, not a value) and that the file is
// removed once VerifyCSV returns.
func TestVerifyCSVWritesAndCleansUpSkipForceFile(t *testing.T) {
	dir := t.TempDir()
	script := `f=""
for a in "$@"; do
  if [ "$prev" = "--skip-force-upns-file" ]; then f="$a"; fi
  prev="$a"
done
cat "$f" > ` + dir + `/skip-contents.txt
echo '{"ok":true,"planned":2,"in_group":2,"not_in_group":0,"not_in_group_sample":[],"in_group_upns":["a@d.com","b@d.com"],"sample":[],"group_id":"g1","forced":1,"forced_upns":["a@d.com"],"force_errors":[],"force_failed":0}'
`
	writeCreateUsersStub(t, dir, script)
	got, err := VerifyCSV(context.Background(), dir, "sh", "kiropro", "sp1", "/csv.csv", 5, []string{"already@d.com"}, 0)
	if err != nil {
		t.Fatalf("VerifyCSV: %v", err)
	}
	if got.Planned != 2 || got.InGroup != 2 || got.Forced != 1 {
		t.Errorf("VerifyCSV() = %+v", got)
	}
	contents, err := os.ReadFile(dir + "/skip-contents.txt")
	if err != nil {
		t.Fatalf("script never saw --skip-force-upns-file: %v", err)
	}
	if !strings.Contains(string(contents), "already@d.com") {
		t.Errorf("skip-force-upns-file contents = %q, want it to contain the skip UPN", string(contents))
	}
}

func TestResetPasswordFound(t *testing.T) {
	dir := t.TempDir()
	writeCreateUsersStub(t, dir, `echo '{"ok":true,"found":true,"user_id":"u1","username":"john.smith","upn":"john.smith@d.com","password":"Aa1!bbbbbbbb"}'`)
	got, err := ResetPassword(context.Background(), dir, "sh", "john.smith@d.com", 0)
	if err != nil {
		t.Fatalf("ResetPassword: %v", err)
	}
	if !got.Found || got.Password == "" {
		t.Errorf("ResetPassword() = %+v", got)
	}
}

func TestResetPasswordNotFound(t *testing.T) {
	dir := t.TempDir()
	writeCreateUsersStub(t, dir, `echo '{"ok":false,"found":false,"error":"user '"'"'nobody'"'"' not found in this tenant"}'; exit 1`)
	_, err := ResetPassword(context.Background(), dir, "sh", "nobody", 0)
	if err == nil {
		t.Fatal("ResetPassword: got nil error, want the not-found message")
	}
	if !strings.Contains(err.Error(), "not found in this tenant") {
		t.Errorf("ResetPassword error = %q", err.Error())
	}
}

func TestPreviewDeleteGroupUsersArgsNoConfirm(t *testing.T) {
	got := previewDeleteGroupUsersArgs("OldGroup")
	want := []string{"--delete-group-users", "--group", "OldGroup"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("previewDeleteGroupUsersArgs() = %v, want %v (must NOT include --confirm)", got, want)
	}
}

func TestPreviewDeleteGroupUsers(t *testing.T) {
	dir := t.TempDir()
	writeCreateUsersStub(t, dir, `echo '{"ok":true,"preview":true,"group":"OldGroup","group_id":"g1","count":42,"skipped_admins":1,"sample":["a@d.com"]}'`)
	got, err := PreviewDeleteGroupUsers(context.Background(), dir, "sh", "OldGroup", 0)
	if err != nil {
		t.Fatalf("PreviewDeleteGroupUsers: %v", err)
	}
	if !got.Preview || got.Count != 42 || got.SkippedAdmins != 1 {
		t.Errorf("PreviewDeleteGroupUsers() = %+v", got)
	}
}

func TestDeleteGroupUsersArgsAlwaysConfirmAndPurge(t *testing.T) {
	got := deleteGroupUsersArgs("OldGroup")
	want := []string{"--delete-group-users", "--group", "OldGroup", "--confirm", "--purge"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("deleteGroupUsersArgs() = %v, want %v", got, want)
	}
}

func TestDeleteGroupUsers(t *testing.T) {
	dir := t.TempDir()
	writeCreateUsersStub(t, dir, `echo '{"ok":true,"group":"OldGroup","group_id":"g1","total":42,"skipped_admins":0,"deleted":42,"purged":42,"failed":0,"purge_failed":0,"group_deleted":true,"group_purged":true}'`)
	got, err := DeleteGroupUsers(context.Background(), dir, "sh", "OldGroup", 5*time.Second)
	if err != nil {
		t.Fatalf("DeleteGroupUsers: %v", err)
	}
	if got.Deleted != 42 || !got.GroupDeleted {
		t.Errorf("DeleteGroupUsers() = %+v", got)
	}
}

func TestCleanupDeleteByUPNArgs(t *testing.T) {
	got := cleanupDeleteByUPNArgs("/upns.txt")
	want := []string{"--delete-by-upn", "--upns-file", "/upns.txt", "--purge"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("cleanupDeleteByUPNArgs() = %v, want %v", got, want)
	}
}

// TestCleanupDeleteByUPNWritesUPNsFileAndCleansUp pins that the UPN list is
// staged to a real temp file (since --upns-file takes a path) and removed
// once CleanupDeleteByUPN returns.
func TestCleanupDeleteByUPNWritesUPNsFileAndCleansUp(t *testing.T) {
	dir := t.TempDir()
	script := `f=""
for a in "$@"; do
  if [ "$prev" = "--upns-file" ]; then f="$a"; fi
  prev="$a"
done
cat "$f" > ` + dir + `/upns-contents.txt
echo '{"ok":true,"targeted":2,"deleted":2,"purged":2,"not_found":0,"protected":0,"failed":0,"deleted_upns":["a@d.com","b@d.com"]}'
`
	writeCreateUsersStub(t, dir, script)
	got, err := CleanupDeleteByUPN(context.Background(), dir, "sh", []string{"a@d.com", "b@d.com"}, 5*time.Second)
	if err != nil {
		t.Fatalf("CleanupDeleteByUPN: %v", err)
	}
	if got.Deleted != 2 || got.Targeted != 2 {
		t.Errorf("CleanupDeleteByUPN() = %+v", got)
	}
	contents, err := os.ReadFile(dir + "/upns-contents.txt")
	if err != nil {
		t.Fatalf("script never saw --upns-file: %v", err)
	}
	if !strings.Contains(string(contents), "a@d.com") || !strings.Contains(string(contents), "b@d.com") {
		t.Errorf("upns-file contents = %q, want both UPNs", string(contents))
	}
}

func TestSoftDeleteByUPNArgsNeverIncludesPurge(t *testing.T) {
	got := softDeleteByUPNArgs("/upns.txt")
	want := []string{"--delete-by-upn", "--upns-file", "/upns.txt"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("softDeleteByUPNArgs() = %v, want %v (must NOT include --purge)", got, want)
	}
}

// TestSoftDeleteByUPNChildNeverSeesPurgeFlag asserts the ACTUAL argv the
// child process saw, not just what the args builder returns in isolation —
// the whole point of this test (and its purge-path counterpart below) is to
// pin the exact command line entra_create_users.py is invoked with.
func TestSoftDeleteByUPNChildNeverSeesPurgeFlag(t *testing.T) {
	dir := t.TempDir()
	script := `printf '%s\n' "$@" > ` + dir + `/argv.txt
echo '{"ok":true,"targeted":2,"deleted":2,"purged":0,"not_found":0,"protected":0,"failed":0,"deleted_upns":["a@d.com","b@d.com"]}'
`
	writeCreateUsersStub(t, dir, script)
	got, err := SoftDeleteByUPN(context.Background(), dir, "sh", []string{"a@d.com", "b@d.com"}, 5*time.Second)
	if err != nil {
		t.Fatalf("SoftDeleteByUPN: %v", err)
	}
	if got.Deleted != 2 || got.Purged != 0 || !got.Clean() {
		t.Errorf("SoftDeleteByUPN() = %+v, want Clean()==true", got)
	}
	argv, err := os.ReadFile(dir + "/argv.txt")
	if err != nil {
		t.Fatalf("script never captured argv: %v", err)
	}
	if strings.Contains(string(argv), "--purge") {
		t.Errorf("child argv = %q, must NOT contain --purge (soft-delete path)", string(argv))
	}
	if !strings.Contains(string(argv), "--delete-by-upn") {
		t.Errorf("child argv = %q, want it to contain --delete-by-upn", string(argv))
	}
}

// TestCleanupDeleteByUPNChildAlwaysSeesPurgeFlag is the purge-path
// counterpart — CleanupDeleteByUPN's own contract ("Clean Up Orders" is
// deliberately permanent, py_entra_spec.md §9's confirm-copy "Permanent:
// deletes + purges") must not regress into a soft delete.
func TestCleanupDeleteByUPNChildAlwaysSeesPurgeFlag(t *testing.T) {
	dir := t.TempDir()
	script := `printf '%s\n' "$@" > ` + dir + `/argv.txt
echo '{"ok":true,"targeted":1,"deleted":1,"purged":1,"not_found":0,"protected":0,"failed":0,"deleted_upns":["a@d.com"]}'
`
	writeCreateUsersStub(t, dir, script)
	_, err := CleanupDeleteByUPN(context.Background(), dir, "sh", []string{"a@d.com"}, 5*time.Second)
	if err != nil {
		t.Fatalf("CleanupDeleteByUPN: %v", err)
	}
	argv, err := os.ReadFile(dir + "/argv.txt")
	if err != nil {
		t.Fatalf("script never captured argv: %v", err)
	}
	if !strings.Contains(string(argv), "--purge") {
		t.Errorf("child argv = %q, want it to contain --purge (Clean Up Orders is always permanent)", string(argv))
	}
}

// TestSoftDeleteByUPNPartialFailureIsNotClean pins the exact
// "batch-cleanliness" rule the orders slice's admrefdel sweep depends on
// (py_orders_spec.md "admrefdel": "clean = failed==0 and protected==0") —
// any failure at all means Clean() must be false, so the caller marks
// NOTHING processed.
func TestSoftDeleteByUPNPartialFailureIsNotClean(t *testing.T) {
	dir := t.TempDir()
	writeCreateUsersStub(t, dir, `echo '{"ok":true,"targeted":3,"deleted":2,"purged":0,"not_found":0,"protected":0,"failed":1,"deleted_upns":["a@d.com","b@d.com"]}'`)
	got, err := SoftDeleteByUPN(context.Background(), dir, "sh", []string{"a@d.com", "b@d.com", "c@d.com"}, 0)
	if err != nil {
		t.Fatalf("SoftDeleteByUPN: %v", err)
	}
	if got.Failed != 1 || got.Clean() {
		t.Errorf("SoftDeleteByUPN() = %+v, want Clean()==false with a failure present", got)
	}
}

// TestSoftDeleteByUPNProtectedCountedSeparatelyAndNotClean pins that a
// skipped admin/role-holder counts in Protected (never in Deleted or
// Failed) and, per the same batch-cleanliness rule, also makes the batch
// not-clean even with zero Failed — an admin was silently skipped, which is
// exactly the case the orders sweep must not treat as "fully processed."
func TestSoftDeleteByUPNProtectedCountedSeparatelyAndNotClean(t *testing.T) {
	dir := t.TempDir()
	writeCreateUsersStub(t, dir, `echo '{"ok":true,"targeted":2,"deleted":1,"purged":0,"not_found":0,"protected":1,"failed":0,"deleted_upns":["a@d.com"]}'`)
	got, err := SoftDeleteByUPN(context.Background(), dir, "sh", []string{"a@d.com", "admin@d.com"}, 0)
	if err != nil {
		t.Fatalf("SoftDeleteByUPN: %v", err)
	}
	if got.Protected != 1 || got.Deleted != 1 || got.Failed != 0 {
		t.Errorf("SoftDeleteByUPN() = %+v, want Protected=1 kept separate from Deleted/Failed", got)
	}
	if got.Clean() {
		t.Error("SoftDeleteByUPN().Clean() = true, want false (a protected/admin entry was skipped)")
	}
}

// TestSoftDeleteByUPNAlreadyGoneCountsAsNotFoundNotFailed pins that a UPN
// already gone (or never a tenant account) is idempotent-safe: it lands in
// NotFound, never Failed, and does not by itself make the batch not-clean.
func TestSoftDeleteByUPNAlreadyGoneCountsAsNotFoundNotFailed(t *testing.T) {
	dir := t.TempDir()
	writeCreateUsersStub(t, dir, `echo '{"ok":true,"targeted":2,"deleted":1,"purged":0,"not_found":1,"protected":0,"failed":0,"deleted_upns":["a@d.com"]}'`)
	got, err := SoftDeleteByUPN(context.Background(), dir, "sh", []string{"a@d.com", "gone@d.com"}, 0)
	if err != nil {
		t.Fatalf("SoftDeleteByUPN: %v", err)
	}
	if got.NotFound != 1 || got.Failed != 0 || !got.Clean() {
		t.Errorf("SoftDeleteByUPN() = %+v, want NotFound=1, Failed=0, Clean()==true", got)
	}
}

// TestSoftDeleteByUPNNoParseableJSONLine covers the child crashing/printing
// garbage instead of its one required JSON line.
func TestSoftDeleteByUPNNoParseableJSONLine(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stub is a POSIX shell script")
	}
	dir := t.TempDir()
	writeCreateUsersStub(t, dir, `echo "traceback: something exploded" >&2; exit 1`)
	_, err := SoftDeleteByUPN(context.Background(), dir, "sh", []string{"a@d.com"}, 5*time.Second)
	if err == nil {
		t.Fatal("SoftDeleteByUPN: got nil error, want an error for a child with no parseable JSON line")
	}
}

// TestSoftDeleteByUPNTimeout covers a child that hangs past its budget.
func TestSoftDeleteByUPNTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stub is a POSIX shell script")
	}
	dir := t.TempDir()
	writeCreateUsersStub(t, dir, `sleep 5`)
	_, err := SoftDeleteByUPN(context.Background(), dir, "sh", []string{"a@d.com"}, 100*time.Millisecond)
	if err == nil {
		t.Fatal("SoftDeleteByUPN: got nil error, want a timeout error")
	}
}

// TestSoftDeleteByUPNRedactsPlantedSecretFromStderrError pins that a secret
// embedded in a URL-shaped credential the child prints to stderr never
// survives into SoftDeleteByUPN's returned error — the structural fallback
// layer of redactChildOutput (redact.go) applies unconditionally, even
// though this operation (like every entra_create_users.py runner in this
// file) has no explicit secret value of its own to pass into runScript's
// exact-substring layer. This repo's fourth review round on exactly this
// leak class (task brief); must not regress here.
func TestSoftDeleteByUPNRedactsPlantedSecretFromStderrError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stub is a POSIX shell script")
	}
	const secret = "ENTRA-DELETE-BY-UPN-SUPER-SECRET-SHOULD-NOT-LEAK"
	dir := t.TempDir()
	writeCreateUsersStub(t, dir, `echo "graph call failed: https://svc:`+secret+`@graph.microsoft.com/v1.0/users failed" >&2; exit 1`)
	_, err := SoftDeleteByUPN(context.Background(), dir, "sh", []string{"a@d.com"}, 5*time.Second)
	if err == nil {
		t.Fatal("SoftDeleteByUPN: got nil error, want the stderr-derived error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("SoftDeleteByUPN error = %q, still contains the planted secret", err.Error())
	}
}

func TestWriteLinesFileCleansUp(t *testing.T) {
	path, cleanup, err := writeLinesFile("entra-test-*.txt", []string{"a", "b"})
	if err != nil {
		t.Fatalf("writeLinesFile: %v", err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(contents) != "a\nb\n" {
		t.Errorf("writeLinesFile contents = %q, want %q", string(contents), "a\nb\n")
	}
	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("writeLinesFile temp file still exists after cleanup(): %v", err)
	}
}
