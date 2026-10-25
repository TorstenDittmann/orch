package control

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/TorstenDittmann/orch/gen/orch/v1alpha1/orchv1alpha1connect"
)

func TestSlowUnauthenticatedRequestsAreCutOff(t *testing.T) {
	previous := unauthenticatedReadTimeout
	unauthenticatedReadTimeout = 200 * time.Millisecond
	t.Cleanup(func() { unauthenticatedReadTimeout = previous })
	_, dataDir, _, _ := bootstrapController(t)
	server, err := Start(context.Background(), Options{DataDir: dataDir, Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })

	connection, err := tls.Dial("tcp", server.Addr().String(), &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"http/1.1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	// Promise a body and never send it.
	path := orchv1alpha1connect.CredentialServiceExchangeAccessTokenProcedure
	if _, err := fmt.Fprintf(connection, "POST %s HTTP/1.1\r\nHost: orch\r\nContent-Type: application/proto\r\nContent-Length: 1000\r\n\r\n", path); err != nil {
		t.Fatal(err)
	}
	_ = connection.SetReadDeadline(time.Now().Add(5 * time.Second))
	started := time.Now()
	_, err = io.ReadAll(connection)
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("slow request held the connection for %s (%v)", elapsed, err)
	}
}
