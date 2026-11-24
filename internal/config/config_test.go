package config

import "testing"

func TestFromEnv(t *testing.T) {
	good := map[string]string{
		"DATABASE_URL":         "postgres://x",
		"WAKESHIFT_SECRET_KEY": "0123456789abcdef0123456789abcdef-random",
		"WAKESHIFT_PUBLIC_URL": "https://wakeshift.dev",
	}
	for name, c := range map[string]struct {
		change map[string]string
		ok     bool
	}{
		"valid":                         {nil, true},
		"no public URL":                 {map[string]string{"WAKESHIFT_PUBLIC_URL": ""}, false},
		"public URL with a path":        {map[string]string{"WAKESHIFT_PUBLIC_URL": "https://wakeshift.dev/app"}, false},
		"plain http in production":      {map[string]string{"WAKESHIFT_PUBLIC_URL": "http://wakeshift.dev"}, false},
		"plain http on localhost":       {map[string]string{"WAKESHIFT_PUBLIC_URL": "http://localhost:8370"}, true},
		"the dev key in production":     {map[string]string{"WAKESHIFT_SECRET_KEY": DevSecretKey}, false},
		"the dev key on localhost":      {map[string]string{"WAKESHIFT_SECRET_KEY": DevSecretKey, "WAKESHIFT_PUBLIC_URL": "http://localhost:8370", "WAKESHIFT_ADDR": "127.0.0.1:8370"}, true},
		"the dev key on ::1":            {map[string]string{"WAKESHIFT_SECRET_KEY": DevSecretKey, "WAKESHIFT_PUBLIC_URL": "http://localhost:8370", "WAKESHIFT_ADDR": "[::1]:8370"}, true},
		"the dev key, every interface":  {map[string]string{"WAKESHIFT_SECRET_KEY": DevSecretKey, "WAKESHIFT_PUBLIC_URL": "http://localhost:8370"}, false},
		"the dev key on 0.0.0.0":        {map[string]string{"WAKESHIFT_SECRET_KEY": DevSecretKey, "WAKESHIFT_PUBLIC_URL": "http://localhost:8370", "WAKESHIFT_ADDR": "0.0.0.0:8370"}, false},
		"the dev key on a LAN address":  {map[string]string{"WAKESHIFT_SECRET_KEY": DevSecretKey, "WAKESHIFT_PUBLIC_URL": "http://localhost:8370", "WAKESHIFT_ADDR": "192.168.1.5:8370"}, false},
		"dev sign-in on loopback":       {map[string]string{"WAKESHIFT_DEV_SIGN_IN": "1", "WAKESHIFT_PUBLIC_URL": "http://localhost:8370", "WAKESHIFT_ADDR": "127.0.0.1:8370"}, true},
		"dev sign-in, every interface":  {map[string]string{"WAKESHIFT_DEV_SIGN_IN": "1", "WAKESHIFT_PUBLIC_URL": "http://localhost:8370"}, false},
		"dev sign-in on 0.0.0.0":        {map[string]string{"WAKESHIFT_DEV_SIGN_IN": "1", "WAKESHIFT_PUBLIC_URL": "http://localhost:8370", "WAKESHIFT_ADDR": "0.0.0.0:8370"}, false},
		"dev sign-in, a short token":    {map[string]string{"WAKESHIFT_DEV_SIGN_IN": "1", "WAKESHIFT_PUBLIC_URL": "http://localhost:8370", "WAKESHIFT_ADDR": "127.0.0.1:8370", "WAKESHIFT_DEV_SIGN_IN_TOKEN": "short"}, false},
		"short key":                     {map[string]string{"WAKESHIFT_SECRET_KEY": "short"}, false},
		"dev sign-in on Cloud Run":      {map[string]string{"WAKESHIFT_DEV_SIGN_IN": "1", "K_SERVICE": "wakeshift"}, false},
		"half a Google client":          {map[string]string{"WAKESHIFT_GOOGLE_CLIENT_ID": "id"}, false},
		"a whole Google client":         {map[string]string{"WAKESHIFT_GOOGLE_CLIENT_ID": "id", "WAKESHIFT_GOOGLE_CLIENT_SECRET": "s"}, true},
		"a stream cap":                  {map[string]string{"WAKESHIFT_MAX_STREAMS": "900"}, true},
		"a negative stream cap":         {map[string]string{"WAKESHIFT_MAX_STREAMS": "-1"}, false},
		"a stream cap that's words":     {map[string]string{"WAKESHIFT_MAX_STREAMS": "lots"}, false},
		"the profiler on":               {map[string]string{"WAKESHIFT_PROFILER": "1"}, true},
		"the profiler off":              {map[string]string{"WAKESHIFT_PROFILER": "0"}, true},
		"the profiler, misspelt":        {map[string]string{"WAKESHIFT_PROFILER": "true"}, false},
		"an installers' release":        {map[string]string{"WAKESHIFT_INSTALLER_URL": "https://github.com/wakeshift/connectors/releases/latest/download"}, true},
		"installers over http":          {map[string]string{"WAKESHIFT_INSTALLER_URL": "http://github.com/wakeshift/connectors/releases/latest/download"}, false},
		"installers with a query":       {map[string]string{"WAKESHIFT_INSTALLER_URL": "https://example.com/x?token=1"}, false},
		"installers, not a URL":         {map[string]string{"WAKESHIFT_INSTALLER_URL": "github.com/wakeshift"}, false},
		"antigravity plugin, local":     {map[string]string{"WAKESHIFT_ANTIGRAVITY_PLUGIN": "/tmp/run/antigravity-plugin"}, true},
		"antigravity plugin, fork":      {map[string]string{"WAKESHIFT_ANTIGRAVITY_PLUGIN": "https://github.com/someone/antigravity-plugin/tree/x"}, true},
		"antigravity plugin, shell":     {map[string]string{"WAKESHIFT_ANTIGRAVITY_PLUGIN": "/tmp/x; id"}, false},
		"opencode's plugin, a version":  {map[string]string{"WAKESHIFT_OPENCODE_PLUGIN": "1.5.0"}, true},
		"opencode's plugin, a dist-tag": {map[string]string{"WAKESHIFT_OPENCODE_PLUGIN": "staging"}, true},
		"opencode's plugin, off":        {map[string]string{"WAKESHIFT_OPENCODE_PLUGIN": "off"}, true},
		"opencode's plugin, a command":  {map[string]string{"WAKESHIFT_OPENCODE_PLUGIN": "1.5.0; id"}, false},
		"opencode's plugin, a spec":     {map[string]string{"WAKESHIFT_OPENCODE_PLUGIN": "@wakeshift/opencode@1.5.0"}, false},
		"pi's extension, a version":     {map[string]string{"WAKESHIFT_PI_PLUGIN": "1.5.0"}, true},
		"pi's extension, a dist-tag":    {map[string]string{"WAKESHIFT_PI_PLUGIN": "staging"}, true},
		"pi's extension, a command":     {map[string]string{"WAKESHIFT_PI_PLUGIN": "1.5.0; id"}, false},
		"connect, a version":            {map[string]string{"WAKESHIFT_CONNECT_PACKAGE": "1.5.0"}, true},
		"connect, a dist-tag":           {map[string]string{"WAKESHIFT_CONNECT_PACKAGE": "staging"}, true},
		"connect, off":                  {map[string]string{"WAKESHIFT_CONNECT_PACKAGE": "off"}, true},
		"connect, a command":            {map[string]string{"WAKESHIFT_CONNECT_PACKAGE": "1.5.0 && id"}, false},
		"a site URL":                    {map[string]string{"WAKESHIFT_SITE_URL": "https://wakeshift.ai"}, true},
		"a site URL with a path":        {map[string]string{"WAKESHIFT_SITE_URL": "https://wakeshift.ai/docs"}, false},
		"a site URL over http":          {map[string]string{"WAKESHIFT_SITE_URL": "http://wakeshift.ai"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			for _, k := range []string{"DATABASE_URL", "WAKESHIFT_SECRET_KEY", "WAKESHIFT_PUBLIC_URL", "WAKESHIFT_ADDR", "PORT", "WAKESHIFT_DEV_SIGN_IN", "WAKESHIFT_DEV_SIGN_IN_TOKEN", "K_SERVICE", "WAKESHIFT_GOOGLE_CLIENT_ID", "WAKESHIFT_GOOGLE_CLIENT_SECRET", "WAKESHIFT_MAX_STREAMS", "WAKESHIFT_PROFILER", "WAKESHIFT_ANTIGRAVITY_PLUGIN", "WAKESHIFT_INSTALLER_URL", "WAKESHIFT_OPENCODE_PLUGIN", "WAKESHIFT_CONNECT_PACKAGE", "WAKESHIFT_SITE_URL"} {
				t.Setenv(k, "")
			}
			for k, v := range good {
				t.Setenv(k, v)
			}
			for k, v := range c.change {
				t.Setenv(k, v)
			}
			got, err := FromEnv()
			if (err == nil) != c.ok {
				t.Fatalf("err = %v", err)
			}
			if err == nil && got.Profiler != (c.change["WAKESHIFT_PROFILER"] == "1") {
				t.Fatalf("Profiler = %v", got.Profiler)
			}
		})
	}
}

// Dev sign-in's token: the one given, else a random one made at startup
// (for the server to print), different each time.
func TestDevSignInToken(t *testing.T) {
	for k, v := range map[string]string{
		"DATABASE_URL": "postgres://x", "WAKESHIFT_SECRET_KEY": DevSecretKey, "WAKESHIFT_PUBLIC_URL": "http://localhost:8370",
		"WAKESHIFT_ADDR": "127.0.0.1:8370", "WAKESHIFT_DEV_SIGN_IN": "1", "WAKESHIFT_DEV_SIGN_IN_TOKEN": "", "K_SERVICE": "",
	} {
		t.Setenv(k, v)
	}
	a, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	b, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !a.DevSignInTokenMade || len(a.DevSignInToken) < 32 || a.DevSignInToken == b.DevSignInToken {
		t.Fatalf("made %v, tokens %q and %q", a.DevSignInTokenMade, a.DevSignInToken, b.DevSignInToken)
	}
	t.Setenv("WAKESHIFT_DEV_SIGN_IN_TOKEN", "bench-token-0123456789")
	c, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if c.DevSignInTokenMade || c.DevSignInToken != "bench-token-0123456789" {
		t.Fatalf("made %v, token %q", c.DevSignInTokenMade, c.DevSignInToken)
	}
	t.Setenv("WAKESHIFT_DEV_SIGN_IN", "")
	if d, err := FromEnv(); err != nil || d.DevSignInToken != "" {
		t.Fatalf("dev sign-in off: token %q (%v)", d.DevSignInToken, err)
	}
}

// The internal admin API's people (WAKESHIFT_ADMIN_EMAILS).
func TestAdminSettings(t *testing.T) {
	for _, k := range []string{"WAKESHIFT_DEV_SIGN_IN", "K_SERVICE", "WAKESHIFT_GOOGLE_CLIENT_ID", "WAKESHIFT_GOOGLE_CLIENT_SECRET", "WAKESHIFT_MAX_STREAMS"} {
		t.Setenv(k, "")
	}
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("WAKESHIFT_SECRET_KEY", "0123456789abcdef0123456789abcdef-random")
	t.Setenv("WAKESHIFT_PUBLIC_URL", "https://wakeshift.dev")
	set := func(admins string) (Config, error) {
		t.Setenv("WAKESHIFT_ADMIN_EMAILS", admins)
		return FromEnv()
	}
	c, err := set("")
	if err != nil || c.AdminEmails != nil {
		t.Fatalf("none: %v %+v", err, c)
	}
	c, err = set(" ada@example.com, ,grace@example.com ")
	if err != nil || len(c.AdminEmails) != 2 || c.AdminEmails[0] != "ada@example.com" || c.AdminEmails[1] != "grace@example.com" {
		t.Fatalf("set: %v %+v", err, c.AdminEmails)
	}
	for name, bad := range map[string]string{
		"an uppercase email": "Ada@example.com",
		"not an email":       "ada",
	} {
		if _, err := set(bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The site's URL defaults to the console's without "app.", and there's
// none for a console elsewhere; WAKESHIFT_SITE_URL overrides it.
func TestSiteURL(t *testing.T) {
	for _, c := range []struct{ public, site, want string }{
		{"https://app.wakeshift.ai", "", "https://wakeshift.ai"},
		{"https://app.staging.wakeshift.ai", "", "https://staging.wakeshift.ai"},
		{"http://localhost:8370", "", ""},
		{"https://wakeshift.dev", "", ""},
		{"https://app.wakeshift.ai", "https://www.wakeshift.ai/", "https://www.wakeshift.ai"},
	} {
		t.Setenv("DATABASE_URL", "postgres://x")
		t.Setenv("WAKESHIFT_SECRET_KEY", "0123456789abcdef0123456789abcdef-random")
		t.Setenv("WAKESHIFT_PUBLIC_URL", c.public)
		t.Setenv("WAKESHIFT_SITE_URL", c.site)
		got, err := FromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if got.SiteURL != c.want {
			t.Errorf("%s, %q: site %q, want %q", c.public, c.site, got.SiteURL, c.want)
		}
	}
}

// Subscribe vouchers: "1" on, "0" off, unset on only on localhost
// (development); anything else is refused.
func TestSubscribeVouchers(t *testing.T) {
	for _, k := range []string{"WAKESHIFT_DEV_SIGN_IN", "K_SERVICE", "WAKESHIFT_GOOGLE_CLIENT_ID", "WAKESHIFT_GOOGLE_CLIENT_SECRET", "WAKESHIFT_MAX_STREAMS", "WAKESHIFT_ADMIN_EMAILS"} {
		t.Setenv(k, "")
	}
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("WAKESHIFT_SECRET_KEY", "0123456789abcdef0123456789abcdef-random")
	for _, tc := range []struct {
		url, value string
		on         bool
	}{
		{"https://wakeshift.dev", "", false},
		{"https://wakeshift.dev", "1", true},
		{"https://wakeshift.dev", "0", false},
		{"http://localhost:8370", "", true},
		{"http://localhost:8370", "0", false},
	} {
		t.Setenv("WAKESHIFT_PUBLIC_URL", tc.url)
		t.Setenv("WAKESHIFT_SUBSCRIBE_VOUCHERS", tc.value)
		c, err := FromEnv()
		if err != nil || c.SubscribeVouchers != tc.on {
			t.Errorf("%s with %q: %v, %v", tc.url, tc.value, c.SubscribeVouchers, err)
		}
	}
	t.Setenv("WAKESHIFT_SUBSCRIBE_VOUCHERS", "true")
	if _, err := FromEnv(); err == nil {
		t.Error(`"true" accepted`)
	}
}

// MCP Events push: "1" on, "0" off, unset on only on localhost
// (development); anything else is refused.
func TestMCPEventsPush(t *testing.T) {
	for _, k := range []string{"WAKESHIFT_DEV_SIGN_IN", "K_SERVICE", "WAKESHIFT_GOOGLE_CLIENT_ID", "WAKESHIFT_GOOGLE_CLIENT_SECRET", "WAKESHIFT_MAX_STREAMS", "WAKESHIFT_ADMIN_EMAILS"} {
		t.Setenv(k, "")
	}
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("WAKESHIFT_SECRET_KEY", "0123456789abcdef0123456789abcdef-random")
	for _, tc := range []struct {
		url, value string
		on         bool
	}{
		{"https://wakeshift.dev", "", false},
		{"https://wakeshift.dev", "1", true},
		{"https://wakeshift.dev", "0", false},
		{"http://localhost:8370", "", true},
		{"http://localhost:8370", "0", false},
	} {
		t.Setenv("WAKESHIFT_PUBLIC_URL", tc.url)
		t.Setenv("WAKESHIFT_MCP_EVENTS_PUSH", tc.value)
		c, err := FromEnv()
		if err != nil || c.MCPEventsPush != tc.on {
			t.Errorf("%s with %q: %v, %v", tc.url, tc.value, c.MCPEventsPush, err)
		}
	}
	t.Setenv("WAKESHIFT_MCP_EVENTS_PUSH", "true")
	if _, err := FromEnv(); err == nil {
		t.Error(`"true" accepted`)
	}
}

func TestProxyHops(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("WAKESHIFT_SECRET_KEY", "0123456789abcdef0123456789abcdef-random")
	t.Setenv("WAKESHIFT_PUBLIC_URL", "https://wakeshift.dev")
	for v, want := range map[string]int{"": 0, "0": 0, "2": 2} {
		t.Setenv("WAKESHIFT_PROXY_HOPS", v)
		c, err := FromEnv()
		if err != nil || c.ProxyHops != want {
			t.Errorf("%q: %d (%v), want %d", v, c.ProxyHops, err, want)
		}
	}
	for _, v := range []string{"-1", "two"} {
		t.Setenv("WAKESHIFT_PROXY_HOPS", v)
		if _, err := FromEnv(); err == nil {
			t.Errorf("%q accepted", v)
		}
	}
}
