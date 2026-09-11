package challenge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return string(b)
}

// A real logged-out interstitial captured in 2026-06 (origin scrubbed): a
// reCAPTCHA v2 checkbox posted back to ?captcha=1 under a "Prove your humanity"
// heading, served with HTTP 200.
func TestDetectsRealCaptchaInterstitial(t *testing.T) {
	html := readFixture(t, "captcha-interstitial.html")
	if got := Detect(html, "Prove your humanity"); got != ReCAPTCHA {
		t.Fatalf("Detect = %q, want %q", got, ReCAPTCHA)
	}
	if !Interactive(html) {
		t.Fatal("Interactive = false, want true (reCAPTCHA checkbox needs a click)")
	}
}

// A real content page whose telemetry code contains the token js_challenge.
// That must not be mistaken for a challenge form's name="js_challenge" input,
// or every successful fetch of such a site would be discarded.
func TestRealContentPageIsNotFlagged(t *testing.T) {
	html := readFixture(t, "content-page-with-telemetry.html")
	if !strings.Contains(html, "js_challenge") {
		t.Fatal("fixture lost its js_challenge telemetry strings")
	}
	if got := Detect(html, "r/example"); got != "" {
		t.Fatalf("Detect = %q, want empty for a real content page", got)
	}
}

func TestHumanityGateMarkers(t *testing.T) {
	page := `<html><head><title>Prove your humanity</title></head><body>` +
		`<h1>Prove your humanity</h1><form method="POST" action="/x/?captcha=1">` +
		`<div class="g-recaptcha" data-sitekey="k"></div></form></body></html>`
	if got := Detect(page, "Prove your humanity"); got != ReCAPTCHA {
		t.Fatalf("Detect = %q, want %q", got, ReCAPTCHA)
	}
}

// A humanity gate without a recognised widget still counts as a block.
func TestHumanityGateWithoutWidget(t *testing.T) {
	page := `<html><head><title>Verify</title></head><body>` +
		`<p>Are you a robot? Complete the check below.</p></body></html>`
	if got := Detect(page, "Verify"); got != Humanity {
		t.Fatalf("Detect = %q, want %q", got, Humanity)
	}
}

// A content-rich page that happens to embed reCAPTCHA (comment form, login
// widget) must be returned, not treated as a block.
func TestContentPageWithCaptchaWidgetIsNotFlagged(t *testing.T) {
	body := strings.Repeat(
		"<p>Loki indexes labels rather than log contents, which is why label "+
			"cardinality dominates both cost and query latency in practice.</p>", 30)
	page := `<html><head><title>Understanding Loki labels</title>` +
		`<script src="https://www.google.com/recaptcha/api.js"></script></head><body><article>` +
		body + `<form id="comment"><div class="g-recaptcha" data-sitekey="k"></div></form>` +
		`</article></body></html>`
	if got := Detect(page, "Understanding Loki labels"); got != "" {
		t.Fatalf("Detect = %q, want empty: captcha in a comment form is not a block", got)
	}
}

func TestSparseCaptchaPagesAreFlagged(t *testing.T) {
	recaptcha := `<html><head><title>Verify</title></head><body>` +
		`<p>Please confirm you are human.</p><div class="g-recaptcha" data-sitekey="k"></div></body></html>`
	if got := Detect(recaptcha, "Verify"); got != ReCAPTCHA {
		t.Fatalf("Detect = %q, want %q", got, ReCAPTCHA)
	}
	hcaptcha := `<html><head><title>Verify</title></head><body>` +
		`<div class="h-captcha" data-sitekey="k"></div>` +
		`<script src="https://hcaptcha.com/1/api.js"></script></body></html>`
	if got := Detect(hcaptcha, "Verify"); got != HCaptcha {
		t.Fatalf("Detect = %q, want %q", got, HCaptcha)
	}
}

func TestDetectsJSChallengeShell(t *testing.T) {
	// Shape observed in production: HTTP 200, body is a JS challenge form.
	html := `<html><head><title>Example</title></head><body class=theme-beta>` +
		`<form><input type="hidden" name="js_challenge" value="1"/></form></body></html>`
	if got := Detect(html, "Example"); got != JSChallenge {
		t.Fatalf("Detect = %q, want %q", got, JSChallenge)
	}
}

func TestDetectsCloudflareInterstitial(t *testing.T) {
	html := `<html><head><title>Just a moment...</title></head><body>` +
		`<div id="cf-chl-widget"></div><script src="/cdn-cgi/challenge-platform/h/b/orchestrate"></script>` +
		`</body></html>`
	if got := Detect(html, "Just a moment..."); got != Cloudflare {
		t.Fatalf("Detect = %q, want %q", got, Cloudflare)
	}
}

func TestDetectsTurnstileAsCloudflare(t *testing.T) {
	html := `<html><body><div class="cf-turnstile" data-sitekey="x"></div>` +
		`<script src="https://challenges.cloudflare.com/turnstile/v0/api.js"></script></body></html>`
	if got := Detect(html, "Verify"); got != Cloudflare {
		t.Fatalf("Detect = %q, want %q", got, Cloudflare)
	}
	if !Interactive(html) {
		t.Fatal("Interactive = false, want true for Turnstile")
	}
}

func TestDetectsOtherVendors(t *testing.T) {
	cases := map[string]string{
		`<html><body><script src="/_Incapsula_Resource?x=1"></script></body></html>`:              Imperva,
		`<html><body><div id="px-captcha"></div></body></html>`:                                   PerimeterX,
		`<html><body><script src="https://geo.captcha-delivery.com/c.js"></script></body></html>`: DataDome,
	}
	for html, want := range cases {
		if got := Detect(html, ""); got != want {
			t.Errorf("Detect(%.40s…) = %q, want %q", html, got, want)
		}
	}
}

func TestDetectsAkamaiAccessDenied(t *testing.T) {
	html := `<html><head><title>Access Denied</title></head><body>` +
		`You don't have permission to access "http://x/" on this server.</body></html>`
	if got := Detect(html, "Access Denied"); got != Akamai {
		t.Fatalf("Detect = %q, want %q", got, Akamai)
	}
}

func TestCleanPageIsNotAChallenge(t *testing.T) {
	html := `<html><head><title>Query Loki | Grafana</title></head><body><main>` +
		`<h1>Query Loki</h1><p>LogQL is the query language for Grafana Loki.</p></main></body></html>`
	if got := Detect(html, "Query Loki | Grafana"); got != "" {
		t.Fatalf("Detect = %q, want empty", got)
	}
	if Interactive(html) {
		t.Fatal("Interactive = true for a clean page")
	}
}

// A real article mentioning a vendor's name in prose must not be flagged just
// for that; markers are structural (script srcs, widget ids, titles).
func TestProseMentioningCloudflareIsNotFlagged(t *testing.T) {
	html := `<html><head><title>How bot protection works</title></head><body><article>` +
		`<p>Sites like this one sit behind Cloudflare and Akamai, which score requests.</p>` +
		`</article></body></html>`
	if got := Detect(html, "How bot protection works"); got != "" {
		t.Fatalf("Detect = %q, want empty (prose mention only)", got)
	}
}

func TestShortBodyWithChallengeTitle(t *testing.T) {
	if got := Detect(`<html><body>Checking…</body></html>`, "Just a moment"); got != Cloudflare {
		t.Fatalf("Detect = %q, want %q", got, Cloudflare)
	}
}

func TestOnlyScansHeadOfLargeDocument(t *testing.T) {
	// A marker beyond sampleLimit (e.g. in a comment thread) must not trip it.
	filler := make([]byte, sampleLimit+100)
	for i := range filler {
		filler[i] = 'x'
	}
	html := `<html><body>` + string(filler) + `<div id="px-captcha"></div></body></html>`
	if got := Detect(html, "Big page"); got != "" {
		t.Fatalf("Detect = %q, want empty (marker past scan window)", got)
	}
}
