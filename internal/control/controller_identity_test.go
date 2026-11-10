package control

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"math/big"
	"testing"
	"time"

	clusterctl "github.com/TorstenDittmann/orch/internal/cluster"
)

func testControllerCredentials(t *testing.T, notAfter time.Time) clusterctl.ControllerCredentials {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: notAfter.AddDate(-1, 0, 0), NotAfter: notAfter}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	return clusterctl.ControllerCredentials{Certificate: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}}
}

func TestControllerIdentityRenewsItselfBeforeExpiry(t *testing.T) {
	now := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	renewals := 0
	var renewErr error
	identity := newControllerIdentity(testControllerCredentials(t, now.Add(60*24*time.Hour)), func() time.Time { return now },
		func(at time.Time) (clusterctl.ControllerCredentials, error) {
			renewals++
			if renewErr != nil {
				return clusterctl.ControllerCredentials{}, renewErr
			}
			return testControllerCredentials(t, at.AddDate(1, 0, 0)), nil
		})
	if identity.renewIfDue() || renewals != 0 {
		t.Fatal("renewed a certificate with 60 days left")
	}
	now = now.Add(16 * 24 * time.Hour)
	renewErr = errors.New("disk full")
	if identity.renewIfDue() || !identity.NotAfter().Equal(time.Date(2027, 3, 2, 0, 0, 0, 0, time.UTC)) {
		t.Fatal("a failed renewal replaced the certificate")
	}
	renewErr = nil
	if !identity.renewIfDue() || !identity.NotAfter().Equal(now.AddDate(1, 0, 0)) {
		t.Fatalf("renewal at 44 days left: NotAfter = %s", identity.NotAfter())
	}
	served, _ := identity.GetCertificate(nil)
	if served == nil || !served.Leaf.NotAfter.Equal(now.AddDate(1, 0, 0)) {
		t.Fatal("TLS does not serve the renewed certificate")
	}
}
