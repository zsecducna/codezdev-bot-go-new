package entra

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// enterpriseapp_test.go covers the happy path of every entra_enterprise_app.py
// runner plus its argv builder. Cross-cutting failure-mode coverage (timeout,
// no parseable line, crash-with-partial-output, secret redaction) lives once
// in runner_test.go since every function here shares that exact plumbing.

func writeEnterpriseAppStub(t *testing.T, dir, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("stub is a POSIX shell script")
	}
	return writeStubScript(t, dir, entraEnterpriseAppScript, body)
}

func TestListDomainsArgs(t *testing.T) {
	if got, want := listDomainsArgs(), []string{"--list-domains"}; !reflect.DeepEqual(got, want) {
		t.Errorf("listDomainsArgs() = %v, want %v", got, want)
	}
}

func TestListDomains(t *testing.T) {
	dir := t.TempDir()
	writeEnterpriseAppStub(t, dir, `echo '{"ok":true,"domains":["a.com","b.com"],"default_domain":"a.com","tenant_id":"tid"}'`)
	got, err := ListDomains(context.Background(), dir, "sh")
	if err != nil {
		t.Fatalf("ListDomains: %v", err)
	}
	want := DomainsResult{OK: true, Domains: []string{"a.com", "b.com"}, DefaultDomain: "a.com", TenantID: "tid"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ListDomains() = %+v, want %+v", got, want)
	}
}

func TestCreateAppArgs(t *testing.T) {
	cases := []struct {
		name, domain, logo string
		want               []string
	}{
		{"App", "kiro.uk", "", []string{"--create", "--name", "App", "--publisher-domain", "kiro.uk"}},
		{"App", "", "", []string{"--create", "--name", "App"}},
		{"App", "kiro.uk", "/logo.png", []string{"--create", "--name", "App", "--publisher-domain", "kiro.uk", "--logo", "/logo.png"}},
	}
	for _, c := range cases {
		if got := createAppArgs(c.name, c.domain, c.logo); !reflect.DeepEqual(got, c.want) {
			t.Errorf("createAppArgs(%q,%q,%q) = %v, want %v", c.name, c.domain, c.logo, got, c.want)
		}
	}
}

func TestCreateAppWithLogoOutcome(t *testing.T) {
	dir := t.TempDir()
	writeEnterpriseAppStub(t, dir, `echo '{"ok":true,"app_object_id":"o1","app_id":"a1","sp_id":"s1","tenant_id":"t1","primary_domain":"d.com","display_name":"App","publisher_domain":"d.com","publisher_domain_manual":false,"logo_set":true}'`)
	got, err := CreateApp(context.Background(), dir, "sh", "App", "", "/logo.png")
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	attempted, ok, detail := got.LogoOutcome()
	if !attempted || !ok || detail != "" {
		t.Errorf("LogoOutcome() = (%v,%v,%q), want (true,true,\"\")", attempted, ok, detail)
	}
	if got.AppID != "a1" || got.SPID != "s1" {
		t.Errorf("CreateApp() = %+v, want app_id=a1 sp_id=s1", got)
	}
}

func TestCreateAppLogoFailureString(t *testing.T) {
	dir := t.TempDir()
	writeEnterpriseAppStub(t, dir, `echo '{"ok":true,"app_object_id":"o1","app_id":"a1","sp_id":"s1","tenant_id":"t1","primary_domain":"d.com","display_name":"App","publisher_domain":"d.com","publisher_domain_manual":false,"logo_set":"failed: http 500"}'`)
	got, err := CreateApp(context.Background(), dir, "sh", "App", "", "/logo.png")
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	attempted, ok, detail := got.LogoOutcome()
	if !attempted || ok || detail != "failed: http 500" {
		t.Errorf("LogoOutcome() = (%v,%v,%q), want (true,false,\"failed: http 500\")", attempted, ok, detail)
	}
}

func TestCreateAppNoLogoOmitsLogoOutcome(t *testing.T) {
	dir := t.TempDir()
	writeEnterpriseAppStub(t, dir, `echo '{"ok":true,"app_object_id":"o1","app_id":"a1","sp_id":"s1","tenant_id":"t1","primary_domain":"d.com","display_name":"App","publisher_domain":"d.com","publisher_domain_manual":false}'`)
	got, err := CreateApp(context.Background(), dir, "sh", "App", "", "")
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	attempted, _, _ := got.LogoOutcome()
	if attempted {
		t.Errorf("LogoOutcome() attempted=true, want false when --logo was never passed")
	}
}

func TestListApps(t *testing.T) {
	dir := t.TempDir()
	writeEnterpriseAppStub(t, dir, `echo '{"ok":true,"apps":[{"sp_id":"s1","app_id":"a1","display_name":"App1","tenant_id":"t1"}]}'`)
	got, err := ListApps(context.Background(), dir, "sh")
	if err != nil {
		t.Fatalf("ListApps: %v", err)
	}
	want := AppsResult{OK: true, Apps: []AppInfo{{SPID: "s1", AppID: "a1", DisplayName: "App1", TenantID: "t1"}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ListApps() = %+v, want %+v", got, want)
	}
}

func TestSAMLSetupArgs(t *testing.T) {
	got := samlSetupArgs("sp1", "/meta.xml", "/out.xml")
	want := []string{"--saml-setup", "--sp-id", "sp1", "--aws-metadata", "/meta.xml", "--out", "/out.xml"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("samlSetupArgs() = %v, want %v", got, want)
	}
}

func TestSAMLSetup(t *testing.T) {
	dir := t.TempDir()
	writeEnterpriseAppStub(t, dir, `echo '{"ok":true,"entity_id":"e1","acs_url":"https://acs","app_id":"a1","tenant_id":"t1","display_name":"App","cert_added":true,"metadata_path":"/out.xml"}'`)
	got, err := SAMLSetup(context.Background(), dir, "sh", "sp1", "/meta.xml", "/out.xml")
	if err != nil {
		t.Fatalf("SAMLSetup: %v", err)
	}
	if got.EntityID != "e1" || got.ACSURL != "https://acs" || !got.CertAdded {
		t.Errorf("SAMLSetup() = %+v", got)
	}
}

func TestConfigureProvisioningArgsDefaultsTargetToKiro(t *testing.T) {
	got := configureProvisioningArgs("sp1", "https://scim", "")
	want := []string{"--provision", "--sp-id", "sp1", "--scim-endpoint", "https://scim", "--target", "kiro"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("configureProvisioningArgs(target=\"\") = %v, want %v", got, want)
	}
}

func TestConfigureProvisioningSecretNeverInArgv(t *testing.T) {
	dir := t.TempDir()
	const secret = "ENTRA-SCIM-SECRET-TOKEN-VALUE"
	dumpFile := filepath.Join(dir, "argv.dump")
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done > " + dumpFile + "\n" +
		`echo '{"ok":true,"job_id":"j1","tested":true,"test_error":null,"started":true,"schema_applied":true,"status_code":"Active"}'`
	if err := os.WriteFile(filepath.Join(dir, entraEnterpriseAppScript), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := ConfigureProvisioning(context.Background(), dir, "sh", "sp1", "https://scim", secret, "aws")
	if err != nil {
		t.Fatalf("ConfigureProvisioning: %v", err)
	}
	if !got.Tested || !got.Started || got.TestError != nil {
		t.Errorf("ConfigureProvisioning() = %+v, want tested/started true, test_error nil", got)
	}
	raw, err := os.ReadFile(dumpFile)
	if err != nil {
		t.Fatalf("argv dump missing: %v", err)
	}
	if strings.Contains(string(raw), secret) {
		t.Errorf("secret leaked into argv: %q", string(raw))
	}
}

func TestConfigureProvisioningProbeFailedIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	writeEnterpriseAppStub(t, dir, `echo '{"ok":true,"job_id":"j1","tested":false,"test_error":"SCIM endpoint returned HTTP 404","started":false,"schema_applied":false}'`)
	got, err := ConfigureProvisioning(context.Background(), dir, "sh", "sp1", "https://scim", "sekret-value-123", "kiro")
	if err != nil {
		t.Fatalf("ConfigureProvisioning: %v, want nil (a failed probe is a normal outcome)", err)
	}
	if got.Tested || got.Started {
		t.Errorf("ConfigureProvisioning() = %+v, want tested=false started=false", got)
	}
	if got.TestError == nil || *got.TestError != "SCIM endpoint returned HTTP 404" {
		t.Errorf("ConfigureProvisioning().TestError = %v, want the probe error", got.TestError)
	}
}

func TestConfigureProvisioningRedactsSecretFromError(t *testing.T) {
	dir := t.TempDir()
	const secret = "ENTRA-SCIM-SECRET-SHOULD-NOT-LEAK"
	writeEnterpriseAppStub(t, dir, `echo "traceback with `+secret+` embedded" >&2; exit 1`)
	_, err := ConfigureProvisioning(context.Background(), dir, "sh", "sp1", "https://scim", secret, "kiro")
	if err == nil {
		t.Fatal("ConfigureProvisioning: got nil error, want the stderr-derived error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("ConfigureProvisioning error = %q, still contains the planted secret", err.Error())
	}
}

func TestDeleteAppArgs(t *testing.T) {
	if got, want := deleteAppArgs("sp1"), []string{"--delete", "--sp-id", "sp1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("deleteAppArgs() = %v, want %v", got, want)
	}
}

func TestDeleteApp(t *testing.T) {
	dir := t.TempDir()
	writeEnterpriseAppStub(t, dir, `echo '{"ok":true,"display_name":"App","deleted_sp":true,"deleted_app":true}'`)
	got, err := DeleteApp(context.Background(), dir, "sh", "sp1")
	if err != nil {
		t.Fatalf("DeleteApp: %v", err)
	}
	if !got.DeletedSP || !got.DeletedApp || got.DisplayName != "App" {
		t.Errorf("DeleteApp() = %+v", got)
	}
}

func TestListAppsErrorEnvelope(t *testing.T) {
	dir := t.TempDir()
	writeEnterpriseAppStub(t, dir, `echo '{"ok":false,"error":"not logged in to Entra"}'; exit 1`)
	_, err := ListApps(context.Background(), dir, "sh")
	if err == nil {
		t.Fatal("ListApps: got nil error, want the not-logged-in message surfaced")
	}
	if !strings.Contains(err.Error(), "not logged in to Entra") {
		t.Errorf("ListApps error = %q, want it to contain the child's message", err.Error())
	}
}
