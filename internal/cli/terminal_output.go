package cli

import (
	"io"
	"os"

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

func sanitizeTerminalOutput(data []byte) []byte {
	clean := make([]byte, 0, len(data))
	for index := 0; index < len(data); index++ {
		value := data[index]
		switch {
		case value == '\n' || value == '\t':
			clean = append(clean, value)
		case value == 0x1b:
			// Keep SGR color sequences: ESC [ digits and semicolons m.
			end := index + 2
			for end < len(data) && (data[end] == ';' || (data[end] >= '0' && data[end] <= '9')) {
				end++
			}
			if index+1 < len(data) && data[index+1] == '[' && end < len(data) && data[end] == 'm' {
				clean = append(clean, data[index:end+1]...)
				index = end
			}
		case value < 0x20 || value == 0x7f:
		case value == 0xc2 && index+1 < len(data) && data[index+1] >= 0x80 && data[index+1] <= 0x9f:
			// C1 control characters, which some terminals act on in UTF-8.
			index++
		default:
			clean = append(clean, value)
		}
	}
	return clean
}
