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
	"os"
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

	// ChromeURL is the DevTools endpoint of an already-running Chrome.
	// http://host:port is resolved to the browser websocket automatically.
	ChromeURL string

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

	LogFormat string // text|json
	LogLevel  string // debug|info|warn|error
}

func Default() Config {
	return Config{
		Addr:                "127.0.0.1:8377",
		ChromeURL:           "http://127.0.0.1:9222",
		MaxTabs:             4,
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
		envBool("BROWSER_FETCH_ALLOW_PRIVATE", &c.AllowPrivate),
		envBool("BROWSER_FETCH_METRICS_AUTH", &c.MetricsAuth),
		envBool("BROWSER_FETCH_ALLOW_NO_TOKEN", &c.AllowNoToken),
	); err != nil {
		return c, err
	}

	fs := flag.NewFlagSet("browser-fetch", flag.ContinueOnError)
	fs.StringVar(&c.Addr, "addr", c.Addr, "listen address")
	fs.StringVar(&c.Token, "token", c.Token, "bearer token for /fetch, /stats, /debug")
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
	fs.BoolVar(&c.AllowPrivate, "allow-private", c.AllowPrivate, "allow private/loopback targets")
	fs.BoolVar(&c.Debug, "debug", c.Debug, "enable /debug and /debug/pprof")
	fs.IntVar(&c.DebugRing, "debug-ring", c.DebugRing, "recent requests retained for /debug")
	fs.BoolVar(&c.MetricsAuth, "metrics-auth", c.MetricsAuth, "require token on /metrics")
	fs.StringVar(&c.LogFormat, "log-format", c.LogFormat, "text|json")
	fs.StringVar(&c.LogLevel, "log-level", c.LogLevel, "debug|info|warn|error")
	if err := fs.Parse(args); err != nil {
		return c, err
	}

	return c, c.validate()
}

func (c Config) validate() error {
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
