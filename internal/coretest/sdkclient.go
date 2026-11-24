package coretest

import (
	"crypto/ed25519"
	"strings"
	"sync"
	"testing"

	"github.com/wakeshift/server/internal/appkey"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// Creds is what a test knows of an app it made a client for: the SDK's
// client keeps its key to itself, so tests that sign their own assertions
// read it here.
type Creds struct {
	ID     string
	Key    ed25519.PrivateKey
	Core   string
	MCPURL string
}

// creds is every client SDKClient made, with its app's credentials.
var creds sync.Map // *wakeshift.Client → Creds

// CredsOf is the credentials SDKClient made app with (zero for another).
func CredsOf(app *wakeshift.Client) Creds {
	c, _ := creds.Load(app)
	cr, _ := c.(Creds)
	return cr
}

// SDKClient is the Go SDK's client for app id with key (RegisterApp's),
// against core: the key in its older form, which names the app, so the
// client knows its ID without asking. It doesn't retry, so a test that
// expects a failure fails at once.
func SDKClient(t testing.TB, id string, key ed25519.PrivateKey, core string, opts ...wakeshift.Option) *wakeshift.Client {
	t.Helper()
	return SDKClientFor(t, Creds{ID: id, Key: key, Core: core}, opts...)
}

// SDKClientFor is SDKClient for cr, remembering its MCP URL too.
func SDKClientFor(t testing.TB, cr Creds, opts ...wakeshift.Option) *wakeshift.Client {
	t.Helper()
	all := append([]wakeshift.Option{wakeshift.WithKey(appkey.FormatOld(appkey.Test, cr.ID, cr.Key.Seed())), wakeshift.WithCore(cr.Core), wakeshift.WithMaxRetries(0)}, opts...)
	c, err := wakeshift.New(all...)
	if err != nil {
		t.Fatal(err)
	}
	creds.Store(c, cr)
	return c
}

// KeyClient is the Go SDK's client for an app key from the console (any
// format) against core, without retries; RawApp signs for it too.
func KeyClient(t testing.TB, key, core string, opts ...wakeshift.Option) *wakeshift.Client {
	t.Helper()
	all := append([]wakeshift.Option{wakeshift.WithKey(key), wakeshift.WithCore(core), wakeshift.WithMaxRetries(0)}, opts...)
	c, err := wakeshift.New(all...)
	if err != nil {
		t.Fatal(err)
	}
	if k, err := appkey.Parse(key); err == nil {
		creds.Store(c, Creds{ID: k.AppID, Key: k.Private(), Core: core})
	}
	return c
}

// AppClient registers app id (RegisterApp) and returns the Go SDK's client
// for it, and its key.
func (c *Core) AppClient(id, name, mcpURL string, opts ...wakeshift.Option) (*wakeshift.Client, ed25519.PrivateKey) {
	c.t.Helper()
	key := c.RegisterApp(id, name, mcpURL)
	return SDKClientFor(c.t, Creds{ID: id, Key: key, Core: c.URL, MCPURL: mcpURL}, opts...), key
}

// App registers app id, named after it, with MCP URL mcpURL, and returns
// its client.
func (c *Core) App(id, mcpURL string, opts ...wakeshift.Option) *wakeshift.Client {
	c.t.Helper()
	app, _ := c.AppClient(id, strings.ToUpper(id[:1])+id[1:], mcpURL, opts...)
	return app
}
