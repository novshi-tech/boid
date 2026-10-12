package api

type StatusError struct {
	Code            int
	Message         string
	OperationReason string
	TargetRequestID string
	TargetKind      string
	TargetID        string
	TargetTaskID    string
}

func (e *StatusError) Error() string {
	return e.Message
}
