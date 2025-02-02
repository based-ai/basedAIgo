// Copyright (C) 2019-2023, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package p2p

import (
	"math"
	"math/rand"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"go.uber.org/zap"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/utils"
	"github.com/ava-labs/avalanchego/utils/heap"
	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/avalanchego/utils/set"
	"github.com/ava-labs/avalanchego/version"

	safemath "github.com/ava-labs/avalanchego/utils/math"
)

const (
	bandwidthHalflife = 5 * time.Minute

	// controls how eagerly we connect to new peers vs. using
	// peers with known good response bandwidth.
	desiredMinResponsivePeers = 20
	newPeerConnectFactor      = 0.1

	// The probability that, when we select a peer, we select randomly rather
	// than based on their performance.
	randomPeerProbability = 0.2
)

// information we track on a given peer
type peerInfo struct {
	version   *version.Application
	bandwidth safemath.Averager
}

// Tracks the bandwidth of responses coming from peers,
// preferring to contact peers with known good bandwidth, connecting
// to new peers with an exponentially decaying probability.
type PeerTracker struct {
	// Lock to protect concurrent access to the peer tracker
	lock sync.Mutex
	// All peers we are connected to
	peers map[ids.NodeID]*peerInfo
	// Peers that we're connected to that we've sent a request to
	// since we most recently connected to them.
	trackedPeers set.Set[ids.NodeID]
	// Peers that we're connected to that responded to the last request they were sent.
	responsivePeers set.Set[ids.NodeID]
	// Max heap that contains the average bandwidth of peers.
	bandwidthHeap          heap.Map[ids.NodeID, safemath.Averager]
	averageBandwidth       safemath.Averager
	log                    logging.Logger
	numTrackedPeers        prometheus.Gauge
	numResponsivePeers     prometheus.Gauge
	averageBandwidthMetric prometheus.Gauge
}

func NewPeerTracker(
	log logging.Logger,
	metricsNamespace string,
	registerer prometheus.Registerer,
) (*PeerTracker, error) {
	t := &PeerTracker{
		peers:           make(map[ids.NodeID]*peerInfo),
		trackedPeers:    make(set.Set[ids.NodeID]),
		responsivePeers: make(set.Set[ids.NodeID]),
		bandwidthHeap: heap.NewMap[ids.NodeID, safemath.Averager](func(a, b safemath.Averager) bool {
			return a.Read() > b.Read()
		}),
		averageBandwidth: safemath.NewAverager(0, bandwidthHalflife, time.Now()),
		log:              log,
		numTrackedPeers: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Namespace: metricsNamespace,
				Name:      "num_tracked_peers",
				Help:      "number of tracked peers",
			},
		),
		numResponsivePeers: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Namespace: metricsNamespace,
				Name:      "num_responsive_peers",
				Help:      "number of responsive peers",
			},
		),
		averageBandwidthMetric: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Namespace: metricsNamespace,
				Name:      "average_bandwidth",
				Help:      "average sync bandwidth used by peers",
			},
		),
	}

	err := utils.Err(
		registerer.Register(t.numTrackedPeers),
		registerer.Register(t.numResponsivePeers),
		registerer.Register(t.averageBandwidthMetric),
	)
	return t, err
}

// Returns true if we're not connected to enough peers.
// Otherwise returns true probabilistically based on the number of tracked peers.
// Assumes p.lock is held.
func (p *PeerTracker) shouldTrackNewPeer() bool {
	numResponsivePeers := p.responsivePeers.Len()
	if numResponsivePeers < desiredMinResponsivePeers {
		return true
	}
	if len(p.trackedPeers) >= len(p.peers) {
		// already tracking all the peers
		return false
	}
	// TODO danlaine: we should consider tuning this probability function.
	// With [newPeerConnectFactor] as 0.1 the probabilities are:
	//
	// numResponsivePeers | probability
	// 100                | 4.5399929762484854e-05
	// 200                | 2.061153622438558e-09
	// 500                | 1.9287498479639178e-22
	// 1000               | 3.720075976020836e-44
	// 2000               | 1.3838965267367376e-87
	// 5000               | 7.124576406741286e-218
	//
	// In other words, the probability drops off extremely quickly.
	newPeerProbability := math.Exp(-float64(numResponsivePeers) * newPeerConnectFactor)
	return rand.Float64() < newPeerProbability // #nosec G404
}

// TODO get rid of minVersion
// Returns a peer that we're connected to.
// If we should track more peers, returns a random peer with version >= [minVersion], if any exist.
// Otherwise, with probability [randomPeerProbability] returns a random peer from [p.responsivePeers].
// With probability [1-randomPeerProbability] returns the peer in [p.bandwidthHeap] with the highest bandwidth.
func (p *PeerTracker) GetAnyPeer(minVersion *version.Application) (ids.NodeID, bool) {
	p.lock.Lock()
	defer p.lock.Unlock()

	if p.shouldTrackNewPeer() {
		for nodeID := range p.peers {
			// if minVersion is specified and peer's version is less, skip
			if minVersion != nil && p.peers[nodeID].version.Compare(minVersion) < 0 {
				continue
			}
			// skip peers already tracked
			if p.trackedPeers.Contains(nodeID) {
				continue
			}
			p.log.Debug(
				"tracking peer",
				zap.Int("trackedPeers", len(p.trackedPeers)),
				zap.Stringer("nodeID", nodeID),
			)
			return nodeID, true
		}
	}

	var (
		nodeID ids.NodeID
		ok     bool
	)
	useRand := rand.Float64() < randomPeerProbability // #nosec G404
	if useRand {
		nodeID, ok = p.responsivePeers.Peek()
	} else {
		nodeID, _, ok = p.bandwidthHeap.Pop()
	}
	if !ok {
		// if no nodes found in the bandwidth heap, return a tracked node at random
		return p.trackedPeers.Peek()
	}
	p.log.Debug(
		"peer tracking: popping peer",
		zap.Stringer("nodeID", nodeID),
		zap.Bool("random", useRand),
	)
	return nodeID, true
}

// Record that we sent a request to [nodeID].
func (p *PeerTracker) TrackPeer(nodeID ids.NodeID) {
	p.lock.Lock()
	defer p.lock.Unlock()

	p.trackedPeers.Add(nodeID)
	p.numTrackedPeers.Set(float64(p.trackedPeers.Len()))
}

// Record that we observed that [nodeID]'s bandwidth is [bandwidth].
// Adds the peer's bandwidth averager to the bandwidth heap.
func (p *PeerTracker) TrackBandwidth(nodeID ids.NodeID, bandwidth float64) {
	p.lock.Lock()
	defer p.lock.Unlock()

	peer := p.peers[nodeID]
	if peer == nil {
		// we're not connected to this peer, nothing to do here
		p.log.Debug("tracking bandwidth for untracked peer", zap.Stringer("nodeID", nodeID))
		return
	}

	now := time.Now()
	if peer.bandwidth == nil {
		peer.bandwidth = safemath.NewAverager(bandwidth, bandwidthHalflife, now)
	} else {
		peer.bandwidth.Observe(bandwidth, now)
	}
	p.bandwidthHeap.Push(nodeID, peer.bandwidth)

	if bandwidth == 0 {
		p.responsivePeers.Remove(nodeID)
	} else {
		p.responsivePeers.Add(nodeID)
		// TODO danlaine: shouldn't we add the observation of 0
		// to the average bandwidth in the if statement?
		p.averageBandwidth.Observe(bandwidth, now)
		p.averageBandwidthMetric.Set(p.averageBandwidth.Read())
	}
	p.numResponsivePeers.Set(float64(p.responsivePeers.Len()))
}

// Connected should be called when [nodeID] connects to this node
func (p *PeerTracker) Connected(nodeID ids.NodeID, nodeVersion *version.Application) {
	p.lock.Lock()
	defer p.lock.Unlock()

	peer := p.peers[nodeID]
	if peer == nil {
		p.peers[nodeID] = &peerInfo{
			version: nodeVersion,
		}
		return
	}

	// Peer is already connected, update the version if it has changed.
	// Log a warning message since the consensus engine should never call Connected on a peer
	// that we have already marked as Connected.
	if nodeVersion.Compare(peer.version) != 0 {
		p.peers[nodeID] = &peerInfo{
			version:   nodeVersion,
			bandwidth: peer.bandwidth,
		}
		p.log.Warn(
			"updating node version of already connected peer",
			zap.Stringer("nodeID", nodeID),
			zap.Stringer("storedVersion", peer.version),
			zap.Stringer("nodeVersion", nodeVersion),
		)
	} else {
		p.log.Warn(
			"ignoring peer connected event for already connected peer with identical version",
			zap.Stringer("nodeID", nodeID),
		)
	}
}

// Disconnected should be called when [nodeID] disconnects from this node
func (p *PeerTracker) Disconnected(nodeID ids.NodeID) {
	p.lock.Lock()
	defer p.lock.Unlock()

	p.bandwidthHeap.Remove(nodeID)
	p.trackedPeers.Remove(nodeID)
	p.numTrackedPeers.Set(float64(p.trackedPeers.Len()))
	p.responsivePeers.Remove(nodeID)
	p.numResponsivePeers.Set(float64(p.responsivePeers.Len()))
	delete(p.peers, nodeID)
}

// Returns the number of peers the node is connected to.
func (p *PeerTracker) Size() int {
	p.lock.Lock()
	defer p.lock.Unlock()

	return len(p.peers)
}
