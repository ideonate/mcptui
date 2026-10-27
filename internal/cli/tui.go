package cli

import (
	"fmt"
	"os"

	"github.com/ideonate/mcptui/internal/config"
	"github.com/ideonate/mcptui/internal/tui"
)

func runTUI(g *globals, p *config.Profile, cfg *config.Config) error {
	light, err := lightTheme(os.Getenv("MCPTUI_THEME"))
	if err != nil {
		return err
	}
	return tui.Run(tui.Options{
		Profile:      p,
		Config:       cfg,
		NoBrowser:    g.noBrowser,
		CallbackHost: g.callbackHost,
		NoHistory:    g.noHistory,
		NoMouse:      g.noMouse,
		Light:        light,
	})
}

// lightTheme reads $MCPTUI_THEME. The theme is never detected: asking the
// terminal for its background blocks on terminals that don't answer (web
// terminals, some relays), so it is dark unless set to "light".
func lightTheme(v string) (bool, error) {
	switch v {
	case "", "dark":
		return false, nil
	case "light":
		return true, nil
	}
	return false, fmt.Errorf("MCPTUI_THEME=%q: want \"dark\" or \"light\"", v)
}
