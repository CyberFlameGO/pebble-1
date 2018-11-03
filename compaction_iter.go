// Copyright 2018 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package pebble

import (
	"fmt"

	"github.com/petermattis/pebble/db"
)

// compactionIter provides a forward-only iterator that encapsulates the logic
// for collapsing entries during compaction. It wraps an internal iterator and
// collapses entries that are no longer necessary because they are shadowed by
// newer entries. The simplest example of this is when the internal iterator
// contains two keys: a.PUT.2 and a.PUT.1. Instead of returning both entries,
// compactionIter collapses the second entry because it is no longer
// necessary. The high-level structure for compactionIter is to iterate over
// its internal iterator and output 1 entry for every user-key. There are three
// complications to this story.
//
// 1. Eliding Deletion Tombstones
//
// Consider the entries a.DEL.2 and a.PUT.1. These entries collapse to
// a.DEL.2. Do we have to output the entry a.DEL.2? Only if a.DEL.2 possibly
// shadows an entry at a lower level. If we're compacting to the base-level in
// the LSM tree then a.DEL.2 is definitely not shadowing an entry at a lower
// level and can be elided.
//
// We can do slightly better than only eliding deletion tombstones at the base
// level by observing that we can elide a deletion tombstone if there are no
// sstables that contain the entry's key. This check is performed by
// elideTombstone.
//
// 2. Merges
//
// The MERGE operation merges the value for an entry with the existing value
// for an entry. The logical value of an entry can be composed of a series of
// merge operations. When compactionIter sees a MERGE, it scans forward in its
// internal iterator collapsing MERGE operations for the same key until it
// encounters a SET or DELETE operation. For example, the keys a.MERGE.4,
// a.MERGE.3, a.MERGE.2 will be collapsed to a.MERGE.4 and the values will be
// merged using the specified db.Merger.
//
// An interesting case here occurs when MERGE is combined with SET. Consider
// the entries a.MERGE.3 and a.SET.2. The collapsed key will be a.SET.3. The
// reason that the kind is changed to SET is because the SET operation acts as
// a barrier preventing further merging. This can be seen better in the
// scenario a.MERGE.3, a.SET.2, a.MERGE.1. The entry a.MERGE.1 may be at lower
// (older) level and not involved in the compaction. If the compaction of
// a.MERGE.3 and a.SET.2 produced a.MERGE.3, a subsequent compaction with
// a.MERGE.1 would merge the values together incorrectly.
//
// 3. Snapshots [TODO(peter): unimplemented]
//
// Snapshots are lightweight point-in-time views of the DB state. At its core,
// a snapshot is a sequence number along with a guarantee from Pebble that it
// will maintain the view of the database at that sequence number. Part of this
// guarantee is relatively straightforward to achieve. When reading from the
// database Pebble will ignore sequence numbers that are larger than the
// snapshot sequence number. The primary complexity with snapshots occurs
// during compaction: the collapsing of entries that are shadowed by newer
// entries is at odds with the guarantee that Pebble will maintain the view of
// the database at the snapshot sequence number. Rather than collapsing entries
// up to the next user key, compactionIter can only collapse entries up to the
// next snapshot boundary. That is, every snapshot boundary potentially causes
// another entry for the same user-key to be emitted. Another way to view this
// is that snapshots define stripes and entries are collapsed within stripes,
// but not across stripes. Consider the following scenario:
//
//   a.PUT.9
//   a.DEL.8
//   a.PUT.7
//   a.DEL.6
//   a.PUT.5
//
// In the absence of snapshots these entries would be collapsed to
// a.PUT.9. What if there is a snapshot at sequence number 6? The entries can
// be divided into two stripes and collapsed within the stripes:
//
//   a.PUT.9        a.PUT.9
//   a.DEL.8  --->
//   a.PUT.7
//   --             --
//   a.DEL.6  --->  a.DEL.6
//   a.PUT.5
//
// All of the rules described earlier still apply, but they are confined to
// operate within a snapshot stripe. Snapshots only affect compaction when the
// snapshot sequence number lies within the range of sequence numbers being
// compacted. In the above example, a snapshot at sequence number 10 or at
// sequence number 5 would not have any effect.
type compactionIter struct {
	cmp            db.Compare
	merge          db.Merge
	iter           db.InternalIterator
	err            error
	key            db.InternalKey
	keyBuf         []byte
	value          []byte
	valueBuf       []byte
	valid          bool
	skip           bool
	elideTombstone func(key []byte) bool
}

func (i *compactionIter) First() {
	if i.err != nil {
		return
	}
	i.iter.First()
	i.Next()
}

func (i *compactionIter) Next() bool {
	if i.err != nil {
		return false
	}

	if i.skip {
		i.skip = false
		// TODO(peter): Rather than calling NextUserKey here, we should advance the
		// iterator manually to the next key looking for any entries which have
		// invalid keys and returning them.
		i.iter.NextUserKey()
	}

	i.valid = false
	for i.iter.Valid() {
		i.key = i.iter.Key()
		switch i.key.Kind() {
		case db.InternalKeyKindDelete:
			if i.elideTombstone(i.key.UserKey) {
				i.iter.NextUserKey()
				continue
			}
			i.value = i.iter.Value()
			i.valid = true
			i.skip = true
			return true

		case db.InternalKeyKindSet:
			i.value = i.iter.Value()
			i.valid = true
			i.skip = true
			return true

		case db.InternalKeyKindMerge:
			return i.mergeNext()

		default:
			i.err = fmt.Errorf("invalid internal key kind: %d", i.key.Kind())
			return false
		}
	}

	return false
}

func (i *compactionIter) mergeNext() bool {
	// Save the current key and value.
	i.keyBuf = append(i.keyBuf[:0], i.iter.Key().UserKey...)
	i.valueBuf = append(i.valueBuf[:0], i.iter.Value()...)
	i.key.UserKey, i.value = i.keyBuf, i.valueBuf
	i.valid = true
	i.skip = true

	// Loop looking for older values for this key and merging them.
	for {
		i.iter.Next()
		if !i.iter.Valid() {
			i.skip = false
			return true
		}
		key := i.iter.Key()
		if i.cmp(i.key.UserKey, key.UserKey) != 0 {
			// We've advanced to the next key.
			i.skip = false
			return true
		}
		switch key.Kind() {
		case db.InternalKeyKindDelete:
			// We've hit a deletion tombstone. Return everything up to this
			// point.
			return true

		case db.InternalKeyKindSet:
			// We've hit a Set value. Merge with the existing value and return. We
			// change the kind of the resulting key to a Set so that it shadows keys
			// in lower levels. That is, MERGE+MERGE+SET -> SET.
			i.value = i.merge(i.key.UserKey, i.value, i.iter.Value(), nil)
			i.key.SetKind(db.InternalKeyKindSet)
			return true

		case db.InternalKeyKindMerge:
			// We've hit another Merge value. Merge with the existing value and
			// continue looping.
			i.value = i.merge(i.key.UserKey, i.value, i.iter.Value(), nil)

		default:
			i.err = fmt.Errorf("invalid internal key kind: %d", i.key.Kind())
			return false
		}
	}
}

func (i *compactionIter) Key() db.InternalKey {
	return i.key
}

func (i *compactionIter) Value() []byte {
	return i.value
}

func (i *compactionIter) Valid() bool {
	return i.valid
}

func (i *compactionIter) Error() error {
	return i.err
}

func (i *compactionIter) Close() error {
	return i.err
}
