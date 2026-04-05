package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"github.com/novshi-tech/boid/internal/api"
)

type Client struct {
	socketPath string
	httpClient *http.Client
}

var ErrAttachDetached = errors.New("attach detached")

func NewUnixClient(socketPath string) *Client {
	return &Client{
		socketPath: socketPath,
		httpClient: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return net.Dial("unix", socketPath)
				},
			},
		},
	}
}

func DefaultSocketPath() string {
	if s := os.Getenv("BOID_SOCKET"); s != "" {
		return s
	}
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "boid.sock")
	}
	uid := strconv.Itoa(os.Getuid())
	runDir := filepath.Join("/run/user", uid)
	if _, err := os.Stat(runDir); err == nil {
		return filepath.Join(runDir, "boid.sock")
	}
	return fmt.Sprintf("/tmp/boid-%s.sock", uid)
}

func (c *Client) Do(method, path string, body any, result any) error {
	var reqBody *bytes.Buffer
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal body: %w", err)
		}
		reqBody = bytes.NewBuffer(data)
	} else {
		reqBody = &bytes.Buffer{}
	}

	req, err := http.NewRequest(method, "http://boid"+path, reqBody)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		var errResp map[string]string
		json.NewDecoder(resp.Body).Decode(&errResp)
		if msg, ok := errResp["error"]; ok {
			return fmt.Errorf("%s", msg)
		}
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	if result != nil {
		if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

// ListJobs - フィルタ付きで全プロジェクト横断のジョブ一覧を取得
func (c *Client) ListJobs(filter api.JobListFilter) ([]api.JobWithContext, error) {
	path := "/api/jobs"
	var params []byte
	if filter.Status != "" {
		params = append(params, ("status=" + filter.Status)...)
	}
	if filter.Interactive != nil {
		sep := ""
		if len(params) > 0 {
			sep = "&"
		}
		if *filter.Interactive {
			params = append(params, (sep + "interactive=true")...)
		} else {
			params = append(params, (sep + "interactive=false")...)
		}
	}
	if len(params) > 0 {
		path += "?" + string(params)
	}

	var jobs []api.JobWithContext
	if err := c.Do("GET", path, nil, &jobs); err != nil {
		return nil, err
	}
	return jobs, nil
}

func (c *Client) AttachJob(jobID string, stdin io.Reader, stdout io.Writer) error {
	if stdout == nil {
		stdout = io.Discard
	}

	conn, err := net.Dial("unix", c.socketPath)
	if err != nil {
		return fmt.Errorf("dial attach socket: %w", err)
	}
	defer conn.Close()

	req, err := http.NewRequest("POST", "http://boid/api/jobs/"+jobID+"/attach", nil)
	if err != nil {
		return fmt.Errorf("create attach request: %w", err)
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "boid-attach")

	if err := req.Write(conn); err != nil {
		return fmt.Errorf("write attach request: %w", err)
	}

	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, req)
	if err != nil {
		return fmt.Errorf("read attach response: %w", err)
	}

	if resp.StatusCode != http.StatusSwitchingProtocols {
		defer resp.Body.Close()
		var errResp map[string]string
		if decodeErr := json.NewDecoder(resp.Body).Decode(&errResp); decodeErr == nil {
			if msg, ok := errResp["error"]; ok {
				return fmt.Errorf("%s", msg)
			}
		}
		return fmt.Errorf("attach failed: HTTP %d", resp.StatusCode)
	}

	outputErrCh := make(chan error, 1)
	go func() {
		_, err := io.Copy(stdout, reader)
		outputErrCh <- normalizeAttachIOError(err)
	}()

	if stdin == nil {
		return <-outputErrCh
	}

	inputErrCh := make(chan error, 1)
	go func() {
		_, err := io.Copy(conn, stdin)
		if err == nil {
			inputErrCh <- io.EOF
			return
		}
		inputErrCh <- err
	}()

	for {
		select {
		case err := <-outputErrCh:
			return normalizeAttachIOError(err)
		case err := <-inputErrCh:
			switch {
			case errors.Is(err, ErrAttachDetached):
				return nil
			case err == nil || errors.Is(err, io.EOF):
				_ = closeConnWrite(conn)
				inputErrCh = nil
			default:
				return normalizeAttachIOError(err)
			}
		}
	}
}

func (c *Client) ResizeJob(jobID string, rows, cols int) error {
	return c.Do("POST", "/api/jobs/"+jobID+"/resize", map[string]int{
		"rows": rows,
		"cols": cols,
	}, nil)
}

func normalizeAttachIOError(err error) error {
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

func closeConnWrite(conn net.Conn) error {
	type closeWriter interface {
		CloseWrite() error
	}
	if cw, ok := conn.(closeWriter); ok {
		return cw.CloseWrite()
	}
	return conn.Close()
}
