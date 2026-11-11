package devtools

import (
	"context"
	"encoding/base64"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nnemirovsky/iwdp-cli/internal/webkit"
)

// HAR 1.2 (http://www.softwareishard.com/blog/har-12-spec/). Every
// required field of the spec is always present; optional ones are omitted
// when unknown. Sizes that WebKit does not report are -1, as the spec
// allows.

type HAR struct {
	Log HARLog `json:"log"`
}

type HARLog struct {
	Version string     `json:"version"`
	Creator HARCreator `json:"creator"`
	Pages   []any      `json:"pages"`
	Entries []HAREntry `json:"entries"`
	Comment string     `json:"comment,omitempty"`
}

type HARCreator struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type HARNameValue struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type HARCookie struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type HARPostData struct {
	MimeType string         `json:"mimeType"`
	Text     string         `json:"text"`
	Params   []HARNameValue `json:"params,omitempty"`
}

type HARRequest struct {
	Method      string         `json:"method"`
	URL         string         `json:"url"`
	HTTPVersion string         `json:"httpVersion"`
	Cookies     []HARCookie    `json:"cookies"`
	Headers     []HARNameValue `json:"headers"`
	QueryString []HARNameValue `json:"queryString"`
	PostData    *HARPostData   `json:"postData,omitempty"`
	HeadersSize int            `json:"headersSize"`
	BodySize    int            `json:"bodySize"`
}

type HARContent struct {
	Size     int64  `json:"size"`
	MimeType string `json:"mimeType"`
	Text     string `json:"text,omitempty"`
	Encoding string `json:"encoding,omitempty"`
	Comment  string `json:"comment,omitempty"`
}

type HARResponse struct {
	Status      int            `json:"status"`
	StatusText  string         `json:"statusText"`
	HTTPVersion string         `json:"httpVersion"`
	Cookies     []HARCookie    `json:"cookies"`
	Headers     []HARNameValue `json:"headers"`
	Content     HARContent     `json:"content"`
	RedirectURL string         `json:"redirectURL"`
	HeadersSize int            `json:"headersSize"`
	BodySize    int64          `json:"bodySize"`
}

type HARTimings struct {
	Blocked float64 `json:"blocked"`
	DNS     float64 `json:"dns"`
	Connect float64 `json:"connect"`
	Send    float64 `json:"send"`
	Wait    float64 `json:"wait"`
	Receive float64 `json:"receive"`
	SSL     float64 `json:"ssl"`
}

type HAREntry struct {
	StartedDateTime string      `json:"startedDateTime"`
	Time            float64     `json:"time"`
	Request         HARRequest  `json:"request"`
	Response        HARResponse `json:"response"`
	Cache           struct{}    `json:"cache"`
	Timings         HARTimings  `json:"timings"`
	Comment         string      `json:"comment,omitempty"`
	// RequestID is the request's log id (Request.ID), a custom field (HAR
	// 1.2 allows fields that start with "_").
	RequestID string `json:"_requestId,omitempty"`
}

// HARBody is a response body for a HAR entry.
type HARBody struct {
	Text   string
	Base64 bool
	Err    string // why there is no body
}

// HAROptions configure BuildHAR.
type HAROptions struct {
	Creator    HARCreator
	ShowValues bool // keep secret header, cookie, query and field values
}

// BuildHAR converts log requests into a HAR 1.2 log. Secret values are
// masked unless ShowValues; bodies holds response bodies by log id
// (Request.ID; content "all").
func BuildHAR(reqs []Request, gaps []Gap, bodies map[string]HARBody, o HAROptions) HAR {
	h := HAR{Log: HARLog{Version: "1.2", Creator: o.Creator, Pages: []any{}, Entries: []HAREntry{}}}
	if h.Log.Creator.Name == "" {
		h.Log.Creator = HARCreator{Name: "iwdp-cli", Version: "dev"}
	}
	if !o.ShowValues {
		h.Log.Comment = "secret header, cookie, query and field values are masked as " + Masked
	}
	for _, g := range gaps {
		h.Log.Comment = strings.TrimPrefix(h.Log.Comment+fmt.Sprintf("; network reset from %s to %s: requests in flight were closed as capture_reset and earlier bodies are gone",
			g.Start.UTC().Format(time.RFC3339Nano), g.End.UTC().Format(time.RFC3339Nano)), "; ")
	}
	for _, r := range reqs {
		h.Log.Entries = append(h.Log.Entries, harEntry(r, bodies, o.ShowValues))
	}
	return h
}

func ms(sec float64) float64 {
	if sec <= 0 {
		return 0
	}
	return math.Round(sec*1e6) / 1e3 // microsecond precision
}

func harEntry(r Request, bodies map[string]HARBody, show bool) HAREntry {
	maskH := func(h map[string]string) map[string]string {
		if show {
			return h
		}
		return MaskHeaders(h)
	}
	u := r.URL
	if !show {
		u = MaskURL(u)
	}
	e := HAREntry{StartedDateTime: r.WallTime.UTC().Format(time.RFC3339Nano), RequestID: r.ID}
	e.Request = HARRequest{
		Method: r.Method, URL: u, HTTPVersion: "HTTP/1.1", Headers: nameValues(maskH(r.RequestHeaders)),
		Cookies: harCookies(headerValue(r.RequestHeaders, "Cookie"), show), QueryString: queryString(u),
		HeadersSize: -1, BodySize: len(r.PostData),
	}
	if r.Method == "" {
		e.Request.Method = "GET"
	}
	if r.PostData != "" {
		mt := headerValue(r.RequestHeaders, "Content-Type")
		text := r.PostData
		if !show {
			text = MaskBody(mt, text)
		}
		e.Request.PostData = &HARPostData{MimeType: mt, Text: text}
	}
	setCookie := headerValue(r.ResponseHeaders, "Set-Cookie")
	e.Response = HARResponse{
		Status: r.Status, StatusText: r.StatusText, HTTPVersion: "HTTP/1.1",
		Headers: nameValues(maskH(r.ResponseHeaders)), Cookies: harSetCookies(setCookie, show),
		RedirectURL: headerValue(maskH(r.ResponseHeaders), "Location"), HeadersSize: -1, BodySize: -1,
		Content: HARContent{Size: r.DataLength, MimeType: r.MimeType},
	}
	if r.EncodedSize > 0 {
		e.Response.BodySize = r.EncodedSize
	}
	if e.Response.Content.MimeType == "" {
		e.Response.Content.MimeType = "x-unknown"
	}
	if b, ok := bodies[r.ID]; ok {
		switch {
		case b.Err != "" && show:
			e.Response.Content.Comment = b.Err
		case b.Err != "":
			e.Response.Content.Comment = MaskText(b.Err)
		case b.Base64 && show:
			e.Response.Content.Text, e.Response.Content.Encoding = b.Text, "base64"
		case b.Base64:
			text, comment := maskBase64(r.MimeType, b.Text)
			e.Response.Content.Text, e.Response.Content.Comment = text, comment
			if text != "" {
				e.Response.Content.Encoding = "base64"
			}
		case show:
			e.Response.Content.Text = b.Text
		default:
			e.Response.Content.Text = MaskBody(r.MimeType, b.Text)
		}
	}
	// Timings: send 0, wait until the response, receive until the end; the
	// entry time is their sum (the spec requires it). Unknown parts are 0.
	wait, receive := 0.0, 0.0
	if r.ResponseTS > 0 && r.StartTS > 0 && r.ResponseTS >= r.StartTS {
		wait = ms(r.ResponseTS - r.StartTS)
		if r.EndTS >= r.ResponseTS {
			receive = ms(r.EndTS - r.ResponseTS)
		}
	} else if r.EndTS > 0 && r.StartTS > 0 && r.EndTS >= r.StartTS {
		wait = ms(r.EndTS - r.StartTS)
	}
	e.Timings = HARTimings{Blocked: -1, DNS: -1, Connect: -1, SSL: -1, Send: 0, Wait: wait, Receive: receive}
	e.Time = wait + receive
	if r.State != StateFinished {
		e.Comment = r.State
		if r.Error != "" && show {
			e.Comment += ": " + r.Error
		} else if r.Error != "" {
			e.Comment += ": " + MaskText(r.Error)
		}
	}
	return e
}

// maskBase64 masks a base64 response body as MaskBody masks text: a body
// that decodes to UTF-8 text is masked by mimeType and encoded again;
// binary data is kept as it is. A body that is not valid base64 is left
// out (text ""), with the reason as comment, since it cannot be masked.
func maskBase64(mimeType, text string) (out, comment string) {
	data, err := base64.StdEncoding.DecodeString(text)
	if err != nil {
		return "", "the body is left out: WebKit sent it as base64, but it does not decode, so it cannot be masked"
	}
	if !utf8.Valid(data) {
		return text, ""
	}
	masked := MaskBody(mimeType, string(data))
	if masked == string(data) {
		return text, ""
	}
	return base64.StdEncoding.EncodeToString([]byte(masked)), ""
}

func headerValue(h map[string]string, name string) string {
	for k, v := range h {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

func nameValues(h map[string]string) []HARNameValue {
	out := []HARNameValue{}
	for _, k := range sortedHeaderNames(h) {
		out = append(out, HARNameValue{Name: k, Value: h[k]})
	}
	return out
}

func queryString(raw string) []HARNameValue {
	out := []HARNameValue{}
	u, err := url.Parse(raw)
	if err != nil {
		return out
	}
	for _, kv := range strings.Split(u.RawQuery, "&") {
		if kv == "" {
			continue
		}
		k, v, _ := strings.Cut(kv, "=")
		ku, err1 := url.QueryUnescape(k)
		vu, err2 := url.QueryUnescape(v)
		if err1 != nil || err2 != nil {
			ku, vu = k, v
		}
		out = append(out, HARNameValue{Name: ku, Value: vu})
	}
	return out
}

func harCookies(header string, show bool) []HARCookie {
	out := []HARCookie{}
	if header == "" {
		return out
	}
	for _, c := range (&http.Request{Header: http.Header{"Cookie": {header}}}).Cookies() {
		v := c.Value
		if !show {
			v = Masked
		}
		out = append(out, HARCookie{Name: c.Name, Value: v})
	}
	return out
}

func harSetCookies(header string, show bool) []HARCookie {
	out := []HARCookie{}
	if header == "" {
		return out
	}
	// WebKit joins several Set-Cookie headers with newlines.
	for _, line := range strings.Split(header, "\n") {
		if c, err := http.ParseSetCookie(strings.TrimSpace(line)); err == nil {
			v := c.Value
			if !show {
				v = Masked
			}
			out = append(out, HARCookie{Name: c.Name, Value: v})
		}
	}
	return out
}

// StartHAR starts a HAR recording on the log: "network har stop" writes the
// requests seen from now on. content is "all" (response bodies) or "none".
func (n *Netlog) StartHAR(content string) error {
	if content != "all" && content != "none" {
		return fmt.Errorf("har content %q: want all or none", content)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.harStart, n.harContent = n.seq, content
	return nil
}

// StopHAR ends the HAR recording and returns its HAR. With content "all"
// it reads each finished response body (Body) inside ctx; a body that
// WebKit no longer has is noted in the entry's content comment.
func (n *Netlog) StopHAR(ctx context.Context, c webkit.Caller, o HAROptions) (HAR, error) {
	n.mu.Lock()
	start, content := n.harStart, n.harContent
	n.harStart = -1
	var reqs []Request
	for _, r := range n.reqs {
		if r.Seq > start {
			reqs = append(reqs, *r)
		}
	}
	var gaps []Gap
	for _, g := range n.gaps {
		if g.AfterSeq >= start {
			gaps = append(gaps, g)
		}
	}
	n.mu.Unlock()
	if start < 0 {
		return HAR{}, fmt.Errorf("no HAR recording runs; start one with 'network har start'")
	}
	bodies := map[string]HARBody{}
	if content == "all" {
		for _, r := range reqs {
			if r.State != StateFinished {
				continue
			}
			data, b64, err := n.Body(ctx, c, r.ID)
			if err != nil {
				bodies[r.ID] = HARBody{Err: err.Error()}
				continue
			}
			bodies[r.ID] = HARBody{Text: string(data), Base64: b64}
		}
	}
	return BuildHAR(reqs, gaps, bodies, o), nil
}
