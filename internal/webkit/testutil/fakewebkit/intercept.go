package fakewebkit

import (
	"encoding/json"
	"slices"
)

// Held requests (WebKit's InspectorNetworkAgent pending intercepts, per
// target). After the first Intercept the fake validates every completion
// command that has no handler: it takes the hold from the target it is
// sent to, at the stage the command completes, and answers "Missing
// pending intercept ..." when that target holds no such request at that
// stage, as WebKit does. A target's holds end with the target (Commit of
// another target, DestroyTarget) and with its Network.disable, which
// continues them. With Options.SuspendOnCommit a commit keeps the replaced
// target's holds in its suspended backend instead (HeldRequests lists
// them); they can be answered again once the target id is announced again.

// Answer is one completion that took a hold.
type Answer struct {
	Target    string
	RequestID string
	Stage     string // "request" or "response"
	Method    string
}

type intercepts struct {
	held    map[string]map[string]string // target -> request id -> stage
	answers []Answer
}

// Intercept makes target hold requestID at stage ("request" or
// "response") and emits Network.requestIntercepted or
// Network.responseIntercepted from target.
func (s *Server) Intercept(target, requestID, stage string) {
	s.InterceptEvent(target, requestID, stage, nil)
}

// InterceptEvent is Intercept with more fields of the event: the request
// of Network.requestIntercepted or the response of
// Network.responseIntercepted (WIP Network.json), which a handler of the
// event matches against its rules.
func (s *Server) InterceptEvent(target, requestID, stage string, fields map[string]any) {
	params := map[string]any{"requestId": requestID}
	for k, v := range fields {
		params[k] = v
	}
	s.mu.Lock()
	if s.intercepts == nil {
		s.intercepts = &intercepts{held: map[string]map[string]string{}}
	}
	if s.intercepts.held[target] == nil {
		s.intercepts.held[target] = map[string]string{}
	}
	s.intercepts.held[target][requestID] = stage
	s.mu.Unlock()
	event := "Network.requestIntercepted"
	if stage == "response" {
		event = "Network.responseIntercepted"
	}
	s.EmitFrom(target, event, params)
}

// HeldRequests returns the ids of the requests that target holds, sorted.
func (s *Server) HeldRequests(target string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	if s.intercepts != nil {
		for id := range s.intercepts.held[target] {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

// Answers returns the completions that took a hold, in order.
func (s *Server) Answers() []Answer {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.intercepts == nil {
		return nil
	}
	return slices.Clone(s.intercepts.answers)
}

// interceptStage returns the stage of the holds that call completes, or ""
// when call is no completion: Network.interceptContinue names it, the
// other completion methods have a fixed stage (WebKit Network.json).
func interceptStage(call Call) string {
	switch call.Method {
	case "Network.interceptContinue":
		var p struct {
			Stage string `json:"stage"`
		}
		_ = json.Unmarshal(call.Params, &p)
		return p.Stage
	case "Network.interceptWithRequest", "Network.interceptRequestWithResponse", "Network.interceptRequestWithError":
		return "request"
	case "Network.interceptWithResponse":
		return "response"
	}
	return ""
}

// takeHold completes the hold that call names on call's target, or returns
// WebKit's error when that target holds no such request at the stage.
func (s *Server) takeHold(call Call) *Error {
	var p struct {
		RequestID string `json:"requestId"`
	}
	_ = json.Unmarshal(call.Params, &p)
	stage := interceptStage(call)
	s.mu.Lock()
	defer s.mu.Unlock()
	held := s.intercepts.held[call.Target]
	if st, ok := held[p.RequestID]; !ok || st != stage {
		return &Error{Code: codeServerError, Message: "Missing pending intercept " + stage + " for given requestId"}
	}
	delete(held, p.RequestID)
	s.intercepts.answers = append(s.intercepts.answers, Answer{Target: call.Target, RequestID: p.RequestID, Stage: stage, Method: call.Method})
	return nil
}

// ContinueHolds ends every hold of target, as Network.disable does without
// a handler: a Network.disable handler that models the backend calls it.
// It returns the ids of the requests whose holds it ended, sorted.
func (s *Server) ContinueHolds(target string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.intercepts == nil {
		return nil
	}
	var ids []string
	for id := range s.intercepts.held[target] {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	delete(s.intercepts.held, target)
	return ids
}

// continueHolds ends every hold of target (Network.disable continues the
// pending intercepts).
func (s *Server) continueHolds(target string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.intercepts.held, target)
}
