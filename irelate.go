// Streaming relation (overlap, distance, KNN) testing of (any number of) sorted files of intervals.
package irelate

import (
	"container/heap"
)

// Relatable provides all the methods for irelate to function.
// See Interval in interval.go for a class that satisfies this interface.
// Related() likely returns and AddRelated() likely appends to a slice of
// relatables. Note that for performance reasons, Relatable should be implemented
// as a pointer to your data-structure (see Interval).
type Relatable interface {
	Chrom() string
	Start() uint32
	End() uint32
	Related() []Relatable // A slice of related Relatable's filled by IRelate
	AddRelated(Relatable) // Adds to the slice of relatables
	Source() uint32       // Internally marks the source (file/stream) of the Relatable
	SetSource(source uint32)
	Less(other Relatable) bool // Determines order of the relatables (chrom, start)
}

// RelatableChannel
type RelatableChannel chan Relatable

func relate(a Relatable, b Relatable, includeSameSourceRelations bool, relativeTo int) {
	if (a.Source() != b.Source()) || includeSameSourceRelations {
		if relativeTo == -1 {
			a.AddRelated(b)
			b.AddRelated(a)
		} else {
			if uint32(relativeTo) == a.Source() {
				a.AddRelated(b)
			}
			if uint32(relativeTo) == b.Source() {
				b.AddRelated(a)
			}
		}
	}
}

// CheckRelatedByOverlap returns true if Relatables overlap.
func CheckRelatedByOverlap(a Relatable, b Relatable) bool {
	return (b.Start() < a.End()) && (b.Chrom() == a.Chrom())
	// note with distance == 0 this just overlap.
	//distance := uint32(0)
	//return (b.Start()-distance < a.End()) && (b.Chrom() == a.Chrom())
}

// CheckKNN relates an interval to its k-nearest neighbors.
// The reporting function will have to do some filtering since this is only
// guaranteed to associate *at least* k neighbors, but it could be returning extra.
func CheckKNN(a Relatable, b Relatable) bool {
	// the first n checked would be the n_closest, but need to consider ties
	// the report function can decide what to do with them.
	k := 4
	r := a.Related()
	if len(r) >= k {
		// TODO: double-check this.
		return r[len(r)-1].Start()-a.End() < b.Start()-a.End()
	}
	return true
}

// filter rewrites the input-slice to remove nils.
func filter(s []Relatable, nils int) []Relatable {
	if len(s) == nils {
		return s[:0]
	}

	j := 0
	for _, v := range s {
		if v != nil {
			s[j] = v
			j++
		}
	}
	return s[:j]
}

// Send the relatables to the channel in sorted order.
// Check that we couldn't later get an item with a lower start from the current cache.
func sendSortedRelatables(sendQ *relatableQueue, cache []Relatable, out chan Relatable) {
	var j int
	for j = 0; j < len(*sendQ) && (len(cache) == 0 || (*sendQ)[j].(Relatable).Less(cache[0])); j++ {
	}
	for i := 0; i < j; i++ {
		out <- heap.Pop(sendQ).(Relatable)
	}
}

// IRelate provides the basis for flexible overlap/proximity/k-nearest neighbor
// testing. IRelate receives merged, ordered Relatables via stream and takes
// function that checks if they are related (see CheckRelatedByOverlap).
// It is guaranteed that !b.Less(a) is true (we can't guarantee that a.Less(b)
// is true since they may have the same start). Once checkRelated returns false,
// it is assumed that no other `b` Relatables could possibly be related to `a`
// and so `a` is sent to the returnQ. It is likely that includeSameSourceRelations
// will only be set to true if one is doing something like a merge.
// streams are a variable number of channels that send intervals.
func IRelate(checkRelated func(a Relatable, b Relatable) bool,
	includeSameSourceRelations bool,
	relativeTo int,
	streams ...RelatableChannel) chan Relatable {

	stream := Merge(streams...)
	out := make(chan Relatable, 64)
	go func() {

		// use the cache to keep relatables to test against.
		cache := make([]Relatable, 1, 1024)
		cache[0] = <-stream

		// Use sendQ to make sure we output in sorted order.
		// We know we can print something when sendQ.minStart < cache.minStart
		sendQ := make(relatableQueue, 0, 1024)
		nils := 0

		// TODO:if we know the ends are sorted (in addition to start) then we have some additional
		// optimizations. As soon as checkRelated is false, then all others in the cache before that
		// should be true... binary search if endSorted and len(cache) > 20?
		//endSorted := true
		for interval := range stream {

			for i, c := range cache {
				// tried using futures for checkRelated to parallelize... got slower
				if c == nil {
					continue
				}
				if checkRelated(c, interval) {
					relate(c, interval, includeSameSourceRelations, relativeTo)
				} else {
					if relativeTo == -1 || c.Source() == uint32(relativeTo) {
						heap.Push(&sendQ, c)
					}
					cache[i] = nil
					nils++
				}
			}

			// only do this when we have a lot of nils as it's expensive to create a new slice.
			if nils > 1 {
				// remove nils from the cache (must do this before sending)
				cache, nils = filter(cache, nils), 0
				// send the elements from cache in order.
				// use heuristic to minimize the sending.
				if len(sendQ) > 12 {
					sendSortedRelatables(&sendQ, cache, out)
				}
			}
			cache = append(cache, interval)

		}
		for _, c := range filter(cache, nils) {
			if c.Source() == uint32(relativeTo) || relativeTo == -1 {
				heap.Push(&sendQ, c)
			}
		}
		for i := 0; i < len(sendQ); i++ {
			out <- sendQ[i]
		}
		close(out)
	}()
	return out
}

// Merge accepts channels of Relatables and merges them in order.
// Streams of Relatable's from different source must be merged to send
// to IRelate.
// This uses a priority queue and acts like python's heapq.merge.
func Merge(streams ...RelatableChannel) RelatableChannel {
	q := make(relatableQueue, 0, len(streams))
	for i, stream := range streams {
		interval := <-stream
		if interval != nil {
			interval.SetSource(uint32(i))
			heap.Push(&q, interval)
		}
	}
	ch := make(chan Relatable, 8)
	go func() {
		var interval Relatable
		for len(q) > 0 {
			interval = heap.Pop(&q).(Relatable)
			source := interval.Source()
			ch <- interval
			// pull the next interval from the same source.
			next_interval, ok := <-streams[source]
			if ok {
				next_interval.SetSource(source)
				heap.Push(&q, next_interval)
			}
		}
		close(ch)
	}()
	return ch
}
