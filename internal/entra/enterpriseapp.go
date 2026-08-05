package entra

import (
	"context"
	"encoding/json"
	"path/filepath"
	"time"
)

// entraEnterpriseAppScript is the vendored child script this file drives —
// Graph work for the Enterprise Application (create/list/SAML/SCIM/delete).
// Not yet present in this Go repo (only aws_idc_provision_cli.py and its
// name-file siblings are vendored so far); the reference source is
// /Users/z/Desktop/Kiro/entra_enterprise_app.py.
const entraEnterpriseAppScript = "entra_enterprise_app.py"

// entraAppTimeout is the fixed subprocess budget for EVERY
// entra_enterprise_app.py invocation regardless of mode — the Python bot's
// _run_entra_app always passes timeout=300 (py_entra_spec.md "Subprocess
// contract (general)").
const entraAppTimeout = 300 * time.Second

// DomainsResult is do_list_domains' JSON shape
// (entra_enterprise_app.py:363-372): the tenant's verified domains, default
// first, feeding the publisher-domain picker shown right after the admin
// names a new Enterprise Application (adm:entracreateapp step 3,
// py_entra_spec.md §3).
type DomainsResult struct {
	OK            bool     `json:"ok"`
	Domains       []string `json:"domains"`
	DefaultDomain string   `json:"default_domain"`
	TenantID      string   `json:"tenant_id"`
}

// listDomainsArgs is a pure, independently testable argv builder — kept
// separate from ListDomains itself so its exact shape can be asserted
// without spawning anything, mirroring internal/provision/login.go's
// idcLoginArgs/m365LoginArgs pattern.
func listDomainsArgs() []string { return []string{"--list-domains"} }

// ListDomains lists the tenant's verified domains (do_list_domains,
// entra_enterprise_app.py:363-372). Failure mode: a non-2xx/expired Graph
// token surfaces as a returned error (not logged in / Graph 403), which the
// handler renders as "❌ Couldn't list domains: {error}" per
// py_entra_spec.md §3 step 3.
func ListDomains(ctx context.Context, scriptDir, python string) (DomainsResult, error) {
	raw, err := runScript(ctx, python, filepath.Join(scriptDir, entraEnterpriseAppScript),
		listDomainsArgs(), nil, entraAppTimeout, nil)
	if err != nil {
		return DomainsResult{}, err
	}
	return decodeResult[DomainsResult](raw, entraEnterpriseAppScript)
}

// CreateAppResult is do_create's JSON shape (entra_enterprise_app.py:396-436).
// LogoSet carries do_create's DYNAMICALLY-TYPED "logo_set" key — absent when
// no --logo was given, `true` (bool) on a successful upload, or a STRING
// failure detail ("read-failed: ..."/"failed: ...") when _set_logo
// (entra_enterprise_app.py:380-392) gave up after its retries — so it is
// decoded as raw JSON here; call LogoOutcome to interpret it.
type CreateAppResult struct {
	OK                    bool            `json:"ok"`
	AppObjectID           string          `json:"app_object_id"`
	AppID                 string          `json:"app_id"`
	SPID                  string          `json:"sp_id"`
	TenantID              string          `json:"tenant_id"`
	PrimaryDomain         string          `json:"primary_domain"`
	DisplayName           string          `json:"display_name"`
	PublisherDomain       string          `json:"publisher_domain"`
	PublisherDomainManual bool            `json:"publisher_domain_manual"`
	LogoSet               json.RawMessage `json:"logo_set"`
}

// LogoOutcome interprets CreateAppResult.LogoSet. attempted reports whether
// a logo path was given at all (the key is absent entirely when it wasn't —
// entra_enterprise_app.py:436 only sets it "if logo_path"); ok/detail
// distinguish a clean upload from _set_logo's failure string.
func (r CreateAppResult) LogoOutcome() (attempted, ok bool, detail string) {
	if len(r.LogoSet) == 0 {
		return false, false, ""
	}
	var b bool
	if json.Unmarshal(r.LogoSet, &b) == nil {
		return true, b, ""
	}
	var s string
	if json.Unmarshal(r.LogoSet, &s) == nil {
		return true, false, s
	}
	return true, false, "unrecognized logo_set value"
}

// createAppArgs is the pure argv builder for --create. publisherDomain and
// logoPath are each omitted entirely when "" — Graph's publisherDomain is
// read-only (do_create never PATCHes it; the admin sets it manually in the
// portal per py_entra_spec.md §3's "Publisher domain — set manually" note),
// and the Python bot only passes --logo when the brand PNG file exists
// (py_entra_spec.md §3 step 4) — that existence check is the caller's job,
// not this package's.
func createAppArgs(name, publisherDomain, logoPath string) []string {
	args := []string{"--create", "--name", name}
	if publisherDomain != "" {
		args = append(args, "--publisher-domain", publisherDomain)
	}
	if logoPath != "" {
		args = append(args, "--logo", logoPath)
	}
	return args
}

// CreateApp instantiates a new Enterprise Application from the generic
// non-gallery template and configures its API scopes + redirect URIs
// (do_create, entra_enterprise_app.py:396-436).
func CreateApp(ctx context.Context, scriptDir, python, name, publisherDomain, logoPath string) (CreateAppResult, error) {
	raw, err := runScript(ctx, python, filepath.Join(scriptDir, entraEnterpriseAppScript),
		createAppArgs(name, publisherDomain, logoPath), nil, entraAppTimeout, nil)
	if err != nil {
		return CreateAppResult{}, err
	}
	return decodeResult[CreateAppResult](raw, entraEnterpriseAppScript)
}

// AppInfo is one entry of do_list_apps' "apps" array
// (entra_enterprise_app.py:627-636) — filtered to service principals owned
// by THIS tenant, sorted by display name.
type AppInfo struct {
	SPID        string `json:"sp_id"`
	AppID       string `json:"app_id"`
	DisplayName string `json:"display_name"`
	TenantID    string `json:"tenant_id"`
}

// AppsResult is do_list_apps' JSON shape, feeding the app pickers used by
// "Generate users", both "Configure provisioning" branches, and "Delete
// Enterprise App" (py_entra_spec.md §4/§7/§10).
type AppsResult struct {
	OK   bool      `json:"ok"`
	Apps []AppInfo `json:"apps"`
}

func listAppsArgs() []string { return []string{"--list-apps"} }

// ListApps lists the tenant's Enterprise Applications (do_list_apps,
// entra_enterprise_app.py:627-636).
func ListApps(ctx context.Context, scriptDir, python string) (AppsResult, error) {
	raw, err := runScript(ctx, python, filepath.Join(scriptDir, entraEnterpriseAppScript),
		listAppsArgs(), nil, entraAppTimeout, nil)
	if err != nil {
		return AppsResult{}, err
	}
	return decodeResult[AppsResult](raw, entraEnterpriseAppScript)
}

// SAMLSetupResult is do_saml_setup's JSON shape
// (entra_enterprise_app.py:546-617): the AWS IAM Identity Center SAML
// identity-source switch, step 1 of "Configure provisioning" → Option 2
// (py_entra_spec.md §7). MetadataPath echoes the outPath the caller passed —
// the caller sends that FILE (the downloaded Azure Federation Metadata XML)
// as a document to the admin; this package never reads its contents.
type SAMLSetupResult struct {
	OK           bool   `json:"ok"`
	EntityID     string `json:"entity_id"`
	ACSURL       string `json:"acs_url"`
	AppID        string `json:"app_id"`
	TenantID     string `json:"tenant_id"`
	DisplayName  string `json:"display_name"`
	CertAdded    bool   `json:"cert_added"`
	MetadataPath string `json:"metadata_path"`
}

func samlSetupArgs(spID, awsMetadataPath, outPath string) []string {
	return []string{"--saml-setup", "--sp-id", spID, "--aws-metadata", awsMetadataPath, "--out", outPath}
}

// SAMLSetup switches the given Enterprise Application to SAML SSO against
// AWS IAM Identity Center and downloads the resulting Azure Federation
// Metadata XML to outPath (do_saml_setup, entra_enterprise_app.py:546-617).
//
// DESTRUCTIVE ON THE WRONG APP: this REPLACES identifierUris/redirectUris on
// the target application — running it against the Kiro OAuth app would
// break api://{appId} (py_entra_spec.md §7's "dedicated app" warning). This
// package does not enforce that; the handler layer must carry the warning
// forward to the admin, exactly as the Python bot's picker title does.
func SAMLSetup(ctx context.Context, scriptDir, python, spID, awsMetadataPath, outPath string) (SAMLSetupResult, error) {
	raw, err := runScript(ctx, python, filepath.Join(scriptDir, entraEnterpriseAppScript),
		samlSetupArgs(spID, awsMetadataPath, outPath), nil, entraAppTimeout, nil)
	if err != nil {
		return SAMLSetupResult{}, err
	}
	return decodeResult[SAMLSetupResult](raw, entraEnterpriseAppScript)
}

// ProvisionResult is do_provision's JSON shape (entra_enterprise_app.py:485-543).
// TestError is nil (JSON null) exactly when Tested is true — the SCIM
// connectivity probe passed and the job was started; when the probe fails
// the job is still created and its secrets still set, but Started stays
// false (a normal, reportable outcome, NOT this function returning an
// error — matching the Python bot's "SCIM configured, but the connection
// test did not pass — NOT started" branch, py_entra_spec.md §7).
type ProvisionResult struct {
	OK            bool    `json:"ok"`
	JobID         string  `json:"job_id"`
	Tested        bool    `json:"tested"`
	TestError     *string `json:"test_error"`
	Started       bool    `json:"started"`
	SchemaApplied bool    `json:"schema_applied"`
	StatusCode    string  `json:"status_code"`
}

// configureProvisioningArgs defaults target to "kiro" (entra_enterprise_app.py's
// own argparse default) when the caller passes "" — the Kiro-direct SCIM
// option (py_entra_spec.md §7 Option 1) never has a reason to say otherwise.
func configureProvisioningArgs(spID, scimEndpoint, target string) []string {
	if target == "" {
		target = "kiro"
	}
	return []string{"--provision", "--sp-id", spID, "--scim-endpoint", scimEndpoint, "--target", target}
}

// ConfigureProvisioning configures SCIM sync for target ("kiro" or "aws"),
// deleting any prior provisioning config first for a clean slate
// (do_provision, entra_enterprise_app.py:485-543 — "each Configure starts
// from a truly clean state"). scimSecretToken travels via the CHILD's
// environment as ENTRA_SCIM_SECRET_TOKEN — NEVER argv (py_entra_spec.md §7
// "Subprocess contract"; subproc.go contract 1) — and is also the one value
// redacted from any error synthesized from the child's stderr.
func ConfigureProvisioning(ctx context.Context, scriptDir, python, spID, scimEndpoint, scimSecretToken, target string) (ProvisionResult, error) {
	env := []string{"ENTRA_SCIM_SECRET_TOKEN=" + scimSecretToken}
	raw, err := runScript(ctx, python, filepath.Join(scriptDir, entraEnterpriseAppScript),
		configureProvisioningArgs(spID, scimEndpoint, target), env, entraAppTimeout, []string{scimSecretToken})
	if err != nil {
		return ProvisionResult{}, err
	}
	return decodeResult[ProvisionResult](raw, entraEnterpriseAppScript)
}

// DeleteAppResult is do_delete's JSON shape (entra_enterprise_app.py:640-670).
// Both booleans are true even when the underlying object was ALREADY gone —
// do_delete treats a 404 as "already deleted", i.e. success/idempotent, not
// a failure (py_entra_spec.md §10 step 4).
type DeleteAppResult struct {
	OK          bool   `json:"ok"`
	DisplayName string `json:"display_name"`
	DeletedSP   bool   `json:"deleted_sp"`
	DeletedApp  bool   `json:"deleted_app"`
}

func deleteAppArgs(spID string) []string { return []string{"--delete", "--sp-id", spID} }

// DeleteApp removes an Enterprise Application's service principal and its
// backing app registration (do_delete, entra_enterprise_app.py:640-670).
// Both objects are only SOFT-deleted by Graph (recoverable ~30 days) — this
// operation never purges, unlike every user-deletion path in this package
// (py_entra_spec.md §10 step 4's "technically reversible" note).
func DeleteApp(ctx context.Context, scriptDir, python, spID string) (DeleteAppResult, error) {
	raw, err := runScript(ctx, python, filepath.Join(scriptDir, entraEnterpriseAppScript),
		deleteAppArgs(spID), nil, entraAppTimeout, nil)
	if err != nil {
		return DeleteAppResult{}, err
	}
	return decodeResult[DeleteAppResult](raw, entraEnterpriseAppScript)
}
