package github

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"
)

const (
	requestTimeout = 15 * time.Second
	maxResponse    = 2 << 20
)

// ErrResponseTooLarge means GitHub (or something posing as it) answered with more than the cap.
var ErrResponseTooLarge = errors.New("github: response too large")

// bounded gives every request a deadline and caps the response body. It sits directly above the
// network, below the allowlist.
type bounded struct {
	next    http.RoundTripper
	timeout time.Duration
	max     int64
}

func (b bounded) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(req.Context(), b.timeout)
	resp, err := b.next.RoundTrip(req.WithContext(ctx))
	if err != nil {
		cancel()
		return nil, err
	}
	if resp.ContentLength > b.max {
		_ = resp.Body.Close()
		cancel()
		return nil, ErrResponseTooLarge
	}
	resp.Body = &cappedBody{rc: resp.Body, left: b.max, cancel: cancel}
	return resp, nil
}

type cappedBody struct {
	rc     io.ReadCloser
	left   int64
	cancel context.CancelFunc
}

func (c *cappedBody) Read(p []byte) (int, error) {
	if c.left < 0 {
		return 0, ErrResponseTooLarge
	}
	// Read one byte past the cap so an oversized body is seen, not silently truncated.
	if int64(len(p)) > c.left+1 {
		p = p[:c.left+1]
	}
	n, err := c.rc.Read(p)
	c.left -= int64(n)
	if c.left < 0 {
		return n - int(-c.left), ErrResponseTooLarge
	}
	return n, err
}

func (c *cappedBody) Close() error {
	err := c.rc.Close()
	c.cancel()
	return err
}
