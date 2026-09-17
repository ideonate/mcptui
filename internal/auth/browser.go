package auth

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// openBrowser tries $BROWSER, the platform opener, then VS Code's
// `code --openExternal`. Failure is not fatal for callers.
func openBrowser(u string) error {
	var attempts [][]string
	if b := os.Getenv("BROWSER"); b != "" {
		for _, cand := range strings.Split(b, string(os.PathListSeparator)) {
			if cand = strings.TrimSpace(cand); cand == "" {
				continue
			}
			if strings.Contains(cand, "%s") {
				fields := strings.Fields(strings.ReplaceAll(cand, "%s", u))
				attempts = append(attempts, fields)
			} else {
				attempts = append(attempts, append(strings.Fields(cand), u))
			}
		}
	}
	switch runtime.GOOS {
	case "darwin":
		attempts = append(attempts, []string{"open", u})
	case "windows":
		attempts = append(attempts, []string{"rundll32", "url.dll,FileProtocolHandler", u})
	default:
		attempts = append(attempts, []string{"xdg-open", u})
	}
	if os.Getenv("TERM_PROGRAM") == "vscode" {
		attempts = append(attempts, []string{"code", "--openExternal", u})
	}
	var errs []error
	for _, argv := range attempts {
		if len(argv) == 0 {
			continue
		}
		if _, err := exec.LookPath(argv[0]); err != nil {
			errs = append(errs, err)
			continue
		}
		cmd := exec.Command(argv[0], argv[1:]...)
		if err := cmd.Start(); err != nil {
			errs = append(errs, err)
			continue
		}
		go func() { _ = cmd.Wait() }()
		return nil
	}
	if len(errs) == 0 {
		return errors.New("no browser opener available")
	}
	return errors.Join(errs...)
}

// OpenBrowser opens u in a browser (see openBrowser).
func OpenBrowser(u string) error { return openBrowser(u) }
