// Package challenge recognises bot-protection interstitials.
//
// These pages arrive with HTTP 200 and a body that is a JavaScript challenge
// rather than the content, so status codes alone cannot detect them. The
// markers are mirrored by pi-search's TypeScript detector (src/fetch/blocked.ts)
// so a client and this gateway agree on what "blocked" means.
package challenge

import (
	"regexp"
	"strings"
)

// interstitialTextLimit is the visible-text budget below which a page that
// embeds a captcha widget is considered to *be* the captcha, rather than a real
// page that merely has one in a comment or login form. A captcha interstitial
// carries a couple of hundred characters of text; a real article has
// thousands.
const interstitialTextLimit = 1200

// Vendor names returned by Detect.
const (
	Cloudflare  = "Cloudflare"
	JSChallenge = "JS challenge"
	Humanity    = "humanity check"
	Imperva     = "Imperva"
	PerimeterX  = "PerimeterX"
	DataDome    = "DataDome"
	Akamai      = "Akamai"
	ReCAPTCHA   = "reCAPTCHA"
	HCaptcha    = "hCaptcha"
)

// sampleLimit caps how much of the document we scan; interstitials are small.
const sampleLimit = 20_000

var (
	reJSChallenge = regexp.MustCompile(`name="js_challenge"`)
	reCloudflare  = regexp.MustCompile(`(?i)<title>\s*just a moment\.{0,3}\s*</title>|cf-chl-|challenge-platform|cf_chl_opt|/cdn-cgi/challenge`)
	reImperva     = regexp.MustCompile(`(?i)_incapsula_resource|incapsula`)
	rePerimeterX  = regexp.MustCompile(`(?i)px-captcha|_pxhd|perimeterx`)
	reDataDome    = regexp.MustCompile(`(?i)geo\.captcha-delivery\.com|datadome`)
	reAkamai      = regexp.MustCompile(`(?i)<title>\s*access denied\s*</title>`)
	reTurnstile   = regexp.MustCompile(`(?i)challenges\.cloudflare\.com/turnstile|cf-turnstile`)

	// Interstitials that ask the visitor to prove they are human, usually
	// wrapping a captcha widget and posting back to the same path.
	reHumanity = regexp.MustCompile(`(?i)prove your humanity|are you a robot|verify you are (a )?human|action="[^"]*[?&]captcha=1"`)

	// Interactive captcha widgets, whoever embeds them.
	reRecaptcha = regexp.MustCompile(`(?i)class="g-recaptcha"|google\.com/recaptcha/api\.js|g-recaptcha-response`)
	reHCaptcha  = regexp.MustCompile(`(?i)hcaptcha\.com/1/api\.js|h-captcha-response|class="h-captcha"`)
)

// Detect returns the vendor blocking this page, or "" when the page looks real.
func Detect(html, title string) string {
	sample := html
	if len(sample) > sampleLimit {
		sample = sample[:sampleLimit]
	}
	switch {
	case reJSChallenge.MatchString(sample):
		return JSChallenge
	case reCloudflare.MatchString(sample), reTurnstile.MatchString(sample):
		return Cloudflare
	case reImperva.MatchString(sample):
		return Imperva
	case rePerimeterX.MatchString(sample):
		return PerimeterX
	case reDataDome.MatchString(sample):
		return DataDome
	case reAkamai.MatchString(sample) && strings.Contains(sample, "permission to access"):
		return Akamai
	// A captcha widget alone proves nothing: plenty of real pages embed one in a
	// login or comment form. Only a near-empty page built around one is a block.
	case reRecaptcha.MatchString(sample) && (isSparse(html) || reHumanity.MatchString(sample)):
		return ReCAPTCHA
	case reHCaptcha.MatchString(sample) && (isSparse(html) || reHumanity.MatchString(sample)):
		return HCaptcha
	case reHumanity.MatchString(sample) && isSparse(html):
		return Humanity
	}
	// A tiny body whose only text is the site name is the shape these shells
	// take once their markers change; treat suspiciously short documents with a
	// challenge-ish title as unresolved rather than returning empty content.
	if len(html) < 2_000 && strings.Contains(strings.ToLower(title), "just a moment") {
		return Cloudflare
	}
	return ""
}

var (
	// RE2 has no backreferences, so each element is spelled out.
	reStripTags = regexp.MustCompile(`(?s)<script[^>]*>.*?</script>|<style[^>]*>.*?</style>` +
		`|<noscript[^>]*>.*?</noscript>|<template[^>]*>.*?</template>`)
	reStripAngles = regexp.MustCompile(`<[^>]+>`)
	reWhitespace  = regexp.MustCompile(`\s+`)
)

// isSparse reports whether the document carries almost no visible text.
func isSparse(html string) bool {
	return visibleTextLen(html) < interstitialTextLimit
}

func visibleTextLen(html string) int {
	t := reStripTags.ReplaceAllString(html, " ")
	t = reStripAngles.ReplaceAllString(t, " ")
	return len(strings.TrimSpace(reWhitespace.ReplaceAllString(t, " ")))
}

// Interactive reports whether the vendor typically needs a human click
// (Turnstile checkbox, hCaptcha) rather than resolving on its own. Callers use
// this to decide whether assist mode is worth the wait.
func Interactive(html string) bool {
	sample := html
	if len(sample) > sampleLimit {
		sample = sample[:sampleLimit]
	}
	return reTurnstile.MatchString(sample) ||
		rePerimeterX.MatchString(sample) ||
		reDataDome.MatchString(sample) ||
		reRecaptcha.MatchString(sample) ||
		reHCaptcha.MatchString(sample)
}
