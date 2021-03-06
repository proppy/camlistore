/*
Copyright 2012 Google Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package sqlindex

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strconv"

	"camlistore.org/pkg/index"
)

type Storage struct {
	DB *sql.DB

	// SetFunc is an optional func to use when REPLACE INTO does not exist
	SetFunc      func(*sql.DB, string, string) error
	BatchSetFunc func(*sql.Tx, string, string) error

	// PlaceHolderFunc optionally replaces ? placeholders with the right ones for the rdbms
	// in use
	PlaceHolderFunc func(string) string
}

func (s *Storage) sql(v string) string {
	if f := s.PlaceHolderFunc; f != nil {
		return f(v)
	}
	return v
}

type batchTx struct {
	tx  *sql.Tx
	err error // sticky

	// SetFunc is an optional func to use when REPLACE INTO does not exist
	SetFunc func(*sql.Tx, string, string) error

	// PlaceHolderFunc optionally replaces ? placeholders with the right ones for the rdbms
	// in use
	PlaceHolderFunc func(string) string
}

func (b *batchTx) sql(v string) string {
	if f := b.PlaceHolderFunc; f != nil {
		return f(v)
	}
	return v
}

func (b *batchTx) Set(key, value string) {
	if b.err != nil {
		return
	}
	if b.SetFunc != nil {
		b.err = b.SetFunc(b.tx, key, value)
		return
	}
	_, b.err = b.tx.Exec(b.sql("REPLACE INTO rows (k, v) VALUES (?, ?)"), key, value)
}

func (b *batchTx) Delete(key string) {
	if b.err != nil {
		return
	}
	_, b.err = b.tx.Exec(b.sql("DELETE FROM rows WHERE k=?"), key)
}

func (s *Storage) BeginBatch() index.BatchMutation {

	tx, err := s.DB.Begin()
	return &batchTx{
		tx:              tx,
		err:             err,
		SetFunc:         s.BatchSetFunc,
		PlaceHolderFunc: s.PlaceHolderFunc,
	}
}

func (s *Storage) CommitBatch(b index.BatchMutation) error {
	bt, ok := b.(*batchTx)
	if !ok {
		return fmt.Errorf("wrong BatchMutation type %T", b)
	}
	if bt.err != nil {
		return bt.err
	}
	return bt.tx.Commit()
}

func (s *Storage) Get(key string) (value string, err error) {
	err = s.DB.QueryRow(s.sql("SELECT v FROM rows WHERE k=?"), key).Scan(&value)
	if err == sql.ErrNoRows {
		err = index.ErrNotFound
	}
	return
}

func (s *Storage) Set(key, value string) error {
	if s.SetFunc != nil {
		return s.SetFunc(s.DB, key, value)
	}
	_, err := s.DB.Exec(s.sql("REPLACE INTO rows (k, v) VALUES (?, ?)"), key, value)
	return err
}

func (s *Storage) Delete(key string) error {
	_, err := s.DB.Exec(s.sql("DELETE FROM rows WHERE k=?"), key)
	return err
}

func (s *Storage) Find(key string) index.Iterator {
	return &iter{
		s:   s,
		low: key,
		op:  ">=",
	}
}

// iter is a iterator over sorted key/value pairs in rows.
type iter struct {
	s   *Storage
	low string
	op  string // ">=" initially, then ">"
	err error  // accumulated error, returned at Close

	rows *sql.Rows // if non-nil, the rows we're reading from

	batchSize int // how big our LIMIT query was
	seen      int // how many rows we've seen this query

	key   string
	value string
}

var errClosed = errors.New("mysqlindexer: Iterator already closed")

func (t *iter) Key() string   { return t.key }
func (t *iter) Value() string { return t.value }

func (t *iter) Close() error {
	if t.rows != nil {
		t.rows.Close()
	}
	err := t.err
	t.err = errClosed
	return err
}

func (t *iter) Next() bool {
	if t.err != nil {
		return false
	}
	if t.rows == nil {
		const batchSize = 50
		t.batchSize = batchSize
		t.rows, t.err = t.s.DB.Query(t.s.sql(
			"SELECT k, v FROM rows WHERE k "+t.op+" ? ORDER BY k LIMIT "+strconv.Itoa(batchSize)),
			t.low)
		if t.err != nil {
			log.Printf("unexpected query error: %v", t.err)
			return false
		}
		t.seen = 0
		t.op = ">"
	}
	if !t.rows.Next() {
		if t.seen == t.batchSize {
			t.rows = nil
			return t.Next()
		}
		return false
	}
	t.err = t.rows.Scan(&t.key, &t.value)
	if t.err != nil {
		log.Printf("unexpected Scan error: %v", t.err)
		return false
	}
	t.low = t.key
	t.seen++
	return true
}
