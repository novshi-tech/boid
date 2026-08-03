package apigateway

// RequestRecorder is called once per request the gateway authorizes far
// enough to know which service/path it targets — i.e. every request past a
// well-formed-route 404, whether it ultimately succeeds, is rejected
// (401/403/502/503), or fails upstream. docs/plans/api-gateway.md §論点3
// ("確定: method + service + path + status を timeline に。body は記録しない")
// — deliberately narrow: no headers, no query string, no request/response
// body, ever.
//
// taskID is the originating task (Registry Entry.TaskID); empty for a
// taskless job, in which case a real recorder is expected to skip recording
// rather than attribute the request to no task (there is nothing in a task
// timeline to attach it to).
type RequestRecorder func(taskID, method, service, path string, status int)

// noopRecorder discards every call. Used as NewServer's default when
// recorder is nil, so Server never needs a nil check at each call site.
func noopRecorder(string, string, string, string, int) {}
