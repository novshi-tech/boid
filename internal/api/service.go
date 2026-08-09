package api

import ()

type StatusError struct {
	Code    int
	Message string
}

func (e *StatusError) Error() string {
	return e.Message
}

type ProjectReloadResult struct {
	Status string   `json:"status"`
	Errors []string `json:"errors,omitempty"`
}
