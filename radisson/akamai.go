package radisson

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	hyper "github.com/Hyper-Solutions/hyper-sdk-go/v3"
	"github.com/Hyper-Solutions/hyper-sdk-go/v3/akamai"
	http "github.com/bogdanfinn/fhttp"
)

// akamaiState is what a session keeps from the bootstrap so it can answer
// later challenges without loading the page again.
type akamaiState struct {
	pageURL string

	sensorURL     string
	sensorScript  string
	sensorContext string

	sbsdPath    string
	sbsdUUID    string
	sbsdScript  string
	sbsdContext string
	sbsdIndex   int
}

// sbsdRe matches the SBSD script tag: a dynamic path with a ?v=<uuid> query
// and, on a hard challenge page, a t token (written as &amp;t= in the HTML).
// The sensor script uses a path of the same shape without a query, which
// akamai.ParseScriptPath finds.
var sbsdRe = regexp.MustCompile(`src="((?:/[\w\-.]+)+)\?v=([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})(?:&(?:amp;)?t=(\d+))?"`)

// BootstrapReport says what the bootstrap had to do.
type BootstrapReport struct {
	SensorURL     string `json:"sensorUrl"`
	SbsdURL       string `json:"sbsdUrl,omitempty"`
	SbsdHardPosts int    `json:"sbsdHardPosts"`
	SbsdPosts     int    `json:"sbsdPosts"`
	SensorPosts   int    `json:"sensorPosts"`
	PixelPosted   bool   `json:"pixelPosted"`
	AbckValid     bool   `json:"abckValid"`
}

// Bootstrap loads the home page and clears Akamai the way a browser does.
// If the page is an SBSD hard challenge it answers it and loads the page
// again. On the real page it posts two SBSD payloads to the ?v= script, then
// sensor data to the _abck script until the cookie carries the ~0~ stop
// signal, with the pixel challenge after the first sensor.
func (s *Session) Bootstrap(ctx context.Context) (*BootstrapReport, error) {
	if s.IP == "" {
		if _, err := s.FetchIP(); err != nil {
			return nil, fmt.Errorf("ip: %w", err)
		}
	}
	page := s.HomeURL()
	s.akamai = akamaiState{pageURL: page}
	rep := &BootstrapReport{}

	html, err := s.loadHome(ctx)
	if err != nil {
		return rep, err
	}
	for {
		m := sbsdRe.FindSubmatch(html)
		if m == nil {
			break
		}
		s.useSbsdScript(string(m[1]), string(m[2]))
		t := string(m[3])
		if err := s.loadSbsdScript(ctx, t); err != nil {
			return rep, err
		}
		if t == "" {
			rep.SbsdURL = Origin + s.akamai.sbsdPath + "?v=" + s.akamai.sbsdUUID
			for i := 0; i < 2; i++ {
				if err := s.postSbsd(ctx, ""); err != nil {
					return rep, err
				}
				rep.SbsdPosts++
			}
			break
		}
		if rep.SbsdHardPosts == 2 {
			return rep, errors.New("still on the sbsd challenge page after 2 answers")
		}
		if err := s.postSbsd(ctx, t); err != nil {
			return rep, err
		}
		rep.SbsdHardPosts++
		if html, err = s.loadHome(ctx); err != nil {
			return rep, err
		}
	}

	path, err := akamai.ParseScriptPath(bytes.NewReader(html))
	if err != nil {
		return rep, fmt.Errorf("sensor script path: %w", err)
	}
	s.akamai.sensorURL = Origin + path
	rep.SensorURL = s.akamai.sensorURL

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, s.akamai.sensorURL, nil)
	req.Header = s.scriptHeaders(page)
	status, script, err := s.do("sensor_script", req)
	if err != nil {
		return rep, fmt.Errorf("sensor script: %w", err)
	}
	if status != http.StatusOK {
		return rep, fmt.Errorf("sensor script: status %d", status)
	}
	s.akamai.sensorScript = string(script)

	for i := 0; i < 3; i++ {
		if err := s.postSensor(ctx); err != nil {
			return rep, err
		}
		rep.SensorPosts++
		// The page posts the pixel between its first and second sensor.
		if i == 0 {
			if err := s.postPixel(ctx, html); err != nil {
				return rep, err
			}
			rep.PixelPosted = true
		}
		if akamai.IsCookieValid(s.Cookie("_abck"), i) {
			rep.AbckValid = true
			break
		}
	}
	if !rep.AbckValid {
		return rep, errors.New("_abck not valid after 3 sensors")
	}
	return rep, nil
}

func (s *Session) loadHome(ctx context.Context) ([]byte, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, s.akamai.pageURL, nil)
	req.Header = s.documentHeaders()
	status, html, err := s.do("home", req)
	if err != nil {
		return nil, fmt.Errorf("home page: %w", err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("home page: status %d", status)
	}
	return html, nil
}

// useSbsdScript switches to an SBSD script. The challenge page and the real
// page serve different scripts, and a context belongs to one script.
func (s *Session) useSbsdScript(path, uuid string) {
	if path == s.akamai.sbsdPath && uuid == s.akamai.sbsdUUID {
		return
	}
	s.akamai.sbsdPath, s.akamai.sbsdUUID = path, uuid
	s.akamai.sbsdScript, s.akamai.sbsdContext, s.akamai.sbsdIndex = "", "", 0
}

func (s *Session) loadSbsdScript(ctx context.Context, t string) error {
	if s.akamai.sbsdScript != "" {
		return nil
	}
	u := Origin + s.akamai.sbsdPath + "?v=" + s.akamai.sbsdUUID
	if t != "" {
		u += "&t=" + t
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	// Chrome sends no priority header for this deferred script.
	req.Header = s.scriptHeaders(s.akamai.pageURL)
	delete(req.Header, "priority")
	order := req.Header[http.HeaderOrderKey]
	req.Header[http.HeaderOrderKey] = order[:len(order)-1]
	status, script, err := s.do("sbsd_script", req)
	if err != nil {
		return fmt.Errorf("sbsd script: %w", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("sbsd script: status %d", status)
	}
	s.akamai.sbsdScript = string(script)
	return nil
}

// postSbsd generates one SBSD payload and posts it. t is empty for the
// passive posts after page load and set to the token from a 429 block.
func (s *Session) postSbsd(ctx context.Context, t string) error {
	o := s.Cookie("sbsd_o")
	if o == "" {
		o = s.Cookie("bm_so")
	}
	in := &hyper.SbsdInput{
		Index:          s.akamai.sbsdIndex,
		UserAgent:      UserAgent,
		Uuid:           s.akamai.sbsdUUID,
		PageUrl:        s.akamai.pageURL,
		OCookie:        o,
		AcceptLanguage: s.acceptLanguage(),
		IP:             s.IP,
	}
	// The first call uploads the script; every later one sends the context
	// the API returned instead.
	if s.akamai.sbsdContext == "" {
		in.Script = s.akamai.sbsdScript
	} else {
		in.Context = s.akamai.sbsdContext
	}
	payload, sbsdCtx, err := s.hyper.GenerateSbsdData(ctx, in)
	if err != nil {
		return fmt.Errorf("hyper sbsd: %w", err)
	}
	s.akamai.sbsdContext = sbsdCtx
	s.akamai.sbsdIndex++

	u := Origin + s.akamai.sbsdPath
	if t != "" {
		u += "?t=" + t
	}
	body, _ := json.Marshal(map[string]string{"body": payload})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	req.Header = s.akamaiPostHeaders("application/json", s.akamai.pageURL)
	status, _, err := s.do("sbsd_post", req)
	if err != nil {
		return fmt.Errorf("sbsd post: %w", err)
	}
	if status >= 300 {
		return fmt.Errorf("sbsd post: status %d", status)
	}
	return nil
}

// postSensor generates sensor data for the current _abck and posts it.
func (s *Session) postSensor(ctx context.Context) error {
	in := &hyper.SensorInput{
		Abck:           s.Cookie("_abck"),
		Bmsz:           s.Cookie("bm_sz"),
		Version:        "3",
		PageUrl:        s.akamai.pageURL,
		UserAgent:      UserAgent,
		ScriptUrl:      s.akamai.sensorURL,
		AcceptLanguage: s.acceptLanguage(),
		IP:             s.IP,
	}
	if s.akamai.sensorContext == "" {
		in.Script = s.akamai.sensorScript
	} else {
		in.Context = s.akamai.sensorContext
	}
	sensor, sensorCtx, err := s.hyper.GenerateSensorData(ctx, in)
	if err != nil {
		return fmt.Errorf("hyper sensor: %w", err)
	}
	s.akamai.sensorContext = sensorCtx

	body, _ := json.Marshal(map[string]string{"sensor_data": sensor})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, s.akamai.sensorURL, bytes.NewReader(body))
	req.Header = s.akamaiPostHeaders("text/plain;charset=UTF-8", s.akamai.pageURL)
	status, resp, err := s.do("sensor_post", req)
	if err != nil {
		return fmt.Errorf("sensor post: %w", err)
	}
	if status != http.StatusCreated || !strings.Contains(string(resp), `"success": true`) {
		return fmt.Errorf("sensor post: status %d body %.80q", status, resp)
	}
	return nil
}

// postPixel answers the pixel challenge: a number in the page, a string in
// the pixel script, and a form post to the script's pixel_ URL.
func (s *Session) postPixel(ctx context.Context, html []byte) error {
	htmlVar, err := akamai.ParsePixelHtmlVar(bytes.NewReader(html))
	if err != nil {
		return fmt.Errorf("pixel: %w", err)
	}
	// The script tag uses a relative path, the parser wants an absolute URL.
	abs := bytes.ReplaceAll(html, []byte(`src="/akam/`), []byte(`src="`+Origin+`/akam/`))
	scriptURL, postURL, err := akamai.ParsePixelScriptURL(bytes.NewReader(abs))
	if err != nil {
		return fmt.Errorf("pixel: %w", err)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, scriptURL, nil)
	req.Header = s.scriptHeaders(s.akamai.pageURL)
	delete(req.Header, "priority")
	order := req.Header[http.HeaderOrderKey]
	req.Header[http.HeaderOrderKey] = order[:len(order)-1]
	status, script, err := s.do("pixel_script", req)
	if err != nil {
		return fmt.Errorf("pixel script: %w", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("pixel script: status %d", status)
	}
	scriptVar, err := akamai.ParsePixelScriptVar(bytes.NewReader(script))
	if err != nil {
		return fmt.Errorf("pixel: %w", err)
	}
	payload, err := s.hyper.GeneratePixelData(ctx, &hyper.PixelInput{
		UserAgent:      UserAgent,
		HTMLVar:        strconv.Itoa(htmlVar),
		ScriptVar:      scriptVar,
		AcceptLanguage: s.acceptLanguage(),
		IP:             s.IP,
	})
	if err != nil {
		return fmt.Errorf("hyper pixel: %w", err)
	}
	req, _ = http.NewRequestWithContext(ctx, http.MethodPost, postURL, strings.NewReader(payload))
	req.Header = s.akamaiPostHeaders("application/x-www-form-urlencoded", s.akamai.pageURL)
	status, _, err = s.do("pixel_post", req)
	if err != nil {
		return fmt.Errorf("pixel post: %w", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("pixel post: status %d", status)
	}
	return nil
}

// sbsdToken extracts t from a 429 SBSD block body: {"t":"183446612"}.
func sbsdToken(body []byte) string {
	var b struct {
		T json.RawMessage `json:"t"`
	}
	if json.Unmarshal(body, &b) != nil || len(b.T) == 0 {
		return ""
	}
	return strings.Trim(string(b.T), `"`)
}

// recover reacts to a challenge on an API call. It returns true when the
// request is worth retrying.
func (s *Session) recover(ctx context.Context, status int, body []byte) (bool, error) {
	switch {
	case status == http.StatusTooManyRequests && s.akamai.sbsdPath != "":
		t := sbsdToken(body)
		if t == "" {
			return false, nil
		}
		return true, s.postSbsd(ctx, t)
	case akamai.IsCookieInvalidated(s.Cookie("_abck")):
		return true, s.postSensor(ctx)
	}
	return false, nil
}
