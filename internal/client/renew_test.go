package client

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"testing"
	"time"
)

func TestClientCertificateRenewsBeforeExpiry(t *testing.T) {
	now := time.Now()
	leaf := func(notAfter time.Time) tls.Certificate {
		return tls.Certificate{Certificate: [][]byte{{1}}, Leaf: &x509.Certificate{NotAfter: notAfter}}
	}
	renewals, closed := 0, 0
	certificate := &clientCertificate{
		current: leaf(now.Add(15 * time.Minute)),
		renew: func(context.Context) (tls.Certificate, error) {
			renewals++
			return leaf(now.Add(30 * time.Minute)), nil
		},
		closeIdle: func() { closed++ },
	}
	if err := certificate.refresh(context.Background(), now); err != nil || renewals != 0 {
		t.Fatalf("fresh certificate renewed: %d, %v", renewals, err)
	}
	if err := certificate.refresh(context.Background(), now.Add(11*time.Minute)); err != nil || renewals != 1 || closed != 1 {
		t.Fatalf("expiring certificate: renewals=%d closed=%d err=%v", renewals, closed, err)
	}
	current, _ := certificate.get(nil)
	if !current.Leaf.NotAfter.Equal(now.Add(30 * time.Minute)) {
		t.Fatalf("handshakes still use the old certificate: %v", current.Leaf.NotAfter)
	}
	certificate.renew = func(context.Context) (tls.Certificate, error) { return tls.Certificate{}, errors.New("token revoked") }
	if err := certificate.refresh(context.Background(), now.Add(29*time.Minute)); err == nil {
		t.Fatal("a failed renewal was ignored")
	}
}
