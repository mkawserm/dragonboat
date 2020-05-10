package badger

import (
	"bytes"
	badgerDb "github.com/dgraph-io/badger/v2"
	badgerDbOptions "github.com/dgraph-io/badger/v2/options"
	"github.com/mkawserm/dragonboat/v3/config"
	"github.com/mkawserm/dragonboat/v3/internal/fileutil"
	"github.com/mkawserm/dragonboat/v3/internal/logdb/kv"
	"github.com/mkawserm/dragonboat/v3/internal/vfs"
	"github.com/mkawserm/dragonboat/v3/raftio"
	"time"
)

const MaxKeyLength = 1024

type badgerWriteBatch struct {
	mDb    *badgerDb.DB
	mTxn   *badgerDb.Txn
	mCount int
}

func (w *badgerWriteBatch) Clear() {
	w.mTxn.Discard()
	w.mTxn = w.mDb.NewTransaction(true)
	w.mCount = 0
}

func (w *badgerWriteBatch) Count() int {
	return w.mCount
}

func (w *badgerWriteBatch) Commit() error {
	return w.mTxn.Commit()
}

func (w *badgerWriteBatch) Put(key []byte, val []byte) {
	if err := w.mTxn.Set(key, val); err == badgerDb.ErrTxnTooBig {
		if err := w.mTxn.Commit(); err != nil {
			panic(err)
		} else {
			w.mCount = 0
		}
	} else if err == nil {
		w.mCount = w.mCount + 1
	} else {
		panic(err)
	}
}

func (w *badgerWriteBatch) Delete(key []byte) {
	if err := w.mTxn.Delete(key); err == badgerDb.ErrTxnTooBig {
		if err := w.mTxn.Commit(); err != nil {
			panic(err)
		} else {
			w.mCount = 0
		}
	} else if err == nil {
		w.mCount = w.mCount + 1
	} else {
		panic(err)
	}
}

func (w *badgerWriteBatch) Destroy() {
	w.mTxn.Discard()
}

var _ kv.IKVStore = &Badger{}

type Badger struct {
	mDb *badgerDb.DB
}

func openBadgerDb(config config.LogDBConfig,
	dir string, walDir string, fs vfs.IFS) (kv.IKVStore, error) {
	if config.IsEmpty() {
		panic("invalid LogDBConfig")
	}

	if err := fileutil.MkdirAll(dir, fs); err != nil {
		return nil, err
	}

	opts := badgerDb.DefaultOptions(dir)

	if len(walDir) > 0 {
		if err := fileutil.MkdirAll(walDir, fs); err != nil {
			return nil, err
		}
		opts.ValueDir = walDir
	}
	opts.Truncate = true
	opts.TableLoadingMode = badgerDbOptions.LoadToRAM
	opts.ValueLogLoadingMode = badgerDbOptions.MemoryMap
	opts.Compression = badgerDbOptions.Snappy
	db, err := badgerDb.Open(opts)
	if err != nil {
		return nil, err
	}

	return &Badger{mDb: db}, nil
}

func (b *Badger) Name() string {
	return "badger"
}

func (b *Badger) Close() error {
	if b.mDb == nil {
		return nil
	}
	_ = b.mDb.Close()
	b.mDb = nil
	return nil
}

func (b *Badger) IterateValue(fk []byte, lk []byte, inc bool, op func(key []byte, data []byte) (bool, error)) error {
	err := b.mDb.View(func(txn *badgerDb.Txn) error {
		it := txn.NewIterator(badgerDb.DefaultIteratorOptions)
		defer it.Close()

		for it.Seek(fk); it.Valid(); it.Next() {
			item := it.Item()
			key := item.Key()
			val, err := item.ValueCopy(nil)
			if err != nil {
				return err
			}

			if inc {
				if bytes.Compare(key, lk) > 0 {
					return nil
				}
			} else {
				if bytes.Compare(key, lk) >= 0 {
					return nil
				}
			}

			cont, err := op(key, val)
			if err != nil {
				return err
			}
			if !cont {
				break
			}
		}
		return nil
	})

	return err
}

func (b *Badger) GetValue(key []byte, op func([]byte) error) error {
	err := b.mDb.View(func(txn *badgerDb.Txn) error {
		item, err := txn.Get(key)

		if err != nil && err != badgerDb.ErrKeyNotFound {
			return err
		}
		if item == nil {
			return nil
		}

		val, err := item.ValueCopy(nil)
		if err != nil {
			return err
		}
		return op(val)
	})

	return err
}

func (b *Badger) SaveValue(key []byte, value []byte) error {
	err := b.mDb.Update(func(txn *badgerDb.Txn) error {
		return txn.Set(key, value)
	})

	return err
}

func (b *Badger) DeleteValue(key []byte) error {
	err := b.mDb.Update(func(txn *badgerDb.Txn) error {
		return txn.Delete(key)
	})

	return err
}

func (b *Badger) GetWriteBatch(ctx raftio.IContext) kv.IWriteBatch {
	if ctx != nil {
		wb := ctx.GetWriteBatch()
		if wb != nil {
			pwb := wb.(*badgerWriteBatch)
			if pwb.mDb == b.mDb {
				return pwb
			}
		}
	}

	return &badgerWriteBatch{
		mDb:    b.mDb,
		mTxn:   b.mDb.NewTransaction(true),
		mCount: 0,
	}
}

func (b *Badger) CommitWriteBatch(wb kv.IWriteBatch) error {
	pwb, ok := wb.(*badgerWriteBatch)
	if !ok {
		panic("unknown type")
	}
	return pwb.mTxn.Commit()
}

func (b *Badger) BulkRemoveEntries(firstKey []byte, lastKey []byte) error {
	return b.CompactEntries(firstKey, lastKey)
}

func (b *Badger) CompactEntries(firstKey []byte, lastKey []byte) error {
	err := b.mDb.Update(func(txn *badgerDb.Txn) error {
		opts := badgerDb.DefaultIteratorOptions
		opts.PrefetchValues = false
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(firstKey); it.Valid(); it.Next() {
			item := it.Item()
			key := item.Key()
			if bytes.Compare(key, lastKey) >= 0 {
				break
			}
			if err := txn.Delete(key); err == badgerDb.ErrTxnTooBig {
				if err := txn.Commit(); err != nil {
					return err
				}
			}
		}
		return nil
	})
	return err
}

func (b *Badger) FullCompaction() error {
	fk := make([]byte, MaxKeyLength)
	lk := make([]byte, MaxKeyLength)
	for i := uint64(0); i < MaxKeyLength; i++ {
		fk[i] = 0
		lk[i] = 0xFF
	}

	return b.CompactEntries(fk, lk)
}

func (b *Badger) RunGC() {
	if b.mDb == nil {
		return
	}

	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
	again:
		if b.mDb == nil {
			return
		}

		err := b.mDb.RunValueLogGC(0.5)
		if err == nil {
			goto again
		}
	}
}

// NewKVStore returns a badger based IKVStore instance.
func NewKVStore(config config.LogDBConfig,
	dir string, wal string, fs vfs.IFS) (kv.IKVStore, error) {
	return openBadgerDb(config, dir, wal, fs)
}
