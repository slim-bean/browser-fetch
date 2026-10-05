package browser

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/chromedp/cdproto/cdp"
	cdppage "github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"github.com/slim-bean/browser-fetch/internal/challenge"
	"github.com/slim-bean/browser-fetch/internal/screenshot"
)

// No scrolling: wait only for currently visible images, at most 1500ms. This
// doesn't promise off-screen lazy images, virtualized lists or gallery slides.
const waitImagesJS = `(async () => {
 const images = [...document.images].filter(img => {
   const r = img.getBoundingClientRect();
   return r.width > 0 && r.height > 0 && r.bottom > 0 && r.top < innerHeight && r.right > 0 && r.left < innerWidth;
 });
 await Promise.race([
   Promise.all(images.map(img => img.complete ? Promise.resolve() : new Promise(resolve => {
     img.addEventListener('load', resolve, {once: true});
     img.addEventListener('error', resolve, {once: true});
   }))),
   new Promise(resolve => setTimeout(resolve, 1500))
 ]);
 return true;
})()`

func (m *Manager) capture(ctx context.Context, p *page, res *Result) (*screenshot.Capture, error) {
	captureCtx, cancel := context.WithTimeout(p.ctx, 15*time.Second)
	defer cancel()
	unwatch := watch(ctx, cancel)
	defer unwatch()
	var ready bool
	if err := chromedp.Run(captureCtx, chromedp.Evaluate(waitImagesJS, &ready, func(params *runtime.EvaluateParams) *runtime.EvaluateParams {
		return params.WithAwaitPromise(true)
	})); err != nil {
		return nil, err
	}
	// Refresh HTML after image settling, without another navigation.
	snap, err := m.read(ctx, p)
	if err != nil {
		return nil, err
	}
	if vendor := challenge.Detect(snap.HTML, snap.Title); vendor != "" {
		return nil, &ChallengeError{Vendor: vendor, URL: snap.URL}
	}
	res.URL, res.Title, res.HTML, res.Status = snap.URL, snap.Title, snap.HTML, p.status(snap.URL)
	var data []byte
	var pageWidth, pageHeight, height int
	err = chromedp.Run(captureCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		ctx = cdp.WithExecutor(ctx, chromedp.FromContext(ctx).Target)
		_, _, _, _, _, size, err := cdppage.GetLayoutMetrics().Do(ctx)
		if err != nil {
			return err
		}
		if size == nil || math.IsNaN(size.Width) || math.IsNaN(size.Height) || math.IsInf(size.Width, 0) || math.IsInf(size.Height, 0) || size.Width < 1 || size.Height < 1 || size.Width > 1e9 || size.Height > 1e9 {
			return errors.New("invalid screenshot layout dimensions")
		}
		pageWidth, pageHeight = int(math.Ceil(size.Width)), int(math.Ceil(size.Height))
		height = min(pageHeight, screenshot.MaxHeight)
		// Explicit clip bounds Chrome's allocation BEFORE any bitmap is returned.
		data, err = cdppage.CaptureScreenshot().WithFormat(cdppage.CaptureScreenshotFormatPng).
			WithCaptureBeyondViewport(true).WithClip(&cdppage.Viewport{X: 0, Y: 0, Width: screenshot.Width, Height: float64(height), Scale: 1}).Do(ctx)
		return err
	}))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	return screenshot.Split(ctx, data, pageWidth, pageHeight, height)
}
