package config

import "testing"

func TestBrowserAPIsRequireCredentialsAndHTTPDiscovery(t *testing.T) {
	c := Default()
	c.AllowNoToken = true
	if err := c.validate(); err != nil {
		t.Fatal("standalone no-token mode changed", err)
	}
	c.EnableCDP = true
	if err := c.validate(); err == nil {
		t.Fatal("CDP accepted without token")
	}
	c.Token = "fixture"
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	c.ChromeURL = "ws://localhost:9222/devtools/browser/example"
	if err := c.validate(); err == nil {
		t.Fatal("CDP proxy needs HTTP discovery")
	}
	c.EnableCDP = false
	c.Token = ""
	c.HistoryRoot = "/profile/chrome"
	if err := c.validate(); err == nil {
		t.Fatal("history accepted without token")
	}
	c.Token = "fixture"
	c.PublicURL = "https://user:password@example.com"
	if err := c.validate(); err == nil {
		t.Fatal("public URL accepted credentials")
	}
}

func TestAdminBandConfigValidation(t *testing.T) {
	c := Default()
	c.AllowNoToken = true
	c.MacroStore = "/tmp/macros"
	c.AdminAddr = ":8081"
	// Band without a token refuses to start.
	if err := c.validate(); err == nil {
		t.Fatal("admin band accepted without admin-token")
	}
	// Agent token reuse is refused: the band is only human-only if its token
	// is not one the agent holds.
	for _, reuse := range []string{"Token", "DriverToken", "ReaderToken"} {
		c2 := c
		c2.AdminToken = "admin-secret"
		switch reuse {
		case "Token":
			c2.Token = "admin-secret"
		case "DriverToken":
			c2.DriverToken = "admin-secret"
		case "ReaderToken":
			c2.ReaderToken = "admin-secret"
		}
		if err := c2.validate(); err == nil {
			t.Fatalf("admin token may not equal %s", reuse)
		}
	}
	c.Token, c.DriverToken, c.ReaderToken = "agent", "driver", "reader"
	c.AdminToken = "admin-secret"
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	// Band without a macro store is meaningless.
	c.MacroStore = ""
	if err := c.validate(); err == nil {
		t.Fatal("admin band accepted without macro-store")
	}
	// Default (no admin addr) is unchanged.
	d := Default()
	d.AllowNoToken = true
	if err := d.validate(); err != nil {
		t.Fatal("default config must not require the band", err)
	}
}
