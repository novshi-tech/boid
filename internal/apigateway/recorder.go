package apigateway

// RequestRecorder is called once per request the gateway authorizes far
// enough to know which service/path it targets — i.e. every request past a
// well-formed-route 404, whether it ultimately succeeds, is rejected
// (401/403/502/503), or fails upstream. docs/plans/api-gateway.md §論点3
// ("確定: method + service + path + status を timeline に。body は記録しない")
// — deliberately narrow: no headers, no query string, no request/response
// body, ever.
//
// RecordedRequest is the payload passed to a RequestRecorder on each call
// (docs/plans/refactoring-backlog.md N11): TaskID, Method, Service, and Path
// were four adjacent same-typed (string) positional parameters, easy to
// transpose without the compiler noticing.
//
// TaskID is the originating task (Registry Entry.TaskID); empty for a
// taskless job, in which case a real recorder is expected to skip recording
// rather than attribute the request to no task (there is nothing in a task
// timeline to attach it to).
type RecordedRequest struct {
	TaskID  string
	Method  string
	Service string
	Path    string
	Status  int
}

type RequestRecorder func(RecordedRequest)

// noopRecorder discards every call. Used as NewServer's default when
// recorder is nil, so Server never needs a nil check at each call site.
func noopRecorder(RecordedRequest) {}
