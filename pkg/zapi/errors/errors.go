// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

// Package errors inspects platform API failures.
//
// The platform reports what went wrong as a code inside the error, so callers
// that need to act on a specific failure -- a subnet that does not exist, a
// quota that is exhausted -- match on that code rather than on the message.
// ErrCode and ErrEqual are how they do it, BackoffErrWrapper keeps the
// underlying error visible through a retry loop, and the event helpers here
// rate-limit what reaches the node's event stream.
//
// What this package does not do is tell a caller whether a failed request took
// effect anyway. The platform's assign and attach calls are not idempotent and
// a timeout says nothing either way, so that question is settled by reconciling
// against the platform, in pkg/daemon.
package errors

import (
	"fmt"
	"strings"

	"github.com/pkg/errors"
	"golang.org/x/time/rate"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/tracing"

	zecCom "github.com/zenlayer/zenlayercloud-sdk-go/zenlayercloud/common"
)

const (
	// 500 Series - Server Errors
	InternalServerError         = "INTERNAL_SERVER_ERROR"         // Internal server error
	ServiceTemporaryUnavailable = "SERVICE_TEMPORARY_UNAVAILABLE" // Service temporarily unavailable, please try again later
	RequestTimedOut             = "REQUEST_TIMED_OUT"             // Request timed out
	// 401 Series - Authentication Errors
	AuthInvalidAuthorization  = "AUTHFAILURE_INVALID_AUTHORIZATION"  // Invalid Authorization header format
	AuthSignatureFailure      = "AUTHFAILURE_SIGNATURE_FAILURE"      // Signature calculation failed
	AuthUnauthorizedOperation = "AUTHFAILURE_UNAUTHORIZED_OPERATION" // Unauthorized operation
	AuthSignatureExpired      = "AUTHFAILURE_SIGNATURE_EXPIRED"      // Signature expired (timestamp must be within 5 minutes of server time)
	// 400 Series - Parameter Errors
	MissingParameter                = "MISSING_PARAMETER"                   // Required parameter is missing
	InvalidParameterType            = "INVALID_PARAMETER_TYPE"              // Invalid parameter type
	InvalidParameterExceedMaximum   = "INVALID_PARAMETER_EXCEED_MAXIMUM"    // Parameter value exceeds maximum limit
	InvalidParameterLessMinimum     = "INVALID_PARAMETER_LESS_MINIMUM"      // Parameter value is below minimum limit
	InvalidParameterExceedLength    = "INVALID_PARAMETER_EXCEED_LENGTH"     // Parameter length exceeds maximum allowed
	InvalidParameterLessLength      = "INVALID_PARAMETER_LESS_LENGTH"       // Parameter length is below minimum required
	InvalidParameterPatternError    = "INVALID_PARAMETER_PATTERN_ERROR"     // Parameter value does not match required pattern
	InvalidParameterDateFormatError = "INVALID_PARAMETER_DATE_FORMAT_ERROR" // Invalid date format in parameter
	InvalidParameterValue           = "INVALID_PARAMETER_VALUE"             // Parameter value is not within the allowed set
	// 403 / 404 Series - Resource Errors
	OperationRecycleResource = "OPERATION_FAILED_FOR_RECYCLE_RESOURCE"     // Operation on a recycled resource is not allowed
	ResourceGroupNotFound    = "OPERATION_FAILED_RESOURCE_GROUP_NOT_FOUND" // Specified resource group not found
)

const (
	// Older error-code spellings. At least InsufficientIpInSubnet is no longer
	// sent -- see the note on OPERATION_DENIED_SUBNET_IP_INSUFFICIENT below --
	// but they stay matched, because a refusal that goes unrecognized costs a
	// whole retry budget before it reaches the caller that could act on it.
	LimitExceededEnisPerInstance  = "LimitExceeded.EnisPerInstance"
	QuotaExceededSecurityGroupIp  = "QuotaExceeded.SecurityGroupIp"
	QuotaExceededEniSecurityGroup = "QuotaExceeded.EniSecurityGroup"
	QuotaExceededEni              = "QuotaExceeded.Eni"
	InvalidPrivateIpMalformed     = "InvalidPrivateIp.Malformed"
	InvalidIpv6Malformed          = "InvalidIpv6.Malformed"
	InsufficientIpInSubnet        = "InsufficientIpInSubnet"
	AccountFlowLimitExceeded      = "AccountFlowLimitExceeded"
	FlowLimitExceeded             = "FlowLimitExceeded"
)

// Several of these codes are declared more than once under different Go names.
// Matching is by string value, so the copies are interchangeable.
const (
	InvalidNicPrimaryCantDelete      = "INVALID_NIC_PRIMARY_CANT_DELETE"               // 409 Primary NIC cannot be deleted
	InvalidNicAttached               = "INVALID_NIC_ATTACHED"                          // 404 NIC is attached to an instance
	InvalidNicStatus                 = "INVALID_NIC_STATUS"                            // 400 NIC status is not supported
	OperationDeniedPostpaidInPromise = "OPERATION_DENIED_POSTPAID_INSTANCE_IN_PROMISE" // 403 Postpaid instance in commitment period not supported
	ResourceNotFound                 = "OPERATION_FAILED_RESOURCE_NOT_FOUND"           // 404 Resource not found

	InvalidNicPrimaryCantDetach = "INVALID_NIC_PRIMARY_CANT_DETACH"              // 409 - Primary NIC cannot be detached
	InstanceStatusNotSupport    = "OPERATION_DENIED_INSTANCE_STATUS_NOT_SUPPORT" // 403 - Instance status is not supported
	NicNotExistInstance         = "OPERATION_DENIED_NIC_NOT_EXIST_INSTANCE"      // 403 - NIC does not exist on any instance
	InstancePrecheckAbnormal    = "INVALID_INSTANCE_PRECHECK_ABNORMAL"           // 400 - Instance precheck failed

	OperationFailedResourceGroupNotFound        = "OPERATION_FAILED_RESOURCE_GROUP_NOT_FOUND"         // 404 - Resource group not found
	InvalidSubnetNotFound                       = "INVALID_SUBNET_NOT_FOUND"                          // 404 - Subnet not found
	InvalidSecurityGroupNotFound                = "INVALID_SECURITY_GROUP_NOT_FOUND"                  // 404 - Security group not found
	OperationDeniedInternetChargeTypeNotSupport = "OPERATION_DENIED_INTERNET_CHARGE_TYPE_NOT_SUPPORT" // 403 - Internet charge type not supported
	// OPERATION_DENIED_FLOW_PACKAGE_NOT_SUPPORTED_HOUR_PERIOD is returned when a
	// committed traffic package size is given for an account on an hourly
	// billing period. Terminal: no amount of retrying changes the account's
	// billing period, and the operator has to pick a different charge type.
	// Without it here the assign burned the whole write backoff and reported a
	// generic timeout, burying the one line that says what to change.
	OperationDeniedFlowPackageNotSupportedHourPeriod = "OPERATION_DENIED_FLOW_PACKAGE_NOT_SUPPORTED_HOUR_PERIOD" // 403 - committed traffic package not supported on hourly billing

	OperationDeniedInstanceStatusNotSupport = "OPERATION_DENIED_INSTANCE_STATUS_NOT_SUPPORT" // 403 - Instance status is not supported
	InvalidInstanceNicLimit                 = "INVALID_INSTANCE_NIC_LIMIT"                   // 400 - NIC count limit exceeded for the instance
	InvalidInstancePrecheckAbnormal         = "INVALID_INSTANCE_PRECHECK_ABNORMAL"           // 400 - Instance precheck failed
	OperationFailedResourceNotFound         = "OPERATION_FAILED_RESOURCE_NOT_FOUND"          // 404 - Resource not found

	INVALID_NIC_LB_ASSOCIATED             = "INVALID_NIC_LB_ASSOCIATED"             // 409 - Backend service IP is associated with load balancer, operation not allowed.
	INVALID_NIC_NOT_FOUND                 = "INVALID_NIC_NOT_FOUND"                 // 404 - Network interface card not found.
	INVALID_NIC_PRIMARY_IP_CANT_DETACH    = "INVALID_NIC_PRIMARY_IP_CANT_DETACH"    // 409 - Primary IP of the network interface cannot be detached.
	INVALID_NIC_STATUS                    = "INVALID_NIC_STATUS"                    // 400 - Current NIC status does not allow this operation.
	INVALID_NIC_HAS_PUBLIC_IP_CANT_DETACH = "INVALID_NIC_HAS_PUBLIC_IP_CANT_DETACH" // 409 - NIC IP is associated with a public elastic IP, operation not allowed.

	OPERATION_DENIED_SUBNET_TYPE_NOT_SUPPORT_IPV4 = "OPERATION_DENIED_SUBNET_TYPE_NOT_SUPPORT_IPV4" // 400 - Subnet type does not support IPv4.
	INVALID_SUBNET_PRIVATE_IPV4_IN_USED           = "INVALID_SUBNET_PRIVATE_IPV4_IN_USED"           // 409 - Private IPv4 already in use.
	INVALID_IP_OUT_OF_RANGE                       = "INVALID_IP_OUT_OF_RANGE"                       // 400 - Private IPv4 is not in subnet range.
	INVALID_NIC_PRIVATE_IP_EXCEED_LIMIT           = "INVALID_NIC_PRIVATE_IP_EXCEED_LIMIT"           // 403 - NIC private IPv4 count limit exceeded.
	// OPERATION_DENIED_NIC_ATTACH_LAN_IP_EXCEED_LIMIT is what an assign call
	// actually returns when a NIC is at its address limit;
	// INVALID_NIC_PRIVATE_IP_EXCEED_LIMIT is the other spelling. It can arrive
	// while the daemon's own count for that NIC says there is still room, so
	// the reply is the authority: eni_multi_ip.go marks the interface
	// forbidInterfaceFull and submitOrder moves on to another one.
	OPERATION_DENIED_NIC_ATTACH_LAN_IP_EXCEED_LIMIT = "OPERATION_DENIED_NIC_ATTACH_LAN_IP_EXCEED_LIMIT"
	// OPERATION_DENIED_SUBNET_IP_INSUFFICIENT is what an assign call actually
	// returns when the subnet itself is out of addresses. The reply names the
	// shortfall, e.g. "insufficient available IPs in the specified subnet
	// '10.0.0.0/25' (required: '8', available: '0')".
	//
	// InsufficientIpInSubnet is the older spelling and is kept for
	// compatibility; the platform does not send it. Matching only that one left
	// subnet exhaustion unrecognized everywhere it mattered: the ENI was never
	// marked forbidAssign so submitOrder kept retrying the same full interface,
	// the call burned a whole backoff because the code was absent from the
	// non-retryable set, and GetResourceLimit never subtracted the unusable
	// capacity, so the scheduler kept placing pods that could not start.
	OPERATION_DENIED_SUBNET_IP_INSUFFICIENT = "OPERATION_DENIED_SUBNET_IP_INSUFFICIENT"
	// OPERATION_DENIED_INSTANCE_ATTACH_NIC_EXCEED_LIMIT is what an attach call
	// returns once the instance already carries as many interfaces as it may.
	// It is distinct from LimitExceededEnisPerInstance, which the create call
	// returns: an interface can be created and then refused at attach, so the
	// cached instance limit has to be refreshed on this code too. Without it
	// the daemon kept believing it had room and looped create -> attach fail
	// -> delete for as long as the cached limit stayed stale.
	OPERATION_DENIED_INSTANCE_ATTACH_NIC_EXCEED_LIMIT = "OPERATION_DENIED_INSTANCE_ATTACH_NIC_EXCEED_LIMIT"
	OPERATION_FAILED_RESOURCE_NOT_FOUND               = "OPERATION_FAILED_RESOURCE_NOT_FOUND" // 404 - Resource not found.
	INVALID_IP_NETWORK_ADDRESS                        = "INVALID_IP_NETWORK_ADDRESS"          // 400 - Network address is invalid.
	INVALID_IP_BROADCAST_ADDRESS                      = "INVALID_IP_BROADCAST_ADDRESS"        // 400 - Broadcast address is invalid.
	INVALID_IP_FIRST_ADDRESS                          = "INVALID_IP_FIRST_ADDRESS"            // 400 - Gateway address is invalid.

	OPERATION_DENIED_NIC_EXIST_IPV6                   = "OPERATION_DENIED_NIC_EXIST_IPV6"                   // 400 - NIC has existing IPv6 address.
	INVALID_SUBNET_IPSTACK_NOT_SUPPORT                = "INVALID_SUBNET_IPSTACK_NOT_SUPPORT"                // 400 - Subnet IP stack type is not supported.
	OPERATION_DENIED_INTERNET_CHARGE_TYPE_NOT_SUPPORT = "OPERATION_DENIED_INTERNET_CHARGE_TYPE_NOT_SUPPORT" // 403 - Internet charge type is not supported.
)

var (
	PublicErrCodes = []string{
		InternalServerError,
		ServiceTemporaryUnavailable,
		RequestTimedOut,
		AuthInvalidAuthorization,
		AuthSignatureFailure,
		AuthUnauthorizedOperation,
		AuthSignatureExpired,
		MissingParameter,
		InvalidParameterType,
		InvalidParameterExceedMaximum,
		InvalidParameterLessMinimum,
		InvalidParameterExceedLength,
		InvalidParameterLessLength,
		InvalidParameterPatternError,
		InvalidParameterDateFormatError,
		InvalidParameterValue,
		OperationRecycleResource,
		ResourceGroupNotFound,
	}
)

// ErrCode returns the OpenAPI error code carried by err, or "" when err is
// not an OpenAPI error. Use it instead of a type assertion: the one-value form
// panics on a non-SDK error, and neither form sees an SDK error that was
// wrapped with context on its way here.
func ErrCode(err error) string {
	var respErr *zecCom.ZenlayerCloudSdkError
	if errors.As(err, &respErr) {
		return respErr.GetCode()
	}
	return ""
}

// ErrEqual reports whether err is an OpenAPI error carrying errCode. It
// unwraps, so an error that was given context on the way up still matches.
func ErrEqual(errCode string, err error) bool {
	var respErr *zecCom.ZenlayerCloudSdkError
	if errors.As(err, &respErr) {
		return respErr.GetCode() == errCode
	}
	return false
}

// BackoffErrWrapper folds the error a backoff loop gave up with into the last
// real error, so an exhausted retry reports what kept failing instead of a
// bare timeout. A nil backErr means the loop succeeded, and realErr is dropped.
func BackoffErrWrapper(backErr, realErr error) error {
	var message string
	if backErr != nil {
		if wait.Interrupted(backErr) {
			message = backErr.Error()
		} else {
			return backErr
		}
	} else {
		return nil // backErr==nil means success, ignore realErr
	}
	if realErr != nil {
		if message != "" {
			return fmt.Errorf("%s due to %w", message, realErr)
		}
		return realErr
	}
	if message == "" {
		return nil
	}
	return errors.New(message)
}

// OpenApiErrCodeChain accumulates OpenAPI error codes to match an error against.
type OpenApiErrCodeChain struct {
	errs []string
}

func (c *OpenApiErrCodeChain) WithPublicErrCodes() *OpenApiErrCodeChain {
	c.errs = append(c.errs, PublicErrCodes...)
	return c
}

func (c *OpenApiErrCodeChain) WithFlowLimitExceeded() *OpenApiErrCodeChain {
	c.errs = append(c.errs, FlowLimitExceeded, AccountFlowLimitExceeded)
	return c
}

func (c *OpenApiErrCodeChain) WithErrCodes(codes ...string) *OpenApiErrCodeChain {
	c.errs = append(c.errs, codes...)
	return c
}

// ErrChainEqual reports whether err carries any of the codes in the chain.
func (c *OpenApiErrCodeChain) ErrChainEqual(err error) bool {
	for _, errCode := range c.errs {
		if ErrEqual(errCode, err) {
			return true
		}
	}
	return false
}

type EventInfoField struct {
	Key   string
	Value interface{}
}

func (f *EventInfoField) String() string {
	return fmt.Sprintf("%s=%v", f.Key, f.Value)
}

var (
	cantRetryErrEventLimiter *rate.Limiter
	flowLimitEventLimiter    *rate.Limiter
)

func AllowRecordErrEvent() bool {
	if cantRetryErrEventLimiter == nil {
		return true
	}
	return cantRetryErrEventLimiter.Allow()
}

func AllowRecordFlowLimitEvent() bool {
	if flowLimitEventLimiter == nil {
		return true
	}
	return flowLimitEventLimiter.Allow()
}

// RecordOpenAPIErrEvent raises a node event for the platform refusals an
// operator has to know about, taking the code and request id off the SDK error
// and appending whatever fields the caller supplies. Anything that is not a
// platform refusal is ignored.
func RecordOpenAPIErrEvent(err error, fields ...EventInfoField) {
	// errors.As, not a type assertion: an assertion answers "not a platform
	// error" for anything that was wrapped with context on its way here, and
	// the failure mode is silence -- no event saying why pods stopped starting.
	// Today's only caller, metrics.ZecAPIErrInc, forwards the error it was
	// given, so this is a guard rather than a repair, the same one ErrCode
	// carries.
	var respErr *zecCom.ZenlayerCloudSdkError
	if !errors.As(err, &respErr) {
		return
	}
	errCode := respErr.GetCode()

	fieldsInfo := ""
	for _, field := range fields {
		fieldsInfo += field.String()
		fieldsInfo += " "
	}
	fmtInfo := fmt.Sprintf("%s, %s", errCode, fieldsInfo)
	if respErr.GetRequestId() != "" {
		fmtInfo = fmt.Sprintf("%s RequestId: %s", fmtInfo, respErr.GetRequestId())
	}

	if strings.HasPrefix(errCode, "QuotaExceeded") ||
		strings.HasPrefix(errCode, "LimitExceeded") {
		if AllowRecordErrEvent() {
			_ = tracing.RecordNodeEvent(v1.EventTypeWarning, tracing.EventVpcResourceQuotaExceeded, fmtInfo)
		}
	}

	switch errCode {
	case InsufficientIpInSubnet, OPERATION_DENIED_SUBNET_IP_INSUFFICIENT:
		if AllowRecordErrEvent() {
			_ = tracing.RecordNodeEvent(v1.EventTypeWarning, tracing.EventInsufficientIpInSubnet, fmtInfo)
		}
	case FlowLimitExceeded, AccountFlowLimitExceeded:
		if AllowRecordFlowLimitEvent() {
			_ = tracing.RecordNodeEvent(v1.EventTypeWarning, tracing.EventOpenApiFlowLimit, fmtInfo)
		}
	}
}

func init() {
	if flowLimitEventLimiter == nil {
		flowLimitEventLimiter = rate.NewLimiter(2, 2)
	}
	if cantRetryErrEventLimiter == nil {
		cantRetryErrEventLimiter = rate.NewLimiter(5, 5)
	}
}

// Sentinel errors propagated to callers.
var (
	ErrNotFound                 = errors.New("not found")
	ErrInvalidDeletionPrimaryIP = errors.New("ip is primary, deletion invalid")
)
