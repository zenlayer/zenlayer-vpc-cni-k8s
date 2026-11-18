// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeCredentialFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, v := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func stsFiles(id, secret, token string, exp time.Time) map[string]string {
	return map[string]string{
		"AccessKeyID":     id,
		"AccessKeySecret": secret,
		"SecurityToken":   token,
		"Expiration":      exp.UTC().Format(time.RFC3339),
	}
}

func TestReadCredentialsLongTerm(t *testing.T) {
	dir := t.TempDir()
	writeCredentialFiles(t, dir, map[string]string{"AccessKeyID": "ak\n", "AccessKeyPassword": "sk\n"})
	c, err := readCredentials(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.temporary() || c.id != "ak" || c.secret != "sk" {
		t.Fatalf("got %+v", c)
	}
}

func TestReadCredentialsTemporary(t *testing.T) {
	dir := t.TempDir()
	exp := time.Date(2026, 11, 17, 8, 0, 0, 0, time.UTC)
	writeCredentialFiles(t, dir, stsFiles("STS.ak", "sk", "token", exp))
	c, err := readCredentials(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !c.temporary() || c.id != "STS.ak" || c.secret != "sk" || c.token != "token" || !c.expiration.Equal(exp) {
		t.Fatalf("got %+v", c)
	}
}

func TestReadCredentialsTemporaryIncomplete(t *testing.T) {
	for name, files := range map[string]map[string]string{
		// A token next to a long-term key file is a half-written temporary
		// credential, not a long-term one.
		"no AccessKeySecret": {"AccessKeyID": "ak", "AccessKeyPassword": "sk", "SecurityToken": "token", "Expiration": "2026-11-17T08:00:00Z"},
		"no Expiration":      {"AccessKeyID": "ak", "AccessKeySecret": "sk", "SecurityToken": "token"},
		"bad Expiration":     {"AccessKeyID": "ak", "AccessKeySecret": "sk", "SecurityToken": "token", "Expiration": "tomorrow"},
		"empty token":        {"AccessKeyID": "ak", "AccessKeySecret": "sk", "SecurityToken": " \n", "Expiration": "2026-11-17T08:00:00Z"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeCredentialFiles(t, dir, files)
			if c, err := readCredentials(dir); err == nil {
				t.Fatalf("accepted %+v", c)
			}
		})
	}
}

func TestReloadSwapsClientOnRenewal(t *testing.T) {
	dir := t.TempDir()
	exp := time.Now().Add(time.Hour)
	writeCredentialFiles(t, dir, stsFiles("STS.ak", "sk1", "token1", exp))
	p, err := NewPlatform(&Config{CredentialDir: dir, RateLimit: RateLimitConfig{ReadQPS: 1, ReadBurst: 1, WriteQPS: 1, WriteBurst: 1}})
	if err != nil {
		t.Fatal(err)
	}
	first := p.client.Load()

	// Unchanged files keep the client.
	if err := p.reload(); err != nil {
		t.Fatal(err)
	}
	if p.client.Load() != first {
		t.Fatal("client replaced although the credential did not change")
	}

	// A renewal may keep the key ID and change only the secret and token.
	writeCredentialFiles(t, dir, stsFiles("STS.ak", "sk2", "token2", exp.Add(time.Hour)))
	if err := p.reload(); err != nil {
		t.Fatal(err)
	}
	if p.client.Load() == first {
		t.Fatal("client kept although the credential was renewed")
	}

	// An unreadable directory keeps the previous client.
	renewed := p.client.Load()
	if err := os.Remove(filepath.Join(dir, "Expiration")); err != nil {
		t.Fatal(err)
	}
	if err := p.reload(); err == nil {
		t.Fatal("reload accepted a credential without Expiration")
	}
	if p.client.Load() != renewed {
		t.Fatal("client replaced by an unreadable credential")
	}
}

func TestNewPlatformWithoutCredentials(t *testing.T) {
	if _, err := NewPlatform(&Config{CredentialDir: t.TempDir()}); err == nil {
		t.Fatal("NewPlatform accepted an empty credential directory")
	}
}
