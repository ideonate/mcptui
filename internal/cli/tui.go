package cli

import (
	"github.com/ideonate/mcptui/internal/config"
	"github.com/ideonate/mcptui/internal/tui"
)

func runTUI(g *globals, p *config.Profile, cfg *config.Config) error {
	return tui.Run(tui.Options{
		Profile:      p,
		Config:       cfg,
		NoBrowser:    g.noBrowser,
		CallbackHost: g.callbackHost,
		NoHistory:    g.noHistory,
		NoMouse:      g.noMouse,
	})
}
