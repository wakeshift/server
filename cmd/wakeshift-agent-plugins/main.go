// Command wakeshift-agent-plugins writes Wakeshift's plugin marketplace,
// github.com/wakeshift/agent-plugins (plugins/agent-plugins), into a
// directory for one Wakeshift, keeping each plugin's pinned connector unless
// told to stamp it with a release's:
//
//	wakeshift-agent-plugins -dir out -issuer https://app.wakeshift.ai \
//	  [-stamp copilot,codex -version 2026.11.02.1912+abc1234 \
//	   -release-url https://github.com/wakeshift/connectors/releases/download/v1.4.21 \
//	   -sums SHA256SUMS]
//
// -sums is the release's SHA256SUMS (already checked against its Sigstore
// signature: .github/workflows/agent-plugins.yml). It prints the plugins
// whose files changed, one per line. The publishing pipeline runs it, and
// the bench, for a marketplace on this machine.
//
// With -plugin <harness>, it writes that harness's standalone plugin
// instead, at the directory's root (github.com/wakeshift/<harness>-plugin,
// such as antigravity-plugin); -stamp is then that harness or all.
//
// With -below <minimums> (WAKESHIFT_CONNECTOR_MIN_VERSIONS' value, e.g.
// "wakeshift=2026.10.30.0132,wakeshift:codex=2026.11.03"), it writes
// nothing: it prints the plugins in -dir, a rendered marketplace, that pin
// no connector or one older than the minimum for their harness.
package main

import (
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/wakeshift/server/internal/connectorversion"
	agentplugins "github.com/wakeshift/server/plugins/agent-plugins"
)

func main() {
	dir := flag.String("dir", "", "the marketplace's directory (a clone of it, to keep the pins it has)")
	issuer := flag.String("issuer", "", "the Wakeshift the plugins trust, e.g. https://app.wakeshift.ai")
	stamp := flag.String("stamp", "", "the plugins to pin to this release's connector, comma-separated, or all")
	ver := flag.String("version", "", "with -stamp: the release's connector version")
	rel := flag.String("release-url", "", "with -stamp: where the release's builds are, <url>/wakeshift-<os>-<arch>")
	sums := flag.String("sums", "", "with -stamp: the release's SHA256SUMS")
	plugin := flag.String("plugin", "", "write this harness's standalone plugin at -dir's root instead of the marketplace ("+strings.Join(agentplugins.Standalone(), ", ")+")")
	below := flag.String("below", "", "print the plugins in -dir pinned below these connector minimums, and write nothing")
	flag.Parse()
	if *below != "" {
		if *dir == "" {
			fail("-below needs -dir")
		}
		m, err := connectorversion.ParseMinimums(*below)
		if err != nil {
			fail(err.Error())
		}
		l, err := agentplugins.Below(*dir, m)
		if err != nil {
			fail(err.Error())
		}
		for _, h := range l {
			fmt.Println(h)
		}
		return
	}
	if *dir == "" || *issuer == "" {
		fail("-dir and -issuer are required")
	}
	if *plugin != "" {
		standalone(*dir, *issuer, *plugin, *stamp, *ver, *rel, *sums)
		return
	}
	pins := map[string]agentplugins.Pin{}
	if *stamp != "" {
		pin := readPin(*ver, *rel, *sums)
		list := strings.Split(*stamp, ",")
		if *stamp == "all" {
			list = agentplugins.Harnesses()
		}
		for _, h := range list {
			if !slices.Contains(agentplugins.Harnesses(), h) {
				fail(fmt.Sprintf("no plugin for %q (%s)", h, strings.Join(agentplugins.Harnesses(), ", ")))
			}
			pins[h] = pin
		}
	}
	changed, err := agentplugins.Render(*dir, *issuer, pins)
	if err != nil {
		fail(err.Error())
	}
	for _, h := range changed {
		fmt.Println(h)
	}
}

// standalone writes harness's standalone plugin at dir's root, pinned to the
// release if stamp names it (or is all), else keeping its pin.
func standalone(dir, issuer, harness, stamp, ver, rel, sums string) {
	if !slices.Contains(agentplugins.Standalone(), harness) {
		fail(fmt.Sprintf("no standalone plugin for %q (%s)", harness, strings.Join(agentplugins.Standalone(), ", ")))
	}
	var pin *agentplugins.Pin
	if stamp != "" {
		if stamp != "all" && !slices.Contains(strings.Split(stamp, ","), harness) {
			fail(fmt.Sprintf("-stamp %s doesn't name %s (with -plugin, -stamp is %s or all)", stamp, harness, harness))
		}
		p := readPin(ver, rel, sums)
		pin = &p
	}
	changed, err := agentplugins.RenderStandalone(dir, harness, issuer, pin)
	if err != nil {
		fail(err.Error())
	}
	if changed {
		fmt.Println(harness)
	}
}

// readPin is the release's pin from -version, -release-url and -sums.
func readPin(ver, rel, sums string) agentplugins.Pin {
	b, err := os.ReadFile(sums)
	if err != nil {
		fail("-stamp needs -sums: " + err.Error())
	}
	s, err := agentplugins.ParseSums(b)
	if err != nil {
		fail(err.Error())
	}
	pin := agentplugins.Pin{Version: ver, URL: strings.TrimSuffix(rel, "/"), Sums: s}
	if err := pin.Check(); err != nil {
		fail(err.Error())
	}
	return pin
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "wakeshift-agent-plugins:", msg)
	os.Exit(1)
}
