package cli

import (
	"io"
	"os"
	"unicode/utf8"

	"github.com/charmbracelet/x/term"
)

// writeWorkloadOutput writes log or command output from a workload. On a
// terminal it drops control characters and escape sequences other than
// colors, so a workload cannot move the cursor, retitle the window, or feed
// the operator's terminal other commands. Pipes and files get the bytes as
// they are.
func writeWorkloadOutput(output io.Writer, data []byte) (int, error) {
	if file, ok := output.(*os.File); !ok || !term.IsTerminal(file.Fd()) {
		return output.Write(data)
	}
	if _, err := output.Write(sanitizeTerminalOutput(data)); err != nil {
		return 0, err
	}
	return len(data), nil
}

// sanitizeTerminalOutput keeps one frame's printable text, newlines, tabs
// and SGR color sequences. It works on each frame alone, so it also drops
// invalid and truncated UTF-8: a C1 control split across two frames, such
// as 0xc2 ending stdout and 0x9b starting stderr, is two invalid bytes and
// never reaches the terminal as one character.
func sanitizeTerminalOutput(data []byte) []byte {
	clean := make([]byte, 0, len(data))
	for index := 0; index < len(data); {
		value, size := utf8.DecodeRune(data[index:])
		switch {
		case value == '\n' || value == '\t':
			clean = append(clean, data[index])
		case value == 0x1b:
			// Keep SGR color sequences: ESC [ digits and semicolons m.
			end := index + 2
			for end < len(data) && (data[end] == ';' || (data[end] >= '0' && data[end] <= '9')) {
				end++
			}
			if index+1 < len(data) && data[index+1] == '[' && end < len(data) && data[end] == 'm' {
				clean = append(clean, data[index:end+1]...)
				size = end + 1 - index
			}
		case value == utf8.RuneError && size == 1:
		case value < 0x20 || (value >= 0x7f && value <= 0x9f):
			// C0 and C1 control characters and DEL.
		default:
			clean = append(clean, data[index:index+size]...)
		}
		index += size
	}
	return clean
}
