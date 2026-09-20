// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

//go:generate protoc --go_out=. --go_opt=paths=source_relative --go-grpc_out=. --go-grpc_opt=paths=source_relative ipam.proto
package rpc
