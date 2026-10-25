package cli

import "testing"

func TestSanitizeTerminalOutputKeepsTextAndColorsOnly(t *testing.T) {
	cases := map[string]string{
		"plain line\n\tindented\n":    "plain line\n\tindented\n",
		"\x1b[31mred\x1b[0m done":     "\x1b[31mred\x1b[0m done",
		"\x1b]0;owned\x07title":       "]0;ownedtitle",
		"\x1b[2J\x1b[Hcleared":        "[2J[Hcleared",
		"over\rwrite\x08\x7f":         "overwrite",
		"c1 \xc2\x9b31m csi, ü stays": "c1 31m csi, ü stays",
		"trailing escape \x1b":        "trailing escape ",
		"unterminated color \x1b[31":  "unterminated color [31",
	}
	for input, want := range cases {
		if got := string(sanitizeTerminalOutput([]byte(input))); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", input, got, want)
		}
	}
}
