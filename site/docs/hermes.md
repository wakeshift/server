# Hermes Agent

Wakeshift wakes Hermes Agent chats through Wakeshift's plugin for Hermes. When one of your tools subscribes a Hermes chat, the plugin connects that chat to Wakeshift; when your event fires, it starts a new turn in the chat, even while it's idle, as long as Hermes is running. It wakes chats through Hermes's API server (the gateway), `hermes chat` CLI and TUI chats, Hermes Desktop chats, and direct messages with your Hermes bot on Telegram and Discord. It doesn't wake group chats, channels or threads on those platforms, or chats on other messaging platforms such as Slack: their messages wait until the chat next uses your tools, and the chat is told so when it subscribes.

In a terminal, chat in Hermes's TUI to see each woken turn as it runs: start it with `hermes --tui`, or set `display.interface: tui` in Hermes's config to make it the default. The classic `hermes chat` runs a woken turn and keeps it in the chat's history, but doesn't show it.

Your users install the plugin once, and it serves every app built on Wakeshift. Usually there's nothing for you to do: when a Hermes chat subscribes and the plugin isn't there, your tool's result asks the agent to offer the install to the user, with Hermes's own installer (the first command below). The agent runs it only if the user agrees. Hermes may hold the command for approval, and in a chat through Hermes's API server nobody can approve it, so the agent also gives the user the command to run where Hermes runs. This page is for setting it up ahead of time, and for shipping it with your own Hermes plugin.

## Install

Run one of these where Hermes runs: inside its container, if it's in one.

From the plugin's public repository, with Hermes's own installer:

```bash
hermes plugins install wakeshift/hermes-plugin --enable
```

Or with Wakeshift's installer, which installs the same plugin from Wakeshift:

```bash
curl -fsSL https://app.wakeshift.ai/install/hermes.sh -o wakeshift-hermes.sh && sh wakeshift-hermes.sh
```

Then start a new Hermes chat, or resume one with `hermes chat --continue` (`hermes chat --tui --continue` in the TUI). If Hermes's gateway is running, restart it: Hermes loads plugins when a chat starts.

**Let it start turns.** Hermes's `allow_gateway_injection` setting is how you let a plugin start turns in your chats, and Wakeshift's plugin starts none, in any chat (through Hermes's API server, `hermes chat`, Hermes Desktop, or a Telegram or Discord direct message), until it's on. Wakeshift's installer allows it for Wakeshift's plugin, and says so. After `hermes plugins install`, allow it yourself:

```bash
hermes config set plugins.entries.wakeshift.allow_gateway_injection true
```

It takes effect at once, without a restart. It lets the plugin start turns in your Hermes chats, and Wakeshift starts them only in conversations connected to it. Until it's on, a chat that connects is told that it can't be woken yet, with this command, and its messages wait until it's on (as long as Hermes is running).

**Hermes Desktop.** Installing the plugin from Hermes Desktop leaves `allow_gateway_injection` off, so a Desktop chat that connects is told it can't be woken yet. Turn it on where Hermes runs:

```bash
hermes config set plugins.entries.wakeshift.allow_gateway_injection true
```

Messages that arrived meanwhile then reach the chat, without a restart.

**Pausing Hermes.** `hermes pause` holds new messages from people, but it doesn't hold the turns Wakeshift's plugin starts, in any kind of chat. To stop woken turns for a while, turn the setting off (`hermes config set plugins.entries.wakeshift.allow_gateway_injection false`); messages then wait, and arrive once you turn it back on.

**Hermes in Docker.** Run the install inside the container as the user Hermes runs as, so the gateway owns the files: `docker exec -u hermes <container> hermes plugins install wakeshift/hermes-plugin --enable`, then `docker exec -u hermes <container> hermes config set plugins.entries.wakeshift.allow_gateway_injection true` and `docker restart <container>`. Wakeshift's installer, run as root on a Hermes home that belongs to another user, switches to that user by itself.

Keep Hermes's data (its `HERMES_HOME`) in a Docker volume. On macOS, a folder from your Mac mounted into the container doesn't keep the file permissions Hermes needs, and its gateway fails with "gateway.sock has mode 0755, expected 0600". Hermes 0.23.0 starts no chat until a model is set: run `hermes model` once, in the container (`docker exec -it -u hermes <container> hermes model`).

**Updating.** When the plugin is too old for Wakeshift, your tool's result asks the agent to offer the update, the same way as the install, with Hermes's own installer. It works however the plugin was installed, and keeps your settings and connected chats:

```bash
hermes plugins install wakeshift/hermes-plugin --enable --force
```

Then resume the chat or start a new one, as after the install. `hermes plugins update wakeshift` works too, but only for a plugin installed with `hermes plugins install` or from Hermes's catalog. When the plugin itself is told it's out of date, it gives the one command that fits how it was installed. If you installed it with Wakeshift's installer from a Wakeshift other than `https://app.wakeshift.ai`, set the plugin's `issuers` setting (below) as well: the update replaces the `config.json` that installer wrote.

The plugin's code is open source, under the Apache License 2.0, at [github.com/wakeshift/hermes-plugin](https://github.com/wakeshift/hermes-plugin). Its README lists everything it does on the user's machine.

## Which Wakeshift the plugin trusts

The plugin acts only on messages from the Wakeshift servers it trusts. By default that's `https://app.wakeshift.ai`. To trust others instead, set the plugin's `issuers` setting, in Hermes's `config.yaml`:

```yaml
plugins:
  entries:
    wakeshift:
      settings:
        issuers:
          - https://app.wakeshift.ai
          - https://wakeshift.example.com
```

- Setting `issuers` replaces the default, so list `https://app.wakeshift.ai` too if you still want it.
- Each entry is an `https://` address with no path. Plain `http://` works only for a Wakeshift on the same machine (`localhost`, `127.0.0.1`, `::1`).
- Wakeshift's installer also records the server it came from in the plugin's `config.json`, which the plugin trusts in addition.
- A change takes effect in new chats, or after a gateway restart.

## The installer's options and exit codes

`hermes.sh` takes two options, also available as environment variables:

| Option | Variable | What it does |
| --- | --- | --- |
| `--chained` | `WAKESHIFT_CHAINED=1` | For another installer that runs this one before any chat: the closing line tells a person to start a new chat or restart the gateway, rather than telling an agent what to say mid-chat. |
| `--quiet` | `WAKESHIFT_QUIET=1` | Prints only one line saying what happened; errors still go to stderr. Implies `--chained`. |

| Exit code | Meaning |
| --- | --- |
| 0 | Installed (or updated) and enabled. Its output says whether the plugin may start turns in your chats (above): it turns that on unless it was turned off. |
| 1 | Refused, and nothing changed: a plugin named `wakeshift` is there that isn't Wakeshift's, or its `config.json` can't be updated. |
| 2 | The shell's own: a syntax error, such as a script cut short by a failed download. Nothing ran. |
| 3 | Written, but not enabled: Hermes's command wasn't found, or enabling the plugin failed. Run `hermes plugins enable wakeshift` where Hermes runs. |
| 4 | The download failed or couldn't be unpacked. Nothing changed. |
| 5 | A tool it needs is missing (`curl`, `tar`, `mktemp`, `sha256sum` or `shasum`). Nothing changed. |
| 6 | A permission problem: it can't write `$HERMES_HOME/plugins/wakeshift`, or it runs as root and can't switch to the Hermes home's owner. Nothing changed. |
| 7 | The plugin it downloaded didn't match the checksum the script carries, so it may have been tampered with. Nothing changed. Don't retry blindly: tell the user. |
| 64 | An option it doesn't know. |

Download the script, then run it, as above. Piped into `sh` (`curl … | sh`), a failed download runs an empty script, which exits 0, so the failure goes unnoticed.

Running the installer again is safe: it updates the plugin in place and keeps its settings, including `allow_gateway_injection` if you turned it off. Set `WAKESHIFT_AS_ROOT=1` to install as root on purpose.

## Bundling Wakeshift with your Hermes plugin

If your app ships a Hermes plugin, you can install Wakeshift's plugin with it, so your users install once. Hermes has no way for one plugin to install another, so your install step runs both.

### In your README

Show both commands, in this order, so users see what gets installed:

```bash
hermes plugins install <your plugin> --enable
hermes plugins install wakeshift/hermes-plugin --enable
hermes config set plugins.entries.wakeshift.allow_gateway_injection true
```

The last line lets Wakeshift's plugin start turns in your users' chats, which it needs to wake any of them (above); say so next to it.

Hermes scans every file of a plugin when it installs it. A `curl <url> | sh` line in your README makes the scan's verdict "caution", which blocks installing your plugin from its repository without `--force`, and the same line in a `.sh` file in your repository fails catalog review. If you show Wakeshift's installer, use the download-then-run form above, which scans clean.

### A one-line installer on your site

For a single command, host a script like this on your website, not in your plugin's repository:

```sh
#!/bin/sh
# Installs <Your app> for Hermes Agent, with Wakeshift's plugin for Hermes, so
# <Your app> can wake your Hermes chats. Run it where Hermes runs.
set -eu

echo "This installs two Hermes plugins:"
echo "  1. <Your app>"
echo "  2. Wakeshift's plugin for Hermes, which lets <Your app> (and other apps that use Wakeshift) wake your chats"

hermes plugins install <your plugin> --enable

# Download, then run: piped into sh, a failed download goes unnoticed.
tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT
code=0
if curl -fsSL https://app.wakeshift.ai/install/hermes.sh -o "$tmp"; then
  sh "$tmp" --chained </dev/null || code=$?
else
  code=4
fi
case "$code" in
0) echo "<Your app> is ready. Start a new Hermes chat to use it." ;;
3) echo "Wakeshift's plugin is installed but not enabled: run 'hermes plugins enable wakeshift' where Hermes runs." >&2; exit 2 ;;
*) echo "<Your app> is installed, but Wakeshift's plugin isn't (exit $code), so <Your app> can't wake your chats yet." >&2; exit 2 ;;
esac
```

Use `--quiet` instead of `--chained` for less output.

### If your plugin is a native one

A native plugin (`plugin.yaml` and `__init__.py`) can declare that it uses Wakeshift's plugin, and check for it when it loads:

```yaml
# plugin.yaml
requires_plugins:
  - id: wakeshift
    version_range: ">=2026.10.31"
```

```python
# __init__.py
def register(ctx):
    if not ctx.has_plugin("wakeshift"):
        log.warning("Wakeshift's plugin for Hermes isn't enabled, so <Your app> can't wake your chats: https://wakeshift.ai/docs/hermes")
```

- Declare `requires_plugins`: Hermes then loads `wakeshift` first. Without it, Hermes loads plugins in alphabetical order, and `has_plugin("wakeshift")` can be false even when it's installed.
- Hermes never installs what `requires_plugins` names; it only logs a warning when it's missing.
- The version of Wakeshift's plugin is its build's, `YYYY.MM.DD.HHMM+<commit>`, and increases with each release.
- A portable plugin (`plugin.json`) can't declare it. You don't need to: when Wakeshift's plugin is missing, your tool's result asks the agent to offer it.

### Consent and terms

- Your install step is where your users agree to the install, so say up front that it installs Wakeshift's plugin, as the script above does. Never install Wakeshift's plugin from your plugin's code or skills without asking.
- Wakeshift's installer shows your users no legal notice. Your terms carry Wakeshift's [Connector Terms](https://wakeshift.ai/connector-terms) and [Privacy Policy](https://wakeshift.ai/privacy) to them, as the [Terms of Service](https://wakeshift.ai/terms#your-users) set out.
- Removing your plugin must leave Wakeshift's in place: other apps may rely on it.

## Uninstall

```bash
hermes plugins remove wakeshift
```

Then delete the plugin's state, the `wakeshift` folder in the Hermes home (`$HERMES_HOME/wakeshift`; by default `~/.hermes/wakeshift`), and remove the setting that let it start turns: `hermes config unset plugins.entries.wakeshift.allow_gateway_injection`. Run these where Hermes runs.
