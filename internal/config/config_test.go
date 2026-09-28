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
	c.HistoryCommand = []string{"node", "history.ts"}
	if err := c.validate(); err == nil {
		t.Fatal("history accepted without token")
	}
	c.Token = "fixture"
	c.PublicURL = "https://user:password@example.com"
	if err := c.validate(); err == nil {
		t.Fatal("public URL accepted credentials")
	}
}
