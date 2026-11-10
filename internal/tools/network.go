package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
)

// httpStatusText returns the standard status text for an HTTP status code.
func httpStatusText(code int) string {
	text := http.StatusText(code)
	if text == "" {
		return "Unknown"
	}
	return text
}

// mimeTypeFromHeaders extracts the MIME type from a Content-Type header, defaulting to text/plain.
func mimeTypeFromHeaders(headers map[string]string) string {
	for k, v := range headers {
		if strings.EqualFold(k, "content-type") {
			// Strip charset etc: "text/html; charset=utf-8" → "text/html"
			if idx := strings.IndexByte(v, ';'); idx >= 0 {
				return strings.TrimSpace(v[:idx])
			}
			return v
		}
	}
	return "text/plain"
}

// GetResponseBody retrieves the body of a network response by request ID.
func GetResponseBody(ctx context.Context, client webkit.Caller, requestID string) (string, bool, error) {
	result, err := client.Send(ctx, "Network.getResponseBody", map[string]string{
		"requestId": requestID,
	})
	if err != nil {
		return "", false, err
	}

	var resp struct {
		Body          string `json:"body"`
		Base64Encoded bool   `json:"base64Encoded"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		return "", false, fmt.Errorf("decoding response body: %w", err)
	}
	return resp.Body, resp.Base64Encoded, nil
}

// SetExtraHeaders sets extra HTTP headers to be sent with every request.
func SetExtraHeaders(ctx context.Context, client webkit.Caller, headers map[string]string) error {
	_, err := client.Send(ctx, "Network.setExtraHTTPHeaders", map[string]interface{}{
		"headers": headers,
	})
	return err
}

// SetRequestInterception enables or disables request interception.
func SetRequestInterception(ctx context.Context, client webkit.Caller, enabled bool) error {
	_, err := client.Send(ctx, "Network.setInterceptionEnabled", map[string]interface{}{
		"enabled": enabled,
	})
	return err
}

// InterceptContinue continues an intercepted request without modification.
func InterceptContinue(ctx context.Context, client webkit.Caller, requestID, stage string) error {
	if stage == "" {
		stage = "request"
	}
	_, err := client.Send(ctx, "Network.interceptContinue", map[string]string{
		"requestId": requestID,
		"stage":     stage,
	})
	return err
}

// InterceptWithResponse provides a custom response for an intercepted request.
// For request-stage interceptions, uses Network.interceptRequestWithResponse (synthetic response).
// For response-stage interceptions, uses Network.interceptWithResponse (modify received response).
func InterceptWithResponse(ctx context.Context, client webkit.Caller, requestID string, stage string, statusCode int, headers map[string]string, content string, base64Encoded bool) error {
	if stage == "" {
		stage = "request"
	}
	if headers == nil {
		headers = map[string]string{}
	}

	if stage == "request" {
		// interceptRequestWithResponse: synthetic response at request stage (skip network).
		_, err := client.Send(ctx, "Network.interceptRequestWithResponse", map[string]interface{}{
			"requestId":     requestID,
			"status":        statusCode,
			"statusText":    httpStatusText(statusCode),
			"mimeType":      mimeTypeFromHeaders(headers),
			"content":       content,
			"base64Encoded": base64Encoded,
			"headers":       headers,
		})
		return err
	}

	// interceptWithResponse: modify response at response stage.
	_, err := client.Send(ctx, "Network.interceptWithResponse", map[string]interface{}{
		"requestId":     requestID,
		"stage":         stage,
		"status":        statusCode,
		"headers":       headers,
		"content":       content,
		"base64Encoded": base64Encoded,
	})
	return err
}

// SetEmulatedConditions configures network emulation with a bandwidth constraint.
func SetEmulatedConditions(ctx context.Context, client webkit.Caller, bytesPerSecondLimit int) error {
	_, err := client.Send(ctx, "Network.setEmulatedConditions", map[string]interface{}{
		"bytesPerSecondLimit": bytesPerSecondLimit,
	})
	return err
}

// SetResourceCachingDisabled enables or disables the resource cache.
func SetResourceCachingDisabled(ctx context.Context, client webkit.Caller, disabled bool) error {
	_, err := client.Send(ctx, "Network.setResourceCachingDisabled", map[string]interface{}{
		"disabled": disabled,
	})
	return err
}
