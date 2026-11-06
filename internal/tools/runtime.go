package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
)

// EvaluateResult holds the result of a JavaScript evaluation.
type EvaluateResult struct {
	Result           webkit.RemoteObject      `json:"result"`
	ExceptionDetails *webkit.ExceptionDetails `json:"exceptionDetails,omitempty"`
	WasThrown        bool                     `json:"wasThrown"`
}

// EvalOptions controls EvaluateScriptWithOptions.
type EvalOptions struct {
	// ReturnByValue returns objects as JSON values instead of remote references.
	ReturnByValue bool
	// AwaitPromise waits for a returned Promise to settle and returns its value.
	AwaitPromise bool
}

// JSException is the error for JavaScript that threw (or a Promise that
// rejected). Message is the thrown value as text, for example
// "TypeError: undefined is not an object"; Stack is the JavaScript stack when
// the thrown value has one.
type JSException struct {
	Message string
	Stack   string
}

func (e *JSException) Error() string {
	if e.Stack == "" {
		return e.Message
	}
	return e.Message + "\n" + e.Stack
}

// promiseClassName identifies a Promise RemoteObject. WebKit has no "promise"
// subtype; a Promise is an object whose className is "Promise".
const promiseClassName = "Promise"

// thisFunction returns the object it is called on; with returnByValue it
// serializes a remote object to JSON.
const thisFunction = "function() { return this; }"

// stackFunction reads the stack of a thrown value.
const stackFunction = "function() { return this && this.stack ? String(this.stack) : \"\"; }"

// EvaluateScript evaluates a JavaScript expression in the page context.
func EvaluateScript(ctx context.Context, client *webkit.Client, expression string, returnByValue bool) (*EvaluateResult, error) {
	return EvaluateScriptWithOptions(ctx, client, expression, EvalOptions{ReturnByValue: returnByValue})
}

// EvaluateScriptWithOptions evaluates a JavaScript expression in the page
// context. A thrown exception is returned as a *JSException together with the
// raw result.
//
// WebKit's Runtime.evaluate has no awaitPromise parameter (iOS 16.7 and
// current WebKit), so AwaitPromise evaluates by reference and then calls
// Runtime.awaitPromise on a returned Promise. A non-Promise object is then
// fetched by value when ReturnByValue is set.
func EvaluateScriptWithOptions(ctx context.Context, client *webkit.Client, expression string, opts EvalOptions) (*EvaluateResult, error) {
	result, err := client.Send(ctx, "Runtime.evaluate", map[string]interface{}{
		"expression":            expression,
		"returnByValue":         opts.ReturnByValue && !opts.AwaitPromise,
		"generatePreview":       true,
		"includeCommandLineAPI": false,
	})
	if err != nil {
		return nil, err
	}

	var evalResult EvaluateResult
	if err := json.Unmarshal(result, &evalResult); err != nil {
		return nil, fmt.Errorf("decoding eval result: %w", err)
	}
	if evalResult.WasThrown {
		return &evalResult, thrownError(ctx, client, &evalResult)
	}
	if !opts.AwaitPromise || evalResult.Result.ObjectID == "" {
		return &evalResult, nil
	}

	if evalResult.Result.ClassName == promiseClassName {
		result, err = client.Send(ctx, "Runtime.awaitPromise", map[string]interface{}{
			"promiseObjectId": evalResult.Result.ObjectID,
			"returnByValue":   opts.ReturnByValue,
			"generatePreview": true,
		})
		if err != nil {
			return nil, fmt.Errorf("awaiting promise: %w", err)
		}
	} else if opts.ReturnByValue {
		result, err = client.Send(ctx, "Runtime.callFunctionOn", map[string]interface{}{
			"objectId":            evalResult.Result.ObjectID,
			"functionDeclaration": thisFunction,
			"returnByValue":       true,
		})
		if err != nil {
			return nil, fmt.Errorf("reading result by value: %w", err)
		}
	} else {
		return &evalResult, nil
	}

	var settled EvaluateResult
	if err := json.Unmarshal(result, &settled); err != nil {
		return nil, fmt.Errorf("decoding awaited result: %w", err)
	}
	if settled.WasThrown {
		return &settled, thrownError(ctx, client, &settled)
	}
	return &settled, nil
}

// thrownError builds a *JSException from a result with wasThrown set. WebKit
// returns the thrown value itself as the result (with description
// String(error)) and no exceptionDetails; the Chrome-style exceptionDetails is
// still honored when present. The stack is read from the thrown object.
func thrownError(ctx context.Context, client *webkit.Client, r *EvaluateResult) *JSException {
	exc := &JSException{Message: exceptionMessage(r)}
	objectID := r.Result.ObjectID
	if r.ExceptionDetails != nil && r.ExceptionDetails.Exception != nil && r.ExceptionDetails.Exception.ObjectID != "" {
		objectID = r.ExceptionDetails.Exception.ObjectID
	}
	if objectID == "" {
		return exc
	}
	raw, err := client.Send(ctx, "Runtime.callFunctionOn", map[string]interface{}{
		"objectId":            objectID,
		"functionDeclaration": stackFunction,
		"returnByValue":       true,
	})
	if err != nil {
		exc.Stack = fmt.Sprintf("(stack unavailable: %v)", err)
		return exc
	}
	var stack EvaluateResult
	if err := json.Unmarshal(raw, &stack); err != nil {
		exc.Stack = fmt.Sprintf("(stack unavailable: decoding: %v)", err)
		return exc
	}
	var text string
	if json.Unmarshal(stack.Result.Value, &text) == nil {
		exc.Stack = text
	}
	return exc
}

// exceptionMessage returns the best available text for a thrown value.
func exceptionMessage(r *EvaluateResult) string {
	if d := r.ExceptionDetails; d != nil && d.Exception != nil && d.Exception.Description != "" {
		return d.Exception.Description
	}
	if r.Result.Description != "" {
		return r.Result.Description
	}
	if len(r.Result.Value) > 0 {
		var text string
		if json.Unmarshal(r.Result.Value, &text) == nil {
			return text
		}
		return string(r.Result.Value)
	}
	if d := r.ExceptionDetails; d != nil && d.Text != "" {
		return d.Text
	}
	return fmt.Sprintf("JavaScript exception without a message (thrown value type %q)", r.Result.Type)
}

// CallFunctionOn calls a function on a remote object.
func CallFunctionOn(ctx context.Context, client *webkit.Client, objectID, functionDeclaration string, args []interface{}, returnByValue bool) (*EvaluateResult, error) {
	params := map[string]interface{}{
		"objectId":            objectID,
		"functionDeclaration": functionDeclaration,
		"returnByValue":       returnByValue,
		"generatePreview":     true,
	}
	if len(args) > 0 {
		callArgs := make([]map[string]interface{}, len(args))
		for i, arg := range args {
			callArgs[i] = map[string]interface{}{"value": arg}
		}
		params["arguments"] = callArgs
	}

	result, err := client.Send(ctx, "Runtime.callFunctionOn", params)
	if err != nil {
		return nil, err
	}

	var evalResult EvaluateResult
	if err := json.Unmarshal(result, &evalResult); err != nil {
		return nil, fmt.Errorf("decoding call result: %w", err)
	}

	if evalResult.WasThrown {
		return &evalResult, thrownError(ctx, client, &evalResult)
	}

	return &evalResult, nil
}

// GetProperties retrieves properties of a remote object.
func GetProperties(ctx context.Context, client *webkit.Client, objectID string, ownProperties bool) ([]webkit.PropertyDescriptor, error) {
	result, err := client.Send(ctx, "Runtime.getProperties", map[string]interface{}{
		"objectId":      objectID,
		"ownProperties": ownProperties,
	})
	if err != nil {
		return nil, err
	}

	var resp struct {
		Properties []webkit.PropertyDescriptor `json:"properties"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		return nil, fmt.Errorf("decoding properties: %w", err)
	}
	return resp.Properties, nil
}
