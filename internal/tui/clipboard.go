package tui

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// copyNative tries a platform clipboard tool, in addition to the OSC 52
// sequence the caller emits. It reports the tool used, or "".
func copyNative(text string) string {
	var tools [][]string
	switch runtime.GOOS {
	case "darwin":
		tools = [][]string{{"pbcopy"}}
	case "windows":
		tools = [][]string{{"clip"}}
	default:
		if os.Getenv("WAYLAND_DISPLAY") != "" {
			tools = append(tools, []string{"wl-copy"})
		}
		if os.Getenv("DISPLAY") != "" {
			tools = append(tools, []string{"xclip", "-selection", "clipboard"}, []string{"xsel", "--clipboard", "--input"})
		}
		tools = append(tools, []string{"clip.exe"}) // WSL
	}
	for _, t := range tools {
		if _, err := exec.LookPath(t[0]); err != nil {
			continue
		}
		cmd := exec.Command(t[0], t[1:]...)
		cmd.Stdin = strings.NewReader(text)
		if cmd.Run() == nil {
			return t[0]
		}
	}
	return ""
}

// copyStatus copies natively (best effort) and describes what happened.
func copyStatus(text string) string {
	if tool := copyNative(text); tool != "" {
		return "copied to clipboard (" + tool + ")"
	}
	return "copied via OSC 52 — if nothing was copied, your terminal may not allow it; use the cat command above"
}
