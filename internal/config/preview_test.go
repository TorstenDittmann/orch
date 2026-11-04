package config

import (
	"slices"
	"testing"
)

func TestPreviewRenamesPrefixesDomainsAndDropsProductionOnlySettings(t *testing.T) {
	public, node := 5432, 30000
	manifest := &Manifest{
		Name: "hello",
		Services: map[string]Service{
			"web": {Replicas: 4, Autoscale: &Autoscale{Min: 2, Max: 8, CPUPercent: 70}, HTTP: &HTTP{
				Port: 8080, Public: true, HTTPS: true, HealthPath: "/",
				Domains:     []string{"example.com", "www.example.com", "*.apps.example.com", "api.example.com"},
				Redirects:   map[string]string{"www.example.com": "example.com"},
				DomainPorts: map[string]int{"api.example.com": 9090},
			}},
			"db":  {Replicas: 1, TCP: &TCP{Port: 5432, PublicPort: &public, NodePort: &node}},
			"dns": {Replicas: 2, UDP: &UDP{Port: 53, NodePort: 5353}},
		},
		Volumes:       map[string]Volume{"data": {Driver: "local", Backup: &VolumeBackup{Schedule: "0 3 * * *", Keep: 7}}},
		Notifications: []Notification{{}},
		Deploy:        Deployment{AutoUpdate: true},
	}
	urls, err := Preview(manifest, 12)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"https://pr-12.api.example.com", "https://pr-12.example.com"}; !slices.Equal(urls, want) {
		t.Fatalf("urls = %v, want %v", urls, want)
	}
	if manifest.Name != "hello-pr-12" {
		t.Fatalf("name = %s", manifest.Name)
	}
	web := manifest.Services["web"]
	if web.Replicas != 1 || web.Autoscale != nil || web.HTTP.Public {
		t.Fatalf("web = %+v", web)
	}
	if want := []string{"pr-12.example.com", "pr-12.www.example.com", "*.pr-12.apps.example.com", "pr-12.api.example.com"}; !slices.Equal(web.HTTP.Domains, want) {
		t.Fatalf("domains = %v", web.HTTP.Domains)
	}
	if web.HTTP.Redirects["pr-12.www.example.com"] != "pr-12.example.com" || web.HTTP.DomainPorts["pr-12.api.example.com"] != 9090 {
		t.Fatalf("redirects = %v ports = %v", web.HTTP.Redirects, web.HTTP.DomainPorts)
	}
	if db := manifest.Services["db"]; db.TCP.PublicPort != nil || db.TCP.NodePort != nil || db.TCP.Port != 5432 {
		t.Fatalf("db = %+v", db.TCP)
	}
	if manifest.Services["dns"].UDP != nil || manifest.Volumes["data"].Backup != nil || manifest.Notifications != nil || manifest.Deploy.AutoUpdate {
		t.Fatalf("manifest kept production-only settings: %+v", manifest)
	}
	if _, err := Preview(&Manifest{Name: "a-very-long-application-name-that-almost-fills-a-dns-label-xx"}, 123456); err == nil {
		t.Fatal("preview name over 63 bytes accepted")
	}
}
