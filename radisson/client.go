// Package radisson scrapes hotel rates from radissonhotels.com without a
// browser. A Session holds one TLS client, one cookie jar and one sticky
// proxy IP; Bootstrap clears Akamai Bot Manager (SBSD and the _abck sensor)
// and the API methods then call the same JSON endpoints the booking funnel
// calls.
package radisson

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/url"

	hyper "github.com/Hyper-Solutions/hyper-sdk-go/v3"
	http "github.com/bogdanfinn/fhttp"
	tlsclient "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

// Browser identity, copied from the recorded Chrome 155 session. The UA,
// sec-ch-ua and platform have to agree with each other on every request.
const (
	UserAgent       = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/155.0.0.0 Safari/537.36"
	SecChUA         = `"Google Chrome";v="155", "Chromium";v="155", "Not(A:Brand";v="24"`
	SecChUAPlatform = `"Windows"`

	Origin = "https://www.radissonhotels.com"
)

// Options configures a Session.
type Options struct {
	// ProxyURL is a sticky residential proxy, e.g.
	// http://user-session-abc:pass@network.joinmassive.com:65534.
	ProxyURL string
	// APIKey is the Hyper Solutions API key.
	APIKey string
	// Locale is the site locale used in page URLs and accept-language, e.g. "en-us".
	Locale string
	// Currency is the ISO currency the rates are quoted in, e.g. "USD".
	Currency string
	// TimeoutSeconds is the per-request timeout. Defaults to 45.
	TimeoutSeconds int
	// InsecureSkipVerify accepts any certificate. Only for routing the
	// scraper through a local debugging proxy such as powhttp.
	InsecureSkipVerify bool
	// BandwidthTracker enables tls-client's byte counter.
	BandwidthTracker bool

	// Trace, when set, is called after every request the session makes to
	// the site, with a step name such as "home" or "sensor_post".
	Trace func(step string, status int, err error)
}

// Session is one scraping identity: a TLS client, its cookies, the IP the
// proxy exits from, and the Akamai state collected during Bootstrap.
type Session struct {
	Client tlsclient.HttpClient
	IP     string

	hyper    *hyper.Session
	apiKey   string
	locale   string
	currency string
	// transactionID mirrors the value the site keeps in sessionStorage: 40
	// random base-15 digits, sent as the transactionid header on API calls.
	transactionID string

	akamai akamaiState
	opts   Options
}

// NewSession builds the TLS client. It makes no requests.
func NewSession(opts Options) (*Session, error) {
	if opts.Locale == "" {
		opts.Locale = "en-us"
	}
	if opts.Currency == "" {
		opts.Currency = "USD"
	}
	if opts.TimeoutSeconds == 0 {
		opts.TimeoutSeconds = 45
	}
	clientOpts := []tlsclient.HttpClientOption{
		tlsclient.WithClientProfile(profiles.Chrome_152),
		tlsclient.WithCookieJar(tlsclient.NewCookieJar()),
		tlsclient.WithNotFollowRedirects(),
		tlsclient.WithTimeoutSeconds(opts.TimeoutSeconds),
		tlsclient.WithRandomTLSExtensionOrder(),
		tlsclient.WithDisableHttp3(),
	}
	if opts.ProxyURL != "" {
		clientOpts = append(clientOpts, tlsclient.WithProxyUrl(opts.ProxyURL))
	}
	if opts.InsecureSkipVerify {
		clientOpts = append(clientOpts, tlsclient.WithInsecureSkipVerify())
	}
	if opts.BandwidthTracker {
		clientOpts = append(clientOpts, tlsclient.WithBandwidthTracker())
	}
	client, err := tlsclient.NewHttpClient(tlsclient.NewNoopLogger(), clientOpts...)
	if err != nil {
		return nil, err
	}
	s := &Session{
		Client:        client,
		hyper:         hyper.NewSession(opts.APIKey),
		apiKey:        opts.APIKey,
		locale:        opts.Locale,
		currency:      opts.Currency,
		transactionID: newTransactionID(),
		opts:          opts,
	}
	// The site's JavaScript sets this cookie when the visitor picks a currency.
	s.setCookie("preferredCurrency", opts.Currency)
	return s, nil
}

func newTransactionID() string {
	const digits = "0123456789abcde"
	b := make([]byte, 40)
	for i := range b {
		n, _ := rand.Int(rand.Reader, big.NewInt(15))
		b[i] = digits[n.Int64()]
	}
	return string(b)
}

// acceptLanguage turns "en-us" into the header Chrome sends for that locale.
func (s *Session) acceptLanguage() string {
	if s.locale == "en-us" {
		return "en-US,en;q=0.9"
	}
	return s.locale
}

// HomeURL is the page the bootstrap loads and the referer for its scripts.
func (s *Session) HomeURL() string { return Origin + "/" + s.locale + "/" }

// FetchIP asks the Hyper Solutions IP endpoint which address the proxy exits
// from. The sensor and SBSD payloads are generated for that IP.
func (s *Session) FetchIP() (string, error) {
	req, _ := http.NewRequest(http.MethodGet, "https://ip.hypersolutions.co/ip", nil)
	req.Header.Set("x-api-key", s.apiKey)
	status, raw, err := s.do("ip", req)
	if err != nil {
		return "", err
	}
	var body struct {
		IP string `json:"ip"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return "", err
	}
	if body.IP == "" {
		return "", fmt.Errorf("no ip in response (status %d)", status)
	}
	s.IP = body.IP
	return body.IP, nil
}

// Cookie returns the current value of a radissonhotels.com cookie.
func (s *Session) Cookie(name string) string {
	u, _ := url.Parse(Origin)
	for _, c := range s.Client.GetCookies(u) {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}

func (s *Session) setCookie(name, value string) {
	u, _ := url.Parse(Origin)
	s.Client.SetCookies(u, []*http.Cookie{{Name: name, Value: value, Path: "/", Domain: ".radissonhotels.com"}})
}

// do sends req and returns the status and the decompressed body. step names
// the request for Options.Trace.
func (s *Session) do(step string, req *http.Request) (int, []byte, error) {
	status, body, err := s.send(req)
	if s.opts.Trace != nil {
		s.opts.Trace(step, status, err)
	}
	return status, body, err
}

func (s *Session) send(req *http.Request) (int, []byte, error) {
	resp, err := s.Client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, body, err
}

// The header sets below follow the recorded Chrome session, including order.
// tls-client writes them in http.HeaderOrderKey order and the pseudo-headers
// in the profile's order.

func (s *Session) documentHeaders() http.Header {
	return http.Header{
		"upgrade-insecure-requests": {"1"},
		"user-agent":                {UserAgent},
		"accept":                    {"text/html,application/xhtml+xml,application/xml;q=0.9,image/jxl,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7"},
		"sec-fetch-site":            {"none"},
		"sec-fetch-mode":            {"navigate"},
		"sec-fetch-user":            {"?1"},
		"sec-fetch-dest":            {"document"},
		"sec-ch-ua":                 {SecChUA},
		"sec-ch-ua-mobile":          {"?0"},
		"sec-ch-ua-platform":        {SecChUAPlatform},
		"accept-encoding":           {"gzip, deflate, br, zstd"},
		"accept-language":           {s.acceptLanguage()},
		"priority":                  {"u=0, i"},
		http.HeaderOrderKey: {
			"upgrade-insecure-requests", "user-agent", "accept", "sec-fetch-site",
			"sec-fetch-mode", "sec-fetch-user", "sec-fetch-dest", "sec-ch-ua",
			"sec-ch-ua-mobile", "sec-ch-ua-platform", "referer", "accept-encoding",
			"accept-language", "cookie", "priority",
		},
	}
}

func (s *Session) scriptHeaders(referer string) http.Header {
	return http.Header{
		"sec-ch-ua-platform": {SecChUAPlatform},
		"user-agent":         {UserAgent},
		"sec-ch-ua":          {SecChUA},
		"sec-ch-ua-mobile":   {"?0"},
		"accept":             {"*/*"},
		"sec-fetch-site":     {"same-origin"},
		"sec-fetch-mode":     {"no-cors"},
		"sec-fetch-dest":     {"script"},
		"referer":            {referer},
		"accept-encoding":    {"gzip, deflate, br, zstd"},
		"accept-language":    {s.acceptLanguage()},
		"priority":           {"u=1"},
		http.HeaderOrderKey: {
			"sec-ch-ua-platform", "user-agent", "sec-ch-ua", "sec-ch-ua-mobile",
			"accept", "sec-fetch-site", "sec-fetch-mode", "sec-fetch-dest",
			"referer", "accept-encoding", "accept-language", "cookie", "priority",
		},
	}
}

// akamaiPostHeaders are used for both the sensor and the SBSD posts.
func (s *Session) akamaiPostHeaders(contentType, referer string) http.Header {
	return http.Header{
		"sec-ch-ua-platform": {SecChUAPlatform},
		"user-agent":         {UserAgent},
		"sec-ch-ua":          {SecChUA},
		"content-type":       {contentType},
		"sec-ch-ua-mobile":   {"?0"},
		"accept":             {"*/*"},
		"origin":             {Origin},
		"sec-fetch-site":     {"same-origin"},
		"sec-fetch-mode":     {"cors"},
		"sec-fetch-dest":     {"empty"},
		"referer":            {referer},
		"accept-encoding":    {"gzip, deflate, br, zstd"},
		"accept-language":    {s.acceptLanguage()},
		"priority":           {"u=1, i"},
		http.HeaderOrderKey: {
			"content-length", "sec-ch-ua-platform", "user-agent", "sec-ch-ua",
			"content-type", "sec-ch-ua-mobile", "accept", "origin",
			"sec-fetch-site", "sec-fetch-mode", "sec-fetch-dest", "referer",
			"accept-encoding", "accept-language", "cookie", "priority",
		},
	}
}

// apiHeaders are the booking funnel's fetch() headers. extra holds the
// per-endpoint ones (x-version, x-currency, x-origin) and post adds origin.
func (s *Session) apiHeaders(referer string, post bool, extra map[string]string) http.Header {
	h := http.Header{
		"transactionid":        {s.transactionID},
		"sec-ch-ua-platform":   {SecChUAPlatform},
		"accept-language":      {s.locale},
		"sec-ch-ua":            {SecChUA},
		"sec-ch-ua-mobile":     {"?0"},
		"user-agent":           {UserAgent},
		"content-type":         {"application/json"},
		"x-source-of-business": {"desktop"},
		"accept":               {"*/*"},
		"sec-fetch-site":       {"same-origin"},
		"sec-fetch-mode":       {"cors"},
		"sec-fetch-dest":       {"empty"},
		"referer":              {referer},
		"accept-encoding":      {"gzip, deflate, br, zstd"},
		"priority":             {"u=1, i"},
	}
	order := []string{"content-length", "transactionid"}
	for _, k := range []string{"x-version"} {
		if v, ok := extra[k]; ok {
			h[k] = []string{v}
			order = append(order, k)
		}
	}
	order = append(order, "sec-ch-ua-platform", "accept-language", "sec-ch-ua", "sec-ch-ua-mobile")
	for _, k := range []string{"x-currency", "x-origin"} {
		if v, ok := extra[k]; ok {
			h[k] = []string{v}
			order = append(order, k)
		}
	}
	order = append(order, "user-agent", "content-type", "x-source-of-business", "accept")
	if post {
		h["origin"] = []string{Origin}
		order = append(order, "origin")
	}
	order = append(order, "sec-fetch-site", "sec-fetch-mode", "sec-fetch-dest", "referer", "accept-encoding", "cookie", "priority")
	h[http.HeaderOrderKey] = order
	return h
}
