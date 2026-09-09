package api

import ()

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

type ProjectReloadResult struct {
	Status string   `json:"status"`
	Errors []string `json:"errors,omitempty"`
}
