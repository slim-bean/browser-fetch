// Package config holds the gateway's runtime configuration.
//
// Every setting is available as a flag and as a BROWSER_FETCH_* environment
// variable; flags win. Defaults are chosen to be safe on a single-user VM:
// loopback bind, token required, modest tab count, and per-host pacing that
// looks like a human clicking links rather than a crawler.
package config

import (
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	// Addr is the listen address. Loopback by default; use the VM's private
	// interface (e.g. 192.168.64.7:8377) to reach it from the host.
	Addr string
	// Token is the bearer token required on /fetch, /stats and /debug.
	Token string
	// AllowNoToken permits starting without a token (local experiments only).
	AllowNoToken bool
	// DriverToken grants sessions/macros and fetch, not root-only CDP/history/admin.
	// Empty = root Token holders only.
	DriverToken string
	// ReaderToken is a restricted token: /fetch only, never sessions. Empty =
	// no separate reader class.
	ReaderToken string

	// ChromeURL is the DevTools endpoint of an already-running Chrome.
	// http://host:port is resolved to the browser websocket automatically.
	ChromeURL string
	// Optional authenticated browser control/history on the same HTTP listener.
	EnableCDP     bool
	PublicURL     string // explicit external base URL for discovery behind a reverse proxy
	HistoryRoot   string // explicit Chromium user-data directory; empty disables history
	HistorySource string // source label/id prefix, not a filesystem path

	// MaxTabs caps concurrent navigations (one tab each).
	MaxTabs int
	// HostGap is the minimum delay between two navigations to the same host.
	HostGap time.Duration
	// HostJitter is added to HostGap, uniformly at random, per request.
	HostJitter time.Duration

	// RequestTimeout bounds a whole /fetch call including queue wait.
	RequestTimeout time.Duration
	// NavTimeout bounds a single navigation.
	NavTimeout time.Duration
	// ChallengeWait is how long to let an interstitial resolve itself.
	ChallengeWait time.Duration
	// AssistTimeout, when > 0, keeps an unsolved challenge tab open this long
	// so a human at the VM's screen can click it. 0 disables assist mode.
	AssistTimeout time.Duration
	// ChallengeRetries re-navigates after an unresolved challenge; the failed
	// attempt often leaves cookies that make the retry succeed.
	ChallengeRetries int
	// ChallengeRetryDelay waits before that retry.
	ChallengeRetryDelay time.Duration

	// BlockMedia drops images/media/fonts to save bandwidth. Off by default:
	// some bot-protection scoring notices that images never loaded.
	BlockMedia bool
	// BackgroundTabs prevents worker creation from activating Chrome.
	BackgroundTabs bool
	// AllowPrivate permits fetching loopback/private/link-local addresses.
	// Off by default so a prompt-injected URL cannot reach VM-internal or
	// cloud-metadata endpoints through the browser.
	AllowPrivate bool

	// Debug enables /debug and /debug/pprof. On by default.
	Debug bool
	// DebugRing is how many recent requests /debug retains.
	DebugRing int
	// MetricsAuth requires the bearer token on /metrics too.
	MetricsAuth bool

	// MaxSessions caps concurrent L1 sessions (one tab each).
	MaxSessions int
	// SessionIdle evicts a session idle longer than this.
	SessionIdle time.Duration
	// ActionTimeout bounds one session action.
	ActionTimeout time.Duration
	// AssertTTL is how long a passing assertion stays fresh for typing.
	AssertTTL time.Duration
	// AllowHosts restricts session navigations to these hosts (comma-separated;
	// entries are exact hostnames; a leading dot matches subdomains). Empty =
	// any host the URL guard already allows. This is a tripwire on the
	// navigate primitive, not a sandbox: clicking a link can still navigate.
	AllowHosts []string
	// MacroStore is the directory macro recordings are persisted to. Empty =
	// macro endpoints disabled.
	MacroStore string
	// AdminAddr is the listen address for the human-only admin band (macro
	// approve/revoke/edit/delete). Empty = admin band disabled.
	AdminAddr string
	// AdminToken is the bearer token for the admin band. It MUST differ from
	// every agent-facing token: the admin band is the only path to approval
	// authority, and it must be unreachable by the agent.
	AdminToken string

	// OpServiceAccountToken is the 1Password service account token used to
	// resolve op:// secret references at replay time. Mounted from a
	// Kubernetes Secret into the gateway pod only; it MUST NOT be set in any
	// agent environment. Empty = no resolver: type steps abort at replay
	// (fail-closed).
	OpServiceAccountToken string

	LogFormat string // text|json
	LogLevel  string // debug|info|warn|error
}

func Default() Config {
	return Config{
		Addr:                "127.0.0.1:8377",
		ChromeURL:           "http://127.0.0.1:9222",
		MaxTabs:             4,
		HistorySource:       "assistant",
		HostGap:             1500 * time.Millisecond,
		HostJitter:          750 * time.Millisecond,
		RequestTimeout:      45 * time.Second,
		NavTimeout:          20 * time.Second,
		ChallengeWait:       15 * time.Second,
		AssistTimeout:       0,
		ChallengeRetries:    1,
		ChallengeRetryDelay: 2 * time.Second,
		Debug:               true,
		DebugRing:           200,
		MaxSessions:         2,
		SessionIdle:         10 * time.Minute,
		ActionTimeout:       30 * time.Second,
		AssertTTL:           30 * time.Second,
		LogFormat:           "text",
		LogLevel:            "info",
	}
}

// Load merges defaults, BROWSER_FETCH_* environment variables, and flags.
func Load(args []string) (Config, error) {
	c := Default()

	// Environment first so flags can override.
	envStr("BROWSER_FETCH_ADDR", &c.Addr)
	envStr("BROWSER_FETCH_TOKEN", &c.Token)
	envStr("BROWSER_FETCH_DRIVER_TOKEN", &c.DriverToken)
	envStr("BROWSER_FETCH_READER_TOKEN", &c.ReaderToken)
	envStr("BROWSER_FETCH_CHROME_URL", &c.ChromeURL)
	envStr("BROWSER_FETCH_LOG_FORMAT", &c.LogFormat)
	envStr("BROWSER_FETCH_LOG_LEVEL", &c.LogLevel)
	if err := errors.Join(
		envInt("BROWSER_FETCH_MAX_TABS", &c.MaxTabs),
		envInt("BROWSER_FETCH_DEBUG_RING", &c.DebugRing),
		envDur("BROWSER_FETCH_HOST_GAP", &c.HostGap),
		envDur("BROWSER_FETCH_HOST_JITTER", &c.HostJitter),
		envDur("BROWSER_FETCH_REQUEST_TIMEOUT", &c.RequestTimeout),
		envDur("BROWSER_FETCH_NAV_TIMEOUT", &c.NavTimeout),
		envDur("BROWSER_FETCH_CHALLENGE_WAIT", &c.ChallengeWait),
		envDur("BROWSER_FETCH_ASSIST_TIMEOUT", &c.AssistTimeout),
		envInt("BROWSER_FETCH_CHALLENGE_RETRIES", &c.ChallengeRetries),
		envDur("BROWSER_FETCH_CHALLENGE_RETRY_DELAY", &c.ChallengeRetryDelay),
		envBool("BROWSER_FETCH_DEBUG", &c.Debug),
		envBool("BROWSER_FETCH_BLOCK_MEDIA", &c.BlockMedia),
		envBool("BROWSER_FETCH_BACKGROUND_TABS", &c.BackgroundTabs),
		envBool("BROWSER_FETCH_ALLOW_PRIVATE", &c.AllowPrivate),
		envBool("BROWSER_FETCH_METRICS_AUTH", &c.MetricsAuth),
		envBool("BROWSER_FETCH_ALLOW_NO_TOKEN", &c.AllowNoToken),
		envInt("BROWSER_FETCH_MAX_SESSIONS", &c.MaxSessions),
		envDur("BROWSER_FETCH_SESSION_IDLE", &c.SessionIdle),
		envDur("BROWSER_FETCH_ACTION_TIMEOUT", &c.ActionTimeout),
		envDur("BROWSER_FETCH_ASSERT_TTL", &c.AssertTTL),
		envStringList("BROWSER_FETCH_ALLOW_HOSTS", &c.AllowHosts),
	); err != nil {
		return c, err
	}

	if os.Getenv("BROWSER_FETCH_HISTORY_COMMAND") != "" {
		return c, errors.New("BROWSER_FETCH_HISTORY_COMMAND was removed; configure BROWSER_FETCH_HISTORY_ROOT for native Chromium history")
	}
	envStr("BROWSER_FETCH_HISTORY_ROOT", &c.HistoryRoot)
	envStr("BROWSER_FETCH_HISTORY_SOURCE", &c.HistorySource)
	envStr("BROWSER_FETCH_MACRO_STORE", &c.MacroStore)
	envStr("BROWSER_FETCH_ADMIN_ADDR", &c.AdminAddr)
	envStr("BROWSER_FETCH_ADMIN_TOKEN", &c.AdminToken)
	envStr("BROWSER_FETCH_OP_TOKEN", &c.OpServiceAccountToken)
	envStr("BROWSER_FETCH_PUBLIC_URL", &c.PublicURL)
	if err := envBool("BROWSER_FETCH_ENABLE_CDP", &c.EnableCDP); err != nil {
		return c, err
	}
	fs := flag.NewFlagSet("browser-fetch", flag.ContinueOnError)
	fs.BoolVar(&c.EnableCDP, "enable-cdp", c.EnableCDP, "expose authenticated /cdp discovery and WebSockets (full browser authority)")
	fs.StringVar(&c.PublicURL, "public-url", c.PublicURL, "external gateway base URL for CDP discovery behind a reverse proxy")
	fs.StringVar(&c.HistoryRoot, "history-root", c.HistoryRoot, "Chromium user-data directory to expose as native history (empty disables)")
	fs.StringVar(&c.HistorySource, "history-source", c.HistorySource, "history source id/label prefix")
	fs.StringVar(&c.Addr, "addr", c.Addr, "listen address")
	fs.StringVar(&c.Token, "token", c.Token, "bearer token for /fetch, /stats, /debug")
	fs.StringVar(&c.DriverToken, "driver-token", c.DriverToken, "extra token allowed to drive sessions")
	fs.StringVar(&c.ReaderToken, "reader-token", c.ReaderToken, "restricted token: /fetch only, no sessions")
	fs.Var(&hostList{&c.AllowHosts}, "allow-hosts", "comma-separated hosts sessions may navigate to")
	fs.StringVar(&c.MacroStore, "macro-store", c.MacroStore, "directory for macro recordings (empty disables macro endpoints)")
	fs.StringVar(&c.AdminAddr, "admin-addr", c.AdminAddr, "listen address for the human-only macro admin band (empty disables)")
	fs.StringVar(&c.AdminToken, "admin-token", c.AdminToken, "bearer token for the admin band; must differ from agent tokens")
	fs.StringVar(&c.OpServiceAccountToken, "op-token", c.OpServiceAccountToken, "1Password service account token for op:// secret resolution (empty disables; prefer the env var)")
	fs.BoolVar(&c.AllowNoToken, "allow-no-token", c.AllowNoToken, "start without a token (local only)")
	fs.StringVar(&c.ChromeURL, "chrome-url", c.ChromeURL, "Chrome DevTools endpoint")
	fs.IntVar(&c.MaxTabs, "max-tabs", c.MaxTabs, "max concurrent navigations")
	fs.DurationVar(&c.HostGap, "host-gap", c.HostGap, "min delay between requests to one host")
	fs.DurationVar(&c.HostJitter, "host-jitter", c.HostJitter, "random extra delay per host request")
	fs.DurationVar(&c.RequestTimeout, "request-timeout", c.RequestTimeout, "overall /fetch deadline")
	fs.DurationVar(&c.NavTimeout, "nav-timeout", c.NavTimeout, "single navigation deadline")
	fs.DurationVar(&c.ChallengeWait, "challenge-wait", c.ChallengeWait, "how long to let a challenge self-resolve")
	fs.DurationVar(&c.AssistTimeout, "assist-timeout", c.AssistTimeout, "hold unsolved challenges for a human (0=off)")
	fs.IntVar(&c.ChallengeRetries, "challenge-retries", c.ChallengeRetries, "re-navigations after an unresolved challenge")
	fs.DurationVar(&c.ChallengeRetryDelay, "challenge-retry-delay", c.ChallengeRetryDelay, "delay before a challenge retry")
	fs.BoolVar(&c.BlockMedia, "block-media", c.BlockMedia, "block images/media/fonts")
	fs.BoolVar(&c.BackgroundTabs, "background-tabs", c.BackgroundTabs, "create worker tabs without activating Chrome")
	fs.BoolVar(&c.AllowPrivate, "allow-private", c.AllowPrivate, "allow private/loopback targets")
	fs.BoolVar(&c.Debug, "debug", c.Debug, "enable /debug and /debug/pprof")
	fs.IntVar(&c.DebugRing, "debug-ring", c.DebugRing, "recent requests retained for /debug")
	fs.BoolVar(&c.MetricsAuth, "metrics-auth", c.MetricsAuth, "require token on /metrics")
	fs.IntVar(&c.MaxSessions, "max-sessions", c.MaxSessions, "max concurrent L1 sessions")
	fs.DurationVar(&c.SessionIdle, "session-idle", c.SessionIdle, "evict sessions idle longer than this")
	fs.DurationVar(&c.ActionTimeout, "action-timeout", c.ActionTimeout, "deadline for one session action")
	fs.DurationVar(&c.AssertTTL, "assert-ttl", c.AssertTTL, "how long a passing assertion stays fresh for typing")
	fs.StringVar(&c.LogFormat, "log-format", c.LogFormat, "text|json")
	fs.StringVar(&c.LogLevel, "log-level", c.LogLevel, "debug|info|warn|error")
	if err := fs.Parse(args); err != nil {
		return c, err
	}

	return c, c.validate()
}

func (c Config) validate() error {
	if (c.EnableCDP || c.HistoryRoot != "") && c.Token == "" {
		return errors.New("CDP/history require a bearer token, even with allow-no-token")
	}
	if !regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`).MatchString(c.HistorySource) {
		return errors.New("history-source must be a lowercase id (1–64 characters)")
	}
	if c.EnableCDP && !strings.HasPrefix(c.ChromeURL, "http://") {
		return errors.New("CDP proxy requires an http:// Chrome discovery endpoint")
	}
	if c.PublicURL != "" {
		u, err := url.Parse(c.PublicURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("public-url must be an http(s) base URL without credentials, query or fragment")
		}
	}
	if c.Token == "" && !c.AllowNoToken {
		return errors.New("no token set: pass -token / BROWSER_FETCH_TOKEN, or -allow-no-token to run unauthenticated")
	}
	if c.MaxTabs < 1 {
		return errors.New("max-tabs must be >= 1")
	}
	if c.DebugRing < 1 {
		return errors.New("debug-ring must be >= 1")
	}
	if !strings.HasPrefix(c.ChromeURL, "http://") && !strings.HasPrefix(c.ChromeURL, "ws://") {
		return fmt.Errorf("chrome-url must start with http:// or ws://, got %q", c.ChromeURL)
	}
	if c.MaxSessions < 1 {
		return errors.New("max-sessions must be >= 1")
	}
	if c.DriverToken != "" && c.DriverToken == c.Token {
		return errors.New("driver-token must differ from token")
	}
	if c.ReaderToken != "" && (c.ReaderToken == c.Token || c.ReaderToken == c.DriverToken) {
		return errors.New("reader-token must differ from the other tokens")
	}
	if c.AdminAddr != "" {
		if c.MacroStore == "" {
			return errors.New("admin band requires macro-store to be set")
		}
		if c.AdminToken == "" {
			return errors.New("admin band requires an admin-token")
		}
		if c.AdminToken == c.Token || c.AdminToken == c.DriverToken || c.AdminToken == c.ReaderToken {
			return errors.New("admin-token must differ from every agent-facing token")
		}
	}
	if c.OpServiceAccountToken != "" {
		for _, clash := range []string{c.Token, c.DriverToken, c.ReaderToken, c.AdminToken} {
			if clash != "" && c.OpServiceAccountToken == clash {
				return errors.New("op-token must differ from every gateway/admin token")
			}
		}
	}
	return nil
}

// hostList is a flag.Value decoding "a.com,.b.com,c.org".
type hostList struct {
	p *[]string
}

func (h hostList) String() string { return strings.Join(*h.p, ",") }

func (h hostList) Set(s string) error {
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = strings.ToLower(strings.TrimSpace(part))
		if part == "" {
			continue
		}
		if !strings.HasPrefix(part, ".") && strings.Count(part, "*") > 0 {
			return fmt.Errorf("allow-hosts entries are exact hosts or .domain wildcards, got %q", part)
		}
		out = append(out, part)
	}
	*h.p = out
	return nil
}

func envStr(key string, dst *string) {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		*dst = v
	}
}

func envInt(key string, dst *int) error {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	*dst = n
	return nil
}

func envDur(key string, dst *time.Duration) error {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	*dst = d
	return nil
}

func envBool(key string, dst *bool) error {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	*dst = b
	return nil
}

func envStringList(key string, dst *[]string) error {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return nil
	}
	return hostList{dst}.Set(v)
}
