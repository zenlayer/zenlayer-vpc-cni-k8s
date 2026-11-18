// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package ec2

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	zecCom "github.com/zenlayer/zenlayercloud-sdk-go/zenlayercloud/common"
)

// TestStsCredentialSigned checks that a client built from a temporary
// credential sends its security token and signs it.
func TestStsCredentialSigned(t *testing.T) {
	var auth, token string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		token = r.Header.Get("X-ZC-Security-Token")
		_, _ = io.WriteString(w, `{"requestId":"r","response":{"dataSet":[]}}`)
	}))
	defer srv.Close()
	// The SDK's HTTP client falls back to the default transport; point it at
	// one that trusts the test server.
	saved := http.DefaultTransport
	http.DefaultTransport = srv.Client().Transport
	defer func() { http.DefaultTransport = saved }()

	c, err := NewZecClientWithStsCredential(zecCom.NewStsCredential("STS.a", "sa", "zst_a"), strings.TrimPrefix(srv.URL, "https://"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.DescribeVpcs(nil); err != nil {
		t.Fatal(err)
	}
	if token != "zst_a" {
		t.Errorf("X-ZC-Security-Token = %q", token)
	}
	if !strings.Contains(auth, "Credential=STS.a,") || !strings.Contains(auth, "SignedHeaders=content-type;host;x-zc-security-token") {
		t.Errorf("Authorization = %q", auth)
	}
}

func TestNewZecClientWithStsCredentialRejectsMissingToken(t *testing.T) {
	if _, err := NewZecClientWithStsCredential(zecCom.NewStsCredential("STS.a", "sa", ""), ""); err == nil {
		t.Fatal("accepted a temporary credential without a security token")
	}
}
