// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package database

import (
	"os"
	"path/filepath"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

type BoltDatabase struct {
	db           *bolt.DB
	bucket       string
	cache        sync.Map
	serializer   Serializer
	deserializer Deserializer
}

func NewBoltHandler(bucket string, path string, serializer Serializer, deserializer Deserializer) (Database, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}

	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: 10 * time.Second})
	if err != nil {
		return nil, err
	}

	boltDb := &BoltDatabase{
		db:           db,
		bucket:       bucket,
		serializer:   serializer,
		deserializer: deserializer,
	}
	err = boltDb.load()
	if err != nil {
		return nil, err
	}

	return boltDb, nil
}

func (d *BoltDatabase) Put(key string, value interface{}) error {
	data, err := d.serializer(value)
	if err != nil {
		return err
	}

	err = d.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(d.bucket))
		return b.Put([]byte(key), data)
	})
	if err != nil {
		return err
	}
	d.cache.Store(key, value)
	return nil
}

func (d *BoltDatabase) load() error {
	if err := d.db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists([]byte(d.bucket))
		return err
	}); err != nil {
		return err
	}

	err := d.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(d.bucket))
		cursor := b.Cursor()
		for k, v := cursor.First(); k != nil; k, v = cursor.Next() {
			obj, err := d.deserializer(v)
			if err != nil {
				return err
			}
			d.cache.Store(string(k), obj)
		}
		return nil
	})
	return err
}

func (d *BoltDatabase) Get(key string) (interface{}, error) {
	value, ok := d.cache.Load(key)
	if !ok {
		return nil, ErrNotFound
	}
	return value, nil
}

func (d *BoltDatabase) List() []interface{} {
	var values []interface{}
	d.cache.Range(func(_, v any) bool {
		values = append(values, v)
		return true
	})
	return values
}

func (d *BoltDatabase) Count() int {
	count := 0
	d.cache.Range(func(key, value any) bool {
		count++
		return true
	})

	return count
}

func (d *BoltDatabase) Delete(key string) error {
	if err := d.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(d.bucket))
		return b.Delete([]byte(key))
	}); err != nil {
		return err
	}
	d.cache.Delete(key)
	return nil
}

func (d *BoltDatabase) Close() error {
	return d.db.Close()
}
