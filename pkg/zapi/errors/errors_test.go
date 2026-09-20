// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package errors_test

import (
	"context"
	stderrors "errors"
	"fmt"
	"strings"
	"testing"

	pkgerrors "github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/util/wait"

	zecCom "github.com/zenlayer/zenlayercloud-sdk-go/zenlayercloud/common"

	zerrors "github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zapi/errors"
)

func sdkErr(code string) error {
	return &zecCom.ZenlayerCloudSdkError{Code: code, Message: "boom", RequestId: "req-1"}
}

func TestErrCode(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"nil error", nil, ""},
		{"plain error", stderrors.New("something went wrong"), ""},
		{"openapi error", sdkErr(zerrors.InvalidParameterValue), zerrors.InvalidParameterValue},
		{"openapi error with empty code", sdkErr(""), ""},
		{
			// ErrCode unwraps; the bare type assertion it replaces at the
			// call sites does not.
			name: "openapi error wrapped with fmt %w",
			err:  fmt.Errorf("create eni: %w", sdkErr(zerrors.QuotaExceededEni)),
			want: zerrors.QuotaExceededEni,
		},
		{
			name: "openapi error wrapped with pkg/errors",
			err:  pkgerrors.Wrap(sdkErr(zerrors.InsufficientIpInSubnet), "assign ip"),
			want: zerrors.InsufficientIpInSubnet,
		},
		{
			name: "openapi error wrapped twice",
			err:  fmt.Errorf("outer: %w", pkgerrors.Wrap(sdkErr(zerrors.FlowLimitExceeded), "inner")),
			want: zerrors.FlowLimitExceeded,
		},
		{
			name: "wrapped plain error",
			err:  fmt.Errorf("outer: %w", stderrors.New("inner")),
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := zerrors.ErrCode(tt.err); got != tt.want {
				t.Errorf("ErrCode() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestErrEqual(t *testing.T) {
	tests := []struct {
		name    string
		errCode string
		err     error
		want    bool
	}{
		{"matching code", zerrors.InvalidNicStatus, sdkErr(zerrors.InvalidNicStatus), true},
		{"different code", zerrors.InvalidNicStatus, sdkErr(zerrors.ResourceNotFound), false},
		{"nil error", zerrors.InvalidNicStatus, nil, false},
		{"plain error", zerrors.InvalidNicStatus, stderrors.New("nope"), false},
		{"empty code against empty-code sdk error", "", sdkErr(""), true},
		{
			// Callers wrap OpenAPI errors on the way up, and ErrChainEqual --
			// hence the retry and quota handling -- is built on ErrEqual
			// seeing through the wrap.
			name:    "wrapped openapi error is matched",
			errCode: zerrors.InvalidNicStatus,
			err:     fmt.Errorf("detach: %w", sdkErr(zerrors.InvalidNicStatus)),
			want:    true,
		},
		{
			name:    "wrapped openapi error with a different code is not matched",
			errCode: zerrors.InvalidNicStatus,
			err:     fmt.Errorf("detach: %w", sdkErr(zerrors.ResourceNotFound)),
			want:    false,
		},
		{
			name:    "doubly wrapped openapi error is matched",
			errCode: zerrors.InvalidNicStatus,
			err:     fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", sdkErr(zerrors.InvalidNicStatus))),
			want:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := zerrors.ErrEqual(tt.errCode, tt.err); got != tt.want {
				t.Errorf("ErrEqual(%q, %v) = %v, want %v", tt.errCode, tt.err, got, tt.want)
			}
		})
	}
}

func TestOpenApiErrCodeChain(t *testing.T) {
	tests := []struct {
		name  string
		chain func() *zerrors.OpenApiErrCodeChain
		err   error
		want  bool
	}{
		{
			name:  "zero value chain matches nothing",
			chain: func() *zerrors.OpenApiErrCodeChain { return &zerrors.OpenApiErrCodeChain{} },
			err:   sdkErr(zerrors.InternalServerError),
			want:  false,
		},
		{
			name: "public err codes match",
			chain: func() *zerrors.OpenApiErrCodeChain {
				return (&zerrors.OpenApiErrCodeChain{}).WithPublicErrCodes()
			},
			err:  sdkErr(zerrors.ServiceTemporaryUnavailable),
			want: true,
		},
		{
			name: "public err codes do not match a non-public code",
			chain: func() *zerrors.OpenApiErrCodeChain {
				return (&zerrors.OpenApiErrCodeChain{}).WithPublicErrCodes()
			},
			err:  sdkErr(zerrors.InvalidNicStatus),
			want: false,
		},
		{
			name: "flow limit exceeded",
			chain: func() *zerrors.OpenApiErrCodeChain {
				return (&zerrors.OpenApiErrCodeChain{}).WithFlowLimitExceeded()
			},
			err:  sdkErr(zerrors.FlowLimitExceeded),
			want: true,
		},
		{
			name: "account flow limit exceeded",
			chain: func() *zerrors.OpenApiErrCodeChain {
				return (&zerrors.OpenApiErrCodeChain{}).WithFlowLimitExceeded()
			},
			err:  sdkErr(zerrors.AccountFlowLimitExceeded),
			want: true,
		},
		{
			name: "explicit codes",
			chain: func() *zerrors.OpenApiErrCodeChain {
				return (&zerrors.OpenApiErrCodeChain{}).WithErrCodes(zerrors.INVALID_NIC_NOT_FOUND)
			},
			err:  sdkErr(zerrors.INVALID_NIC_NOT_FOUND),
			want: true,
		},
		{
			name: "builders compose",
			chain: func() *zerrors.OpenApiErrCodeChain {
				return (&zerrors.OpenApiErrCodeChain{}).
					WithPublicErrCodes().
					WithFlowLimitExceeded().
					WithErrCodes(zerrors.InsufficientIpInSubnet)
			},
			err:  sdkErr(zerrors.InsufficientIpInSubnet),
			want: true,
		},
		{
			name: "composed chain still rejects an unlisted code",
			chain: func() *zerrors.OpenApiErrCodeChain {
				return (&zerrors.OpenApiErrCodeChain{}).
					WithPublicErrCodes().
					WithFlowLimitExceeded().
					WithErrCodes(zerrors.InsufficientIpInSubnet)
			},
			err:  sdkErr(zerrors.InvalidNicPrimaryCantDelete),
			want: false,
		},
		{
			name: "nil error never matches",
			chain: func() *zerrors.OpenApiErrCodeChain {
				return (&zerrors.OpenApiErrCodeChain{}).WithPublicErrCodes().WithFlowLimitExceeded()
			},
			err:  nil,
			want: false,
		},
		{
			name: "plain error never matches",
			chain: func() *zerrors.OpenApiErrCodeChain {
				return (&zerrors.OpenApiErrCodeChain{}).WithPublicErrCodes()
			},
			err:  stderrors.New(zerrors.InternalServerError),
			want: false,
		},
		{
			name: "WithErrCodes with no arguments is a no-op",
			chain: func() *zerrors.OpenApiErrCodeChain {
				return (&zerrors.OpenApiErrCodeChain{}).WithErrCodes()
			},
			err:  sdkErr(zerrors.InternalServerError),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.chain().ErrChainEqual(tt.err); got != tt.want {
				t.Errorf("ErrChainEqual() = %v, want %v", got, tt.want)
			}
		})
	}
}

// Each builder must return the same pointer it was called on, so that a chain
// can be built as one expression and the original variable still sees every
// stage.
func TestOpenApiErrCodeChainBuildersReturnReceiver(t *testing.T) {
	c := &zerrors.OpenApiErrCodeChain{}
	if got := c.WithPublicErrCodes(); got != c {
		t.Error("WithPublicErrCodes did not return the receiver")
	}
	if got := c.WithFlowLimitExceeded(); got != c {
		t.Error("WithFlowLimitExceeded did not return the receiver")
	}
	if got := c.WithErrCodes("x"); got != c {
		t.Error("WithErrCodes did not return the receiver")
	}
}

func TestPublicErrCodesContent(t *testing.T) {
	seen := map[string]bool{}
	for _, code := range zerrors.PublicErrCodes {
		if code == "" {
			t.Error("PublicErrCodes contains an empty code, which would make ErrChainEqual match any SDK error with no code")
		}
		if seen[code] {
			t.Errorf("PublicErrCodes contains duplicate code %q", code)
		}
		seen[code] = true
	}
	for _, want := range []string{
		zerrors.InternalServerError,
		zerrors.ServiceTemporaryUnavailable,
		zerrors.RequestTimedOut,
	} {
		if !seen[want] {
			t.Errorf("PublicErrCodes is missing the retryable server error %q", want)
		}
	}
}

// waitTimeout builds the error a wait loop returns when it runs out of steps.
//
// It is constructed rather than taken from the package's exported sentinel,
// which is deprecated: ErrorInterrupted wrapping this exact message is what that
// sentinel is defined as. wait.Interrupted reports true for it, which is what
// BackoffErrWrapper keys off, and the message is what the assertions below pin.
func waitTimeout() error {
	return wait.ErrorInterrupted(stderrors.New("timed out waiting for the condition"))
}

func TestBackoffErrWrapper(t *testing.T) {
	realErr := stderrors.New("api said no")
	otherBackErr := stderrors.New("condition function failed")

	tests := []struct {
		name     string
		backErr  error
		realErr  error
		wantNil  bool
		wantIs   error // errors.Is target, when the original error must be preserved
		contains []string
	}{
		{
			name:    "no backoff error means success",
			backErr: nil,
			realErr: realErr,
			wantNil: true,
		},
		{
			name:    "no errors at all",
			backErr: nil,
			realErr: nil,
			wantNil: true,
		},
		{
			// A non-interrupt backoff error is the condition function's own
			// error, so it is returned untouched.
			name:    "non interrupt backoff error is returned as is",
			backErr: otherBackErr,
			realErr: realErr,
			wantIs:  otherBackErr,
		},
		{
			name:     "timeout with real error",
			backErr:  waitTimeout(),
			realErr:  realErr,
			contains: []string{"timed out waiting for the condition", "due to", "api said no"},
		},
		{
			name:     "timeout without real error keeps the timeout message",
			backErr:  waitTimeout(),
			realErr:  nil,
			contains: []string{"timed out waiting for the condition"},
		},
		{
			name:     "context deadline exceeded counts as interrupted",
			backErr:  context.DeadlineExceeded,
			realErr:  realErr,
			contains: []string{context.DeadlineExceeded.Error(), "due to", "api said no"},
		},
		{
			name:     "context canceled counts as interrupted",
			backErr:  context.Canceled,
			realErr:  realErr,
			contains: []string{context.Canceled.Error(), "due to", "api said no"},
		},
		{
			name:     "interrupt wrapped by fmt is still recognized",
			backErr:  fmt.Errorf("waiting for eni: %w", context.DeadlineExceeded),
			realErr:  realErr,
			contains: []string{"waiting for eni", "due to", "api said no"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := zerrors.BackoffErrWrapper(tt.backErr, tt.realErr)
			if tt.wantNil {
				if got != nil {
					t.Fatalf("BackoffErrWrapper() = %v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatal("BackoffErrWrapper() = nil, want an error")
			}
			if tt.wantIs != nil && !stderrors.Is(got, tt.wantIs) {
				t.Errorf("BackoffErrWrapper() = %v, want it to wrap %v", got, tt.wantIs)
			}
			for _, want := range tt.contains {
				if !strings.Contains(got.Error(), want) {
					t.Errorf("BackoffErrWrapper() = %q, want it to contain %q", got.Error(), want)
				}
			}
		})
	}
}

// The SDK error must survive the wrapper, or retry decisions further up the
// stack can no longer read its code.
func TestBackoffErrWrapperKeepsOpenAPICodeReadable(t *testing.T) {
	t.Run("non interrupt backoff error", func(t *testing.T) {
		api := sdkErr(zerrors.QuotaExceededEni)
		got := zerrors.BackoffErrWrapper(api, nil)
		if code := zerrors.ErrCode(got); code != zerrors.QuotaExceededEni {
			t.Errorf("ErrCode() = %q, want %q", code, zerrors.QuotaExceededEni)
		}
	})

	t.Run("timeout keeps the real error recoverable", func(t *testing.T) {
		api := sdkErr(zerrors.QuotaExceededEni)
		got := zerrors.BackoffErrWrapper(waitTimeout(), api)
		if got == nil {
			t.Fatal("BackoffErrWrapper() = nil, want an error")
		}
		// Both halves survive: the backoff message for humans, and the
		// wrapped OpenAPI error for the code.
		if !strings.Contains(got.Error(), zerrors.QuotaExceededEni) {
			t.Errorf("BackoffErrWrapper() = %q, want it to mention %q", got.Error(), zerrors.QuotaExceededEni)
		}
		if code := zerrors.ErrCode(got); code != zerrors.QuotaExceededEni {
			t.Errorf("ErrCode() = %q, want %q", code, zerrors.QuotaExceededEni)
		}
		if !zerrors.ErrEqual(zerrors.QuotaExceededEni, got) {
			t.Errorf("ErrEqual() = false, want true through the wrapped timeout")
		}
		if !stderrors.Is(got, api) {
			t.Errorf("errors.Is() = false, want the original error to be recoverable")
		}
	})
}

func TestEventInfoFieldString(t *testing.T) {
	tests := []struct {
		name  string
		field zerrors.EventInfoField
		want  string
	}{
		{"string value", zerrors.EventInfoField{Key: "eni", Value: "eni-123"}, "eni=eni-123"},
		{"int value", zerrors.EventInfoField{Key: "count", Value: 3}, "count=3"},
		{"nil value", zerrors.EventInfoField{Key: "k", Value: nil}, "k=<nil>"},
		{"empty key", zerrors.EventInfoField{Key: "", Value: "v"}, "=v"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := tt.field
			if got := f.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}

// RecordOpenAPIErrEvent's only non-test caller, metrics.ZecAPIErrInc, hands it
// every API failure whether or not it came from the SDK, so a non-SDK error
// must be ignored rather than panic.
func TestRecordOpenAPIErrEventIgnoresNonSDKErrors(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("RecordOpenAPIErrEvent panicked: %v", r)
		}
	}()
	zerrors.RecordOpenAPIErrEvent(nil)
	zerrors.RecordOpenAPIErrEvent(stderrors.New("plain"), zerrors.EventInfoField{Key: "k", Value: "v"})
}

func TestSentinelErrors(t *testing.T) {
	sentinels := map[string]error{
		"ErrNotFound":                 zerrors.ErrNotFound,
		"ErrInvalidDeletionPrimaryIP": zerrors.ErrInvalidDeletionPrimaryIP,
	}
	for name, err := range sentinels {
		t.Run(name, func(t *testing.T) {
			if err == nil {
				t.Fatal("sentinel is nil")
			}
			if err.Error() == "" {
				t.Error("sentinel has an empty message")
			}
			if !stderrors.Is(fmt.Errorf("wrapped: %w", err), err) {
				t.Error("sentinel is not recoverable with errors.Is after wrapping")
			}
			for otherName, other := range sentinels {
				if otherName != name && stderrors.Is(err, other) {
					t.Errorf("%s and %s are indistinguishable with errors.Is", name, otherName)
				}
			}
		})
	}
}
