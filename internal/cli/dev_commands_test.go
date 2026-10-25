package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/TorstenDittmann/orch/internal/config"
)

func TestLocalizeDevManifestServesDomainsOnLocalhost(t *testing.T) {
	publicPort, nodePort := 7000, 7001
	manifest := &config.Manifest{
		Name: "shop",
		Services: map[string]config.Service{
			"web": {
				HTTP:        &config.HTTP{Port: 8080, Domains: []string{"shop.example.com", "www.shop.example.com"}, HTTPS: true},
				Constraints: map[string]string{"region": "eu-west"},
			},
			"internal": {HTTP: &config.HTTP{Port: 8080, HTTPS: true}},
			"game": {
				TCP: &config.TCP{Port: 7777, PublicPort: &publicPort, NodePort: &nodePort},
				UDP: &config.UDP{Port: 7777, NodePort: 7777},
			},
		},
		CronJobs: map[string]config.CronJob{"backup": {Constraints: map[string]string{"storage": "fast"}}},
	}
	urls, ports := localizeDevManifest(manifest, 8080)

	if want := []string{"http://web.shop.localhost:8080"}; !reflect.DeepEqual(urls, want) {
		t.Fatalf("urls = %v, want %v", urls, want)
	}
	web := manifest.Services["web"]
	if !reflect.DeepEqual(web.HTTP.Domains, []string{"web.shop.localhost"}) || web.HTTP.HTTPS || web.Constraints != nil {
		t.Fatalf("web = %#v", web)
	}
	if internal := manifest.Services["internal"].HTTP; len(internal.Domains) != 0 || !internal.HTTPS {
		t.Fatalf("internal service without domains changed: %#v", internal)
	}
	if manifest.CronJobs["backup"].Constraints != nil {
		t.Fatal("cron job constraints were kept")
	}
	want := []devPort{{"tcp", 7000, "game"}, {"tcp", 7001, "game"}, {"udp", 7777, "game"}}
	if !reflect.DeepEqual(ports, want) {
		t.Fatalf("ports = %v, want %v", ports, want)
	}
	if got := ports[2].publish(); got != "127.0.0.1:7777:7777/udp" {
		t.Fatalf("publish = %q", got)
	}
}

func TestLocalizeDevManifestPublicServiceAndDefaultHTTPPort(t *testing.T) {
	manifest := &config.Manifest{Name: "site", Services: map[string]config.Service{
		"web": {HTTP: &config.HTTP{Port: 8080, Public: true}},
	}}
	urls, _ := localizeDevManifest(manifest, 80)
	if want := []string{"http://localhost (web)", "http://web.site.localhost"}; !reflect.DeepEqual(urls, want) {
		t.Fatalf("urls = %v, want %v", urls, want)
	}
}

func TestReadDevEnvFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), devEnvFile)
	contents := "# local secrets\n\ndatabase-url=libsql://localhost:8080?a=b\napi-token = \"two words\\n\"\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	values, err := readDevEnvFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"database-url": "libsql://localhost:8080?a=b", "api-token": "two words\n"}
	if !reflect.DeepEqual(values, want) {
		t.Fatalf("values = %q, want %q", values, want)
	}
	if values, err := readDevEnvFile(filepath.Join(t.TempDir(), "missing")); err != nil || len(values) != 0 {
		t.Fatalf("missing file = %v, %v", values, err)
	}
	for _, invalid := range []string{"no-separator\n", "empty=\n", "=value\n"} {
		if err := os.WriteFile(path, []byte(invalid), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := readDevEnvFile(path); err == nil {
			t.Fatalf("%q was accepted", invalid)
		}
	}
}

func TestLocalizeDevManifestGivesEachHTTPPortALocalDomain(t *testing.T) {
	manifest := &config.Manifest{Name: "storage", Services: map[string]config.Service{
		"rustfs": {HTTP: &config.HTTP{Port: 9000, Domains: []string{"console.example.com", "s3.example.com"}, HTTPS: true,
			DomainPorts: map[string]int{"console.example.com": 9001}}},
	}}
	urls, _ := localizeDevManifest(manifest, 8080)
	if want := []string{"http://rustfs.storage.localhost:8080", "http://rustfs-9001.storage.localhost:8080"}; !reflect.DeepEqual(urls, want) {
		t.Fatalf("urls = %v, want %v", urls, want)
	}
	http := manifest.Services["rustfs"].HTTP
	if !reflect.DeepEqual(http.Domains, []string{"rustfs-9001.storage.localhost", "rustfs.storage.localhost"}) ||
		!reflect.DeepEqual(http.DomainPorts, map[string]int{"rustfs-9001.storage.localhost": 9001}) {
		t.Fatalf("http = %#v", http)
	}
}
