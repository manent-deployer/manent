package app

import (
	"testing"

	"github.com/manent-deployer/manent/internal/config"
)

func TestUIConfigAudience(t *testing.T) {
	cfg := config.Default()
	cfg.Auth.Mode = config.AuthModeEnabled
	cfg.Auth.OIDC = &config.OIDC{Issuer: "http://127.0.0.1:4444", Audience: "manent", UsernameClaim: "sub", UI: &config.OIDCUI{ClientID: "ui"}}
	if ui := NewUIConfig(cfg); ui.Audience != "" {
		t.Errorf("audience without ui.audience: %q", ui.Audience)
	}
	cfg.Auth.OIDC.UI.Audience = "manent"
	if ui := NewUIConfig(cfg); ui.Audience != "manent" || ui.ClientID != "ui" {
		t.Errorf("ui config: %+v", ui)
	}
}
