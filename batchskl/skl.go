/*
 * Copyright 2017 Dgraph Labs, Inc. and Contributors
 * Modifications copyright (C) 2017 Andy Kimball and Contributors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

/*
Adapted from RocksDB inline skiplist.

Key differences:
- No optimization for sequential inserts (no "prev").
- No custom comparator.
- Support overwrites. This requires care when we see the same key when inserting.
  For RocksDB or LevelDB, overwrites are implemented as a newer sequence number in the key, so
	there is no need for values. We don't intend to support versioning. In-place updates of values
	would be more efficient.
- We discard all non-concurrent code.
- We do not support Splices. This simplifies the code a lot.
- No AllocateNode or other pointer arithmetic.
- We combine the findLessThan, findGreaterOrEqual, etc into one function.
*/

/*
Further adapted from Badger: https://github.com/dgraph-io/badger.

Key differences:
- Support for previous pointers - doubly linked lists. Note that it's up to higher
  level code to deal with the intermediate state that occurs during insertion,
  where node A is linked to node B, but node B is not yet linked back to node A.
- Iterator includes mutator functions.
*/

/*
Further adapted from arenaskl: https://github.com/andy-kimball/arenaskl

Key differences:
- Removed support for deletion.
- Removed support for concurrency.
- External storage of keys.
- Node storage grows to an arbitrary size.
*/

package batchskl

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"unsafe"
)

const (
	maxHeight     = 20
	maxNodeSize   = int(unsafe.Sizeof(node{}))
	linksSize     = int(unsafe.Sizeof(links{}))
	keyPrefixSize = int(unsafe.Sizeof(KeyPrefix(0)))
)

var ErrRecordExists = errors.New("record with this key already exists")

type links struct {
	next uint32
	prev uint32
}

type KeyPrefix uint64

type node struct {
	// The offset of the key in storage. See Storage.Get.
	key uint32
	// A fixed 8-byte prefix of the key, used to avoid retrieval of the key
	// during seek operations. The key retrieval can be expensive purely due to
	// cache misses while the prefix stored here will be in the same cache line
	// as the key and the links making accessing and comparing against it almost
	// free.
	prefix KeyPrefix
	// Most nodes do not need to use the full height of the link tower, since the
	// probability of each successive level decreases exponentially. Because
	// these elements are never accessed, they do not need to be allocated.
	// Therefore, when a node is allocated, its memory footprint is deliberately
	// truncated to not include unneeded link elements.
	links [maxHeight]links
}

// Storage ...
type Storage interface {
	Get(offset uint32) []byte
	Prefix(key []byte) KeyPrefix
	Compare(a []byte, b uint32) int
}

// Skiplist ...
type Skiplist struct {
	storage Storage
	nodes   []byte
	head    uint32
	tail    uint32
	height  uint32 // Current height: 1 <= height <= maxHeight
}

var (
	probabilities [maxHeight]uint32
)

func init() {
	const pValue = 1 / math.E

	// Precompute the skiplist probabilities so that only a single random number
	// needs to be generated and so that the optimal pvalue can be used (inverse
	// of Euler's number).
	p := float64(1.0)
	for i := 0; i < maxHeight; i++ {
		probabilities[i] = uint32(float64(math.MaxUint32) * p)
		p *= pValue
	}
}

// NewSkiplist constructs and initializes a new, empty skiplist.
func NewSkiplist(storage Storage, initBufSize int) *Skiplist {
	if initBufSize < 256 {
		initBufSize = 256
	}
	s := &Skiplist{
		storage: storage,
		nodes:   make([]byte, 0, initBufSize),
		height:  1,
	}

	// Allocate head and tail nodes.
	s.head = s.newNode(maxHeight, 0, 0)
	s.tail = s.newNode(maxHeight, 0, 0)

	// Link all head/tail levels together.
	for i := uint32(0); i < maxHeight; i++ {
		s.setNext(s.head, i, s.tail)
		s.setPrev(s.tail, i, s.head)
	}

	return s
}

// NewIterator returns a new Iterator object. Note that it is safe for an
// iterator to be copied by value.
func (s *Skiplist) NewIterator() Iterator {
	return Iterator{list: s}
}

func (s *Skiplist) newNode(height, key uint32, prefix KeyPrefix) uint32 {
	if height < 1 || height > maxHeight {
		panic("height cannot be less than one or greater than the max height")
	}

	unusedSize := (maxHeight - int(height)) * linksSize
	offset := s.alloc(uint32(maxNodeSize - unusedSize))
	nd := s.node(offset)

	nd.key = key
	nd.prefix = prefix
	return offset
}

func (s *Skiplist) alloc(size uint32) uint32 {
	offset := uint32(len(s.nodes))
	newSize := offset + size
	if cap(s.nodes) < int(newSize) {
		allocSize := uint32(cap(s.nodes) * 2)
		if allocSize < newSize {
			allocSize = newSize
		}
		tmp := make([]byte, len(s.nodes), allocSize)
		copy(tmp, s.nodes)
		s.nodes = tmp
	}

	s.nodes = s.nodes[:newSize]
	return offset
}

func (s *Skiplist) node(offset uint32) *node {
	return (*node)(unsafe.Pointer(&s.nodes[offset]))
}

func (s *Skiplist) randomHeight() uint32 {
	rnd := rand.Uint32()
	h := uint32(1)
	for h < maxHeight && rnd <= probabilities[h] {
		h++
	}
	return h
}

func (s *Skiplist) findSpliceForLevel(
	key []byte, prefix KeyPrefix, level, start uint32,
) (prev, next uint32, found bool) {
	prev = start

	for {
		// Assume prev.key < key.
		next = s.getNext(prev, level)
		if next == s.tail {
			// Tail node, so done.
			break
		}

		nextPrefix := s.getKeyPrefix(next)
		if prefix < nextPrefix {
			// We are done for this level, since prev.key < key < next.key.
			break
		}
		if prefix == nextPrefix {
			cmp := s.storage.Compare(key, s.getKey(next))
			if cmp == 0 {
				// Equality case.
				found = true
				break
			}
			if cmp < 0 {
				// We are done for this level, since prev.key < key < next.key.
				break
			}
		}

		// Keep moving right on this level.
		prev = next
	}

	return
}

func (s *Skiplist) getKey(nd uint32) uint32 {
	return s.node(nd).key
}

func (s *Skiplist) getKeyPrefix(nd uint32) KeyPrefix {
	return s.node(nd).prefix
}

func (s *Skiplist) getNext(nd, h uint32) uint32 {
	return s.node(nd).links[h].next
}

func (s *Skiplist) getPrev(nd, h uint32) uint32 {
	return s.node(nd).links[h].prev
}

func (s *Skiplist) setNext(nd, h, next uint32) {
	s.node(nd).links[h].next = next
}

func (s *Skiplist) setPrev(nd, h, prev uint32) {
	s.node(nd).links[h].prev = prev
}

func (s *Skiplist) debug() string {
	var buf bytes.Buffer
	for level := uint32(0); level < s.height; level++ {
		var count int
		for nd := s.head; nd != s.tail; nd = s.getNext(nd, level) {
			count++
		}
		fmt.Fprintf(&buf, "%d: %d\n", level, count)
	}
	return buf.String()
}
