package coretest_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/harnesses"
)

// TestMain gives the tests' commands a HOME of their own, where the link
// files results tell the agent to write go (cmdRE writes them, as the
// agent's file tool would): never the user's.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "coretest-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Setenv("HOME", home)
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

// cmdRE finds a command a result gives (re's group 1, which must end with
// the loop's done; fi, and start with the link file: harnesses.LinkStart), and, as the agent
// would, writes the link file its link step names in HOME. Its
// FindStringSubmatch returns the command and the url it waits on (the
// link with the command's query, as the command once held it); with
// urlOnly, only the link.
type cmdRE struct {
	re      *regexp.Regexp
	urlOnly bool
}

// fileName is the link file a command reads, and the query it adds.
var (
	fileName  = regexp.MustCompile(`^f=\$HOME/\.wakeshift/links/([0-9a-f]{16}); `)
	loopQuery = regexp.MustCompile(`u="\$b([^"]*)"; c=`)
)

// loopCmd matches a command whose start has vars: group 1, the whole
// command, from its function to its call.
func loopCmd(vars string) string {
	return `(f=\$HOME/\.wakeshift/links/[0-9a-f]{16}; [^\n]*?` + vars + `.*?; done; fi)`
}

func (c cmdRE) FindStringSubmatch(text string) []string {
	m := c.re.FindStringSubmatch(text)
	if m == nil {
		return nil
	}
	command := m[1]
	if strings.HasPrefix(command, "{") { // Cursor's Shell call: the command inside
		var shell struct{ Command string }
		if json.Unmarshal([]byte(command), &shell) != nil {
			return nil
		}
		command = shell.Command
	}
	key := fileName.FindStringSubmatch(command)
	if key == nil {
		return nil
	}
	link := linkFor(text, key[1])
	if link == "" {
		// Cursor's command claims its link (harnesses.Claim): claimed here,
		// as the command would, the first time it's found.
		link = claimFor(command, key[1])
	}
	if link == "" {
		return nil
	}
	writeLinkFile(os.Getenv("HOME"), link)
	if c.urlOnly {
		return []string{m[0], link}
	}
	q := loopQuery.FindStringSubmatch(command)
	if q == nil {
		return nil
	}
	return []string{m[0], m[1], link + q[1]}
}

func (c cmdRE) FindString(text string) string {
	if m := c.FindStringSubmatch(text); m != nil {
		return m[0]
	}
	return ""
}

// claimFor claims the link a claiming command's code hands over, if it's
// the link of the file key: once (a code works once), so the link is
// remembered by the code, and by its file's key.
func claimFor(command, key string) string {
	m := coretest.ClaimRE.FindStringSubmatch(command)
	if m == nil {
		return ""
	}
	claimedMu.Lock()
	defer claimedMu.Unlock()
	link, ok := claimed[m[1]]
	if !ok {
		link, _ = coretest.ClaimLink(m[2], m[1], "")
		claimed[m[1]] = link
		if link != "" {
			claimed[harnesses.LinkKey(link)] = link
		}
	}
	if link == "" {
		// A later result's code for the same link, refused while its
		// command listens: the agent has the file already.
		link = claimed[key]
	}
	if link == "" || harnesses.LinkKey(link) != key {
		return ""
	}
	return link
}

var (
	claimedMu sync.Mutex
	claimed   = map[string]string{}
)

// linkFor is the link text's link step gives for the file key.
func linkFor(text, key string) string {
	for _, s := range coretest.LinkStepRE.FindAllStringSubmatch(text, -1) {
		if s[1] == key && harnesses.LinkKey(s[2]) == key {
			return s[2]
		}
	}
	return ""
}

// writeLinkFile writes link's file in home as an agent's file tool would
// (0644, in folders it makes 0755).
func writeLinkFile(home, link string) {
	dir := filepath.Join(home, ".wakeshift", "links")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, harnesses.LinkKey(link)), []byte(link+"\n"), 0o644)
}

// setLink makes the link file of the command for link hold another link
// (the url a renewal handed over: the loop would have saved it).
func setLink(t *testing.T, link, other string) {
	t.Helper()
	f := filepath.Join(os.Getenv("HOME"), ".wakeshift", "links", harnesses.LinkKey(strings.SplitN(link, "&", 2)[0]))
	if err := os.WriteFile(f, []byte(strings.SplitN(other, "&", 2)[0]+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (c cmdRE) MatchString(text string) bool { return c.FindStringSubmatch(text) != nil }

// readLinkFile is what the link file of the command for link holds.
func readLinkFile(t *testing.T, link string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(os.Getenv("HOME"), ".wakeshift", "links", harnesses.LinkKey(strings.SplitN(link, "&", 2)[0])))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}
