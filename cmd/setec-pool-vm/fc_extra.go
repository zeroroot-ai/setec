/*
Copyright 2026 The Setec Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// extraClient is a narrow HTTP-over-Unix-socket helper for the
// Firecracker REST endpoints the launcher uses during bring-up:
// /boot-source, /drives/:id, /machine-config, /actions. The main
// firecracker.Client in internal/firecracker covers only the
// snapshot/pause surface; we keep the extra helper local so the
// exported Client interface stays small.
type extraClient struct {
	http *http.Client
	base string
}

func newExtraClient(socketPath string) *extraClient {
	return &extraClient{
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					d := net.Dialer{Timeout: 5 * time.Second}
					return d.DialContext(ctx, "unix", socketPath)
				},
				MaxIdleConns:    2,
				IdleConnTimeout: 30 * time.Second,
			},
			Timeout: 30 * time.Second,
		},
		base: "http://firecracker",
	}
}

func (c *extraClient) do(ctx context.Context, path string, body any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.base+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
	return fmt.Errorf("%s %s: %d %s: %s",
		http.MethodPut, path, resp.StatusCode, http.StatusText(resp.StatusCode), bytes.TrimSpace(msg))
}
