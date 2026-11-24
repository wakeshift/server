package botkeys_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/wakeshift/server/internal/botkeys"
	"github.com/wakeshift/server/internal/connector"
	"github.com/wakeshift/server/internal/downloads"
	"github.com/wakeshift/server/internal/harnesses"
)

// What the connector signs is what the core verifies: the bot's ID and
// the challenge, in botkeys.Message's shape.
func TestConnectorSignsWhatTheCoreVerifies(t *testing.T) {
	const bot = "4f1c2b3a-9d8e-4c7b-a6f5-0e1d2c3b4a59"
	ch := botkeys.Format(botkeys.Setup, "0b8f3c1e-2d4a-4b6c-8e0f-1a2b3c4d5e6f", "5e6f7a8b-9c0d-4e1f-a2b3-c4d5e6f7a8b9", []byte("0123456789abcdef"))
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	if string(connector.GrokMessage(bot, ch)) != string(botkeys.Message(bot, ch)) {
		t.Fatal("the connector's signed text isn't botkeys.Message")
	}
	if !botkeys.Verify(pub, bot, ch, ed25519.Sign(priv, connector.GrokMessage(bot, ch))) {
		t.Fatal("a signature by the connector doesn't verify")
	}
	// The v2 register proof and the fetch: one fixed vector, the same on
	// both sides.
	const ws, app = "0b8f3c1e-2d4a-4b6c-8e0f-1a2b3c4d5e6f", "ci"
	want := "wakeshift-proof-v2\n" + bot + "\n" + ch + "\n" + ws + "\n" + app
	if string(botkeys.RegisterMessage(bot, ch, ws, app)) != want || string(connector.GrokRegisterMessage(bot, ch, ws, app)) != want {
		t.Fatal("the v2 register proof's signed text differs between the core and the connector")
	}
	if !botkeys.VerifyRegister(pub, bot, ch, ws, app, ed25519.Sign(priv, connector.GrokRegisterMessage(bot, ch, ws, app))) {
		t.Fatal("a v2 signature by the connector doesn't verify")
	}
	if string(botkeys.FetchMessage(bot, "1700000000")) != "wakeshift-fetch\n"+bot+"\n1700000000" ||
		string(connector.GrokFetchMessage(bot, "1700000000")) != string(botkeys.FetchMessage(bot, "1700000000")) {
		t.Fatal("the fetch's signed text differs between the core and the connector")
	}
	for _, id := range []string{bot, "bc-123", "a.b_c:d-e"} {
		if botkeys.BotIDOK(id) != connector.GrokBotOK(id) {
			t.Errorf("%q: the core and the connector disagree on the bot ID", id)
		}
	}
	for _, id := range []string{"", "a b", "a'b", "a/b", "$(id)", "a\nb", ".", "..", "a..b"} {
		if botkeys.BotIDOK(id) || connector.GrokBotOK(id) {
			t.Errorf("%q accepted", id)
		}
	}
}

// The texts give the connector's commands, exactly, and nothing a model
// would copy and adapt: no openssl, no file-writing step, no challenge to
// put into a command, and nothing that reads CURSOR_CONVERSATION_ID. The
// routine's command is fixed text with the bot's ID and the issuer written
// in, and it keeps the bot check, the conversation ID rule and the
// fallback for a reset VM.
func TestGrokNotes(t *testing.T) {
	const base, app, fg, bot, id = "https://app.wakeshift.ai", "Standup", "FOREGROUND-TEXT", "4f1c2b3a-9d8e-4c7b-a6f5-0e1d2c3b4a59", "0b8f3c1e-2d4a-4b6c-8e0f-1a2b3c4d5e6f"
	dir := t.TempDir()
	for _, p := range []string{"linux-amd64", "linux-arm64"} {
		if err := os.WriteFile(filepath.Join(dir, "wakeshift-"+p), []byte("connector "+p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if (&downloads.Server{PublicURL: base}).GrokInstallCommand() != "" {
		t.Fatal("an install command with no connector built")
	}
	in := harnesses.GrokInstall{Base: base, Command: (&downloads.Server{PublicURL: base, ConnectorDir: dir}).GrokInstallCommand()}
	install := harnesses.GrokInstallCommand(in)
	sum := func(b string) string { h := sha256.Sum256([]byte(b)); return hex.EncodeToString(h[:]) }
	for _, want := range []string{"linux-amd64) want=" + sum("connector linux-amd64"), "linux-arm64) want=" + sum("connector linux-arm64"),
		`curl -fsSL "` + base + `/download/wakeshift/$platform"`, `"$HOME/.wakeshift/bin/wakeshift"`, "didn't match its checksum; nothing was installed"} {
		if !strings.Contains(install, want) {
			t.Errorf("the install command lacks %q: %s", want, install)
		}
	}
	if regexp.MustCompile(`\|\s*(sh|bash)\b`).MatchString(install) || strings.Contains(install, "bash -c") {
		t.Errorf("the install command pipes into a shell: %s", install)
	}
	if got := harnesses.GrokProveCommand(base, bot, ""); got != "$HOME/.wakeshift/bin/wakeshift grok prove --bot '"+bot+"' --issuer '"+base+"'" {
		t.Errorf("the routine's command: %s", got)
	}
	key := harnesses.GrokKeyNote(app, in, bot, id, fg)
	for _, want := range []string{install, harnesses.GrokInitCommand(base, bot), harnesses.GrokProveCommand(base, bot, id), "Don't change the commands or write your own",
		"Never print, copy or send the key file's contents", "wait for messages in the foreground instead: " + fg} {
		if !strings.Contains(key, want) {
			t.Errorf("GrokKeyNote lacks %q", want)
		}
	}
	instr := harnesses.GrokRoutineInstruction(base, bot)
	for _, want := range []string{"if \"bot\" isn't exactly `" + bot + "`", "run exactly `" + harnesses.GrokProveCommand(base, bot, "") + "`", "nothing from the body goes into it",
		"or the command isn't found (this bot's VM was reset)", "pass `" + bot + "` (this bot's ID), never `printenv CURSOR_CONVERSATION_ID`", `"wakeshift.joined"`, `"wakeshift.events"`} {
		if !strings.Contains(instr, want) {
			t.Errorf("GrokRoutineInstruction lacks %q", want)
		}
	}
	hook := harnesses.GrokWebhookNote(app, base, bot, "LINK", 24*time.Hour, fg)
	if !strings.Contains(hook, "within 24 hours") || !strings.Contains(hook, instr) || !strings.Contains(hook, harnesses.GrokRoutineName) || !strings.HasSuffix(hook, fg) {
		t.Errorf("GrokWebhookNote: %s", hook)
	}
	missing := harnesses.GrokKeyMissingNote(app, in, bot, id, fg)
	if !strings.Contains(missing, "Don't make a new key unless they ask") || !strings.Contains(missing, install) || !strings.Contains(missing, harnesses.GrokInitCommand(base, bot)) {
		t.Errorf("GrokKeyMissingNote: %s", missing)
	}
	dead := harnesses.GrokDeadNote(app, "unauthorized", in, bot, id, "LINK", time.Hour, fg)
	for name, text := range map[string]string{"key": key, "routine": instr, "webhook": hook, "missing": missing, "dead": dead} {
		for _, not := range []string{"openssl", "umask", "<<", "file-writing tool", "wakeshift:v1:", "CHALLENGE"} {
			if strings.Contains(text, not) {
				t.Errorf("the %s text has %q", name, not)
			}
		}
	}
	for reason, want := range map[string]string{"unauthorized": "rotated", "not_found": "deleted", "failing": "for a day"} {
		if !strings.Contains(harnesses.GrokDeadNote(app, reason, in, bot, id, "LINK", time.Hour, fg), want) || !strings.Contains(harnesses.GrokStoppedLine(app, reason), want) {
			t.Errorf("dead reason %s", reason)
		}
	}
}
