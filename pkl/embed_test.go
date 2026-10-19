package pklschema

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestModuleReaderServesOnlyApplicationSchema(t *testing.T) {
	reader := ModuleReader{}
	want, err := reader.Read(*mustParseURL(t, ApplicationURI))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(want, "module orch.Application") {
		t.Fatalf("embedded schema does not declare the application module")
	}

	for _, raw := range []string{
		"orch:/v1alpha1/../Application.pkl",
		"orch:/v1alpha1/Unknown.pkl",
		"orch://host/v1alpha1/Application.pkl",
		"orch:/v1alpha1/Application.pkl?variant=other",
		"orch:/v1alpha1/Application.pkl#fragment",
	} {
		t.Run(raw, func(t *testing.T) {
			if _, err := reader.Read(*mustParseURL(t, raw)); err == nil {
				t.Fatalf("Read(%q) error = nil", raw)
			}
		})
	}
}

func TestMirrorServesOnlyApplicationSchema(t *testing.T) {
	mirror, err := StartMirror()
	if err != nil {
		t.Fatal(err)
	}
	defer mirror.Close()
	base := mirror.Rewrites()[SchemaBaseURL]
	if !strings.HasPrefix(base, "http://127.0.0.1:") {
		t.Fatalf("mirror base = %q, want loopback", base)
	}
	if !strings.HasSuffix(ApplicationURL, "/pkl/v1alpha1/Application.pkl") || !strings.HasPrefix(ApplicationURL, SchemaBaseURL) {
		t.Fatalf("ApplicationURL %q does not point at the repository schema", ApplicationURL)
	}

	response, err := http.Get(base + "v1alpha1/Application.pkl")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || string(body) != string(ApplicationSource()) {
		t.Fatalf("schema response = %d, body matches = %v", response.StatusCode, string(body) == string(ApplicationSource()))
	}

	for _, path := range []string{"v1alpha1/Unknown.pkl", "tests/Application.pkl", "v1alpha1/"} {
		response, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("GET %s = %d, want 404", path, response.StatusCode)
		}
	}
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}
