package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/nnemirovsky/agent-ios/internal/webkit"
)

// GetDocument returns the root DOM node. A reply without a root (a page
// that is still loading, or a reply that lost its body) is an error, so a
// caller never gets a nil root.
func GetDocument(ctx context.Context, client webkit.Caller, depth int) (*webkit.Node, error) {
	params := map[string]interface{}{}
	if depth > 0 {
		params["depth"] = depth
	}

	result, err := client.Send(ctx, "DOM.getDocument", params)
	if err != nil {
		return nil, err
	}

	var resp struct {
		Root *webkit.Node `json:"root"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		return nil, fmt.Errorf("decoding document: %w", err)
	}
	if resp.Root == nil {
		return nil, fmt.Errorf("DOM.getDocument returned no document root (reply %s); the page may still be loading: wait for it to load, then try again", result)
	}
	return resp.Root, nil
}

// QuerySelector finds the first element matching a CSS selector.
func QuerySelector(ctx context.Context, client webkit.Caller, nodeID int, selector string) (int, error) {
	result, err := client.Send(ctx, "DOM.querySelector", map[string]interface{}{
		"nodeId":   nodeID,
		"selector": selector,
	})
	if err != nil {
		return 0, err
	}

	var resp struct {
		NodeID int `json:"nodeId"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		return 0, fmt.Errorf("decoding querySelector: %w", err)
	}
	if resp.NodeID == 0 {
		return 0, fmt.Errorf("no element matches selector %q", selector)
	}
	return resp.NodeID, nil
}

// EventListenerInfo describes an event listener on a node.
type EventListenerInfo struct {
	Type        string `json:"type"`
	UseCapture  bool   `json:"useCapture"`
	IsAttribute bool   `json:"isAttribute"`
	NodeID      int    `json:"nodeId,omitempty"`
	HandlerBody string `json:"handlerBody,omitempty"`
}
