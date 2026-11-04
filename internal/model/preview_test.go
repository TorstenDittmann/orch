package model

import "testing"

func TestPreviewBase(t *testing.T) {
	for name, want := range map[string]struct {
		base   string
		number int
		ok     bool
	}{
		"hello-pr-12":         {"hello", 12, true},
		"my-pr-app-pr-3":      {"my-pr-app", 3, true},
		"hello":               {},
		"hello-pr-":           {},
		"hello-pr-012":        {},
		"hello-pr-1x":         {},
		"-pr-1":               {},
		"hello-preview":       {},
		"hello-pr-1234567890": {},
	} {
		base, number, ok := PreviewBase(name)
		if base != want.base || number != want.number || ok != want.ok {
			t.Errorf("PreviewBase(%q) = %q, %d, %v", name, base, number, ok)
		}
	}
	if PreviewApplication("hello", 7) != "hello-pr-7" || PreviewSecretsApplication("hello") != "hello-preview" {
		t.Fatal("preview names")
	}
}
