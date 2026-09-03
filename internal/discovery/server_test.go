package discovery

import (
	"context"
	"net"
	"net/netip"
	"reflect"
	"testing"
	"time"

	"github.com/miekg/dns"
)

type responseRecorder struct{ message *dns.Msg }

func (r *responseRecorder) LocalAddr() net.Addr             { return &net.UDPAddr{} }
func (r *responseRecorder) RemoteAddr() net.Addr            { return &net.UDPAddr{} }
func (r *responseRecorder) WriteMsg(message *dns.Msg) error { r.message = message.Copy(); return nil }
func (r *responseRecorder) Write([]byte) (int, error)       { return 0, nil }
func (r *responseRecorder) Close() error                    { return nil }
func (r *responseRecorder) TsigStatus() error               { return nil }
func (r *responseRecorder) TsigTimersOnly(bool)             {}
func (r *responseRecorder) Hijack()                         {}

func TestPrivateRecordsAreStableAndHealthFiltered(t *testing.T) {
	server, err := New(DefaultZone, nil)
	if err != nil {
		t.Fatal(err)
	}
	name := ServiceName("example", "web")
	if err := server.Replace(Snapshot{
		Known: map[string]struct{}{name: {}},
		Addresses: map[string][]netip.Addr{name: {
			netip.MustParseAddr("10.88.0.9"), netip.MustParseAddr("10.88.0.3"), netip.MustParseAddr("10.88.0.3"),
		}},
	}); err != nil {
		t.Fatal(err)
	}
	request := new(dns.Msg)
	request.SetQuestion(name, dns.TypeA)
	recorder := &responseRecorder{}
	server.ServeDNS(recorder, request)
	if recorder.message == nil || recorder.message.Rcode != dns.RcodeSuccess || len(recorder.message.Answer) != 2 {
		t.Fatalf("reply = %#v", recorder.message)
	}
	if got := recorder.message.Answer[0].(*dns.A).A.String(); got != "10.88.0.3" {
		t.Fatalf("first address = %s", got)
	}
	if recorder.message.Answer[0].Header().Ttl != RecordTTL {
		t.Fatalf("TTL = %d", recorder.message.Answer[0].Header().Ttl)
	}
}

func TestForwardersAreBoundedAndExcludeTheGateway(t *testing.T) {
	server, err := New(DefaultZone, []string{
		"10.88.0.1:53", "1.1.1.1:53", "1.1.1.1:53", "8.8.8.8:53", "9.9.9.9:53", "4.4.4.4:53",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := server.excludeForwardingLoop("10.88.0.1:53"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(server.forwarders, []string{"1.1.1.1:53", "8.8.8.8:53", "9.9.9.9:53"}) {
		t.Fatalf("forwarders = %v", server.forwarders)
	}
}

func TestServerCanRestartAfterClose(t *testing.T) {
	t.Parallel()
	server, err := New(DefaultZone, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Start("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	if err := server.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := server.Close(context.Background()); err != nil {
		t.Fatalf("idempotent close: %v", err)
	}
	if err := server.Start("127.0.0.1:0"); err != nil {
		t.Fatalf("restart: %v", err)
	}
	t.Cleanup(func() { _ = server.Close(context.Background()) })
}

func TestPrivateNODATAAndNXDOMAIN(t *testing.T) {
	server, _ := New(DefaultZone, nil)
	known := ServiceName("example", "web")
	if err := server.Replace(Snapshot{Known: map[string]struct{}{known: {}}, Addresses: map[string][]netip.Addr{}}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name  string
		rcode int
	}{{known, dns.RcodeSuccess}, {ServiceName("missing", "web"), dns.RcodeNameError}} {
		request := new(dns.Msg)
		request.SetQuestion(test.name, dns.TypeA)
		recorder := &responseRecorder{}
		server.ServeDNS(recorder, request)
		if recorder.message.Rcode != test.rcode || len(recorder.message.Answer) != 0 {
			t.Fatalf("%s reply = %#v", test.name, recorder.message)
		}
	}
}

func TestServerAnswersUDPAndTCPAndShutsDown(t *testing.T) {
	server, err := New(DefaultZone, nil)
	if err != nil {
		t.Fatal(err)
	}
	name := ServiceName("example", "web")
	if err := server.Replace(Snapshot{
		Known:     map[string]struct{}{name: {}},
		Addresses: map[string][]netip.Addr{name: {netip.MustParseAddr("10.88.0.3")}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := server.Start("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	address := server.tcp.Listener.Addr().String()
	if got := server.udp.PacketConn.LocalAddr().String(); got != address {
		t.Fatalf("UDP address = %q, TCP address = %q", got, address)
	}
	for _, network := range []string{"udp", "tcp"} {
		request := new(dns.Msg)
		request.SetQuestion(name, dns.TypeA)
		reply := exchangeEventually(t, &dns.Client{Net: network, Timeout: time.Second}, request, address)
		if !reply.Authoritative || reply.Rcode != dns.RcodeSuccess || len(reply.Answer) != 1 {
			t.Fatalf("%s reply = %#v", network, reply)
		}
		request.SetQuestion(name, dns.TypeAAAA)
		reply = exchangeEventually(t, &dns.Client{Net: network, Timeout: time.Second}, request, address)
		if reply.Rcode != dns.RcodeSuccess || len(reply.Answer) != 0 {
			t.Fatalf("%s AAAA reply = %#v", network, reply)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := server.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if connection, err := net.DialTimeout("tcp", address, 100*time.Millisecond); err == nil {
		_ = connection.Close()
		t.Fatal("TCP listener remained open after shutdown")
	}
}

func TestLargeUDPResponseIsTruncated(t *testing.T) {
	server, _ := New(DefaultZone, nil)
	name := ServiceName("example", "web")
	addresses := make([]netip.Addr, 0, 200)
	for third := 1; third <= 2; third++ {
		for fourth := 2; fourth < 102 && len(addresses) < 200; fourth++ {
			addresses = append(addresses, netip.AddrFrom4([4]byte{10, 88, byte(third), byte(fourth)}))
		}
	}
	if err := server.Replace(Snapshot{Known: map[string]struct{}{name: {}}, Addresses: map[string][]netip.Addr{name: addresses}}); err != nil {
		t.Fatal(err)
	}
	if err := server.Start("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Close(ctx)
	})
	request := new(dns.Msg)
	request.SetQuestion(name, dns.TypeA)
	request.SetEdns0(512, false)
	reply := exchangeEventually(t, &dns.Client{Net: "udp", Timeout: time.Second, UDPSize: 512}, request, server.udp.PacketConn.LocalAddr().String())
	if !reply.Truncated || len(reply.Answer) >= len(addresses) {
		t.Fatalf("large UDP reply was not bounded: truncated=%t answers=%d", reply.Truncated, len(reply.Answer))
	}
}

func TestForwardingRetriesOverTCPAfterTruncation(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	packet, err := net.ListenPacket("udp", address)
	if err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	handler := dns.HandlerFunc(func(writer dns.ResponseWriter, request *dns.Msg) {
		reply := new(dns.Msg)
		reply.SetReply(request)
		if writer.LocalAddr().Network() == "udp" {
			reply.Truncated = true
		} else {
			reply.Answer = []dns.RR{&dns.A{
				Hdr: dns.RR_Header{Name: request.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
				A:   net.ParseIP("192.0.2.8"),
			}}
		}
		_ = writer.WriteMsg(reply)
	})
	udpServer := &dns.Server{PacketConn: packet, Handler: handler}
	tcpServer := &dns.Server{Listener: listener, Handler: handler}
	go func() { _ = udpServer.ActivateAndServe() }()
	go func() { _ = tcpServer.ActivateAndServe() }()
	t.Cleanup(func() {
		_ = udpServer.Shutdown()
		_ = tcpServer.Shutdown()
	})
	server, err := New(DefaultZone, []string{address})
	if err != nil {
		t.Fatal(err)
	}
	request := new(dns.Msg)
	request.SetQuestion("example.net.", dns.TypeA)
	recorder := &responseRecorder{}
	server.ServeDNS(recorder, request)
	if recorder.message == nil || recorder.message.Truncated || len(recorder.message.Answer) != 1 {
		t.Fatalf("forwarded reply = %#v", recorder.message)
	}
}

func TestDiscoveryRejectsPublicAddressesAndOversizedRequests(t *testing.T) {
	server, _ := New(DefaultZone, nil)
	name := ServiceName("example", "web")
	if err := server.Replace(Snapshot{
		Known:     map[string]struct{}{name: {}},
		Addresses: map[string][]netip.Addr{name: {netip.MustParseAddr("203.0.113.8")}},
	}); err == nil {
		t.Fatal("public service address was accepted")
	}
	request := new(dns.Msg)
	request.SetQuestion("example.net.", dns.TypeA)
	for index := 0; request.Len() <= maxRequestBytes; index++ {
		request.Extra = append(request.Extra, &dns.TXT{
			Hdr: dns.RR_Header{Name: "padding.example.net.", Rrtype: dns.TypeTXT, Class: dns.ClassINET},
			Txt: []string{"012345678901234567890123456789012345678901234567890123456789"},
		})
	}
	recorder := &responseRecorder{}
	server.ServeDNS(recorder, request)
	if recorder.message == nil || recorder.message.Rcode != dns.RcodeFormatError {
		t.Fatalf("oversized reply = %#v", recorder.message)
	}
}

func exchangeEventually(t *testing.T, client *dns.Client, request *dns.Msg, address string) *dns.Msg {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		reply, _, err := client.Exchange(request, address)
		if err == nil {
			return reply
		}
		lastErr = err
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("DNS exchange: %v", lastErr)
	return nil
}
