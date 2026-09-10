package server

import (
	"sort"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/sandbox"
)

// A card continuation can read the source Card's attachments as well as files
// supplied in answers to its own asks. Own-task files take precedence. The
// additional scope comes only from the broker token, never a caller's ID.
func (e *boidBuiltinExecutor) listTaskAttachments(ctx sandbox.TokenContext, taskID string) ([]string, error) {
	names, err := api.ListAttachments(e.attachmentsRoot, taskID)
	if err != nil || ctx.TaskID != taskID || ctx.CardID == "" || ctx.CardRequestID == "" {
		return names, err
	}
	cardNames, err := api.ListAttachments(e.attachmentsRoot, ctx.CardID)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(names))
	for _, n := range names {
		seen[n] = true
	}
	for _, n := range cardNames {
		if !seen[n] {
			names = append(names, n)
			seen[n] = true
		}
	}
	sort.Strings(names)
	return names, nil
}

func (e *boidBuiltinExecutor) readTaskAttachment(ctx sandbox.TokenContext, taskID, name string) ([]byte, error) {
	if ctx.TaskID == taskID && ctx.CardID != "" && ctx.CardRequestID != "" {
		names, err := api.ListAttachments(e.attachmentsRoot, taskID)
		if err != nil {
			return nil, err
		}
		own := false
		for _, n := range names {
			if n == name {
				own = true
				break
			}
		}
		if !own {
			return api.ReadAttachment(e.attachmentsRoot, ctx.CardID, name)
		}
	}
	return api.ReadAttachment(e.attachmentsRoot, taskID, name)
}
