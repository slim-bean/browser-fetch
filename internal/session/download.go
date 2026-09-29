package session

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/chromedp"
)

// DownloadAction triggers a download and returns the file bytes. It sets a
// per-tab download behaviour (allowAndName into a temp dir), optionally clicks
// a selector to start the download, then waits for the CDP completed event and
// reads the file. The temp file is removed after reading.
type DownloadAction struct {
	// ClickSelector starts the download by clicking this element. Empty means
	// the download was already triggered (e.g. a form submit in a prior action
	// issued with download-pending behaviour).
	ClickSelector string `json:"click_selector,omitempty"`
	// FilenameRegexp, when set, must match the suggested filename; the first
	// matching download completes the action.
	FilenameRegexp string `json:"filename_regexp,omitempty"`
	// TimeoutMS bounds the wait for the completed event.
	TimeoutMS int64 `json:"timeout_ms,omitempty"`
	// MaxBytes caps the returned file size.
	MaxBytes int64 `json:"max_bytes,omitempty"`
}

func (DownloadAction) Kind() string { return "download" }

type DownloadResult struct {
	Filename      string `json:"filename"`
	URL           string `json:"url"`
	Bytes         int    `json:"bytes"`
	SHA256        string `json:"sha256"`
	ContentBase64 string `json:"content_base64"`
}

// downloadCommandContext derives a context from the tab's chromedp context
// for the download's command phase (SetDownloadBehavior + trigger click). The
// caller's context (HTTP request) still cancels it early; the wait loop after
// the commands continues to use the caller's ctx.
func downloadCommandContext(ctx context.Context, tab Tab) (context.Context, context.CancelFunc, func()) {
	cmdCtx, cancel := context.WithTimeout(tab.Ctx(), 30*time.Second)
	callerDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			cancel()
		case <-callerDone:
		}
	}()
	return cmdCtx, cancel, func() { close(callerDone) }
}

func dispatchDownload(ctx context.Context, tab Tab, a DownloadAction) (any, error) {
	if a.ClickSelector == "" && a.FilenameRegexp == "" {
		// Without a click we cannot know which download to wait for; require
		// at least one anchor.
		return nil, errors.New("download needs click_selector")
	}
	dir, err := os.MkdirTemp("", "bf-download-")
	if err != nil {
		return nil, fmt.Errorf("temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	timeout := time.Duration(a.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	maxBytes := a.MaxBytes
	if maxBytes <= 0 {
		maxBytes = 50 << 20 // 50 MiB
	}

	// Events fire on the tab's context; register before triggering.
	completed := make(chan *browser.EventDownloadProgress, 4)
	began := make(chan *browser.EventDownloadWillBegin, 4)
	chromedp.ListenTarget(tab.Ctx(), func(ev any) {
		switch e := ev.(type) {
		case *browser.EventDownloadWillBegin:
			select {
			case began <- e:
			default:
			}
		case *browser.EventDownloadProgress:
			select {
			case completed <- e:
			default:
			}
		}
	})

	// Browser-domain commands (SetDownloadBehavior) and the trigger click run
	// on the tab's chromedp context; ctx only bounds the wait loop below.
	cmdCtx, cancelCmd, unwatch := downloadCommandContext(ctx, tab)
	defer unwatch()
	defer cancelCmd()

	if err := runOn(cmdCtx, browser.SetDownloadBehavior("allowAndName").
		WithDownloadPath(dir).WithEventsEnabled(true)); err != nil {
		return nil, fmt.Errorf("set download behavior: %w", err)
	}

	if a.ClickSelector != "" {
		if err := runOn(cmdCtx, chromedp.Click(a.ClickSelector, chromedp.ByQueryAll, chromedp.NodeVisible)); err != nil {
			return nil, fmt.Errorf("download trigger click: %w", err)
		}
	}

	deadline := time.Now().Add(timeout)
	var guid, filename, url string
	for {
		select {
		case b := <-began:
			if guid == "" || b.GUID == guid {
				guid = b.GUID
				filename, url = b.SuggestedFilename, b.URL
			}
		case p := <-completed:
			if guid == "" {
				guid = p.GUID
			}
			if p.GUID != guid {
				continue
			}
			if p.State == browser.DownloadProgressStateCanceled {
				return nil, fmt.Errorf("download canceled: %s", filename)
			}
			if p.State != browser.DownloadProgressStateCompleted {
				if time.Now().After(deadline) {
					return nil, errors.New("download did not complete in time")
				}
				continue
			}
			path := p.FilePath
			if path == "" {
				path = filepath.Join(dir, guid)
			}
			return readDownload(path, filename, url, maxBytes)
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Until(deadline)):
			return nil, errors.New("download did not complete in time")
		}
	}
}

func readDownload(path, filename, url string, maxBytes int64) (any, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("downloaded file: %w", err)
	}
	if st.Size() > maxBytes {
		return nil, fmt.Errorf("download %d bytes exceeds cap %d", st.Size(), maxBytes)
	}
	buf, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(buf)
	return DownloadResult{
		Filename:      filename,
		URL:           url,
		Bytes:         len(buf),
		SHA256:        fmt.Sprintf("%x", sum),
		ContentBase64: base64.StdEncoding.EncodeToString(buf),
	}, nil
}
