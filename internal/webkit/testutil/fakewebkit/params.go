package fakewebkit

import (
	"encoding/json"
	"fmt"
	"slices"
)

// param is one parameter of a WebKit protocol command.
type param struct {
	name     string
	typ      string // the type name of WebKit's BackendDispatcher getter
	optional bool
	enum     []string
}

// commandParams are the parameters of the Network interception commands,
// copied from WebKit's Source/JavaScriptCore/inspector/protocol/Network.json
// (commands interceptContinue to interceptRequestWithError; the same in the
// iOS, WebKitGTK and main branches). RequestId is a string, Headers an
// object, NetworkStage and ResourceErrorType string enums.
var commandParams = map[string][]param{
	"Network.interceptContinue": {
		{name: "requestId", typ: "String"},
		{name: "stage", typ: "String", enum: []string{"request", "response"}},
	},
	"Network.interceptWithRequest": {
		{name: "requestId", typ: "String"},
		{name: "url", typ: "String", optional: true},
		{name: "method", typ: "String", optional: true},
		{name: "headers", typ: "Object", optional: true},
		{name: "postData", typ: "String", optional: true},
	},
	"Network.interceptWithResponse": {
		{name: "requestId", typ: "String"},
		{name: "content", typ: "String"},
		{name: "base64Encoded", typ: "Boolean"},
		{name: "mimeType", typ: "String", optional: true},
		{name: "status", typ: "Integer", optional: true},
		{name: "statusText", typ: "String", optional: true},
		{name: "headers", typ: "Object", optional: true},
	},
	"Network.interceptRequestWithResponse": {
		{name: "requestId", typ: "String"},
		{name: "content", typ: "String"},
		{name: "base64Encoded", typ: "Boolean"},
		{name: "mimeType", typ: "String"},
		{name: "status", typ: "Integer"},
		{name: "statusText", typ: "String"},
		{name: "headers", typ: "Object"},
	},
	"Network.interceptRequestWithError": {
		{name: "requestId", typ: "String"},
		{name: "errorType", typ: "String", enum: []string{"General", "AccessControl", "Cancellation", "Timeout"}},
	},
}

// JSON-RPC error codes that WebKit's BackendDispatcher sends.
const (
	codeInvalidParams = -32602
	codeServerError   = -32000
)

// checkParams validates the parameters of method like WebKit's generated
// backend dispatcher: every required parameter is present and every present
// one has its type (InvalidParams, one entry in data per problem, then the
// summary "Some arguments of method ... can't be processed"), and an enum
// value is known (ServerError "Unknown <name>: <value>"). It returns nil for
// valid parameters and for methods without a definition here.
func checkParams(method string, raw json.RawMessage) *Error {
	defs, ok := commandParams[method]
	if !ok {
		return nil
	}
	var params map[string]any
	_ = json.Unmarshal(raw, &params) // absent, null or not an object: no params object
	var errs []Error
	for _, d := range defs {
		v, present := params[d.name]
		switch {
		case params == nil && !d.optional:
			errs = append(errs, Error{Code: codeInvalidParams, Message: fmt.Sprintf("'params' object must contain required parameter '%s' with type '%s'.", d.name, d.typ)})
		case !present && !d.optional:
			errs = append(errs, Error{Code: codeInvalidParams, Message: fmt.Sprintf("Parameter '%s' with type '%s' was not found.", d.name, d.typ)})
		case present && !hasType[d.typ](v):
			errs = append(errs, Error{Code: codeInvalidParams, Message: fmt.Sprintf("Parameter '%s' has wrong type. It must be '%s'.", d.name, d.typ)})
		}
	}
	if len(errs) > 0 {
		errs = append(errs, Error{Code: codeInvalidParams, Message: fmt.Sprintf("Some arguments of method '%s' can't be processed", method)})
		return pendingErrors(errs)
	}
	for _, d := range defs {
		if s, isString := params[d.name].(string); isString && d.enum != nil && !slices.Contains(d.enum, s) {
			return pendingErrors([]Error{{Code: codeServerError, Message: fmt.Sprintf("Unknown %s: %s", d.name, s)}})
		}
	}
	return nil
}

// pendingErrors builds the reply error of WebKit's sendPendingErrors: the
// code and message of the last error, every error in data.
func pendingErrors(errs []Error) *Error {
	last := errs[len(errs)-1]
	return &Error{Code: last.Code, Message: last.Message, Data: errs}
}

// hasType reports, by type name, whether a decoded JSON value converts with
// the BackendDispatcher getter of that type (JSON::Value asString,
// asBoolean, asInteger, asObject; asInteger takes any number).
var hasType = map[string]func(v any) bool{
	"String":  func(v any) bool { _, ok := v.(string); return ok },
	"Boolean": func(v any) bool { _, ok := v.(bool); return ok },
	"Integer": func(v any) bool { _, ok := v.(float64); return ok },
	"Object":  func(v any) bool { _, ok := v.(map[string]any); return ok },
}
