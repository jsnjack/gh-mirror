package store

import (
	"encoding/binary"
	"math"
	"sort"
)

// mutualCandidateLimit bounds the semantic candidates checked for mutual similarity.
const mutualCandidateLimit = 100

// neighbourFields lists the ticket fields averaged into candidate ticket vectors,
// with the same weights as seed vectors built without comments.
var neighbourFields = map[string]int{"title": 0, "body": 1, "labels": 2}
var neighbourWeights = [3]float32{4, 1, 1.5}

// seedNeighbours accumulates ticket vectors during a seed scan. Duplicates tend
// to rank each other highly, while broadly similar tickets rank many others first.
type seedNeighbours struct {
	sums   map[TicketID]*[3][]float32
	counts map[TicketID]*[3]int
}

func newSeedNeighbours() *seedNeighbours {
	return &seedNeighbours{sums: map[TicketID]*[3][]float32{}, counts: map[TicketID]*[3]int{}}
}

// add sums one validated stored vector into its ticket's field average.
func (n *seedNeighbours) add(id TicketID, field string, raw []byte) {
	f, ok := neighbourFields[field]
	if !ok {
		return
	}
	if n.sums[id] == nil {
		n.sums[id] = &[3][]float32{}
		n.counts[id] = &[3]int{}
	}
	sum := &n.sums[id][f]
	if *sum == nil {
		*sum = make([]float32, len(raw)/4)
	}
	for i := range *sum {
		(*sum)[i] += math.Float32frombits(binary.LittleEndian.Uint32(raw[i*4:]))
	}
	n.counts[id][f]++
}

// vectors returns normalized weighted ticket vectors for tickets with a title or body.
func (n *seedNeighbours) vectors() (map[TicketID][]float32, []TicketID) {
	out := map[TicketID][]float32{}
	ids := []TicketID{}
	for id, sums := range n.sums {
		counts := n.counts[id]
		if counts[0] == 0 && counts[1] == 0 {
			continue
		}
		var v []float32
		for f, sum := range sums {
			if counts[f] == 0 {
				continue
			}
			if v == nil {
				v = make([]float32, len(sum))
			}
			for i, x := range sum {
				v[i] += x * neighbourWeights[f] / float32(counts[f])
			}
		}
		if v, err := normalizeVector(v); err == nil {
			out[id] = v
			ids = append(ids, id)
		}
	}
	return out, ids
}

// mutualOrder sorts candidates by the seed's rank among each candidate's own
// neighbours, keeping the semantic order for ties and tickets without vectors.
func (n *seedNeighbours) mutualOrder(seed []float32, candidates []TicketID) []TicketID {
	vectors, ids := n.vectors()
	ranks := make(map[TicketID]int, len(candidates))
	for _, c := range candidates {
		vc, ok := vectors[c]
		if !ok {
			ranks[c] = len(ids) + 1
			continue
		}
		threshold := dot32(vc, seed)
		rank := 1
		for _, t := range ids {
			if t != c && dot32(vc, vectors[t]) > threshold {
				rank++
			}
		}
		ranks[c] = rank
	}
	out := append([]TicketID{}, candidates...)
	sort.SliceStable(out, func(a, b int) bool { return ranks[out[a]] < ranks[out[b]] })
	return out
}

func dot32(a, b []float32) float32 {
	var sum float32
	for i, x := range a {
		sum += x * b[i]
	}
	return sum
}
