// Package config is how shartifacts is started: command-line flags, each of which can also be
// set with a SHARTIFACTS_* environment variable, checked together before anything listens.
//
// The origins decide the operating mode:
//
//   - Private (no -share-origin): the workspace answers LAN, tailnet and loopback clients,
//     and nothing is reachable from the internet. No domain is needed.
//   - Public sharing (-share-origin): the workspace stays private, and one public hostname of
//     your own serves read-only artifact links and public app links, nothing else.
//     -app-origin adds a hostname per app under a wildcard, so app pages keep browser storage.
//
// -public-origin is independent of both: the https:// address people use for the private
// workspace when a TLS-terminating proxy (tailscale serve, Caddy, nginx) fronts it.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrUsage is a command line the flag package refused; it has already printed why.
var ErrUsage = errors.New("invalid command line")

// EnvPrefix starts every environment variable: -share-origin is SHARTIFACTS_SHARE_ORIGIN.
const EnvPrefix = "SHARTIFACTS_"

type Config struct {
	Listen string
	// ShareListen is the public listener, on when ShareOrigin is set: the address the edge
	// proxy forwards to, on which only share and app links are served whatever the Host.
	// "off" (empty after Parse) leaves only the main listener.
	ShareListen  string
	Data         string
	AllowedHosts []string
	// PublicOrigin is the external origin of a TLS-terminating proxy in front of the
	// workspace; its hostname is allowed without listing it in AllowedHosts.
	PublicOrigin string
	ShareOrigin  string
	AppOrigin    string
	// AuthProxy accepts workspace requests a proxy forwards from public addresses, for a
	// proxy that authenticates people itself.
	AuthProxy     bool
	NoChunkUpload bool
	ClaudeModel   string
	Connectors    string

	Runner       bool
	RunnerListen string
	AppsDir      string
	RunnerURL    string
	GoBinary     string
	AppProcs     int
	AppIdle      time.Duration
	// AppKeepVersions is how many of each app's newest built versions keep their files.
	AppKeepVersions int
	// AppDataQuota bounds each app's DATA_DIR, in bytes; 0 is no limit.
	AppDataQuota ByteSize
}

// EnvName is the environment variable that sets a flag.
func EnvName(flagName string) string {
	return EnvPrefix + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}

// Parse reads the command line, then the environment for every flag the command line left
// unset (an empty variable counts as unset), and checks the result. With -help it returns
// flag.ErrHelp after writing the usage to out.
func Parse(args []string, getenv func(string) string, out io.Writer) (*Config, error) {
	c := &Config{}
	fs := flag.NewFlagSet("shartifacts", flag.ContinueOnError)
	fs.SetOutput(out)
	var hosts string
	fs.StringVar(&c.Listen, "listen", ":8080", "address the workspace listens on")
	fs.StringVar(&c.ShareListen, "share-listen", ":8081", "with -share-origin: the address that serves only public share and app links, for your edge proxy to forward to. Whatever Host the proxy sends, nothing of the workspace is served here. \"off\" turns it off")
	fs.StringVar(&c.Data, "data", "./data", "data directory: artifacts, media, apps and history (back up this folder)")
	fs.StringVar(&hosts, "allowed-hosts", "", "comma-separated extra hostnames the workspace answers to, e.g. shartifacts.local,myserver.tailnet.ts.net; LAN IPs, localhost and the -public-origin hostname are always allowed")
	fs.StringVar(&c.PublicOrigin, "public-origin", "", "the https:// address people use for the workspace when a TLS-terminating proxy fronts it, e.g. https://myserver.tailnet.ts.net (tailscale serve) or https://shartifacts.example.com. Links agents hand out use it. Empty when people reach the app directly")
	fs.StringVar(&c.ShareOrigin, "share-origin", "", "public hostname for read-only artifact links and public app links, e.g. https://share.example.com. On it the server answers only share pages, never the workspace. Empty keeps everything private")
	fs.StringVar(&c.AppOrigin, "app-origin", "", "with -share-origin: a wildcard origin giving each app's public links a hostname of its own, e.g. https://*.share.example.com, so app pages keep localStorage, IndexedDB and cookies. Needs wildcard DNS and certificates (see docs/sharing.md)")
	fs.BoolVar(&c.AuthProxy, "auth-proxy", false, "the reverse proxy in front of the workspace authenticates everyone it forwards (Cloudflare Access, Authelia, oauth2-proxy). Without it, workspace requests a proxy forwards from a public address are refused, because the workspace is for private networks, with or without its own sign-in")
	fs.BoolVar(&c.NoChunkUpload, "no-chunk-upload", false, "hide the base64 chunked-upload MCP tools to save agent context (the HTTP upload stays available)")
	fs.StringVar(&c.ClaudeModel, "claude-model", "claude-opus-5-5", "model behind window.claude.complete and claude.use(\"sample\"), which are on only when ANTHROPIC_API_KEY is set")
	fs.StringVar(&c.Connectors, "connectors", "", "JSON file of MCP servers artifacts may call through claude.use(\"mcp\"): [{\"name\", \"url\", \"headers\"}]; credentials stay on this server")
	fs.BoolVar(&c.Runner, "runner", false, "run as the app runner sidecar: build and run app backends for a shartifacts whose -runner-url points here, and nothing else")
	fs.StringVar(&c.RunnerListen, "runner-listen", ":8090", "with -runner: the runner API's address (keep it on the compose network; never publish it)")
	fs.StringVar(&c.AppsDir, "apps-dir", "", "with -runner: the apps directory shared with shartifacts (its data/apps); default ./data/apps")
	fs.StringVar(&c.RunnerURL, "runner-url", "", "build and run app backends in a runner sidecar at this URL, e.g. http://runner:8090; empty runs them beside this process")
	fs.StringVar(&c.GoBinary, "go", "go", "Go toolchain that builds Go app backends")
	fs.IntVar(&c.AppProcs, "app-procs", 16, "most app backends running at once; the least recently used stops to make room")
	fs.DurationVar(&c.AppIdle, "app-idle", 15*time.Minute, "stop an app backend after this long without requests")
	fs.IntVar(&c.AppKeepVersions, "app-keep-versions", 20, "how many of each app's newest built versions keep their files, and so can be rolled back to; the live version and versions links pin are kept as well")
	fs.Var(&c.AppDataQuota, "app-data-quota", "with the runner: the most disk each app's backend data may take, e.g. 2GiB or 500MB; a backend over it is stopped until its data shrinks. Empty or 0 is no limit")
	fs.Usage = func() {
		fmt.Fprintf(out, "Usage: shartifacts [flags]\n       shartifacts auth COMMAND   (sign-in: shartifacts auth help)\n\nEvery flag can also be set with an environment variable: -share-origin is %s.\nA flag on the command line wins over the environment.\n\n", EnvName("share-origin"))
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, err
		}
		return nil, ErrUsage
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument %q (flags start with -)", fs.Arg(0))
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	var err error
	fs.VisitAll(func(f *flag.Flag) {
		if v := strings.TrimSpace(getenv(EnvName(f.Name))); v != "" && !set[f.Name] && err == nil {
			if e := fs.Set(f.Name, v); e != nil {
				err = fmt.Errorf("%s=%q: %v", EnvName(f.Name), v, e)
			}
		}
	})
	if err != nil {
		return nil, err
	}
	for _, h := range strings.Split(hosts, ",") {
		if h = hostname(h); h != "" {
			c.AllowedHosts = append(c.AllowedHosts, h)
		}
	}
	if err := c.check(); err != nil {
		return nil, err
	}
	return c, nil
}

// hostname reduces an -allowed-hosts entry to the bare name the server compares, forgiving a
// scheme, port or trailing slash or dot.
func hostname(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	if h == "" {
		return ""
	}
	if !strings.Contains(h, "://") {
		h = "//" + h
	}
	u, err := url.Parse(h)
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(u.Hostname(), ".")
}

// origin normalizes an origin setting: a bare hostname means https, and a trailing slash
// is dropped. It refuses paths, queries and other schemes.
func origin(name, v string) (string, error) {
	v = strings.TrimRight(strings.TrimSpace(v), "/")
	if v == "" {
		return "", nil
	}
	if !strings.Contains(v, "://") {
		v = "https://" + v
	}
	u, err := url.Parse(v)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("-%s %q is not an origin like https://host.example.com", name, v)
	}
	if u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", fmt.Errorf("-%s %q must be just scheme and host, with no path", name, v)
	}
	scheme, host := strings.ToLower(u.Scheme), strings.ToLower(u.Host)
	// Browsers leave a default port out of Host, so an origin must too, or nothing matches.
	if p := u.Port(); scheme == "https" && p == "443" || scheme == "http" && p == "80" {
		host = strings.TrimSuffix(host, ":"+p)
	}
	return scheme + "://" + host, nil
}

func originHost(o string) string {
	if o == "" {
		return ""
	}
	u, err := url.Parse(o)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

func (c *Config) check() error {
	var err error
	if c.AppKeepVersions < 1 {
		return fmt.Errorf("-app-keep-versions %d must be at least 1: the live version always keeps its files, and rollback needs the ones before it", c.AppKeepVersions)
	}
	if strings.EqualFold(c.ShareListen, "off") {
		c.ShareListen = ""
	}
	if c.PublicOrigin, err = origin("public-origin", c.PublicOrigin); err != nil {
		return err
	}
	if c.ShareOrigin, err = origin("share-origin", c.ShareOrigin); err != nil {
		return err
	}
	if c.AppOrigin, err = origin("app-origin", c.AppOrigin); err != nil {
		return err
	}
	for _, o := range []struct{ name, v string }{{"public-origin", c.PublicOrigin}, {"share-origin", c.ShareOrigin}} {
		if strings.Contains(o.v, "*") {
			return fmt.Errorf("-%s %q cannot hold a wildcard; only -app-origin does", o.name, o.v)
		}
	}
	// The workspace's names: everything a person on the network may open it by.
	workspace := append([]string{}, c.AllowedHosts...)
	if h := originHost(c.PublicOrigin); h != "" {
		workspace = append(workspace, h)
		if !contains(c.AllowedHosts, h) {
			c.AllowedHosts = append(c.AllowedHosts, h)
		}
	}
	share := originHost(c.ShareOrigin)
	if share != "" && contains(workspace, share) {
		return fmt.Errorf("-share-origin %s is also a workspace hostname (-allowed-hosts or -public-origin): the share hostname must be a name of its own, the only one the internet reaches", c.ShareOrigin)
	}
	if c.AppOrigin == "" {
		return nil
	}
	if c.ShareOrigin == "" {
		return errors.New("-app-origin needs -share-origin as well: app links fall back to the share hostname")
	}
	suffix, ok := strings.CutPrefix(originHost(c.AppOrigin), "*.")
	// A wildcard needs a name under it (not *.com), except *.localhost, which browsers resolve
	// to this machine for testing.
	if !ok || suffix == "" || strings.Contains(suffix, "*") || !strings.Contains(suffix, ".") && suffix != "localhost" {
		return fmt.Errorf("-app-origin %q needs one leading wildcard label, e.g. https://*.share.example.com", c.AppOrigin)
	}
	for _, h := range append(workspace, share) {
		if label, ok := strings.CutSuffix(h, "."+suffix); ok && label != "" && !strings.Contains(label, ".") {
			return fmt.Errorf("%s is one of the app hostnames -app-origin %s covers, so it would serve only app links; give apps a wildcard of their own, e.g. https://*.%s", h, c.AppOrigin, share)
		}
	}
	return nil
}

// ByteSize is a flag holding a size in bytes, written as 500MB, 2GiB or a plain number of
// bytes. K, M, G and T alone, and with iB, are powers of 1024; with B, powers of 1000.
type ByteSize int64

func (b *ByteSize) String() string {
	if b == nil || *b == 0 {
		return ""
	}
	n := int64(*b)
	for _, u := range []string{"TiB", "GiB", "MiB", "KiB"} {
		if size := sizeUnits[strings.ToLower(u)]; n%size == 0 {
			return fmt.Sprintf("%d%s", n/size, u)
		}
	}
	return fmt.Sprint(n)
}

var sizeUnits = map[string]int64{
	"": 1, "b": 1,
	"k": 1 << 10, "kib": 1 << 10, "kb": 1e3,
	"m": 1 << 20, "mib": 1 << 20, "mb": 1e6,
	"g": 1 << 30, "gib": 1 << 30, "gb": 1e9,
	"t": 1 << 40, "tib": 1 << 40, "tb": 1e12,
}

func (b *ByteSize) Set(v string) error {
	v = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(v), " ", ""))
	i := strings.IndexFunc(v, func(r rune) bool { return (r < '0' || r > '9') && r != '.' })
	if i < 0 {
		i = len(v)
	}
	unit, ok := sizeUnits[v[i:]]
	n, err := strconv.ParseFloat(v[:i], 64)
	if !ok || err != nil || n < 0 {
		return fmt.Errorf("%q is not a size like 500MB or 2GiB", v)
	}
	if n*float64(unit) >= 1<<62 {
		return fmt.Errorf("%q is too large", v)
	}
	*b = ByteSize(n * float64(unit))
	return nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if strings.EqualFold(x, v) {
			return true
		}
	}
	return false
}

// Mode names the operating mode, for logs and the UI: "private" or "public-sharing".
func (c *Config) Mode() string {
	if c.ShareOrigin == "" {
		return "private"
	}
	return "public-sharing"
}

// Summary describes what this configuration exposes, a line at a time, for the startup log.
func (c *Config) Summary() []string {
	var lines []string
	if c.ShareOrigin == "" {
		lines = append(lines, "mode: private: the workspace answers LAN, tailnet and loopback clients only, and nothing is public (set "+EnvName("share-origin")+" to share links outside your network)")
	} else {
		edge := "; point your edge proxy at " + c.ShareListen
		if c.ShareListen == "" {
			edge = "; the public listener is off, so your edge proxy must forward them to " + c.Listen
		}
		lines = append(lines, "mode: public sharing: the workspace stays private; "+c.ShareOrigin+" serves only share links (/s/…) and public app links (/p/…)"+edge)
		if c.AppOrigin != "" {
			lines = append(lines, "app links: each app on a hostname of its own under "+c.AppOrigin)
		}
	}
	if c.PublicOrigin != "" {
		lines = append(lines, "workspace address: "+c.PublicOrigin+" (through a TLS-terminating proxy, which must stay reachable only from your LAN or tailnet)")
	}
	if c.AuthProxy {
		lines = append(lines, "auth proxy: workspace requests forwarded from public addresses are accepted; the proxy in front must authenticate every one of them")
	}
	return lines
}
