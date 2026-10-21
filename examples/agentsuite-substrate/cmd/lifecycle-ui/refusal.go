package main

import "regexp"

// refusal is a FAILED_PRECONDITION snapshot-fidelity refusal, split into what the page shows. The
// message is the daemon's, unchanged; these fields are read from it in the forms the sessions
// service and the substrate runtime write (session/service.go checkpointRefusal,
// runtime/substrate requireCheckpoint):
//
//	suspend: session "<uid>": runtime: checkpoint does not meet the required contract:
//	actor <atespace>/<name>: required MEMORY with a durable URI, observed VOLUMES (URI present: true);
//	actor remains suspended: ...
//
// A FAILED_PRECONDITION that names none of them (another refusal) yields no refusal, and the page
// shows the message alone.
type refusal struct {
	Op       string `json:"op,omitempty"`
	Session  string `json:"session,omitempty"`
	Actor    string `json:"actor,omitempty"`
	Required string `json:"required,omitempty"`
	Observed string `json:"observed,omitempty"`
	// Suspended is set when the message says the actor was left suspended.
	Suspended bool `json:"suspended"`
	// MemoryLost is set when the message says the session's in-RAM state cannot be restored.
	MemoryLost bool `json:"memoryLost"`
}

var (
	refusalOp       = regexp.MustCompile(`^(suspend|resume|exec|fork): session "`)
	refusalSession  = regexp.MustCompile(`session "([A-Za-z0-9._-]+)"`)
	refusalActor    = regexp.MustCompile(`actor ([A-Za-z0-9._-]+/[A-Za-z0-9._-]+):`)
	refusalRequired = regexp.MustCompile(`required ([A-Z]+)\b`)
	refusalObserved = regexp.MustCompile(`observed ([A-Z]+)\b`)
	refusalLeft     = regexp.MustCompile(`actor remains suspended`)
	refusalLost     = regexp.MustCompile(`in-memory harness state is lost`)
)

func parseRefusal(msg string) *refusal {
	r := &refusal{
		Session:    submatch(refusalSession, msg),
		Actor:      submatch(refusalActor, msg),
		Required:   submatch(refusalRequired, msg),
		Observed:   submatch(refusalObserved, msg),
		Suspended:  refusalLeft.MatchString(msg),
		MemoryLost: refusalLost.MatchString(msg),
	}
	if m := refusalOp.FindStringSubmatch(msg); m != nil {
		r.Op = m[1]
	}
	if r.Actor == "" && r.Required == "" && r.Observed == "" {
		return nil
	}
	return r
}

func submatch(re *regexp.Regexp, s string) string {
	if m := re.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return ""
}
