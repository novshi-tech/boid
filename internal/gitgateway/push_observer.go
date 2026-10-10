package gitgateway

import (
	"bytes"
	"fmt"
	"io"
	"strconv"
	"strings"
)

type replayBody struct {
	io.Reader
	io.Closer
}

// observeReceivePack observes requested refs while leaving the packfile streaming.
func observeReceivePack(body io.ReadCloser, observe func(string)) (io.ReadCloser, error) {
	var prefix bytes.Buffer
	var refs []string
	for prefix.Len() < 1024*1024 {
		var header [4]byte
		if _, err := io.ReadFull(body, header[:]); err != nil {
			return nil, err
		}
		prefix.Write(header[:])
		n, err := strconv.ParseUint(string(header[:]), 16, 16)
		if err != nil {
			return nil, fmt.Errorf("invalid receive-pack packet length")
		}
		if n == 0 {
			for _, ref := range refs {
				observe(ref)
			}
			return replayBody{Reader: io.MultiReader(bytes.NewReader(prefix.Bytes()), body), Closer: body}, nil
		}
		if n < 4 {
			return nil, fmt.Errorf("invalid receive-pack packet length")
		}
		packet := make([]byte, int(n)-4)
		if _, err := io.ReadFull(body, packet); err != nil {
			return nil, err
		}
		prefix.Write(packet)
		line, _, _ := strings.Cut(string(packet), "\x00")
		fields := strings.Fields(line)
		if len(fields) == 3 && strings.HasPrefix(fields[2], "refs/") {
			refs = append(refs, fields[2])
		}
	}
	return nil, fmt.Errorf("receive-pack command list exceeds 1 MiB")
}

// ObservePush attaches a job-scoped observer without changing repository permissions.
func (r *Registry) ObservePush(token string, observe func(RepoKey, string)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if entry := r.entries[token]; entry != nil {
		entry.ObservePush = observe
	}
}
