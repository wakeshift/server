package main

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/wakeshift/server/internal/config"
	"github.com/wakeshift/server/internal/harnesses"
	"github.com/wakeshift/server/internal/npmtag"
)

// packageSource is which version of a harness's npm package the install
// offers give (WAKESHIFT_OPENCODE_PLUGIN, WAKESHIFT_PI_PLUGIN,
// WAKESHIFT_OPENCLAW_PLUGIN, WAKESHIFT_CONNECT_PACKAGE): a fixed version, or a dist-tag to look up;
// neither for Wakeshift's installer. Without the setting, only production
// offers its packages (they trust production), at "latest".
func packageSource(setting, publicURL string) (version, tag string) {
	switch {
	case setting == "off":
		return "", ""
	case setting == "":
		if publicURL != harnesses.ProductionIssuer {
			return "", ""
		}
		return "", "latest"
	case harnesses.ValidPackageVersion(setting):
		return setting, ""
	}
	return "", setting
}

// packageSetting is the setting for one of Wakeshift's npm packages:
// opencode's and OpenClaw's plugins, pi's extension, and @wakeshift/connect (Cursor's npx
// command).
func packageSetting(cfg config.Config, pkg string) string {
	switch pkg {
	case harnesses.ConnectPackage:
		return cfg.ConnectPackage
	case "@wakeshift/openclaw":
		return cfg.OpenclawPlugin
	case "@wakeshift/opencode":
		return cfg.OpencodePlugin
	case "@wakeshift/pi":
		return cfg.PiPlugin
	}
	return "off"
}

// offerPackages sets the version of each harness's npm package the offers
// give, keeping it current with its dist-tag in the background: one lookup
// per package, for every harness that offers it.
func offerPackages(ctx context.Context, cfg config.Config) {
	ids := map[string][]string{} // package → harness IDs
	for _, h := range harnesses.All() {
		if h.Package != "" && h.PackageCommand != "" {
			ids[h.Package] = append(ids[h.Package], h.ID)
		}
	}
	hc := &http.Client{Timeout: 20 * time.Second}
	for pkg, l := range ids {
		set := func(v string) {
			for _, id := range l {
				harnesses.SetPackageVersion(id, v)
			}
		}
		version, tag := packageSource(packageSetting(cfg, pkg), cfg.PublicURL)
		if version != "" {
			set(version)
		}
		if tag != "" {
			go npmtag.Watch(ctx, hc, npmtag.Registry, pkg, tag, 15*time.Minute, offerable(tag), set)
		}
	}
}

// offerable is which versions a dist-tag may point the offers at. Releases
// put only X.Y.Z on "latest" (a prerelease goes on "next"), so there a
// prerelease is refused: npm also points "latest" at a package's first
// version whatever its tag, such as the placeholder published to set up
// trusted publishing (0.0.0-placeholder).
func offerable(tag string) func(string) bool {
	return func(v string) bool {
		return harnesses.ValidPackageVersion(v) && (tag != "latest" || !strings.Contains(v, "-"))
	}
}
