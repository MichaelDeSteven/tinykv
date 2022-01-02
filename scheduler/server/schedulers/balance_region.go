// Copyright 2017 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package schedulers

import (
	"sort"

	"github.com/pingcap-incubator/tinykv/scheduler/server/core"
	"github.com/pingcap-incubator/tinykv/scheduler/server/schedule"
	"github.com/pingcap-incubator/tinykv/scheduler/server/schedule/operator"
	"github.com/pingcap-incubator/tinykv/scheduler/server/schedule/opt"
)

func init() {
	schedule.RegisterSliceDecoderBuilder("balance-region", func(args []string) schedule.ConfigDecoder {
		return func(v interface{}) error {
			return nil
		}
	})
	schedule.RegisterScheduler("balance-region", func(opController *schedule.OperatorController, storage *core.Storage, decoder schedule.ConfigDecoder) (schedule.Scheduler, error) {
		return newBalanceRegionScheduler(opController), nil
	})
}

const (
	// balanceRegionRetryLimit is the limit to retry schedule for selected store.
	balanceRegionRetryLimit = 10
	balanceRegionName       = "balance-region-scheduler"
)

type balanceRegionScheduler struct {
	*baseScheduler
	name         string
	opController *schedule.OperatorController
}

// newBalanceRegionScheduler creates a scheduler that tends to keep regions on
// each store balanced.
func newBalanceRegionScheduler(opController *schedule.OperatorController, opts ...BalanceRegionCreateOption) schedule.Scheduler {
	base := newBaseScheduler(opController)
	s := &balanceRegionScheduler{
		baseScheduler: base,
		opController:  opController,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// BalanceRegionCreateOption is used to create a scheduler with an option.
type BalanceRegionCreateOption func(s *balanceRegionScheduler)

func (s *balanceRegionScheduler) GetName() string {
	if s.name != "" {
		return s.name
	}
	return balanceRegionName
}

func (s *balanceRegionScheduler) GetType() string {
	return "balance-region"
}

func (s *balanceRegionScheduler) IsScheduleAllowed(cluster opt.Cluster) bool {
	return s.opController.OperatorCount(operator.OpRegion) < cluster.GetRegionScheduleLimit()
}

func (s *balanceRegionScheduler) Schedule(cluster opt.Cluster) *operator.Operator {
	// Your Code Here (3C).
	ss := make([]*core.StoreInfo, 0)
	for _, s := range cluster.GetStores() {
		if s.IsUp() && s.DownTime() <= cluster.GetMaxStoreDownTime() {
			ss = append(ss, s)
		}
	}
	sort.Slice(ss, func(i, j int) bool {
		return ss[i].GetRegionSize() > ss[j].GetRegionSize()
	})
	var region *core.RegionInfo
	var store, targetStore *core.StoreInfo
	for _, s := range ss {
		cluster.GetPendingRegionsWithLock(s.GetID(), func(rc core.RegionsContainer) {
			region = rc.RandomRegion(nil, nil)
		})
		if region != nil {
			store = s
			break
		}
		cluster.GetFollowersWithLock(s.GetID(), func(rc core.RegionsContainer) {
			region = rc.RandomRegion(nil, nil)
		})
		if region != nil {
			store = s
			break
		}
		cluster.GetLeadersWithLock(s.GetID(), func(rc core.RegionsContainer) {
			region = rc.RandomRegion(nil, nil)
		})
		if region != nil {
			store = s
			break
		}
	}
	if region == nil || len(region.GetStoreIds()) < cluster.GetMaxReplicas() {
		return nil
	}
	for i := len(ss) - 1; i >= 0; i-- {
		if _, ok := region.GetStoreIds()[ss[i].GetID()]; !ok {
			targetStore = ss[i]
			break
		}
	}
	if targetStore == nil {
		return nil
	}
	if store.GetRegionSize()-targetStore.GetRegionSize() < 2*region.GetApproximateSize() {
		return nil
	}
	p, err := cluster.AllocPeer(targetStore.GetID())
	if err != nil {
		return nil
	}
	op, err := operator.CreateMovePeerOperator("", cluster, region, operator.OpBalance, store.GetID(), targetStore.GetID(), p.GetId())
	if err != nil {
		return nil
	}
	return op
}
