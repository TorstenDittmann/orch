package cluster

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRenewCertificatesReissuesExpiredLeavesFromExistingCA(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	ctx := context.Background()
	initialized, err := Bootstrap(ctx, BootstrapOptions{
		Name: "test", Server: "https://controller.example", DataDir: filepath.Join(root, "state"),
		ContextPath: filepath.Join(root, "config", "context.json"), Now: func() time.Time { return bootstrapTestTime },
	})
	if err != nil {
		t.Fatal(err)
	}
	pki := filepath.Join(initialized.DataDir, controllerIdentityDirectory)
	caBefore, _ := os.ReadFile(filepath.Join(pki, "ca.pem"))
	caKeyBefore, _ := os.ReadFile(filepath.Join(pki, "ca-key.pem"))
	controllerBefore, _ := os.ReadFile(filepath.Join(pki, "controller.pem"))

	expired := bootstrapTestTime.AddDate(1, 1, 0)
	if _, err := LoadControllerCredentials(initialized.DataDir, initialized.Cluster, expired); err == nil {
		t.Fatal("controller identity is valid after one year")
	}
	if _, err := LoadAdministratorCredentials(initialized.ContextPath, expired); err == nil {
		t.Fatal("administrator identity is valid after one year")
	}

	adminDir := filepath.Join(root, "renewed-admin")
	result, err := RenewCertificates(ctx, RenewCertificatesOptions{DataDir: initialized.DataDir, AdminDir: adminDir, Now: func() time.Time { return expired }})
	if err != nil {
		t.Fatal(err)
	}
	if result.Cluster.ID != initialized.Cluster.ID || result.ContextPath != filepath.Join(adminDir, "context.json") {
		t.Fatalf("result = %+v", result)
	}
	if !result.ControllerNotAfter.Equal(expired.AddDate(1, 0, 0)) || !result.AdministratorNotAfter.Equal(expired.AddDate(1, 0, 0)) {
		t.Fatalf("renewed lifetimes = %s, %s", result.ControllerNotAfter, result.AdministratorNotAfter)
	}
	controller, err := LoadControllerCredentials(initialized.DataDir, initialized.Cluster, expired)
	if err != nil {
		t.Fatalf("renewed controller identity: %v", err)
	}
	if controller.Certificate.Leaf.DNSNames[0] != "controller.example" {
		t.Fatalf("renewed controller DNS names = %v", controller.Certificate.Leaf.DNSNames)
	}
	administrator, err := LoadAdministratorCredentials(result.ContextPath, expired)
	if err != nil {
		t.Fatalf("renewed administrator identity: %v", err)
	}
	if administrator.Context.Cluster.ID != initialized.Cluster.ID || administrator.Context.Server != initialized.Cluster.Endpoint {
		t.Fatalf("renewed context = %+v", administrator.Context)
	}
	if !bytes.Equal(administrator.CACertificatePEM, caBefore) {
		t.Fatal("administrator directory carries a different CA")
	}
	for name, want := range map[string][]byte{"ca.pem": caBefore, "ca-key.pem": caKeyBefore} {
		if got, _ := os.ReadFile(filepath.Join(pki, name)); !bytes.Equal(got, want) {
			t.Fatalf("%s changed during renewal", name)
		}
	}
	for _, name := range controllerPKIFiles {
		assertMode(t, filepath.Join(pki, name), 0o600)
	}
	assertMode(t, pki, 0o700)
	assertMode(t, adminDir, 0o700)
	assertMode(t, result.ContextPath, 0o600)
	assertMode(t, filepath.Join(adminDir, administratorRootDirectory, initialized.Cluster.ID, "admin-key.pem"), 0o600)
	entries, err := os.ReadDir(pki)
	if err != nil || len(entries) != len(controllerPKIFiles) {
		t.Fatalf("identity directory has staging leftovers: %v %v", entries, err)
	}

	// An existing administrator directory is never overwritten, and the
	// controller identity stays untouched when renewal is refused.
	renewed, _ := os.ReadFile(filepath.Join(pki, "controller.pem"))
	if bytes.Equal(renewed, controllerBefore) {
		t.Fatal("controller certificate was not replaced")
	}
	if _, err := RenewCertificates(ctx, RenewCertificatesOptions{DataDir: initialized.DataDir, AdminDir: adminDir, Now: func() time.Time { return expired }}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("renewal into existing admin dir = %v", err)
	}
	if after, _ := os.ReadFile(filepath.Join(pki, "controller.pem")); !bytes.Equal(after, renewed) {
		t.Fatal("refused renewal changed the controller certificate")
	}
}

func TestRenewCertificatesRequiresValidCAAndSafePaths(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	ctx := context.Background()
	initialized, err := Bootstrap(ctx, BootstrapOptions{
		Name: "test", Server: "https://127.0.0.1:9443", DataDir: filepath.Join(root, "state"),
		ContextPath: filepath.Join(root, "config", "context.json"), Now: func() time.Time { return bootstrapTestTime },
	})
	if err != nil {
		t.Fatal(err)
	}
	controllerBefore, _ := os.ReadFile(filepath.Join(initialized.DataDir, controllerIdentityDirectory, "controller.pem"))
	afterCA := bootstrapTestTime.AddDate(11, 0, 0)
	adminDir := filepath.Join(root, "admin")
	if _, err := RenewCertificates(ctx, RenewCertificatesOptions{DataDir: initialized.DataDir, AdminDir: adminDir, Now: func() time.Time { return afterCA }}); err == nil || !strings.Contains(err.Error(), "CA") {
		t.Fatalf("renewal with expired CA = %v", err)
	}
	if _, err := os.Lstat(adminDir); !os.IsNotExist(err) {
		t.Fatal("failed renewal created the administrator directory")
	}
	if after, _ := os.ReadFile(filepath.Join(initialized.DataDir, controllerIdentityDirectory, "controller.pem")); !bytes.Equal(after, controllerBefore) {
		t.Fatal("failed renewal changed the controller certificate")
	}
	for _, options := range []RenewCertificatesOptions{
		{DataDir: initialized.DataDir},
		{AdminDir: adminDir},
		{DataDir: initialized.DataDir, AdminDir: filepath.Join(initialized.DataDir, "admin")},
	} {
		if _, err := RenewCertificates(ctx, options); err == nil {
			t.Fatalf("RenewCertificates(%+v) succeeded", options)
		}
	}
	result, err := RenewCertificates(ctx, RenewCertificatesOptions{DataDir: initialized.DataDir, AdminDir: adminDir, Now: func() time.Time { return bootstrapTestTime.AddDate(0, 6, 0) }})
	if err != nil {
		t.Fatal(err)
	}
	controller, err := LoadControllerCredentials(initialized.DataDir, initialized.Cluster, result.ControllerNotAfter.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(controller.Certificate.Leaf.IPAddresses) != 1 || controller.Certificate.Leaf.IPAddresses[0].String() != "127.0.0.1" {
		t.Fatalf("renewed controller IP SANs = %v", controller.Certificate.Leaf.IPAddresses)
	}
}

func TestRestoreFlagsExpiredControllerLeafForRenewal(t *testing.T) {
	options := recoveryFixture(t, false)
	expired := bootstrapTestTime.AddDate(1, 1, 0)
	options.Now = func() time.Time { return expired }
	result, err := Restore(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Ready || !result.ControllerCertificateExpired {
		t.Fatalf("restore = %+v", result)
	}
	if _, err := LoadControllerCredentials(result.DataDir, result.Cluster, expired); err == nil {
		t.Fatal("restored controller identity unexpectedly valid")
	}
	if _, err := RenewCertificates(context.Background(), RenewCertificatesOptions{DataDir: result.DataDir, AdminDir: filepath.Join(filepath.Dir(result.DataDir), "admin"), Now: options.Now}); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadControllerCredentials(result.DataDir, result.Cluster, expired); err != nil {
		t.Fatalf("renewed restored controller identity: %v", err)
	}

	// An expired CA still blocks recovery.
	afterCA := recoveryFixture(t, false)
	afterCA.Now = func() time.Time { return bootstrapTestTime.AddDate(11, 0, 0) }
	if _, err := InspectBackup(context.Background(), afterCA); err == nil {
		t.Fatal("inspected recovery material with an expired CA")
	}
}
