package devtools

import "encoding/json"

// cdpResourceTypes maps WIP resource types (Network.json, Page.ResourceType)
// to CDP Network.ResourceType names. WIP has "StyleSheet" and "Beacon";
// CDP spells the first "Stylesheet" and reports beacons as "Ping".
var cdpResourceTypes = map[string]string{
	"Document": "Document", "StyleSheet": "Stylesheet", "Image": "Image", "Font": "Font",
	"Script": "Script", "XHR": "XHR", "Fetch": "Fetch", "Ping": "Ping", "Beacon": "Ping",
	"WebSocket": "WebSocket", "Other": "Other",
}

// CDPResourceType is the CDP Network.ResourceType of the WIP resource
// type t ("Other" for an unknown one).
func CDPResourceType(t string) string { return cdpType(t) }

func cdpType(t string) string {
	if c, ok := cdpResourceTypes[t]; ok {
		return c
	}
	return "Other"
}

type cdpRequest struct {
	URL            string            `json:"url"`
	Method         string            `json:"method"`
	Headers        map[string]string `json:"headers"`
	PostData       string            `json:"postData,omitempty"`
	HasPostData    bool              `json:"hasPostData,omitempty"`
	InitialPrio    string            `json:"initialPriority"`
	ReferrerPolicy string            `json:"referrerPolicy"`
}

type cdpResponse struct {
	URL               string            `json:"url"`
	Status            int               `json:"status"`
	StatusText        string            `json:"statusText"`
	Headers           map[string]string `json:"headers"`
	MimeType          string            `json:"mimeType"`
	ConnectionReused  bool              `json:"connectionReused"`
	ConnectionID      int               `json:"connectionId"`
	EncodedDataLength int64             `json:"encodedDataLength"`
	FromDiskCache     bool              `json:"fromDiskCache,omitempty"`
	SecurityState     string            `json:"securityState"`
}

func toCDPResponse(r *wipResponse) *cdpResponse {
	if r == nil {
		return nil
	}
	h := r.Headers
	if h == nil {
		h = map[string]string{}
	}
	return &cdpResponse{
		URL: r.URL, Status: r.Status, StatusText: r.StatusText, Headers: h, MimeType: r.MimeType,
		FromDiskCache: r.Source == "disk-cache", SecurityState: "unknown",
	}
}

// ToCDP converts one WIP Network event into the CDP event of the same
// meaning (Chrome DevTools Protocol, Network domain): walltime becomes
// wallTime, resource types use CDP names, responses gain the fields CDP
// marks required, and loadingFinished carries encodedDataLength. Events
// without a CDP counterpart that clients can use (memory-cache hits, which
// CDP reports only after a requestWillBeSent) give ok false. The requestId
// stays WebKit's; Netlog.SubscribeCDP replaces it with the log id.
func ToCDP(method string, params json.RawMessage) (string, json.RawMessage, bool) {
	return toCDP(method, params, "")
}

// toCDP is ToCDP with requestId id ("" keeps WebKit's request id).
func toCDP(method string, params json.RawMessage, id string) (string, json.RawMessage, bool) {
	var e wipEvent
	if json.Unmarshal(params, &e) != nil || e.RequestID == "" {
		return "", nil, false
	}
	if id != "" {
		e.RequestID = id
	}
	var out any
	switch method {
	case "Network.requestWillBeSent":
		if e.Request == nil {
			return "", nil, false
		}
		h := e.Request.Headers
		if h == nil {
			h = map[string]string{}
		}
		out = map[string]any{
			"requestId": e.RequestID, "loaderId": e.LoaderID, "documentURL": documentURL(params),
			"request": cdpRequest{
				URL: e.Request.URL, Method: e.Request.Method, Headers: h, PostData: e.Request.PostData,
				HasPostData: e.Request.PostData != "", InitialPrio: "Medium", ReferrerPolicy: "strict-origin-when-cross-origin",
			},
			"timestamp": e.Timestamp, "wallTime": e.Walltime, "initiator": initiator(params),
			"redirectHasExtraInfo": false, "type": cdpType(e.Type), "frameId": e.FrameID,
			"redirectResponse": toCDPResponse(e.RedirectResponse),
		}
		if e.RedirectResponse == nil {
			delete(out.(map[string]any), "redirectResponse")
		}
	case "Network.responseReceived":
		if e.Response == nil {
			return "", nil, false
		}
		out = map[string]any{
			"requestId": e.RequestID, "loaderId": e.LoaderID, "timestamp": e.Timestamp,
			"type": cdpType(e.Type), "response": toCDPResponse(e.Response), "hasExtraInfo": false, "frameId": e.FrameID,
		}
	case "Network.dataReceived":
		out = map[string]any{"requestId": e.RequestID, "timestamp": e.Timestamp, "dataLength": e.DataLength, "encodedDataLength": e.EncodedLength}
	case "Network.loadingFinished":
		var enc int64
		if e.Metrics != nil {
			enc = e.Metrics.ResponseBodyBytesReceived
		}
		out = map[string]any{"requestId": e.RequestID, "timestamp": e.Timestamp, "encodedDataLength": enc}
	case "Network.loadingFailed":
		out = map[string]any{"requestId": e.RequestID, "timestamp": e.Timestamp, "type": "Other", "errorText": e.ErrorText, "canceled": e.Canceled}
	default:
		return "", nil, false
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return "", nil, false
	}
	return method, raw, true
}

func documentURL(params json.RawMessage) string {
	var v struct {
		DocumentURL string `json:"documentURL"`
	}
	_ = json.Unmarshal(params, &v)
	return v.DocumentURL
}

// initiator keeps WIP's initiator (type, url, lineNumber): CDP's Initiator
// has the same fields; a missing one becomes {"type":"other"}.
func initiator(params json.RawMessage) json.RawMessage {
	var v struct {
		Initiator json.RawMessage `json:"initiator"`
	}
	if json.Unmarshal(params, &v) == nil && len(v.Initiator) > 0 && string(v.Initiator) != "null" {
		return v.Initiator
	}
	return json.RawMessage(`{"type":"other"}`)
}
