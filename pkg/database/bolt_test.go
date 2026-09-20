// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package database

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
)

type testRecord struct {
	Name string `json:"name"`
	IP   string `json:"ip"`
}

func jsonSerializer(v interface{}) ([]byte, error) {
	return json.Marshal(v)
}

func jsonDeserializer(data []byte) (interface{}, error) {
	rec := &testRecord{}
	if err := json.Unmarshal(data, rec); err != nil {
		return nil, err
	}
	return rec, nil
}

// newTestDB opens a database under a fresh temp dir and returns it with its
// path. The cleanup closes it, tolerating os.ErrClosed because several subtests
// close the handle themselves -- bolt holds an exclusive lock on the file, so
// reopening it means closing it first.
func newTestDB(t *testing.T) (Database, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sub", "dir", "test.db")
	db, err := NewBoltHandler("test-bucket", path, jsonSerializer, jsonDeserializer)
	if err != nil {
		t.Fatalf("NewBoltHandler(%q) failed: %v", path, err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			t.Errorf("Close() failed: %v", err)
		}
	})
	return db, path
}

func TestNewBoltHandler(t *testing.T) {
	t.Run("creates missing parent directories and the db file", func(t *testing.T) {
		_, path := newTestDB(t)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("db file was not created: %v", err)
		}
		if info.Size() == 0 {
			t.Error("db file is empty, want an initialized bolt file")
		}
	})

	t.Run("starts empty", func(t *testing.T) {
		db, _ := newTestDB(t)
		if got := db.Count(); got != 0 {
			t.Errorf("Count() on a fresh db = %d, want 0", got)
		}
		if got := db.List(); got != nil {
			t.Errorf("List() on a fresh db = %v, want nil", got)
		}
	})

	t.Run("path pointing at a directory is an error", func(t *testing.T) {
		dir := t.TempDir()
		db, err := NewBoltHandler("bucket", dir, jsonSerializer, jsonDeserializer)
		if err == nil {
			_ = db.Close()
			t.Fatalf("NewBoltHandler(%q) on a directory succeeded, want an error", dir)
		}
	})

	t.Run("empty bucket name is an error", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "test.db")
		db, err := NewBoltHandler("", path, jsonSerializer, jsonDeserializer)
		if err == nil {
			_ = db.Close()
			t.Fatal("NewBoltHandler with an empty bucket name succeeded, want an error")
		}
	})
}

func TestBoltDatabasePutGet(t *testing.T) {
	t.Run("round trip returns the stored value", func(t *testing.T) {
		db, _ := newTestDB(t)
		want := &testRecord{Name: "pod-a", IP: "10.0.0.1"}

		if err := db.Put("pod-a", want); err != nil {
			t.Fatalf("Put failed: %v", err)
		}

		got, err := db.Get("pod-a")
		if err != nil {
			t.Fatalf("Get failed: %v", err)
		}
		rec, ok := got.(*testRecord)
		if !ok {
			t.Fatalf("Get returned %T, want *testRecord", got)
		}
		if *rec != *want {
			t.Errorf("Get returned %+v, want %+v", *rec, *want)
		}
	})

	t.Run("missing key returns ErrNotFound", func(t *testing.T) {
		db, _ := newTestDB(t)

		got, err := db.Get("nope")
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("Get(missing) error = %v, want ErrNotFound", err)
		}
		if got != nil {
			t.Errorf("Get(missing) = %v, want nil", got)
		}
	})

	t.Run("empty key is rejected by the backing store", func(t *testing.T) {
		db, _ := newTestDB(t)

		if err := db.Put("", &testRecord{Name: "blank"}); err == nil {
			t.Fatal("Put with an empty key succeeded, want an error from bolt")
		}
		// The cache must not be polluted when the write is rejected.
		if _, err := db.Get(""); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get(\"\") after a rejected Put = %v, want ErrNotFound", err)
		}
		if got := db.Count(); got != 0 {
			t.Errorf("Count() = %d, want 0", got)
		}
	})

	t.Run("put overwrites an existing key", func(t *testing.T) {
		db, path := newTestDB(t)

		if err := db.Put("pod-a", &testRecord{Name: "pod-a", IP: "10.0.0.1"}); err != nil {
			t.Fatalf("first Put failed: %v", err)
		}
		if err := db.Put("pod-a", &testRecord{Name: "pod-a", IP: "10.0.0.2"}); err != nil {
			t.Fatalf("second Put failed: %v", err)
		}

		if got := db.Count(); got != 1 {
			t.Errorf("Count() after overwrite = %d, want 1", got)
		}
		got, err := db.Get("pod-a")
		if err != nil {
			t.Fatalf("Get failed: %v", err)
		}
		if ip := got.(*testRecord).IP; ip != "10.0.0.2" {
			t.Errorf("Get returned ip %q, want the overwritten value %q", ip, "10.0.0.2")
		}

		// The overwrite must reach disk as well, not only the in-memory cache.
		if err := db.Close(); err != nil {
			t.Fatalf("Close failed: %v", err)
		}
		reopened, err := NewBoltHandler("test-bucket", path, jsonSerializer, jsonDeserializer)
		if err != nil {
			t.Fatalf("reopen failed: %v", err)
		}
		defer reopened.Close()
		got, err = reopened.Get("pod-a")
		if err != nil {
			t.Fatalf("Get after reopen failed: %v", err)
		}
		if ip := got.(*testRecord).IP; ip != "10.0.0.2" {
			t.Errorf("after reopen Get returned ip %q, want %q", ip, "10.0.0.2")
		}
	})

	t.Run("serializer error is propagated and nothing is stored", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "test.db")
		sentinel := errors.New("boom")
		db, err := NewBoltHandler("bucket", path,
			func(interface{}) ([]byte, error) { return nil, sentinel },
			jsonDeserializer)
		if err != nil {
			t.Fatalf("NewBoltHandler failed: %v", err)
		}
		defer db.Close()

		if err := db.Put("k", "v"); !errors.Is(err, sentinel) {
			t.Errorf("Put error = %v, want %v", err, sentinel)
		}
		if _, err := db.Get("k"); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get after a failed Put returned %v, want ErrNotFound", err)
		}
		if got := db.Count(); got != 0 {
			t.Errorf("Count() after a failed Put = %d, want 0", got)
		}
	})
}

func TestBoltDatabaseListAndCount(t *testing.T) {
	t.Run("empty database", func(t *testing.T) {
		db, _ := newTestDB(t)
		if got := db.List(); len(got) != 0 {
			t.Errorf("List() = %v, want empty", got)
		}
		if got := db.Count(); got != 0 {
			t.Errorf("Count() = %d, want 0", got)
		}
	})

	t.Run("returns every stored value", func(t *testing.T) {
		db, _ := newTestDB(t)
		want := []string{"pod-a", "pod-b", "pod-c"}
		for _, name := range want {
			if err := db.Put(name, &testRecord{Name: name}); err != nil {
				t.Fatalf("Put(%q) failed: %v", name, err)
			}
		}

		if got := db.Count(); got != len(want) {
			t.Errorf("Count() = %d, want %d", got, len(want))
		}

		values := db.List()
		if len(values) != len(want) {
			t.Fatalf("List() returned %d values, want %d", len(values), len(want))
		}
		var names []string
		for _, v := range values {
			rec, ok := v.(*testRecord)
			if !ok {
				t.Fatalf("List returned %T, want *testRecord", v)
			}
			names = append(names, rec.Name)
		}
		sort.Strings(names)
		for i := range want {
			if names[i] != want[i] {
				t.Errorf("List names = %v, want %v", names, want)
				break
			}
		}
	})

	t.Run("count tracks deletes", func(t *testing.T) {
		db, _ := newTestDB(t)
		for i := 0; i < 3; i++ {
			if err := db.Put(fmt.Sprintf("k%d", i), &testRecord{Name: fmt.Sprintf("k%d", i)}); err != nil {
				t.Fatalf("Put failed: %v", err)
			}
		}
		if err := db.Delete("k1"); err != nil {
			t.Fatalf("Delete failed: %v", err)
		}
		if got := db.Count(); got != 2 {
			t.Errorf("Count() after delete = %d, want 2", got)
		}
		if got := len(db.List()); got != 2 {
			t.Errorf("len(List()) after delete = %d, want 2", got)
		}
	})
}

func TestBoltDatabaseDelete(t *testing.T) {
	t.Run("deleted key is no longer found", func(t *testing.T) {
		db, _ := newTestDB(t)
		if err := db.Put("pod-a", &testRecord{Name: "pod-a"}); err != nil {
			t.Fatalf("Put failed: %v", err)
		}
		if err := db.Delete("pod-a"); err != nil {
			t.Fatalf("Delete failed: %v", err)
		}
		if _, err := db.Get("pod-a"); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get after Delete error = %v, want ErrNotFound", err)
		}
	})

	t.Run("deleting a missing key is not an error", func(t *testing.T) {
		db, _ := newTestDB(t)
		if err := db.Delete("nope"); err != nil {
			t.Errorf("Delete(missing) = %v, want nil", err)
		}
	})

	t.Run("delete is idempotent", func(t *testing.T) {
		db, _ := newTestDB(t)
		if err := db.Put("pod-a", &testRecord{Name: "pod-a"}); err != nil {
			t.Fatalf("Put failed: %v", err)
		}
		for i := 0; i < 3; i++ {
			if err := db.Delete("pod-a"); err != nil {
				t.Fatalf("Delete #%d failed: %v", i, err)
			}
		}
		if got := db.Count(); got != 0 {
			t.Errorf("Count() = %d, want 0", got)
		}
	})

	t.Run("delete reaches disk", func(t *testing.T) {
		db, path := newTestDB(t)
		if err := db.Put("pod-a", &testRecord{Name: "pod-a"}); err != nil {
			t.Fatalf("Put failed: %v", err)
		}
		if err := db.Delete("pod-a"); err != nil {
			t.Fatalf("Delete failed: %v", err)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("Close failed: %v", err)
		}

		reopened, err := NewBoltHandler("test-bucket", path, jsonSerializer, jsonDeserializer)
		if err != nil {
			t.Fatalf("reopen failed: %v", err)
		}
		defer reopened.Close()
		if got := reopened.Count(); got != 0 {
			t.Errorf("Count() after reopen = %d, want 0", got)
		}
	})
}

func TestBoltDatabaseReload(t *testing.T) {
	t.Run("values survive close and reopen", func(t *testing.T) {
		db, path := newTestDB(t)
		want := map[string]string{"pod-a": "10.0.0.1", "pod-b": "10.0.0.2"}
		for name, ip := range want {
			if err := db.Put(name, &testRecord{Name: name, IP: ip}); err != nil {
				t.Fatalf("Put(%q) failed: %v", name, err)
			}
		}
		if err := db.Close(); err != nil {
			t.Fatalf("Close failed: %v", err)
		}

		reopened, err := NewBoltHandler("test-bucket", path, jsonSerializer, jsonDeserializer)
		if err != nil {
			t.Fatalf("reopen failed: %v", err)
		}
		defer reopened.Close()

		if got := reopened.Count(); got != len(want) {
			t.Errorf("Count() after reopen = %d, want %d", got, len(want))
		}
		for name, ip := range want {
			got, err := reopened.Get(name)
			if err != nil {
				t.Fatalf("Get(%q) after reopen failed: %v", name, err)
			}
			if got.(*testRecord).IP != ip {
				t.Errorf("Get(%q) after reopen = %+v, want ip %q", name, got, ip)
			}
		}
	})

	t.Run("a different bucket does not see the values", func(t *testing.T) {
		db, path := newTestDB(t)
		if err := db.Put("pod-a", &testRecord{Name: "pod-a"}); err != nil {
			t.Fatalf("Put failed: %v", err)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("Close failed: %v", err)
		}

		other, err := NewBoltHandler("other-bucket", path, jsonSerializer, jsonDeserializer)
		if err != nil {
			t.Fatalf("opening a second bucket failed: %v", err)
		}
		defer other.Close()
		if got := other.Count(); got != 0 {
			t.Errorf("Count() in a different bucket = %d, want 0", got)
		}
	})

	t.Run("deserializer error fails the open", func(t *testing.T) {
		db, path := newTestDB(t)
		if err := db.Put("pod-a", &testRecord{Name: "pod-a"}); err != nil {
			t.Fatalf("Put failed: %v", err)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("Close failed: %v", err)
		}

		sentinel := errors.New("cannot decode")
		reopened, err := NewBoltHandler("test-bucket", path, jsonSerializer,
			func([]byte) (interface{}, error) { return nil, sentinel })
		if err == nil {
			_ = reopened.Close()
			t.Fatal("NewBoltHandler with a failing deserializer succeeded, want an error")
		}
		if !errors.Is(err, sentinel) {
			t.Errorf("error = %v, want %v", err, sentinel)
		}
		if reopened != nil {
			t.Error("NewBoltHandler returned a handle alongside an error, want nil")
		}
	})
}

func TestBoltDatabaseConcurrentAccess(t *testing.T) {
	db, _ := newTestDB(t)

	const workers = 8
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("pod-%d", i)
			if err := db.Put(key, &testRecord{Name: key}); err != nil {
				t.Errorf("Put(%q) failed: %v", key, err)
				return
			}
			if _, err := db.Get(key); err != nil {
				t.Errorf("Get(%q) failed: %v", key, err)
			}
			db.List()
			db.Count()
		}(i)
	}
	wg.Wait()

	if got := db.Count(); got != workers {
		t.Errorf("Count() = %d, want %d", got, workers)
	}
}
