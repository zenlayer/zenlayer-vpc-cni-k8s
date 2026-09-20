// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

//

// Package database provides the daemon's local persistent store.
//
// The daemon must survive its own restart without losing track of which pod
// holds which address, and it cannot rely on the API server being reachable at
// that moment, so the record lives on the node's disk. The Database interface
// keeps callers off the storage engine and lets tests substitute an in-memory
// one.
package database

import "errors"

// ErrNotFound is what Database.Get returns for a key that is not stored.
// ErrEOF is declared but not returned anywhere in this repository.
var (
	ErrEOF      = errors.New("no remain item to pop")
	ErrNotFound = errors.New("could not find this key in database")
)

// Database is a key-value store whose contents survive a restart. It holds
// values as interface{}; an implementation is constructed with a Serializer and
// a Deserializer to convert them for storage.
type Database interface {

	// Put stores value under key, replacing any existing value.
	Put(key string, value interface{}) error

	// Get returns the value stored under key. A missing key yields
	// ErrNotFound, which callers should test with errors.Is.
	Get(key string) (interface{}, error)

	// List returns every stored value, in no defined order.
	List() []interface{}

	// Count returns the number of stored entries.
	Count() int

	// Delete removes key. Deleting a key that is not stored is not an error.
	Delete(key string) error

	// Close closes the database.
	Close() error
}

// Serializer transforms the given object into serialized format.
type Serializer func(interface{}) ([]byte, error)

// Deserializer transforms the given serialized data into the original object.
type Deserializer func([]byte) (interface{}, error)
