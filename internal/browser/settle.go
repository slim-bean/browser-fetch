package browser

import (
	"context"
	"log/slog"
	"time"

	"github.com/chromedp/chromedp"

	"github.com/slim-bean/browser-fetch/internal/challenge"
)

// snapshot is one atomic read of a page's identity and markup.
type snapshot struct {
	URL   string `json:"url"`
	Title string `json:"title"`
	HTML  string `json:"html"`
}

// Ready reports whether the snapshot contains a usable document. While a
// challenge page redirects, document.documentElement is briefly null and
// document.title empty, so reads must tolerate that instead of failing the
// whole fetch.
func (s snapshot) Ready() bool { return len(s.HTML) > 0 }

// readJS returns URL, title and markup in a single evaluation so the three
// cannot disagree across a navigation boundary. Null-guarded on purpose.
const readJS = `(function () {
  var de = document.documentElement;
  return {
    url: location.href,
    title: document.title || '',
    html: de ? de.outerHTML : ''
  };
})()`

// readTimeout bounds a single snapshot evaluation.
const readTimeout = 10 * time.Second

// pollInterval is how often we re-read a page we are waiting on.
const pollInterval = 500 * time.Millisecond

// readyWait is how long to wait for a document to exist after navigation
// before treating the page as broken.
const readyWait = 8 * time.Second

func (m *Manager) read(ctx context.Context, p *page) (snapshot, error) {
	readCtx, cancel := context.WithTimeout(p.ctx, readTimeout)
	defer cancel()
	unwatch := watch(ctx, cancel)
	defer unwatch()

	var snap snapshot
	if err := chromedp.Run(readCtx, chromedp.Evaluate(readJS, &snap)); err != nil {
		return snapshot{}, err
	}
	return snap, nil
}

// settle turns a freshly navigated tab into a Result, waiting out interstitials
// and optionally pausing for a human to click one.
func (m *Manager) settle(ctx context.Context, p *page, req Request, log *slog.Logger) (*Result, error) {
	snap, err := m.awaitReady(ctx, p)
	if err != nil {
		return nil, err
	}

	vendor := challenge.Detect(snap.HTML, snap.Title)
	var assistedFor time.Duration

	// Phase 1: let the interstitial resolve itself (Cloudflare reloads, JS
	// challenges that redirect once solved).
	if vendor != "" {
		log.Info("challenge detected, waiting for it to resolve",
			"vendor", vendor, "wait", m.opts.ChallengeWait)
		snap, vendor, err = m.waitOut(ctx, p, m.opts.ChallengeWait, snap)
		if err != nil {
			return nil, err
		}
	}

	// Phase 2: assist mode. Only useful with a visible browser, which is the
	// deployment this gateway is built for.
	if vendor != "" {
		interactive := challenge.Interactive(snap.HTML)
		assistWait := m.opts.AssistTimeout
		if req.Assist != nil {
			assistWait = *req.Assist
		}
		if assistWait > 0 {
			started := time.Now()
			m.addAssist(p.id, req.URL, vendor)
			log.Warn("challenge needs a human: solve it in the browser window",
				"vendor", vendor, "interactive", interactive,
				"tab", p.id, "wait", assistWait)
			snap, vendor, err = m.waitOut(ctx, p, assistWait, snap)
			m.removeAssist(p.id)
			assistedFor = time.Since(started)
			if err != nil {
				return nil, err
			}
		}
		if vendor != "" {
			return nil, &ChallengeError{Vendor: vendor, Interactive: interactive, URL: req.URL}
		}
		log.Info("challenge cleared", "after", assistedFor.Round(time.Millisecond))
	}

	return &Result{
		URL:        snap.URL,
		Title:      snap.Title,
		HTML:       snap.HTML,
		Status:     p.status(snap.URL),
		AssistedMS: assistedFor.Milliseconds(),
	}, nil
}

// awaitReady polls until the tab has a document, tolerating transient read
// failures caused by an in-flight navigation.
func (m *Manager) awaitReady(ctx context.Context, p *page) (snapshot, error) {
	deadline := time.Now().Add(readyWait)
	var lastErr error
	for {
		snap, err := m.read(ctx, p)
		if err == nil && snap.Ready() {
			return snap, nil
		}
		if err != nil {
			// A dead target is fatal; a mid-navigation read is not.
			if isFatalTabErr(err) {
				return snapshot{}, &NavError{Err: err}
			}
			lastErr = err
		}
		if time.Now().After(deadline) {
			if lastErr != nil {
				return snapshot{}, &NavError{Err: lastErr}
			}
			return snapshot{}, &NavError{Err: errEmptyDocument}
		}
		if err := sleepCtx(ctx, pollInterval); err != nil {
			return snapshot{}, err
		}
	}
}

// waitOut polls until the page no longer looks like a challenge, or until the
// limit expires. The last good snapshot is returned either way, so callers can
// report which vendor is still blocking.
func (m *Manager) waitOut(
	ctx context.Context,
	p *page,
	limit time.Duration,
	current snapshot,
) (snapshot, string, error) {
	deadline := time.Now().Add(limit)
	latest := current
	vendor := challenge.Detect(current.HTML, current.Title)

	for vendor != "" {
		if err := sleepCtx(ctx, pollInterval); err != nil {
			return latest, vendor, err
		}
		snap, err := m.read(ctx, p)
		if err != nil {
			if isFatalTabErr(err) {
				return latest, vendor, &NavError{Err: err}
			}
			// Transient: the challenge is probably navigating. Keep waiting.
		} else if snap.Ready() {
			latest = snap
			vendor = challenge.Detect(snap.HTML, snap.Title)
		}
		if vendor != "" && time.Now().After(deadline) {
			return latest, vendor, nil
		}
	}
	return latest, "", nil
}
