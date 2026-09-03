package pklschema

import (
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

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}
