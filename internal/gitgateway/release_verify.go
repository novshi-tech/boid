package gitgateway

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// VerifyRelease checks a remote branch tip through the gateway's normal authorization and credentials.
func (s *Server) VerifyRelease(ctx context.Context, repo RepoKey, namespace, branch, commit string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	token := s.registry.Register(map[RepoKey]Permission{repo: PermFetch}, namespace)
	defer s.registry.Unregister(token)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://gitgateway"+PathPrefix+token+"/"+string(repo)+".git/info/refs?service=git-upload-pack", nil)
	if err != nil {
		return false, err
	}
	response := &releaseResponse{header: make(http.Header)}
	s.ServeHTTP(response, req)
	if response.code != http.StatusOK || response.overflow {
		return false, fmt.Errorf("git gateway ref advertisement unavailable (status %d)", response.code)
	}
	reader := bytes.NewReader(response.body.Bytes())
	validRefs := false
	matches := false
	for reader.Len() > 0 {
		var header [4]byte
		if _, err := io.ReadFull(reader, header[:]); err != nil {
			return false, fmt.Errorf("truncated ref advertisement")
		}
		size, err := strconv.ParseUint(string(header[:]), 16, 16)
		if err != nil || size > 0 && size < 4 {
			return false, fmt.Errorf("invalid ref advertisement packet")
		}
		if size == 0 {
			continue
		}
		packet := make([]byte, int(size)-4)
		if _, err := io.ReadFull(reader, packet); err != nil {
			return false, fmt.Errorf("truncated ref advertisement packet")
		}
		line, _, _ := strings.Cut(string(packet), "\x00")
		fields := strings.Fields(line)
		if len(fields) != 2 || len(fields[0]) != 40 && len(fields[0]) != 64 {
			continue
		}
		if _, err := hex.DecodeString(fields[0]); err != nil {
			return false, err
		}
		validRefs = true
		if fields[1] == "refs/heads/"+strings.TrimPrefix(branch, "refs/heads/") {
			matches = strings.HasPrefix(fields[0], commit)
		}
	}
	if !validRefs {
		return false, fmt.Errorf("no refs advertised")
	}
	return matches, nil
}

type releaseResponse struct {
	header   http.Header
	code     int
	body     bytes.Buffer
	overflow bool
}

func (r *releaseResponse) Header() http.Header { return r.header }
func (r *releaseResponse) WriteHeader(code int) {
	if r.code == 0 {
		r.code = code
	}
}
func (r *releaseResponse) Write(data []byte) (int, error) {
	if r.code == 0 {
		r.code = http.StatusOK
	}
	if r.body.Len()+len(data) > 8*1024*1024 {
		r.overflow = true
		return len(data), nil
	}
	return r.body.Write(data)
}
