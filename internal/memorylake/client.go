package memorylake

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type APIError struct {
	Code, Message string
	HTTP          int
}

func (e *APIError) Error() string {
	return fmt.Sprintf("memorylake %s (%d): %s", e.Code, e.HTTP, e.Message)
}

type Client struct {
	base string
	key  string
	http *http.Client
}

func NewClient(cfg Config) *Client {
	return &Client{
		base: strings.TrimRight(cfg.BaseURL, "/"),
		key:  cfg.APIKey,
		http: &http.Client{Timeout: time.Duration(cfg.TimeoutMS) * time.Millisecond},
	}
}

type wrapper struct {
	Success   bool            `json:"success"`
	Message   string          `json:"message"`
	ErrorCode string          `json:"error_code"`
	Data      json.RawMessage `json:"data"`
}

func (c *Client) doJSON(method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var w wrapper
	if err := json.Unmarshal(raw, &w); err != nil {
		return &APIError{Code: "BAD_RESPONSE", Message: string(raw), HTTP: resp.StatusCode}
	}
	if !w.Success {
		return &APIError{Code: w.ErrorCode, Message: w.Message, HTTP: resp.StatusCode}
	}
	if out != nil && len(w.Data) > 0 {
		return json.Unmarshal(w.Data, out)
	}
	return nil
}
