// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

// Package metadata reads this instance's own network configuration from the
// metadata service.
//
// It is the only source for facts the API does not report per-node, such as
// which interfaces are attached and what gateway each one uses. Missing entries
// are common rather than exceptional -- an interface in a subnet without a given
// address family simply has no key for it -- so callers must be prepared to
// distinguish absent from failed.
package metadata

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/errors"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zlog"
)

var log = zlog.GetLogger().WithFields(zlog.Fields{"subsys": "metadata"})

// metadataURL is the base URL of the link-local instance metadata service,
// reachable from the instance itself with no credentials. It is how the daemon
// learns about the ENIs attached to its own node.
//
// Reference for the paths metadata_wrapper.go appends to it:
// https://docs.console.zenlayer.com/welcome/elastic-compute/get-started/manage-instances/get-instance-metadata
const (
	metadataURL = "http://169.254.169.254/latest/meta-data"
)

// metadataTimeout bounds one call to the instance metadata service. The service
// answers locally over the link-local address, so a healthy reply arrives in
// milliseconds and anything approaching this bound means it is in trouble;
// waiting longer only delays the caller's fallback. Most callers get one shot;
// the loops that poll for an address to appear or disappear after an assign or
// unassign wrap this in their own backoff, so a slow reply is still covered.
var metadataTimeout = time.Second * 3

// EC2Metadata is a client for the instance metadata service.
type EC2Metadata struct {
	*http.Client
}

// New returns a metadata client whose requests are bounded by metadataTimeout.
func New() *EC2Metadata {
	return &EC2Metadata{
		&http.Client{
			Timeout: metadataTimeout,
		},
	}
}

// GetMetadata returns the body served at path, trimmed of surrounding
// whitespace. A non-200 reply comes back as *StatusError.
func (c *EC2Metadata) GetMetadata(ctx context.Context, path string) (info string, err error) {
	var req *http.Request
	var resp *http.Response
	var respBytes []byte
	info = ""
	url := fmt.Sprintf("%s/%s", metadataURL, path)

	defer func() {
		if err != nil {
			fmtErr := fmt.Sprintf("Call metadata failed, path: %s, err: %v", url, err)
			log.Error(fmtErr)
		}
	}()

	if req, err = http.NewRequestWithContext(ctx, http.MethodGet, url, nil); err != nil {
		return
	}

	if resp, err = c.Do(req); err != nil {
		return
	}
	defer func(Body io.ReadCloser) {
		_ = Body.Close()
	}(resp.Body)

	if resp.StatusCode != http.StatusOK {
		respBytes, _ = io.ReadAll(resp.Body)
		err = &StatusError{StatusCode: resp.StatusCode, Body: string(respBytes)}
		return
	}
	if respBytes, err = io.ReadAll(resp.Body); err != nil {
		return
	}
	info = strings.TrimSpace(string(respBytes))
	return
}

// StatusError is a non-200 reply from the metadata service.
//
// The status code is kept as a field rather than only formatted into the
// message so that metrics can label by it: Error() embeds the response body and
// is therefore unbounded, and a prometheus label must not be. The text is
// unchanged from when this was a plain errors.New, so logs read the same.
type StatusError struct {
	StatusCode int
	Body       string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("HttpRequestStatus: %d, HttpResponse: %s", e.StatusCode, e.Body)
}

// MetricLabel satisfies metrics.LabelledError: the status code is bounded,
// unlike Error(), which embeds the response body.
func (e *StatusError) MetricLabel() string {
	return strconv.Itoa(e.StatusCode)
}

// StatusLabel renders the outcome of a metadata call as a metric label value.
// It is nil-safe, and coarse on purpose: prometheus keeps one time series per
// distinct label combination, so the raw error text -- which embeds the HTTP
// response body -- would grow the series count without bound.
func StatusLabel(err error) string {
	if err == nil {
		return ""
	}
	var se *StatusError
	if errors.As(err, &se) {
		return strconv.Itoa(se.StatusCode)
	}
	return "error"
}
