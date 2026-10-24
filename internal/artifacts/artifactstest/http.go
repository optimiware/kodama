package artifactstest

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/artifacts"
)

var transferClient = &http.Client{Timeout: 20 * time.Second}

// httpUpload performs a PUT instruction as the holder would.
func httpUpload(tr artifacts.Transfer, data []byte) int {
	req, err := http.NewRequest(tr.Method, tr.URL, bytes.NewReader(data))
	if err != nil {
		return 0
	}
	req.ContentLength = int64(len(data))
	for k, v := range tr.Headers {
		if strings.EqualFold(k, "Content-Length") || strings.EqualFold(k, "Host") {
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := transferClient.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<14))
	return resp.StatusCode
}

// httpDownload performs a GET instruction as the holder would.
func httpDownload(tr artifacts.Transfer) (int, []byte) {
	req, err := http.NewRequest(tr.Method, tr.URL, nil)
	if err != nil {
		return 0, nil
	}
	resp, err := transferClient.Do(req)
	if err != nil {
		return 0, nil
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}
