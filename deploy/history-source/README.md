# Optional local history-helper source

`../../scripts/stage-history.sh /path/to/pi-browser` creates the ignored
`pi-browser.tar.gz` bundle here. Build with `PI_BROWSER_SOURCE=local` to use it.
This is an explicit development override, not a release dependency pin.

Default/release builds ignore the bundle and fetch `PI_BROWSER_REF` from the
pi-browser repository. Pin a commit that includes `bin/history.ts`.
