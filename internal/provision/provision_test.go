package provision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"codezdev-bot/internal/subproc"
)

// Batch-driver tests, ported from tests/test_provision.py. Both seams (the
// browser logins and the pool posters) are faked, so nothing here launches a
// browser or touches the network — the same isolation Python gets by
// monkeypatching kp._run_login / kp.kiro_go_add_key.

// ── fakes ────────────────────────────────────────────────────────────────────

// fakeLogin is a LoginRunner whose two methods are supplied per test. A nil
// method fails loudly rather than silently succeeding, so a test that routes a
// line to the wrong flow is caught immediately.
type fakeLogin struct {
	idc  func(ctx context.Context, accountsFile string, index int, keyOutDir string, headless bool, stepDelay float64) (subproc.Result, error)
	m365 func(ctx context.Context, m365File string, index int, outDir, email string, headless bool, stepDelay float64) (subproc.Result, error)
}

func (f *fakeLogin) RunIDCLogin(ctx context.Context, accountsFile string, index int, keyOutDir string, headless bool, stepDelay float64) (subproc.Result, error) {
	if f.idc == nil {
		return subproc.Result{}, errors.New("unexpected idc login")
	}
	return f.idc(ctx, accountsFile, index, keyOutDir, headless, stepDelay)
}

func (f *fakeLogin) RunM365Login(ctx context.Context, m365File string, index int, outDir, email string, headless bool, stepDelay float64) (subproc.Result, error) {
	if f.m365 == nil {
		return subproc.Result{}, errors.New("unexpected m365 login")
	}
	return f.m365(ctx, m365File, index, outDir, email, headless, stepDelay)
}

// keyCall / accountCall record what reached the pool.
type keyCall struct {
	Key      string
	Nickname string
	Region   string
}

type accountCall struct {
	Account  json.RawMessage
	Nickname string
}

// fakePool records every add and replays a scripted response or error.
type fakePool struct {
	mu       sync.Mutex
	keys     []keyCall
	accounts []accountCall

	keyResp  *AddResponse
	keyErr   error
	acctResp *AddResponse
	acctErr  error
}

func (p *fakePool) AddKiroAPIKey(_ context.Context, kiroAPIKey, nickname, region string) (*AddResponse, error) {
	p.mu.Lock()
	p.keys = append(p.keys, keyCall{Key: kiroAPIKey, Nickname: nickname, Region: region})
	p.mu.Unlock()
	if p.keyErr != nil {
		return nil, p.keyErr
	}
	return p.keyResp, nil
}

func (p *fakePool) AddKiroAccount(_ context.Context, account json.RawMessage, nickname string) (*AddResponse, error) {
	p.mu.Lock()
	p.accounts = append(p.accounts, accountCall{Account: account, Nickname: nickname})
	p.mu.Unlock()
	if p.acctErr != nil {
		return nil, p.acctErr
	}
	return p.acctResp, nil
}

func (p *fakePool) nicknames() []string {
	out := []string{}
	for _, c := range p.keys {
		out = append(out, c.Nickname)
	}
	return out
}

// okAdd is the ordinary "fresh slot created" response.
func okAdd(region string) *AddResponse {
	return &AddResponse{Success: true, Added: []AddedSlot{{ID: "a1", Region: region}}}
}

const testKSK = "ksk_zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"

// writeKeyJSON writes the key file kiro-web-login.save_api_key would have
// written for `user`.
func writeKeyJSON(t *testing.T, keyOutDir, user, apiKey string) {
	t.Helper()
	if err := os.MkdirAll(keyOutDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body, _ := json.Marshal(map[string]any{
		"username": user, "key_name": "Kiro-Go", "api_key": apiKey, "created_unix": 1,
	})
	p := filepath.Join(keyOutDir, "kiro-api-key_"+safeUser(user)+".json")
	if err := os.WriteFile(p, body, 0o600); err != nil {
		t.Fatalf("write key json: %v", err)
	}
}

// idcLoginOK is the _fake_login stand-in (test_provision.py:50): resolve the
// account from the file by index, then write its key JSON.
func idcLoginOK(t *testing.T, apiKey string) func(context.Context, string, int, string, bool, float64) (subproc.Result, error) {
	return func(_ context.Context, accountsFile string, index int, keyOutDir string, _ bool, _ float64) (subproc.Result, error) {
		raw, err := os.ReadFile(accountsFile)
		if err != nil {
			return subproc.Result{}, err
		}
		accts := ParseIDCAccounts(string(raw))
		if index < 1 || index > len(accts) {
			return subproc.Result{}, fmt.Errorf("index %d out of range (%d accounts)", index, len(accts))
		}
		writeKeyJSON(t, keyOutDir, accts[index-1].Username, apiKey)
		return subproc.Result{}, nil
	}
}

// rewriteIDCSeed mirrors kiro-web-login.update_account_mfa_secret: rewrite the
// indexed line as username|password|start_url|mfa_secret.
func rewriteIDCSeed(accountsFile string, index int, seed string) error {
	raw, err := os.ReadFile(accountsFile)
	if err != nil {
		return err
	}
	lines := []string{}
	for _, l := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	if index < 1 || index > len(lines) {
		return fmt.Errorf("index %d out of range", index)
	}
	parts := strings.Split(lines[index-1], "|")
	if len(parts) == 3 {
		parts[2] = strings.TrimRight(parts[2], " \t")
		parts = append(parts, seed)
	} else {
		parts[3] = seed
	}
	lines[index-1] = strings.Join(parts, "|")
	return os.WriteFile(accountsFile, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
}

// m365LoginOK is the _fake_m365_login stand-in (test_provision.py:196). Because
// the bridge passes --email, that address (NOT the token's JWT claim) names the
// file and lands in the payload's `email`.
func m365LoginOK(t *testing.T, registerSeed string) func(context.Context, string, int, string, string, bool, float64) (subproc.Result, error) {
	return func(_ context.Context, m365File string, index int, outDir, email string, _ bool, _ float64) (subproc.Result, error) {
		if registerSeed != "" {
			// Mirror update_m365_secret: rewrite this line as email|password|secret.
			raw, err := os.ReadFile(m365File)
			if err != nil {
				return subproc.Result{}, err
			}
			lines := []string{}
			for _, l := range strings.Split(string(raw), "\n") {
				if strings.TrimSpace(l) != "" {
					lines = append(lines, l)
				}
			}
			parts := strings.Split(lines[index-1], "|")
			lines[index-1] = parts[0] + "|" + parts[1] + "|" + registerSeed
			if err := os.WriteFile(m365File, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
				return subproc.Result{}, err
			}
		}
		writeAccountJSON(t, outDir, email)
		return subproc.Result{}, nil
	}
}

func writeAccountJSON(t *testing.T, outDir, email string) {
	t.Helper()
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body, _ := json.Marshal(map[string]any{
		"id": "uuid-1", "accessToken": "eyJ" + strings.Repeat("a", 40), "refreshToken": "rt-1",
		"authMethod": "external_idp", "region": "eu-central-1", "enabled": true,
		"email": email, "provider": "AzureAD",
		"profileArn": "arn:aws:codewhisperer:eu-central-1:1:profile/X",
		"clientId":   "cid", "tokenEndpoint": "https://idp/token",
		"issuerUrl": "https://idp/", "scopes": "s offline_access",
	})
	p := filepath.Join(outDir, "kiro-go-account_"+safeComponent(email)+".json")
	if err := os.WriteFile(p, body, 0o600); err != nil {
		t.Fatalf("write account json: %v", err)
	}
}

// newTestProvisioner wires a Provisioner onto a throwaway archive root, with a
// silent logger so the salvage warning doesn't pollute test output.
func newTestProvisioner(t *testing.T, login LoginRunner, pool PoolClient) (*Provisioner, string) {
	t.Helper()
	archive := t.TempDir()
	p := New(Options{
		Login:       login,
		Pool:        pool,
		ArchiveRoot: archive,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return p, archive
}

func run(t *testing.T, p *Provisioner, text string, ro RunOptions) []Result {
	t.Helper()
	res, err := p.Provision(context.Background(), text, ro)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	return res
}

// ── idc flow ─────────────────────────────────────────────────────────────────

func TestIDCAddedAndArchived(t *testing.T) {
	pool := &fakePool{keyResp: okAdd("us-east-1")}
	p, _ := newTestProvisioner(t, &fakeLogin{idc: idcLoginOK(t, testKSK)}, pool)

	res := run(t, p, idcText, RunOptions{Headless: true})
	if len(res) != 1 {
		t.Fatalf("got %d results, want 1", len(res))
	}
	r := res[0]
	if r.Status != StatusAdded {
		t.Errorf("status = %s (%s), want added", r.Status, r.Detail)
	}
	if !strings.HasPrefix(r.KeyMasked, "ksk_") || strings.Contains(r.KeyMasked, strings.Repeat("z", 32)) {
		t.Errorf("KeyMasked = %q, want a masked key", r.KeyMasked)
	}
	if r.Archived == "" || !isFile(r.Archived) {
		t.Errorf("key JSON not archived after success (Archived=%q)", r.Archived)
	}
	if got := pool.nicknames(); len(got) != 1 || got[0] != "tg-pool-user.one" {
		t.Errorf("nicknames = %v, want the key tied to its username", got)
	}
	// The RAW key is what reaches the pool, only the report is masked.
	if pool.keys[0].Key != testKSK {
		t.Errorf("pool got key %q, want the raw key", pool.keys[0].Key)
	}
}

func TestIDCDuplicateStillArchives(t *testing.T) {
	pool := &fakePool{keyResp: &AddResponse{
		Success: true,
		Added:   []AddedSlot{{ID: "x", Region: "us-east-1", Duplicate: true}},
	}}
	p, _ := newTestProvisioner(t, &fakeLogin{idc: idcLoginOK(t, testKSK)}, pool)

	r := run(t, p, idcText, RunOptions{Headless: true})[0]
	if r.Status != StatusDuplicate {
		t.Errorf("status = %s, want duplicate", r.Status)
	}
	if !strings.Contains(r.Detail, "already in pool") {
		t.Errorf("detail = %q, want it to say already in pool", r.Detail)
	}
	// The credential IS in the pool, so the local copy must not linger.
	if r.Archived == "" {
		t.Error("duplicate must still archive (the key is in the pool)")
	}
}

func TestIDCPoolRejectedIsNotArchived(t *testing.T) {
	pool := &fakePool{keyErr: &PoolError{
		Endpoint: "add_kiro_api_key", StatusCode: 400,
		Message: "kiroApiKey must start with ksk_",
	}}
	p, _ := newTestProvisioner(t, &fakeLogin{idc: idcLoginOK(t, testKSK)}, pool)

	r := run(t, p, idcText, RunOptions{Headless: true})[0]
	if r.Status != StatusPoolRejected {
		t.Errorf("status = %s, want pool_rejected", r.Status)
	}
	// A failed add leaves the JSON in place so the operator can retry without
	// re-running a multi-minute browser login.
	if r.Archived != "" {
		t.Errorf("failed add archived the credential (%q) — it must stay retryable", r.Archived)
	}
	if !strings.Contains(r.Detail, "must start with ksk_") {
		t.Errorf("detail = %q, want the server's error text", r.Detail)
	}
}

func TestIDCLoginFailedWhenNoKeyProduced(t *testing.T) {
	// Login "succeeds" (exit 0) but writes NO key JSON.
	login := &fakeLogin{idc: func(context.Context, string, int, string, bool, float64) (subproc.Result, error) {
		return subproc.Result{Stderr: "boom"}, nil
	}}
	p, _ := newTestProvisioner(t, login, &fakePool{keyResp: okAdd("us-east-1")})

	r := run(t, p, idcText, RunOptions{Headless: true})[0]
	if r.Status != StatusLoginFailed {
		t.Errorf("status = %s, want login_failed", r.Status)
	}
	if !strings.Contains(r.Detail, "no API key produced") || !strings.Contains(r.Detail, "boom") {
		t.Errorf("detail = %q, want the reason plus the stderr tail", r.Detail)
	}
}

func TestIDCNonKSKKeyIsNeverPosted(t *testing.T) {
	pool := &fakePool{keyResp: okAdd("us-east-1")}
	p, _ := newTestProvisioner(t, &fakeLogin{idc: idcLoginOK(t, "notaksk_key")}, pool)

	r := run(t, p, idcText, RunOptions{Headless: true})[0]
	if r.Status != StatusNoKey {
		t.Errorf("status = %s, want no_key", r.Status)
	}
	if len(pool.keys) != 0 {
		t.Errorf("a non-ksk_ key was sent to the pool: %v", pool.keys)
	}
}

func TestIDCDryRunArchivesButNeverPosts(t *testing.T) {
	pool := &fakePool{keyResp: okAdd("us-east-1")}
	p, _ := newTestProvisioner(t, &fakeLogin{idc: idcLoginOK(t, testKSK)}, pool)

	r := run(t, p, idcText, RunOptions{Headless: true, DryRun: true})[0]
	if r.Status != StatusDryRun {
		t.Errorf("status = %s, want dry_run", r.Status)
	}
	if len(pool.keys) != 0 {
		t.Error("dry run posted to the pool")
	}
	// Still archived, so the working dir doesn't retain the live key.
	if r.Archived == "" || !isFile(r.Archived) {
		t.Error("dry run must still archive the local key out of the working dir")
	}
}

// ── idc MFA-seed capture (heavily test-pinned) ───────────────────────────────

func TestIDCNewMFASeedSurfacedAndPersisted(t *testing.T) {
	// An AWS IdC tenant whose policy mandates MFA makes kiro-web-login.py enrol
	// a TOTP device on the first sign-in and write the seed into field 4 of the
	// TEMP accounts file — which is wiped when the run ends. If the seed isn't
	// captured here it is gone, and the account is MFA-locked out of every
	// future sign-in with no recovery path.
	const seed = "IXLJE2K3G5WNKQK7NWGR3ZYXOJO56RLK"
	inner := idcLoginOK(t, testKSK)
	login := &fakeLogin{idc: func(ctx context.Context, accountsFile string, index int, keyOutDir string, h bool, sd float64) (subproc.Result, error) {
		if err := rewriteIDCSeed(accountsFile, index, seed); err != nil {
			return subproc.Result{}, err
		}
		return inner(ctx, accountsFile, index, keyOutDir, h, sd)
	}}
	p, _ := newTestProvisioner(t, login, &fakePool{keyResp: okAdd("us-east-1")})

	r := run(t, p, idcText, RunOptions{Headless: true})[0]
	if r.NewMFA != seed {
		t.Errorf("NewMFA = %q, want the enrolled seed", r.NewMFA)
	}
	if r.Status != StatusAdded {
		t.Errorf("status = %s, want MFA enrollment to still yield a pooled key", r.Status)
	}
	assertRewriteRecord(t, r.RewriteSaved, "user.one", seed)
}

func TestIDCSeedCapturedEvenWhenLoginTimesOut(t *testing.T) {
	// REGRESSION (test_provision.py:372): capture once ran only on the happy
	// path. kiro-web-login.py persists a minted seed BEFORE binding the device,
	// so a login that enrols and then hangs has the seed on disk in a temp file
	// about to be wiped. Losing it there bricks the account.
	const seed = "OKJMNSE33IJGFUM2QPO5575R4ZZIO2JC"
	login := &fakeLogin{idc: func(_ context.Context, accountsFile string, index int, _ string, _ bool, _ float64) (subproc.Result, error) {
		if err := rewriteIDCSeed(accountsFile, index, seed); err != nil {
			return subproc.Result{}, err
		}
		return subproc.Result{TimedOut: true}, fmt.Errorf("kiro-web-login.py: %w after 6m0s", subproc.ErrTimeout)
	}}
	p, _ := newTestProvisioner(t, login, &fakePool{keyResp: okAdd("us-east-1")})

	r := run(t, p, idcText, RunOptions{Headless: true})[0]
	if r.Status != StatusLoginFailed {
		t.Errorf("status = %s, want login_failed", r.Status)
	}
	if !strings.Contains(r.Detail, "timed out") {
		t.Errorf("detail = %q, want it to name the timeout", r.Detail)
	}
	if r.NewMFA != seed {
		t.Errorf("NewMFA = %q, want the seed enrolled before the timeout", r.NewMFA)
	}
	assertRewriteRecord(t, r.RewriteSaved, "user.one", seed)
}

func TestIDCTimeoutAfterKeyCaptureIsSalvaged(t *testing.T) {
	// The other half of kiro_provision.py:744-752: a kill landing AFTER the key
	// JSON was written leaves a perfectly good key on disk. Returning early
	// would strand a live ksk_ that Kiro gives us no way to revoke.
	inner := idcLoginOK(t, testKSK)
	login := &fakeLogin{idc: func(ctx context.Context, accountsFile string, index int, keyOutDir string, h bool, sd float64) (subproc.Result, error) {
		if _, err := inner(ctx, accountsFile, index, keyOutDir, h, sd); err != nil {
			return subproc.Result{}, err
		}
		return subproc.Result{TimedOut: true}, fmt.Errorf("%w after 6m0s", subproc.ErrTimeout)
	}}
	pool := &fakePool{keyResp: okAdd("us-east-1")}
	p, _ := newTestProvisioner(t, login, pool)

	r := run(t, p, idcText, RunOptions{Headless: true})[0]
	if r.Status != StatusAdded {
		t.Errorf("status = %s (%s), want the captured key salvaged into the pool", r.Status, r.Detail)
	}
	if len(pool.keys) != 1 {
		t.Errorf("pool calls = %d, want the salvaged key posted once", len(pool.keys))
	}
}

func TestIDCPreexistingMFANotReportedAsNew(t *testing.T) {
	// A 4-field line whose seed the login leaves alone must not be re-reported
	// as newly enrolled — that would spam the operator with a "save this!"
	// notice on every routine re-provision of an already-enrolled account.
	p, _ := newTestProvisioner(t, &fakeLogin{idc: idcLoginOK(t, testKSK)}, &fakePool{keyResp: okAdd("us-east-1")})

	r := run(t, p, "user.one|pw123|https://x.portal.aws/|OLDSEEDOLDSEEDOLDSEEDOLDSEEDOLDS",
		RunOptions{Headless: true})[0]
	if r.NewMFA != "" {
		t.Errorf("NewMFA = %q, want an unchanged seed not reported as new", r.NewMFA)
	}
	if r.Status != StatusAdded {
		t.Errorf("status = %s, want a 4-field line to provision normally", r.Status)
	}
}

func TestIDCTempFileCarriesStoredSeedToTheLogin(t *testing.T) {
	// REGRESSION (test_provision.py:338): the temp idc file was once rebuilt as
	// 3 fields, silently dropping a stored seed. kiro-web-login.py answers the
	// per-sign-in MFA challenge from field 4, so an enrolled account
	// re-provisioned through the pool would strand at manual_action_required.
	var seen string
	inner := idcLoginOK(t, testKSK)
	login := &fakeLogin{idc: func(ctx context.Context, accountsFile string, index int, keyOutDir string, h bool, sd float64) (subproc.Result, error) {
		raw, err := os.ReadFile(accountsFile)
		if err != nil {
			return subproc.Result{}, err
		}
		lines := []string{}
		for _, l := range strings.Split(string(raw), "\n") {
			if strings.TrimSpace(l) != "" {
				lines = append(lines, l)
			}
		}
		seen = lines[index-1]
		return inner(ctx, accountsFile, index, keyOutDir, h, sd)
	}}
	p, _ := newTestProvisioner(t, login, &fakePool{keyResp: okAdd("us-east-1")})

	const seed = "IXLJE2K3G5WNKQK7NWGR3ZYXOJO56RLK"
	run(t, p, "user.one|pw123|https://x.portal.aws/|"+seed, RunOptions{Headless: true})
	parts := strings.Split(seen, "|")
	if len(parts) < 4 || parts[3] != seed {
		t.Errorf("temp idc line = %q, want the stored seed as field 4", seen)
	}

	// An account with NO seed must stay 3-field: an empty 4th field would be
	// read back as a seed of "" and is just noise.
	seen = ""
	run(t, p, idcText, RunOptions{Headless: true})
	if strings.Count(seen, "|") != 2 {
		t.Errorf("seedless idc line = %q, want it to stay 3-field", seen)
	}
}

// ── m365 flow ────────────────────────────────────────────────────────────────

func TestM365AddedAndArchived(t *testing.T) {
	pool := &fakePool{acctResp: okAdd("eu-central-1")}
	p, _ := newTestProvisioner(t, &fakeLogin{m365: m365LoginOK(t, "")}, pool)

	r := run(t, p, m365Text, RunOptions{Headless: true})[0]
	if r.Status != StatusAdded {
		t.Errorf("status = %s (%s), want added", r.Status, r.Detail)
	}
	if r.Flow != FlowM365 {
		t.Errorf("flow = %s, want m365", r.Flow)
	}
	if r.Archived == "" || !isFile(r.Archived) {
		t.Error("account JSON not archived after success")
	}
	if len(pool.accounts) != 1 || pool.accounts[0].Nickname != "tg-pool-m365.user@corp.example" {
		t.Errorf("nickname = %v, want the pool account tied to its source email", pool.accounts)
	}
	// The whole account JSON must reach the pool VERBATIM: without the refresh
	// material the ~1h access token would expire and never renew.
	var fwd map[string]any
	if err := json.Unmarshal(pool.accounts[0].Account, &fwd); err != nil {
		t.Fatalf("forwarded account is not JSON: %v", err)
	}
	for k, want := range map[string]string{
		"refreshToken": "rt-1", "tokenEndpoint": "https://idp/token",
		"clientId": "cid", "scopes": "s offline_access",
		"profileArn": "arn:aws:codewhisperer:eu-central-1:1:profile/X",
	} {
		if fwd[k] != want {
			t.Errorf("forwarded %s = %v, want %q", k, fwd[k], want)
		}
	}
}

func TestM365NewMFASeedSurfacedAndPersisted(t *testing.T) {
	pool := &fakePool{acctResp: okAdd("eu-central-1")}
	p, _ := newTestProvisioner(t, &fakeLogin{m365: m365LoginOK(t, "JBSWY3DPEHPK3PXP")}, pool)

	r := run(t, p, m365Text, RunOptions{Headless: true})[0]
	if r.NewMFA != "JBSWY3DPEHPK3PXP" {
		t.Errorf("NewMFA = %q, want the newly registered seed", r.NewMFA)
	}
	assertRewriteRecord(t, r.RewriteSaved, "m365.user@corp.example", "JBSWY3DPEHPK3PXP")
}

func TestM365MFASeedRecoveredFromStderr(t *testing.T) {
	// If the helper enrolls a seed but FAILS to write it back, the seed exists
	// only on its stderr — which a successful login otherwise discards. The
	// upstream account's MFA is already changed at that point, so missing it
	// leaves the account permanently locked out.
	inner := m365LoginOK(t, "")
	login := &fakeLogin{m365: func(ctx context.Context, f string, i int, outDir, email string, h bool, sd float64) (subproc.Result, error) {
		if _, err := inner(ctx, f, i, outDir, email, h, sd); err != nil {
			return subproc.Result{}, err
		}
		return subproc.Result{Stderr: "  !! WARNING: could not persist MFA secret: disk full\n" +
			"  !! MFA secret for " + email + ": RECOVERED4SEED\n"}, nil
	}}
	p, _ := newTestProvisioner(t, login, &fakePool{acctResp: okAdd("eu-central-1")})

	r := run(t, p, m365Text, RunOptions{Headless: true})[0]
	if r.Status != StatusAdded {
		t.Errorf("status = %s, want the login to still count as added", r.Status)
	}
	if r.NewMFA != "RECOVERED4SEED" {
		t.Errorf("NewMFA = %q, want the seed recovered from stderr", r.NewMFA)
	}
	assertRewriteRecord(t, r.RewriteSaved, "m365.user@corp.example", "RECOVERED4SEED")
}

func TestM365StderrSeedForAnotherAccountIgnored(t *testing.T) {
	// The stderr scrape must be identity-checked: attributing another account's
	// seed would report the wrong credential for this one.
	inner := m365LoginOK(t, "")
	login := &fakeLogin{m365: func(ctx context.Context, f string, i int, outDir, email string, h bool, sd float64) (subproc.Result, error) {
		if _, err := inner(ctx, f, i, outDir, email, h, sd); err != nil {
			return subproc.Result{}, err
		}
		return subproc.Result{Stderr: "  !! MFA secret for someone.else@corp.example: NOTMYSEED\n"}, nil
	}}
	p, _ := newTestProvisioner(t, login, &fakePool{acctResp: okAdd("eu-central-1")})

	r := run(t, p, m365Text, RunOptions{Headless: true})[0]
	if r.NewMFA != "" {
		t.Errorf("NewMFA = %q, want another account's seed never attributed here", r.NewMFA)
	}
}

func TestM365PreexistingMFANotReportedAsNew(t *testing.T) {
	p, _ := newTestProvisioner(t, &fakeLogin{m365: m365LoginOK(t, "")}, &fakePool{acctResp: okAdd("eu-central-1")})
	r := run(t, p, "m365.user@corp.example|pw456|OLDSEED", RunOptions{Headless: true})[0]
	if r.NewMFA != "" {
		t.Errorf("NewMFA = %q, want an unchanged seed not reported as new", r.NewMFA)
	}
}

func TestM365RepairedSlotIsNotAnNoOp(t *testing.T) {
	// Re-uploading a dead account is the operator's REPAIR: the pool revives the
	// slot with the fresh tokens and returns repaired=true. Rendering that as a
	// plain "already in pool" would tell the admin their fix did nothing.
	pool := &fakePool{acctResp: &AddResponse{
		Success: true, Enabled: true,
		Added: []AddedSlot{{ID: "a1", Region: "eu-central-1", Duplicate: true, Repaired: true}},
	}}
	p, _ := newTestProvisioner(t, &fakeLogin{m365: m365LoginOK(t, "")}, pool)

	r := run(t, p, m365Text, RunOptions{Headless: true})[0]
	if r.Status != StatusRepaired {
		t.Errorf("status = %s, want repaired", r.Status)
	}
	if !strings.Contains(r.Detail, "revived") {
		t.Errorf("detail = %q, want it to say revived", r.Detail)
	}
	if r.Archived == "" {
		t.Error("a repaired slot must still archive (the credential is in the pool)")
	}
}

func TestM365PoolRejectedIsNotArchived(t *testing.T) {
	pool := &fakePool{acctErr: &PoolError{Endpoint: "add_kiro_account", StatusCode: 400, Message: "accessToken is required"}}
	p, _ := newTestProvisioner(t, &fakeLogin{m365: m365LoginOK(t, "")}, pool)

	r := run(t, p, m365Text, RunOptions{Headless: true})[0]
	if r.Status != StatusPoolRejected {
		t.Errorf("status = %s, want pool_rejected", r.Status)
	}
	if r.Archived != "" {
		t.Error("a failed m365 add must not archive (it stays retryable)")
	}
}

func TestM365NeverAttributesAnotherAccountsCredential(t *testing.T) {
	// out_dir is SHARED across the batch and a failed add deliberately leaves
	// its JSON behind (retryable). If a later account's login produces nothing,
	// it must report login_failed — never inherit the leftover file.
	// Attributing account A's live token to account B would add A twice and
	// silently drop B.
	inner := m365LoginOK(t, "")
	login := &fakeLogin{m365: func(ctx context.Context, f string, i int, outDir, email string, h bool, sd float64) (subproc.Result, error) {
		if i == 1 {
			return inner(ctx, f, i, outDir, email, h, sd)
		}
		return subproc.Result{ExitCode: 1, Stderr: "ERROR: sign-in failed"}, nil
	}}
	// Reject the add so account 1's JSON is LEFT in the shared out dir.
	pool := &fakePool{acctErr: errors.New("pool down")}
	p, _ := newTestProvisioner(t, login, pool)

	res := run(t, p, "a@corp.example|p1\nb@corp.example|p2\n", RunOptions{Headless: true})
	if res[0].Status != StatusPoolRejected {
		t.Errorf("first account status = %s, want pool_rejected", res[0].Status)
	}
	if res[1].Status != StatusLoginFailed {
		t.Errorf("second account status = %s, want login_failed (it must NOT inherit the leftover JSON)", res[1].Status)
	}
}

func TestM365LoginFailedSurfacesStderrTail(t *testing.T) {
	login := &fakeLogin{m365: func(context.Context, string, int, string, string, bool, float64) (subproc.Result, error) {
		return subproc.Result{ExitCode: 1, Stderr: "ERROR: failed to resolve profile ARN"}, nil
	}}
	p, _ := newTestProvisioner(t, login, &fakePool{acctResp: okAdd("eu-central-1")})

	r := run(t, p, m365Text, RunOptions{Headless: true})[0]
	if r.Status != StatusLoginFailed {
		t.Errorf("status = %s, want login_failed", r.Status)
	}
	if !strings.Contains(r.Detail, "profile ARN") {
		t.Errorf("detail = %q, want the stderr tail surfaced for triage", r.Detail)
	}
}

func TestM365DryRunArchivesButNeverPosts(t *testing.T) {
	pool := &fakePool{acctResp: okAdd("eu-central-1")}
	p, _ := newTestProvisioner(t, &fakeLogin{m365: m365LoginOK(t, "")}, pool)

	r := run(t, p, m365Text, RunOptions{Headless: true, DryRun: true})[0]
	if r.Status != StatusDryRun {
		t.Errorf("status = %s, want dry_run", r.Status)
	}
	if len(pool.accounts) != 0 {
		t.Error("m365 dry run posted to the pool")
	}
	if r.Archived == "" {
		t.Error("m365 dry run must still archive the refresh token out of the working dir")
	}
	if !strings.Contains(r.Detail, "external_idp") || !strings.Contains(r.Detail, "eu-central-1") {
		t.Errorf("detail = %q, want it to describe the account it would add", r.Detail)
	}
}

// ── rawkey flow ──────────────────────────────────────────────────────────────

func TestRawKeyAddedWithoutLogin(t *testing.T) {
	pool := &fakePool{keyResp: okAdd("us-east-1")}
	// No login methods at all: a rawkey batch must never touch the browser
	// seam, which is why a keys-only paste works on a host with no browser
	// stack installed.
	p, _ := newTestProvisioner(t, &fakeLogin{}, pool)

	r := run(t, p, testKSK, RunOptions{Headless: true})[0]
	if r.Status != StatusAdded {
		t.Errorf("status = %s (%s), want added", r.Status, r.Detail)
	}
	if r.Flow != FlowRawKey {
		t.Errorf("flow = %s, want rawkey", r.Flow)
	}
	// SECURITY: the reported identity must be the MASKED key, and the nickname
	// with it — the raw key reaches the pool request and nothing else.
	if r.Username == testKSK || !strings.Contains(r.Username, "…") {
		t.Errorf("Username = %q, want the masked key", r.Username)
	}
	if strings.Contains(pool.keys[0].Nickname, strings.Repeat("z", 32)) {
		t.Errorf("nickname leaked the raw key: %q", pool.keys[0].Nickname)
	}
	if pool.keys[0].Key != testKSK {
		t.Errorf("pool got %q, want the raw key in the request itself", pool.keys[0].Key)
	}
	// Nothing was written to disk for a pasted key, so nothing is archived.
	if r.Archived != "" {
		t.Errorf("rawkey archived %q, want nothing", r.Archived)
	}
}

func TestRawKeyDryRunNeverPosts(t *testing.T) {
	pool := &fakePool{keyResp: okAdd("us-east-1")}
	p, _ := newTestProvisioner(t, &fakeLogin{}, pool)

	r := run(t, p, testKSK, RunOptions{DryRun: true})[0]
	if r.Status != StatusDryRun || len(pool.keys) != 0 {
		t.Errorf("status = %s, pool calls = %d; want dry_run with no posts", r.Status, len(pool.keys))
	}
	if strings.Contains(r.Detail, strings.Repeat("z", 32)) {
		t.Errorf("dry-run detail leaked the raw key: %q", r.Detail)
	}
}

// ── mixed batches, indexing, isolation ───────────────────────────────────────

func TestMixedBatchRoutesEachLine(t *testing.T) {
	// The headline behaviour: one upload, all flows, each line to its own login
	// script and its own admin endpoint, results in file order.
	pool := &fakePool{keyResp: okAdd("us-east-1"), acctResp: okAdd("eu-central-1")}
	login := &fakeLogin{idc: idcLoginOK(t, testKSK), m365: m365LoginOK(t, "")}
	p, _ := newTestProvisioner(t, login, pool)

	mixed := "u1|p1|https://a.aws/\na@corp.example|p2\nu3|p3|https://c.aws/\n"
	res := run(t, p, mixed, RunOptions{Headless: true})
	if len(res) != 3 {
		t.Fatalf("got %d results, want 3", len(res))
	}
	for _, r := range res {
		if r.Status != StatusAdded {
			t.Errorf("%s: status = %s (%s)", r.Username, r.Status, r.Detail)
		}
	}
	if res[0].Flow != FlowIDC || res[1].Flow != FlowM365 || res[2].Flow != FlowIDC {
		t.Errorf("flows = %v, want results to keep file order + flow", []Flow{res[0].Flow, res[1].Flow, res[2].Flow})
	}
	if got := pool.nicknames(); len(got) != 2 || got[0] != "tg-pool-u1" || got[1] != "tg-pool-u3" {
		t.Errorf("add_kiro_api_key nicknames = %v, want only the idc lines", got)
	}
	if len(pool.accounts) != 1 || pool.accounts[0].Nickname != "tg-pool-a@corp.example" {
		t.Errorf("add_kiro_account calls = %v, want only the m365 line", pool.accounts)
	}
}

func TestFlowIndexIsPerFlow(t *testing.T) {
	// Each login script numbers --index over only the lines IT parses, so the
	// two flows must get separate files with independent 1-based index spaces.
	// A shared file would desynchronize them and log in the WRONG account.
	type seenRow struct {
		flow  Flow
		index int
		user  string
	}
	var seen []seenRow
	firstField := func(path string, index int) string {
		raw, _ := os.ReadFile(path)
		lines := []string{}
		for _, l := range strings.Split(string(raw), "\n") {
			if strings.TrimSpace(l) != "" {
				lines = append(lines, l)
			}
		}
		if index < 1 || index > len(lines) {
			return "<out of range>"
		}
		return strings.Split(lines[index-1], "|")[0]
	}
	innerIDC, innerM365 := idcLoginOK(t, testKSK), m365LoginOK(t, "")
	login := &fakeLogin{
		idc: func(ctx context.Context, f string, i int, dir string, h bool, sd float64) (subproc.Result, error) {
			seen = append(seen, seenRow{FlowIDC, i, firstField(f, i)})
			return innerIDC(ctx, f, i, dir, h, sd)
		},
		m365: func(ctx context.Context, f string, i int, dir, email string, h bool, sd float64) (subproc.Result, error) {
			seen = append(seen, seenRow{FlowM365, i, firstField(f, i)})
			return innerM365(ctx, f, i, dir, email, h, sd)
		},
	}
	p, _ := newTestProvisioner(t, login, &fakePool{keyResp: okAdd("us-east-1"), acctResp: okAdd("eu-central-1")})

	mixed := "x@corp.example|p1\n" + // m365 #1
		"u1|p2|https://a.aws/\n" + // idc  #1
		"y@corp.example|p3\n" + // m365 #2
		"u2|p4|https://b.aws/\n" // idc  #2
	run(t, p, mixed, RunOptions{Headless: true})

	want := []seenRow{
		{FlowM365, 1, "x@corp.example"},
		{FlowIDC, 1, "u1"},
		{FlowM365, 2, "y@corp.example"},
		{FlowIDC, 2, "u2"},
	}
	if len(seen) != len(want) {
		t.Fatalf("saw %d logins, want %d: %+v", len(seen), len(want), seen)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Errorf("login %d = %+v, want %+v (each flow must index its own file)", i, seen[i], want[i])
		}
	}
}

func TestPerAccountErrorIsolation(t *testing.T) {
	// A per-account failure must never abort the remaining accounts — including
	// a panic, which Python's bare `except Exception` covers.
	calls := 0
	inner := idcLoginOK(t, testKSK)
	login := &fakeLogin{idc: func(ctx context.Context, f string, i int, dir string, h bool, sd float64) (subproc.Result, error) {
		calls++
		if i == 2 {
			panic("simulated per-account bug")
		}
		return inner(ctx, f, i, dir, h, sd)
	}}
	p, _ := newTestProvisioner(t, login, &fakePool{keyResp: okAdd("us-east-1")})

	text := "u1|p1|https://a.aws/\nu2|p2|https://b.aws/\nu3|p3|https://c.aws/\n"
	res := run(t, p, text, RunOptions{Headless: true})
	if len(res) != 3 {
		t.Fatalf("got %d results, want all 3 accounts reported", len(res))
	}
	if res[0].Status != StatusAdded || res[2].Status != StatusAdded {
		t.Errorf("neighbours were affected: %s / %s", res[0].Status, res[2].Status)
	}
	if res[1].Status != StatusError || !strings.Contains(res[1].Detail, "simulated per-account bug") {
		t.Errorf("failing account = %s / %q, want an isolated error", res[1].Status, res[1].Detail)
	}
	if calls != 3 {
		t.Errorf("login called %d times, want the batch to continue past the failure", calls)
	}
}

func TestProgressCallbackStreamsAndIsIsolated(t *testing.T) {
	type prog struct {
		idx, total int
		username   string
		status     Status
	}
	var got []prog
	pool := &fakePool{keyResp: okAdd("us-east-1")}
	p, _ := newTestProvisioner(t, &fakeLogin{idc: idcLoginOK(t, testKSK)}, pool)

	text := "u1|p1|https://a.aws/\nu2|p2|https://b.aws/\n"
	run(t, p, text, RunOptions{Headless: true, Progress: func(i, n int, u string, s Status, _ string) {
		got = append(got, prog{i, n, u, s})
	}})
	want := []prog{{1, 2, "u1", StatusAdded}, {2, 2, "u2", StatusAdded}}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("progress = %+v, want %+v", got, want)
	}

	// A panicking callback is best-effort and must not take the batch down.
	res := run(t, p, text, RunOptions{Headless: true, Progress: func(int, int, string, Status, string) {
		panic("callback exploded")
	}})
	if len(res) != 2 {
		t.Errorf("a panicking progress callback broke the batch (%d results)", len(res))
	}
}

func TestProgressReportsMaskedRawKey(t *testing.T) {
	// The driver reports Result.Username, not the account value, precisely so a
	// raw key never reaches a progress line.
	var seen string
	p, _ := newTestProvisioner(t, &fakeLogin{}, &fakePool{keyResp: okAdd("us-east-1")})
	run(t, p, testKSK, RunOptions{Progress: func(_, _ int, u string, _ Status, _ string) { seen = u }})
	if seen == testKSK || !strings.Contains(seen, "…") {
		t.Errorf("progress username = %q, want the masked key", seen)
	}
}

func TestSingleFlight(t *testing.T) {
	// Only ONE batch may run at a time: the m365 OAuth callback binds a fixed
	// loopback port that cannot move, and a second batch would also mean a
	// second browser fleet on the host.
	release := make(chan struct{})
	entered := make(chan struct{})
	inner := idcLoginOK(t, testKSK)
	login := &fakeLogin{idc: func(ctx context.Context, f string, i int, dir string, h bool, sd float64) (subproc.Result, error) {
		close(entered)
		<-release
		return inner(ctx, f, i, dir, h, sd)
	}}
	p, _ := newTestProvisioner(t, login, &fakePool{keyResp: okAdd("us-east-1")})

	done := make(chan error, 1)
	go func() {
		_, err := p.Provision(context.Background(), idcText, RunOptions{Headless: true})
		done <- err
	}()
	<-entered

	if _, err := p.Provision(context.Background(), idcText, RunOptions{Headless: true}); !errors.Is(err, ErrBusy) {
		t.Errorf("concurrent Provision err = %v, want ErrBusy", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("first batch: %v", err)
	}
	// The lock must be RELEASED afterwards, or the feature would wedge
	// permanently "already running" until the next restart.
	if _, err := p.Provision(context.Background(), idcText, RunOptions{Headless: true}); err != nil {
		t.Errorf("lock not released after the batch: %v", err)
	}
}

func TestEmptyTextTakesNoLockAndReturnsNothing(t *testing.T) {
	p, archive := newTestProvisioner(t, &fakeLogin{}, &fakePool{})
	res, err := p.Provision(context.Background(), "Accounts: 0\n\n", RunOptions{})
	if err != nil || len(res) != 0 {
		t.Fatalf("got (%v, %v), want no results and no error", res, err)
	}
	// No working dir, no run tag, nothing in the archive.
	entries, _ := os.ReadDir(archive)
	if len(entries) != 0 {
		t.Errorf("an empty batch touched the archive root: %v", entries)
	}
}

func TestWorkingDirIsWipedAfterTheBatch(t *testing.T) {
	// The working dir holds the plaintext accounts files (usernames, passwords,
	// TOTP seeds) and any unarchived credential — it MUST NOT survive the run.
	var workDir string
	inner := idcLoginOK(t, testKSK)
	login := &fakeLogin{idc: func(ctx context.Context, f string, i int, dir string, h bool, sd float64) (subproc.Result, error) {
		workDir = filepath.Dir(f)
		// Prove the accounts file was created 0600 while it existed.
		if fi, err := os.Stat(f); err == nil && fi.Mode().Perm() != 0o600 {
			t.Errorf("accounts file mode = %v, want 0600", fi.Mode().Perm())
		}
		if fi, err := os.Stat(workDir); err == nil && fi.Mode().Perm() != 0o700 {
			t.Errorf("working dir mode = %v, want 0700", fi.Mode().Perm())
		}
		return inner(ctx, f, i, dir, h, sd)
	}}
	// Reject the add so a credential JSON is deliberately LEFT unarchived.
	p, _ := newTestProvisioner(t, login, &fakePool{keyErr: errors.New("pool down")})

	run(t, p, idcText, RunOptions{Headless: true})
	if workDir == "" {
		t.Fatal("login never ran")
	}
	if _, err := os.Stat(workDir); !os.IsNotExist(err) {
		t.Errorf("working dir %s survived the batch (err=%v)", workDir, err)
	}
}

func TestNicknamePrefixAndRegionForwarded(t *testing.T) {
	pool := &fakePool{keyResp: okAdd("ap-southeast-1")}
	p, _ := newTestProvisioner(t, &fakeLogin{idc: idcLoginOK(t, testKSK)}, pool)

	run(t, p, idcText, RunOptions{Headless: true, NicknamePrefix: "restock", Region: "ap-southeast-1"})
	if pool.keys[0].Nickname != "restock-user.one" {
		t.Errorf("nickname = %q, want the custom prefix", pool.keys[0].Nickname)
	}
	if pool.keys[0].Region != "ap-southeast-1" {
		t.Errorf("region = %q, want it forwarded", pool.keys[0].Region)
	}
}

func TestOrderFileProvisionsEndToEnd(t *testing.T) {
	// The full path: an uploaded order .txt runs each expanded idc account
	// through the real batch driver and adds its key to the pool, exactly as a
	// one-line idc paste would.
	pool := &fakePool{keyResp: okAdd("us-east-1")}
	p, _ := newTestProvisioner(t, &fakeLogin{idc: idcLoginOK(t, testKSK)}, pool)

	res := run(t, p, orderText, RunOptions{Headless: true})
	if len(res) != 2 {
		t.Fatalf("got %d results, want both order accounts", len(res))
	}
	for _, r := range res {
		if r.Status != StatusAdded {
			t.Errorf("%s: status = %s (%s)", r.Username, r.Status, r.Detail)
		}
	}
	want := []string{"tg-pool-camilo.radelic@duke-kr.win", "tg-pool-dana.two@duke-kr.win"}
	got := pool.nicknames()
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("nicknames = %v, want %v", got, want)
	}
}

// ── summarizeAdd / tailDetail units ──────────────────────────────────────────

func TestSummarizeAdd(t *testing.T) {
	cases := []struct {
		name       string
		resp       *AddResponse
		wantStatus Status
		wantOK     bool
		detailHas  string
	}{
		{"nil response", nil, StatusPoolRejected, false, "unexpected add response shape"},
		{"empty added", &AddResponse{Success: true}, StatusPoolRejected, false, "pool accepted nothing"},
		{"fresh", okAdd("us-east-1"), StatusAdded, true, "added (us-east-1)"},
		{
			"all duplicate",
			&AddResponse{Added: []AddedSlot{{Duplicate: true, Region: "a"}, {Duplicate: true, Region: "b"}}},
			StatusDuplicate, true, "already in pool (a, b)",
		},
		{
			"partial duplicate is a fresh add",
			&AddResponse{Added: []AddedSlot{{Duplicate: true, Region: "a"}, {Region: "b"}}},
			StatusAdded, true, "added (a, b)",
		},
		{
			"repaired outranks duplicate",
			&AddResponse{Added: []AddedSlot{{Duplicate: true, Repaired: true, Region: "eu-central-1"}}},
			StatusRepaired, true, "revived (was banned)",
		},
		{
			"skipped regions reported",
			&AddResponse{Added: []AddedSlot{{Region: "us-east-1"}}, Skipped: []SkippedSlot{{Region: "eu-central-1", Error: "403"}}},
			StatusAdded, true, "skipped 1 region(s)",
		},
		{
			"missing region renders as ?",
			&AddResponse{Added: []AddedSlot{{ID: "x"}}},
			StatusAdded, true, "added (?)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var res Result
			ok := summarizeAdd(&res, tc.resp)
			if ok != tc.wantOK {
				t.Errorf("ok = %v, want %v", ok, tc.wantOK)
			}
			if res.Status != tc.wantStatus {
				t.Errorf("status = %s, want %s", res.Status, tc.wantStatus)
			}
			if !strings.Contains(res.Detail, tc.detailHas) {
				t.Errorf("detail = %q, want it to contain %q", res.Detail, tc.detailHas)
			}
		})
	}
}

func TestTailDetail(t *testing.T) {
	// The cloakbrowser "Update available" nag is only 3 lines deep, so it once
	// EVICTED the actual error from the report — accounts failed with nothing
	// but the nag to show for it.
	noisy := subproc.Result{Stderr: "ERROR: the real problem\n" +
		"Update available: cloakbrowser 2.0\n" +
		"run: pip install -U cloakbrowser\n" +
		"still noise: Update available: again\n"}
	got := tailDetail(noisy)
	if !strings.Contains(got, "the real problem") {
		t.Errorf("tail = %q, want the real error to survive the nag filter", got)
	}

	// Filtering to nothing falls back to the unfiltered tail: a bare
	// "no API key produced" with no context would be strictly worse.
	onlyNoise := subproc.Result{Stderr: "Update available: cloakbrowser 2.0\n"}
	if !strings.Contains(tailDetail(onlyNoise), "Update available") {
		t.Errorf("tail = %q, want the nag when it is all there is", tailDetail(onlyNoise))
	}

	// stdout is used only when stderr is empty.
	if !strings.Contains(tailDetail(subproc.Result{Stdout: "from stdout"}), "from stdout") {
		t.Error("stdout not used as the fallback source")
	}
	// At most three lines.
	many := subproc.Result{Stderr: "l1\nl2\nl3\nl4\nl5\n"}
	if got := tailDetail(many); strings.Contains(got, "l2") || !strings.Contains(got, "l5") {
		t.Errorf("tail = %q, want only the last three lines", got)
	}
	// No output at all yields no suffix.
	if got := tailDetail(subproc.Result{}); got != "" {
		t.Errorf("tail = %q, want empty for no output", got)
	}

	// CRLF is normalized (Python's .splitlines()), so no stray CR survives into
	// the reported detail.
	if got := tailDetail(subproc.Result{Stderr: "ERROR: bad\r\n"}); strings.Contains(got, "\r") {
		t.Errorf("tail = %q, want CR normalized away", got)
	}
}

func TestTailDetailRedactsTheMFASeed(t *testing.T) {
	// SECURITY REGRESSION: kiro-go-login.py's last-ditch stderr report carries a
	// LIVE TOTP seed. Unlike Python, this detail string is streamed through
	// ProgressFunc to a Telegram chat log, so an unredacted copy would persist
	// the seed somewhere we do not control. It is not lost by redacting:
	// provisionM365 captures it into Result.NewMFA and the 0600 archive record
	// before tailDetail ever runs.
	got := tailDetail(subproc.Result{
		Stderr: "  !! MFA secret for bob@corp.example: SUPERSECRETSEED\nERROR: sign-in failed\n",
	})
	if strings.Contains(got, "SUPERSECRETSEED") {
		t.Errorf("tail leaked the TOTP seed: %q", got)
	}
	if !strings.Contains(got, "<redacted>") || !strings.Contains(got, "bob@corp.example") {
		t.Errorf("tail = %q, want the line kept for triage with the seed redacted", got)
	}
}

func TestTailDetailRedactsEveryCredentialReportLine(t *testing.T) {
	// SECURITY REGRESSION: redacting ONLY the m365 seed above left the idc
	// siblings wide open. kiro-web-login.py prints the reset password to
	// STDERR — the stream tailDetail PREFERS — and the :575-578 variant returns
	// an error straight after, so no key JSON exists, the account is reported
	// login_failed, and this tail becomes the Telegram-visible detail. That is
	// a live plaintext password published into a chat log we do not control.
	//
	// One case per emitting call site, values chosen so a partial redaction
	// (e.g. stopping at the first space) still fails the test.
	cases := []struct {
		name, line, secret string
	}{
		{"reset rejected by the portal (kiro-web-login.py:577)",
			"  !! attempted NEW PASSWORD for alice@corp.example: H9-f2h kEP_a#IQ1", "H9-f2h kEP_a#IQ1"},
		{"accounts-file write failed (kiro-web-login.py:586,1017)",
			"  !! NEW PASSWORD for alice@corp.example: Tr0ub4dor&3 with spaces", "Tr0ub4dor&3 with spaces"},
		{"enrolled seed on stdout (kiro-web-login.py:704)",
			"  -> MFA SECRET for alice@corp.example: JBSWY3DPEHPK3PXP", "JBSWY3DPEHPK3PXP"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Once via stderr (the preferred source) and once via stdout (the
			// fallback), because kiro-web-login.py uses both streams.
			for _, res := range []subproc.Result{
				{Stderr: tc.line + "\nERROR: sign-in failed\n"},
				{Stdout: tc.line + "\nERROR: sign-in failed\n"},
			} {
				got := tailDetail(res)
				if strings.Contains(got, tc.secret) {
					t.Errorf("tail leaked the credential: %q", got)
				}
				if !strings.Contains(got, "<redacted>") || !strings.Contains(got, "alice@corp.example") {
					t.Errorf("tail = %q, want the line kept for triage with the value redacted", got)
				}
				// The genuine diagnostic must survive alongside the redaction.
				if !strings.Contains(got, "sign-in failed") {
					t.Errorf("tail = %q, want the real error still surfaced", got)
				}
			}
		})
	}

	// A non-credential "!!" diagnostic must NOT be redacted — over-redaction
	// would blind the operator to the failure they have to debug.
	keep := tailDetail(subproc.Result{Stderr: "  !! WARNING: could not update accounts file: disk full\n"})
	if !strings.Contains(keep, "disk full") {
		t.Errorf("tail = %q, want a non-credential diagnostic kept intact", keep)
	}
}

func TestM365LoginFailedDetailNeverCarriesTheSeed(t *testing.T) {
	// End-to-end shape of the leak: the helper enrolled a seed, failed to write
	// it back, AND produced no account JSON. The seed must reach NewMFA and the
	// archive record — but never Result.Detail, which the progress callback
	// relays onward.
	const seed = "LEAKYSEEDLEAKYSEED"
	login := &fakeLogin{m365: func(context.Context, string, int, string, string, bool, float64) (subproc.Result, error) {
		return subproc.Result{ExitCode: 1, Stderr: "  !! MFA secret for m365.user@corp.example: " + seed + "\n" +
			"ERROR: failed to resolve profile ARN\n"}, nil
	}}
	p, _ := newTestProvisioner(t, login, &fakePool{acctResp: okAdd("eu-central-1")})

	var progressDetail string
	r := run(t, p, m365Text, RunOptions{Headless: true, Progress: func(_, _ int, _ string, _ Status, d string) {
		progressDetail = d
	}})[0]

	if r.Status != StatusLoginFailed {
		t.Errorf("status = %s, want login_failed", r.Status)
	}
	if r.NewMFA != seed {
		t.Errorf("NewMFA = %q, want the seed still captured", r.NewMFA)
	}
	assertRewriteRecord(t, r.RewriteSaved, "m365.user@corp.example", seed)
	if strings.Contains(r.Detail, seed) {
		t.Errorf("Result.Detail leaked the seed: %q", r.Detail)
	}
	if strings.Contains(progressDetail, seed) {
		t.Errorf("the progress callback received the seed: %q", progressDetail)
	}
	// The triage value must survive the redaction.
	if !strings.Contains(r.Detail, "profile ARN") {
		t.Errorf("Detail = %q, want the real error still surfaced", r.Detail)
	}
}

func TestIDCKeyIsSalvagedWhenTheParentContextDies(t *testing.T) {
	// REGRESSION: the salvage lookup once ran only for subproc.ErrTimeout.
	// Python only handles TimeoutExpired because it has no parent-context
	// notion; here a caller-imposed batch deadline or a shutdown cancellation
	// surfaces as ctx.Err() instead — and strands exactly the same live ksk_
	// that Kiro gives us no way to revoke.
	inner := idcLoginOK(t, testKSK)
	login := &fakeLogin{idc: func(ctx context.Context, f string, i int, dir string, h bool, sd float64) (subproc.Result, error) {
		if _, err := inner(ctx, f, i, dir, h, sd); err != nil {
			return subproc.Result{}, err
		}
		// The key is on disk; the batch context then dies.
		return subproc.Result{}, context.DeadlineExceeded
	}}
	pool := &fakePool{keyResp: okAdd("us-east-1")}
	p, _ := newTestProvisioner(t, login, pool)

	r := run(t, p, idcText, RunOptions{Headless: true})[0]
	if r.Status != StatusAdded {
		t.Errorf("status = %s (%s), want the captured key salvaged", r.Status, r.Detail)
	}
	if len(pool.keys) != 1 {
		t.Errorf("pool calls = %d, want the salvaged key posted", len(pool.keys))
	}
}

func TestIDCSubprocessErrorWithNoKeyIsStillAnError(t *testing.T) {
	// The other half of the same change: when the lookup finds nothing, a
	// genuine failure-to-run must still report StatusError (not login_failed),
	// because the two mean different things to the operator.
	login := &fakeLogin{idc: func(context.Context, string, int, string, bool, float64) (subproc.Result, error) {
		return subproc.Result{}, errors.New("fork/exec /venv/bin/python: no such file or directory")
	}}
	p, _ := newTestProvisioner(t, login, &fakePool{keyResp: okAdd("us-east-1")})

	r := run(t, p, idcText, RunOptions{Headless: true})[0]
	if r.Status != StatusError {
		t.Errorf("status = %s, want error", r.Status)
	}
	if !strings.Contains(r.Detail, "login subprocess error") {
		t.Errorf("detail = %q", r.Detail)
	}
}

func TestArchiveRootIsRequired(t *testing.T) {
	// An empty ArchiveRoot makes filepath.Join("", runTag) a RELATIVE path, so
	// live ksk_ keys, OAuth account JSONs and plaintext TOTP-seed records would
	// land under the process CWD. Refuse before any work starts.
	p := New(Options{
		Login:  &fakeLogin{idc: idcLoginOK(t, testKSK)},
		Pool:   &fakePool{keyResp: okAdd("us-east-1")},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if _, err := p.Provision(context.Background(), idcText, RunOptions{}); err == nil {
		t.Fatal("an empty ArchiveRoot was accepted")
	}
	// And the refusal must not leave the single-flight lock held.
	p2, _ := newTestProvisioner(t, &fakeLogin{idc: idcLoginOK(t, testKSK)}, &fakePool{keyResp: okAdd("us-east-1")})
	if _, err := p2.Provision(context.Background(), idcText, RunOptions{}); err != nil {
		t.Errorf("a properly configured Provisioner failed: %v", err)
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

// assertRewriteRecord checks the archive-dir record that makes a failed
// Telegram report recoverable rather than fatal: it must exist, hold the
// account and the secret, and be 0600 (it holds a LIVE secret).
func assertRewriteRecord(t *testing.T, path, account, secret string) {
	t.Helper()
	if path == "" {
		t.Fatal("rewritten credential was not persisted to the archive dir")
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat rewrite record: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("rewrite record mode = %v, want 0600 (it holds a live secret)", fi.Mode().Perm())
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read rewrite record: %v", err)
	}
	if !strings.Contains(string(body), account) || !strings.Contains(string(body), secret) {
		t.Errorf("rewrite record = %q, want it to hold the account + secret", body)
	}
}
