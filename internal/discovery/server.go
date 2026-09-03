package discovery

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
)

const (
	DefaultZone          = "svc.internal."
	RecordTTL            = 5
	maxForwarders        = 3
	maxConcurrentQueries = 128
	maxTCPQueries        = 32
	udpMessageSize       = 1232
	maxRequestBytes      = 4 << 10
	maxConfiguredServers = 16
)

type Snapshot struct {
	Known     map[string]struct{}
	Addresses map[string][]netip.Addr
}

type Server struct {
	mu         sync.RWMutex
	zone       string
	snapshot   Snapshot
	forwarders []string
	udp        *dns.Server
	tcp        *dns.Server
	queries    chan struct{}
	lifecycle  sync.Mutex
}

func New(zone string, forwarders []string) (*Server, error) {
	zone = strings.ToLower(dns.Fqdn(zone))
	if zone != DefaultZone {
		return nil, fmt.Errorf("private DNS zone must be %s", DefaultZone)
	}
	clean := make([]string, 0, len(forwarders))
	seen := make(map[string]struct{})
	for _, value := range forwarders {
		host, port, err := net.SplitHostPort(value)
		portNumber, portErr := strconv.ParseUint(port, 10, 16)
		if err != nil || net.ParseIP(host) == nil || portErr != nil || portNumber == 0 {
			return nil, fmt.Errorf("invalid DNS forwarder %q", value)
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		clean = append(clean, value)
		if len(clean) == maxConfiguredServers {
			break
		}
	}
	return &Server{
		zone: zone, forwarders: clean,
		snapshot: Snapshot{Known: map[string]struct{}{}, Addresses: map[string][]netip.Addr{}},
		queries:  make(chan struct{}, maxConcurrentQueries),
	}, nil
}

func ServiceName(application, service string) string {
	return strings.ToLower(service + "." + application + "." + DefaultZone)
}

func SystemForwarders(path string) ([]string, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read host resolver configuration: %w", err)
	}
	var result []string
	seen := make(map[string]struct{})
	for _, line := range strings.Split(string(contents), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "nameserver" {
			continue
		}
		address, err := netip.ParseAddr(fields[1])
		if err != nil {
			continue
		}
		value := net.JoinHostPort(address.String(), "53")
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("host resolver configuration has no usable nameserver")
	}
	return result, nil
}

func (s *Server) Replace(snapshot Snapshot) error {
	known := make(map[string]struct{}, len(snapshot.Known))
	addresses := make(map[string][]netip.Addr, len(snapshot.Addresses))
	for name := range snapshot.Known {
		canonical := strings.ToLower(dns.Fqdn(name))
		if !dns.IsSubDomain(s.zone, canonical) || canonical == s.zone {
			return fmt.Errorf("DNS record %q is outside %s", name, s.zone)
		}
		known[canonical] = struct{}{}
	}
	for name, values := range snapshot.Addresses {
		canonical := strings.ToLower(dns.Fqdn(name))
		if _, ok := known[canonical]; !ok {
			return fmt.Errorf("DNS addresses for unknown record %q", name)
		}
		seen := make(map[netip.Addr]struct{})
		copyValues := make([]netip.Addr, 0, len(values))
		for _, address := range values {
			if !address.Is4() || !address.IsPrivate() {
				return fmt.Errorf("DNS record %q has a non-private IPv4 address", name)
			}
			if _, exists := seen[address]; exists {
				continue
			}
			seen[address] = struct{}{}
			copyValues = append(copyValues, address)
		}
		sort.Slice(copyValues, func(i, j int) bool { return copyValues[i].Less(copyValues[j]) })
		addresses[canonical] = copyValues
	}
	s.mu.Lock()
	s.snapshot = Snapshot{Known: known, Addresses: addresses}
	s.mu.Unlock()
	return nil
}

func (s *Server) ServeDNS(response dns.ResponseWriter, request *dns.Msg) {
	if request != nil && request.Len() > maxRequestBytes {
		failure := new(dns.Msg)
		failure.SetRcode(request, dns.RcodeFormatError)
		writeMessage(response, request, failure)
		return
	}
	select {
	case s.queries <- struct{}{}:
		defer func() { <-s.queries }()
	default:
		failure := new(dns.Msg)
		if request != nil {
			failure.SetRcode(request, dns.RcodeServerFailure)
		}
		writeMessage(response, request, failure)
		return
	}
	if request == nil || len(request.Question) != 1 {
		failure := new(dns.Msg)
		if request != nil {
			failure.SetRcode(request, dns.RcodeFormatError)
		}
		writeMessage(response, request, failure)
		return
	}
	question := request.Question[0]
	name := strings.ToLower(dns.Fqdn(question.Name))
	if dns.IsSubDomain(s.zone, name) {
		s.servePrivate(response, request, name, question.Qtype)
		return
	}
	s.forward(response, request)
}

func (s *Server) servePrivate(response dns.ResponseWriter, request *dns.Msg, name string, qtype uint16) {
	message := new(dns.Msg)
	message.SetReply(request)
	message.Authoritative = true
	s.mu.RLock()
	_, known := s.snapshot.Known[name]
	addresses := append([]netip.Addr(nil), s.snapshot.Addresses[name]...)
	s.mu.RUnlock()
	if !known {
		message.Rcode = dns.RcodeNameError
		writeMessage(response, request, message)
		return
	}
	if qtype == dns.TypeA {
		for _, address := range addresses {
			message.Answer = append(message.Answer, &dns.A{
				Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: RecordTTL},
				A:   net.IP(address.AsSlice()),
			})
		}
	}
	writeMessage(response, request, message)
}

func (s *Server) forward(response dns.ResponseWriter, request *dns.Msg) {
	udpClient := &dns.Client{Net: "udp", Timeout: time.Second, UDPSize: udpMessageSize}
	tcpClient := &dns.Client{Net: "tcp", Timeout: time.Second}
	s.mu.RLock()
	forwarders := append([]string(nil), s.forwarders...)
	s.mu.RUnlock()
	for _, server := range forwarders {
		reply, _, err := udpClient.Exchange(request.Copy(), server)
		if err == nil && reply != nil && reply.Truncated {
			reply, _, err = tcpClient.Exchange(request.Copy(), server)
		}
		if err == nil && reply != nil {
			writeMessage(response, request, reply)
			return
		}
	}
	failure := new(dns.Msg)
	failure.SetRcode(request, dns.RcodeServerFailure)
	writeMessage(response, request, failure)
}

func writeMessage(response dns.ResponseWriter, request, message *dns.Msg) {
	if response != nil && strings.HasPrefix(response.LocalAddr().Network(), "udp") {
		size := uint16(512)
		if request != nil {
			if option := request.IsEdns0(); option != nil && option.UDPSize() > size {
				size = option.UDPSize()
			}
		}
		if size > udpMessageSize {
			size = udpMessageSize
		}
		message.Truncate(int(size))
	}
	_ = response.WriteMsg(message)
}

func (s *Server) Start(address string) error {
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	if strings.TrimSpace(address) == "" {
		return fmt.Errorf("DNS listen address is required")
	}
	if s.udp != nil || s.tcp != nil {
		return fmt.Errorf("DNS server is already started")
	}
	if err := s.excludeForwardingLoop(address); err != nil {
		return err
	}
	tcpListener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("listen for workload DNS over TCP: %w", err)
	}
	udpAddress := address
	host, port, _ := net.SplitHostPort(address)
	if port == "0" {
		udpAddress = net.JoinHostPort(host, strconv.Itoa(tcpListener.Addr().(*net.TCPAddr).Port))
	}
	udpPacket, err := net.ListenPacket("udp", udpAddress)
	if err != nil {
		_ = tcpListener.Close()
		return fmt.Errorf("listen for workload DNS over UDP: %w", err)
	}
	type startEvent struct {
		protocol string
		started  bool
		err      error
	}
	events := make(chan startEvent, 4)
	udpServer := &dns.Server{PacketConn: udpPacket, Handler: s, UDPSize: udpMessageSize,
		NotifyStartedFunc: func() { events <- startEvent{protocol: "UDP", started: true} },
		ReadTimeout:       time.Second, WriteTimeout: time.Second}
	tcpServer := &dns.Server{Listener: tcpListener, Handler: s, MaxTCPQueries: maxTCPQueries,
		NotifyStartedFunc: func() { events <- startEvent{protocol: "TCP", started: true} },
		ReadTimeout:       time.Second, WriteTimeout: time.Second, IdleTimeout: func() time.Duration { return time.Second }}
	s.udp = udpServer
	s.tcp = tcpServer
	go func() {
		if err := udpServer.ActivateAndServe(); err != nil {
			events <- startEvent{protocol: "UDP", err: err}
		}
	}()
	go func() {
		if err := tcpServer.ActivateAndServe(); err != nil {
			events <- startEvent{protocol: "TCP", err: err}
		}
	}()
	started := 0
	for started != 2 {
		event := <-events
		if event.err != nil {
			_ = udpPacket.Close()
			_ = tcpListener.Close()
			s.udp = nil
			s.tcp = nil
			return fmt.Errorf("start workload DNS over %s: %w", event.protocol, event.err)
		}
		if event.started {
			started++
		}
	}
	return nil
}

func (s *Server) excludeForwardingLoop(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("invalid DNS listen address %q", address)
	}
	listenAddress, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("DNS listen address must use a literal IP")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	filtered := make([]string, 0, len(s.forwarders))
	for _, value := range s.forwarders {
		forwardHost, _, _ := net.SplitHostPort(value)
		forwardAddress, _ := netip.ParseAddr(forwardHost)
		if forwardAddress == listenAddress {
			continue
		}
		filtered = append(filtered, value)
	}
	if len(filtered) > maxForwarders {
		filtered = filtered[:maxForwarders]
	}
	s.forwarders = filtered
	return nil
}

func (s *Server) Close(ctx context.Context) error {
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	var errors []error
	if s.udp != nil {
		errors = append(errors, s.udp.ShutdownContext(ctx))
	}
	if s.tcp != nil {
		errors = append(errors, s.tcp.ShutdownContext(ctx))
	}
	s.udp = nil
	s.tcp = nil
	for _, err := range errors {
		if err != nil {
			return err
		}
	}
	return nil
}
