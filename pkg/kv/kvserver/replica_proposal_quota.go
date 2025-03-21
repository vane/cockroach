// Copyright 2019 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package kvserver

import (
	"bytes"
	"context"
	"time"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/kv/kvpb"
	"github.com/cockroachdb/cockroach/pkg/raft"
	"github.com/cockroachdb/cockroach/pkg/raft/raftpb"
	"github.com/cockroachdb/cockroach/pkg/raft/tracker"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/settings"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/quotapool"
	"github.com/cockroachdb/errors"
)

var enableRaftProposalQuota = settings.RegisterBoolSetting(
	settings.SystemOnly,
	"kv.raft.proposal_quota.enabled",
	"set to true to enable waiting for and acquiring quota before issuing Raft "+
		"proposals, false to disable",
	true,
)

func (r *Replica) maybeAcquireProposalQuota(
	ctx context.Context, ba *kvpb.BatchRequest, quota uint64,
) (*quotapool.IntAlloc, error) {
	// We don't want to delay lease requests or transfers, in particular
	// expiration lease extensions. These are small and latency-sensitive.
	if ba.IsSingleRequestLeaseRequest() || ba.IsSingleTransferLeaseRequest() {
		return nil, nil
	}

	// If the quota pool is disabled via the setting, we don't need to acquire
	// quota.
	if !enableRaftProposalQuota.Get(&r.store.cfg.Settings.SV) {
		// TODO(kvoli): Once we have a setting for RACv2 pull vs push mode, we
		// should abstract this check into a function that also disables quota
		// acquisition for pull mode.
		return nil, nil
	}

	r.mu.RLock()
	quotaPool := r.mu.proposalQuota
	desc := r.mu.state.Desc
	r.mu.RUnlock()

	// Quota acquisition only takes place on the leader replica,
	// r.mu.proposalQuota is set to nil if a node is a follower (see
	// updateProposalQuotaRaftMuLocked). For the cases where the range lease
	// holder is not the same as the range leader, i.e. the lease holder is a
	// follower, r.mu.proposalQuota == nil. This means all quota acquisitions
	// go through without any throttling whatsoever but given how short lived
	// these scenarios are we don't try to remedy any further.
	//
	// NB: It is necessary to allow proposals with a nil quota pool to go
	// through, for otherwise a follower could never request the lease.

	if quotaPool == nil {
		return nil, nil
	}

	if !quotaPoolEnabledForRange(desc) {
		return nil, nil
	}

	// Trace if we're running low on available proposal quota; it might explain
	// why we're taking so long.
	if log.HasSpan(ctx) {
		if q := quotaPool.ApproximateQuota(); q < quotaPool.Capacity()/10 {
			log.Eventf(ctx, "quota running low, currently available ~%d", q)
		}
	}
	alloc, err := quotaPool.Acquire(ctx, quota)
	// Let quotapool errors due to being closed pass through.
	if errors.HasType(err, (*quotapool.ErrClosed)(nil)) {
		err = nil
	}
	return alloc, err
}

func quotaPoolEnabledForRange(desc *roachpb.RangeDescriptor) bool {
	// The NodeLiveness range does not use a quota pool. We don't want to
	// throttle updates to the NodeLiveness range even if a follower is falling
	// behind because this could result in cascading failures.
	return !bytes.HasPrefix(desc.StartKey, keys.NodeLivenessPrefix)
}

var logSlowRaftProposalQuotaAcquisition = quotapool.OnSlowAcquisition(
	base.SlowRequestThreshold, quotapool.LogSlowAcquisition,
)

func (r *Replica) updateProposalQuotaRaftMuLocked(
	ctx context.Context, lastLeaderID roachpb.ReplicaID,
) {
	now := r.Clock().PhysicalTime()
	r.mu.Lock()
	defer r.mu.Unlock()

	status := r.mu.internalRaftGroup.BasicStatus()
	if r.mu.leaderID != lastLeaderID {
		if r.replicaID == r.mu.leaderID {
			// We're becoming the leader.
			// Initialize the proposalQuotaBaseIndex at the applied index.
			// After the proposal quota is enabled all entries applied by this replica
			// will be appended to the quotaReleaseQueue. The proposalQuotaBaseIndex
			// and the quotaReleaseQueue together track status.Applied exactly.
			r.mu.proposalQuotaBaseIndex = kvpb.RaftIndex(status.Applied)
			if r.mu.proposalQuota != nil {
				log.Fatal(ctx, "proposalQuota was not nil before becoming the leader")
			}
			if releaseQueueLen := len(r.mu.quotaReleaseQueue); releaseQueueLen != 0 {
				log.Fatalf(ctx, "len(r.mu.quotaReleaseQueue) = %d, expected 0", releaseQueueLen)
			}

			// Raft may propose commands itself (specifically the empty
			// commands when leadership changes), and these commands don't go
			// through the code paths where we acquire quota from the pool. To
			// offset this we reset the quota pool whenever leadership changes
			// hands.
			r.mu.proposalQuota = quotapool.NewIntPool(
				"raft proposal",
				uint64(r.store.cfg.RaftProposalQuota),
				logSlowRaftProposalQuotaAcquisition,
			)
			r.mu.lastUpdateTimes = make(map[roachpb.ReplicaID]time.Time)
			r.mu.lastUpdateTimes.updateOnBecomeLeader(r.mu.state.Desc.Replicas().Descriptors(), now)
			r.mu.replicaFlowControlIntegration.onBecameLeader(ctx)
			r.mu.lastProposalAtTicks = r.mu.ticks // delay imminent quiescence
		} else if r.mu.proposalQuota != nil {
			// We're becoming a follower.
			// We unblock all ongoing and subsequent quota acquisition goroutines
			// (if any) and release the quotaReleaseQueue so its allocs are pooled.
			r.mu.proposalQuota.Close("leader change")
			r.mu.proposalQuota.Release(r.mu.quotaReleaseQueue...)
			r.mu.quotaReleaseQueue = nil
			r.mu.proposalQuota = nil
			r.mu.lastUpdateTimes = nil
			r.mu.replicaFlowControlIntegration.onBecameFollower(ctx)
		}
		return
	} else if r.mu.proposalQuota == nil {
		if r.replicaID == r.mu.leaderID {
			log.Fatal(ctx, "leader has uninitialized proposalQuota pool")
		}
		// We're a follower.
		return
	}

	// We're still the leader.
	// Find the minimum index that active followers have acknowledged.

	// commitIndex is used to determine whether a newly added replica has fully
	// caught up.
	commitIndex := kvpb.RaftIndex(status.Commit)
	// Initialize minIndex to the currently applied index. The below progress
	// checks will only decrease the minIndex. Given that the quotaReleaseQueue
	// cannot correspond to values beyond the applied index there's no reason
	// to consider progress beyond it as meaningful.
	minIndex := kvpb.RaftIndex(status.Applied)

	r.mu.internalRaftGroup.WithProgress(func(id raftpb.PeerID, _ raft.ProgressType, progress tracker.Progress) {
		rep, ok := r.mu.state.Desc.GetReplicaDescriptorByID(roachpb.ReplicaID(id))
		if !ok {
			return
		}

		// Only consider followers that are active. Inactive ones don't decrease
		// minIndex - i.e. they don't hold up releasing quota.
		//
		// The policy for determining who's active is stricter than the one used
		// for purposes of quiescing. Failure to consider a dead/stuck node as
		// such for the purposes of releasing quota can have bad consequences
		// (writes will stall), whereas for quiescing the downside is lower.

		if !r.mu.lastUpdateTimes.isFollowerActiveSince(rep.ReplicaID, now, r.store.cfg.RangeLeaseDuration) {
			return
		}
		// At this point, we know that either we communicated with this replica
		// recently, or we became the leader recently. The latter case is ambiguous
		// w.r.t. the actual state of that replica, but it is temporary.

		// Note that the Match field has different semantics depending on
		// the State.
		//
		// In state ProgressStateReplicate, the Match index is optimistically
		// updated whenever a message is *sent* (not received). Due to Raft
		// flow control, only a reasonably small amount of data can be en
		// route to a given follower at any point in time.
		//
		// In state ProgressStateProbe, the Match index equals Next-1, and
		// it tells us the leader's optimistic best guess for the right log
		// index (and will try once per heartbeat interval to update its
		// estimate). In the usual case, the follower responds with a hint
		// when it rejects the first probe and the leader replicates or
		// sends a snapshot. In the case in which the follower does not
		// respond, the leader reduces Match by one each heartbeat interval.
		// But if the follower does not respond, we've already filtered it
		// out above. We use the Match index as is, even though the follower
		// likely isn't there yet because that index won't go up unless the
		// follower is actually catching up, so it won't cause it to fall
		// behind arbitrarily.
		//
		// Another interesting tidbit about this state is that the Paused
		// field is usually true as it is used to limit the number of probes
		// (i.e. appends) sent to this follower to one per heartbeat
		// interval.
		//
		// In state ProgressStateSnapshot, the Match index is the last known
		// (possibly optimistic, depending on previous state) index before
		// the snapshot went out. Once the snapshot applies, the follower
		// will enter ProgressStateReplicate again. So here the Match index
		// works as advertised too.

		// Only consider followers who are in advance of the quota base
		// index. This prevents a follower from coming back online and
		// preventing throughput to the range until it has caught up.
		if kvpb.RaftIndex(progress.Match) < r.mu.proposalQuotaBaseIndex {
			return
		}
		if _, paused := r.mu.pausedFollowers[roachpb.ReplicaID(id)]; paused {
			// We are dropping MsgApp to this store, so we are effectively treating
			// it as non-live for the purpose of replication and are letting it fall
			// behind intentionally.
			//
			// See #79215.
			return
		}
		if progress.Match > 0 && kvpb.RaftIndex(progress.Match) < minIndex {
			minIndex = kvpb.RaftIndex(progress.Match)
		}
		// If this is the most recently added replica, and it has caught up, clear
		// our state that was tracking it. This is unrelated to managing proposal
		// quota, but this is a convenient place to do so.
		if rep.ReplicaID == r.mu.lastReplicaAdded && kvpb.RaftIndex(progress.Match) >= commitIndex {
			r.mu.lastReplicaAdded = 0
			r.mu.lastReplicaAddedTime = time.Time{}
		}
	})

	if r.mu.proposalQuotaBaseIndex < minIndex {
		// We've persisted at least minIndex-r.mu.proposalQuotaBaseIndex entries
		// to the raft log on all 'active' replicas and applied at least minIndex
		// entries locally since last we checked, so we are able to release the
		// difference back to the quota pool.
		numReleases := minIndex - r.mu.proposalQuotaBaseIndex

		// NB: Release deals with cases where allocs being released do not originate
		// from this incarnation of quotaReleaseQueue, which can happen if a
		// proposal acquires quota while this replica is the raft leader in some
		// term and then commits while at a different term.
		r.mu.proposalQuota.Release(r.mu.quotaReleaseQueue[:numReleases]...)
		r.mu.quotaReleaseQueue = r.mu.quotaReleaseQueue[numReleases:]
		r.mu.proposalQuotaBaseIndex += numReleases
	}
	// Assert the sanity of the base index and the queue. Queue entries should
	// correspond to applied entries. It should not be possible for the base
	// index and the not yet released applied entries to not equal the applied
	// index.
	releasableIndex := r.mu.proposalQuotaBaseIndex + kvpb.RaftIndex(len(r.mu.quotaReleaseQueue))
	if releasableIndex != kvpb.RaftIndex(status.Applied) {
		log.Fatalf(ctx, "proposalQuotaBaseIndex (%d) + quotaReleaseQueueLen (%d) = %d"+
			" must equal the applied index (%d)",
			r.mu.proposalQuotaBaseIndex, len(r.mu.quotaReleaseQueue), releasableIndex,
			status.Applied)
	}

	// Tick the replicaFlowControlIntegration interface. This is as convenient a
	// place to do it as any other. Much like the quota pool code above, the
	// flow control integration layer considers raft progress state for
	// individual replicas, and whether they've been recently active.
	r.mu.replicaFlowControlIntegration.onRaftTicked(ctx)
}
