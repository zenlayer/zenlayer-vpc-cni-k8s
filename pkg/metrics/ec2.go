// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"errors"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	zecCom "github.com/zenlayer/zenlayercloud-sdk-go/zenlayercloud/common"
	zecApi "github.com/zenlayer/zenlayercloud-sdk-go/zenlayercloud/zec20250901"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zapi/ec2"
	apiErr "github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zapi/errors"
)

var (
	Ec2APILatency = prometheus.NewSummaryVec(
		prometheus.SummaryOpts{
			Name: "zec_api_latency_ms",
			Help: "zenlayercloud API call latency in ms",
		},
		[]string{"api", "error", "status"},
	)
	Ec2ApiReq = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "zec_api_req_count",
			Help: "The number of requests made to zenlayercloud EC2 APIs by CNI",
		},
		[]string{"fn"},
	)
	Ec2APIErr = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "zec_api_error_count",
			Help: "The number of times zenlayercloud API returns an error",
		},
		[]string{"api", "error"},
	)
	MetadataLatency = prometheus.NewSummaryVec(
		prometheus.SummaryOpts{
			Name: "zec_metadata_api_latency_ms",
			Help: "zenlayercloud metadata API call latency in ms",
		},
		[]string{"metadata", "error", "status"},
	)

	MetadataErr = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "zec_metadata_api_error_count",
			Help: "The number of times zenlayercloud metadata API returns an error",
		},
		[]string{"metadata", "error"},
	)
)

// ZecAPIErrInc records an OpenAPI failure, both as a metric and as a node event.
func ZecAPIErrInc(api string, err error) {
	// Only the two labels Ec2APIErr declares. "error" carries what
	// ZecReqErrCode returns: the SDK error code, which is bounded, or the raw
	// error text for anything the SDK did not produce, which is not. The
	// per-request id never becomes a label -- prometheus keeps one time series
	// per distinct label combination -- so callers that need it should log it.
	Ec2APIErr.With(prometheus.Labels{"api": api, "error": ZecReqErrCode(err)}).Inc()

	// Also surface the error on the node, from the one place every OpenAPI error
	// passes through: all twelve wrapper methods below call this on failure.
	// Without a caller here the node events RecordOpenAPIErrEvent can raise --
	// quota exceeded, subnet exhausted, OpenAPI throttled, credential rejected,
	// permission missing, node clock skewed, OpenAPI unreachable -- are
	// unreachable.
	//
	// Safe to call on every error: it ignores anything that is not an SDK
	// error, records only those classes, and rate limits them itself.
	apiErr.RecordOpenAPIErrEvent(err, apiErr.EventInfoField{Key: "api", Value: api})
}

// MetadataErrInc records a metadata service failure.
func MetadataErrInc(metadata string, err error) {
	// errLabel rather than err.Error(): the metadata error text embeds the HTTP
	// response body, which as a label would grow the time series count without
	// bound.
	MetadataErr.With(prometheus.Labels{"metadata": metadata, "error": errLabel(err)}).Inc()
}

// LabelledError is implemented by errors that can name themselves in a metric
// label with a bounded value. It exists so a package reporting errors here can
// supply that value without this package importing it -- pkg/zapi/metadata
// already imports metrics, so a direct type reference would be an import cycle.
type LabelledError interface {
	MetricLabel() string
}

// errLabel renders an error as a metric label value. Nil-safe, and coarse by
// default: prometheus keeps one time series per distinct label combination, so
// anything unbounded (raw error text, a request id) must not reach a label.
func errLabel(err error) string {
	if err == nil {
		return ""
	}
	var le LabelledError
	if errors.As(err, &le) {
		return le.MetricLabel()
	}
	return "error"
}

// ZecReqId extracts the OpenAPI request id, for logging and support tickets.
// Deliberately not used as a metric label: it is unique per request, so it
// would create one prometheus time series per call.
func ZecReqId(err error) string {
	if err == nil {
		return ""
	}
	var zerr *zecCom.ZenlayerCloudSdkError
	if errors.As(err, &zerr) {
		return zerr.GetRequestId()
	}
	return ""
}

// ZecReqErrCode returns the SDK error code, or the error's own text when the
// error did not come from the SDK. Only the code half is bounded, so a label
// filled from this is bounded only as long as the SDK produced the error.
func ZecReqErrCode(err error) string {
	if err == nil {
		return ""
	}
	var zerr *zecCom.ZenlayerCloudSdkError
	if errors.As(err, &zerr) {
		return zerr.GetCode()
	}
	return err.Error() // Unknown err code
}

// MetricEC2Wrapper decorates an ec2.EC2: every call is counted and timed, and
// every failure also goes to ZecAPIErrInc.
type MetricEC2Wrapper struct {
	ec2SVC ec2.EC2
}

func NewMetricEC2Wrapper(p ec2.EC2) *MetricEC2Wrapper {
	return &MetricEC2Wrapper{ec2SVC: p}
}

// DescribeInstancesByIPv4 forwards to the wrapped client when it supports the
// lookup.
func (m *MetricEC2Wrapper) DescribeInstancesByIPv4(ip string, pageNum int) (*zecApi.DescribeInstancesResponse, error) {
	d, ok := m.ec2SVC.(ec2.InstanceByIPDescriber)
	if !ok {
		return nil, fmt.Errorf("the platform client cannot describe instances by address")
	}
	start := time.Now()
	resp, err := d.DescribeInstancesByIPv4(ip, pageNum)
	Ec2ApiReq.WithLabelValues("DescribeInstances").Inc()
	Ec2APILatency.WithLabelValues("DescribeInstances", fmt.Sprint(err != nil), ZecReqErrCode(err)).Observe(MsSince(start))
	if err != nil {
		ZecAPIErrInc("DescribeInstances", err)
	}
	return resp, err
}

func (m *MetricEC2Wrapper) DescribeInstances(instanceIds []string) (*zecApi.DescribeInstancesResponse, error) {
	start := time.Now()
	resp, err := m.ec2SVC.DescribeInstances(instanceIds)
	Ec2ApiReq.WithLabelValues("DescribeInstances").Inc()
	duration := MsSince(start)
	Ec2APILatency.WithLabelValues("DescribeInstances", fmt.Sprint(err != nil), ZecReqErrCode(err)).Observe(duration)
	if err != nil {
		ZecAPIErrInc("DescribeInstances", err)
	}
	return resp, err
}

func (m *MetricEC2Wrapper) DescribeSubnets(subnetIds []string) (*zecApi.DescribeSubnetsResponse, error) {
	start := time.Now()
	resp, err := m.ec2SVC.DescribeSubnets(subnetIds)
	Ec2ApiReq.WithLabelValues("DescribeSubnets").Inc()
	duration := MsSince(start)
	Ec2APILatency.WithLabelValues("DescribeSubnets", fmt.Sprint(err != nil), ZecReqErrCode(err)).Observe(duration)
	if err != nil {
		ZecAPIErrInc("DescribeSubnets", err)
	}
	return resp, err
}

func (m *MetricEC2Wrapper) DescribeVpcs(vpcIds []string) (*zecApi.DescribeVpcsResponse, error) {
	start := time.Now()
	resp, err := m.ec2SVC.DescribeVpcs(vpcIds)
	Ec2ApiReq.WithLabelValues("DescribeVpcs").Inc()
	duration := MsSince(start)
	Ec2APILatency.WithLabelValues("DescribeVpcs", fmt.Sprint(err != nil), ZecReqErrCode(err)).Observe(duration)
	if err != nil {
		ZecAPIErrInc("DescribeVpcs", err)
	}
	return resp, err
}

func (m *MetricEC2Wrapper) AssignPrivateIpAddress(nicId string, addresses []string, count int) (*zecApi.BatchAssignNetworkInterfaceIpv4Response, error) {
	start := time.Now()
	resp, err := m.ec2SVC.AssignPrivateIpAddress(nicId, addresses, count)
	Ec2ApiReq.WithLabelValues("AssignPrivateIpAddress").Inc()
	duration := MsSince(start)
	Ec2APILatency.WithLabelValues("AssignPrivateIpAddress", fmt.Sprint(err != nil), ZecReqErrCode(err)).Observe(duration)
	if err != nil {
		ZecAPIErrInc("AssignPrivateIpAddress", err)
	}
	return resp, err
}

func (m *MetricEC2Wrapper) UnAssignPrivateIpAddress(nicId string, addresses []string) (*zecApi.UnassignNetworkInterfaceIpv4Response, error) {
	start := time.Now()
	resp, err := m.ec2SVC.UnAssignPrivateIpAddress(nicId, addresses)
	Ec2ApiReq.WithLabelValues("UnAssignPrivateIpAddress").Inc()
	duration := MsSince(start)
	Ec2APILatency.WithLabelValues("UnAssignPrivateIpAddress", fmt.Sprint(err != nil), ZecReqErrCode(err)).Observe(duration)
	if err != nil {
		ZecAPIErrInc("UnAssignPrivateIpAddress", err)
	}
	return resp, err
}

func (m *MetricEC2Wrapper) UnassignNetworkInterfaceIpv6(nicId string) (*zecApi.UnassignNetworkInterfaceIpv6Response, error) {
	start := time.Now()
	resp, err := m.ec2SVC.UnassignNetworkInterfaceIpv6(nicId)
	Ec2ApiReq.WithLabelValues("UnassignNetworkInterfaceIpv6").Inc()
	duration := MsSince(start)
	Ec2APILatency.WithLabelValues("UnassignNetworkInterfaceIpv6", fmt.Sprint(err != nil), ZecReqErrCode(err)).Observe(duration)
	if err != nil {
		ZecAPIErrInc("UnassignNetworkInterfaceIpv6", err)
	}
	return resp, err
}

func (m *MetricEC2Wrapper) AssignNetworkInterfaceIpv6(nicId string, billing ec2.IPv6Billing) (*zecApi.AssignNetworkInterfaceIpv6Response, error) {
	start := time.Now()
	resp, err := m.ec2SVC.AssignNetworkInterfaceIpv6(nicId, billing)
	Ec2ApiReq.WithLabelValues("AssignNetworkInterfaceIpv6").Inc()
	duration := MsSince(start)
	Ec2APILatency.WithLabelValues("AssignNetworkInterfaceIpv6", fmt.Sprint(err != nil), ZecReqErrCode(err)).Observe(duration)
	if err != nil {
		ZecAPIErrInc("AssignNetworkInterfaceIpv6", err)
	}
	return resp, err
}

func (m *MetricEC2Wrapper) DescribeNetworkInterfaceAttributes(req *zecApi.DescribeNetworkInterfacesRequest) (*zecApi.DescribeNetworkInterfacesResponse, error) {
	start := time.Now()
	resp, err := m.ec2SVC.DescribeNetworkInterfaceAttributes(req)
	Ec2ApiReq.WithLabelValues("DescribeNetworkInterfaceAttributes").Inc()
	duration := MsSince(start)
	Ec2APILatency.WithLabelValues("DescribeNetworkInterfaceAttributes", fmt.Sprint(err != nil), ZecReqErrCode(err)).Observe(duration)
	if err != nil {
		ZecAPIErrInc("DescribeNetworkInterfaceAttributes", err)
	}
	return resp, err
}

func (m *MetricEC2Wrapper) CreateNetworkInterface(name, subnetId, securityGroupId string, tags map[string]string) (*zecApi.CreateNetworkInterfaceResponse, error) {
	start := time.Now()
	resp, err := m.ec2SVC.CreateNetworkInterface(name, subnetId, securityGroupId, tags)
	Ec2ApiReq.WithLabelValues("CreateNetworkInterface").Inc()
	duration := MsSince(start)
	Ec2APILatency.WithLabelValues("CreateNetworkInterface", fmt.Sprint(err != nil), ZecReqErrCode(err)).Observe(duration)
	if err != nil {
		ZecAPIErrInc("CreateNetworkInterface", err)
	}
	return resp, err
}

func (m *MetricEC2Wrapper) AttachNetworkInterface(nicId, instanceId string) (*zecApi.AttachNetworkInterfaceResponse, error) {
	start := time.Now()
	resp, err := m.ec2SVC.AttachNetworkInterface(nicId, instanceId)
	Ec2ApiReq.WithLabelValues("AttachNetworkInterface").Inc()
	duration := MsSince(start)
	Ec2APILatency.WithLabelValues("AttachNetworkInterface", fmt.Sprint(err != nil), ZecReqErrCode(err)).Observe(duration)
	if err != nil {
		ZecAPIErrInc("AttachNetworkInterface", err)
	}
	return resp, err
}

func (m *MetricEC2Wrapper) DetachNetworkInterface(nicId string) (*zecApi.DetachNetworkInterfaceResponse, error) {
	start := time.Now()
	resp, err := m.ec2SVC.DetachNetworkInterface(nicId)
	Ec2ApiReq.WithLabelValues("DetachNetworkInterface").Inc()
	duration := MsSince(start)
	Ec2APILatency.WithLabelValues("DetachNetworkInterface", fmt.Sprint(err != nil), ZecReqErrCode(err)).Observe(duration)
	if err != nil {
		ZecAPIErrInc("DetachNetworkInterface", err)
	}
	return resp, err
}

func (m *MetricEC2Wrapper) DeleteNetworkInterface(nicId string) (*zecApi.DeleteNetworkInterfaceResponse, error) {
	start := time.Now()
	resp, err := m.ec2SVC.DeleteNetworkInterface(nicId)
	Ec2ApiReq.WithLabelValues("DeleteNetworkInterface").Inc()
	duration := MsSince(start)
	Ec2APILatency.WithLabelValues("DeleteNetworkInterface", fmt.Sprint(err != nil), ZecReqErrCode(err)).Observe(duration)
	if err != nil {
		ZecAPIErrInc("DeleteNetworkInterface", err)
	}
	return resp, err
}
