// Copyright 2014, Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package actionnode

// This file contains utility functions to be used with actionnode /
// topology server.

import (
	"fmt"
	"sync"
	"time"

	log "github.com/golang/glog"
	"github.com/youtube/vitess/go/vt/concurrency"
	"github.com/youtube/vitess/go/vt/hook"
	"github.com/youtube/vitess/go/vt/key"
	"github.com/youtube/vitess/go/vt/topo"
)

var (
	// DefaultLockTimeout is a good value to use as a default for
	// locking a shard / keyspace.
	DefaultLockTimeout = 30 * time.Second
)

// LockKeyspace will lock the keyspace in the topology server.
// UnlockKeyspace should be called if this returns no error.
func (n *ActionNode) LockKeyspace(ts topo.Server, keyspace string, lockTimeout time.Duration, interrupted chan struct{}) (lockPath string, err error) {
	log.Infof("Locking keyspace %v for action %v", keyspace, n.Action)
	return ts.LockKeyspaceForAction(keyspace, n.ToJson(), lockTimeout, interrupted)
}

// UnlockKeyspace unlocks a previously locked keyspace.
func (n *ActionNode) UnlockKeyspace(ts topo.Server, keyspace string, lockPath string, actionError error) error {
	// first update the actionNode
	if actionError != nil {
		log.Infof("Unlocking keyspace %v for action %v with error %v", keyspace, n.Action, actionError)
		n.Error = actionError.Error()
		n.State = ACTION_STATE_FAILED
	} else {
		log.Infof("Unlocking keyspace %v for successful action %v", keyspace, n.Action)
		n.Error = ""
		n.State = ACTION_STATE_DONE
	}
	err := ts.UnlockKeyspaceForAction(keyspace, lockPath, n.ToJson())
	if actionError != nil {
		if err != nil {
			// this will be masked
			log.Warningf("UnlockKeyspaceForAction failed: %v", err)
		}
		return actionError
	}
	return err
}

// LockShard will lock the shard in the topology server.
// UnlockShard should be called if this returns no error.
func (n *ActionNode) LockShard(ts topo.Server, keyspace, shard string, lockTimeout time.Duration, interrupted chan struct{}) (lockPath string, err error) {
	log.Infof("Locking shard %v/%v for action %v", keyspace, shard, n.Action)
	return ts.LockShardForAction(keyspace, shard, n.ToJson(), lockTimeout, interrupted)
}

// UnlockShard unlocks a previously locked shard.
func (n *ActionNode) UnlockShard(ts topo.Server, keyspace, shard string, lockPath string, actionError error) error {
	// first update the actionNode
	if actionError != nil {
		log.Infof("Unlocking shard %v/%v for action %v with error %v", keyspace, shard, n.Action, actionError)
		n.Error = actionError.Error()
		n.State = ACTION_STATE_FAILED
	} else {
		log.Infof("Unlocking shard %v/%v for successful action %v", keyspace, shard, n.Action)
		n.Error = ""
		n.State = ACTION_STATE_DONE
	}
	err := ts.UnlockShardForAction(keyspace, shard, lockPath, n.ToJson())
	if actionError != nil {
		if err != nil {
			// this will be masked
			log.Warningf("UnlockShardForAction failed: %v", err)
		}
		return actionError
	}
	return err
}

// ConfigureTabletHook configures the right parameters for a hook
// running locally on a tablet.
func ConfigureTabletHook(hk *hook.Hook, tabletAlias topo.TabletAlias) {
	if hk.ExtraEnv == nil {
		hk.ExtraEnv = make(map[string]string, 1)
	}
	hk.ExtraEnv["TABLET_ALIAS"] = tabletAlias.String()
}

// Scrap will update the tablet type to 'Scrap', and remove it from
// the serving graph.
//
// 'force' means we are not on the tablet being scrapped, so it is
// probably dead. So if 'force' is true, we will also remove pending
// remote actions.  And if 'force' is false, we also run an optional
// hook.
func Scrap(ts topo.Server, tabletAlias topo.TabletAlias, force bool) error {
	tablet, err := ts.GetTablet(tabletAlias)
	if err != nil {
		return err
	}

	// If you are already scrap, skip updating replication data. It won't
	// be there anyway.
	wasAssigned := tablet.IsAssigned()
	tablet.Type = topo.TYPE_SCRAP
	tablet.Parent = topo.TabletAlias{}
	// Update the tablet first, since that is canonical.
	err = topo.UpdateTablet(ts, tablet)
	if err != nil {
		return err
	}

	// Remove any pending actions. Presumably forcing a scrap
	// means you don't want the agent doing anything and the
	// machine requires manual attention.
	if force {
		err := ts.PurgeTabletActions(tabletAlias, ActionNodeCanBePurged)
		if err != nil {
			log.Warningf("purge actions failed: %v", err)
		}
	}

	if wasAssigned {
		err = topo.DeleteTabletReplicationData(ts, tablet.Tablet)
		if err != nil {
			if err == topo.ErrNoNode {
				log.V(6).Infof("no ShardReplication object for cell %v", tablet.Alias.Cell)
				err = nil
			}
			if err != nil {
				log.Warningf("remove replication data for %v failed: %v", tablet.Alias, err)
			}
		}
	}

	// run a hook for final cleanup, only in non-force mode.
	// (force mode executes on the vtctl side, not on the vttablet side)
	if !force {
		hk := hook.NewSimpleHook("postflight_scrap")
		ConfigureTabletHook(hk, tablet.Alias)
		if hookErr := hk.ExecuteOptional(); hookErr != nil {
			// we don't want to return an error, the server
			// is already in bad shape probably.
			log.Warningf("Scrap: postflight_scrap failed: %v", hookErr)
		}
	}

	return nil
}

// ChangeType changes the type of the tablet and possibly also updates
// the health informaton for it. Make this external, since these
// transitions need to be forced from time to time.
//
// - if health is nil, we don't touch the Tablet's Health record.
// - if health is an empty map, we clear the Tablet's Health record.
// - if health has values, we overwrite the Tablet's Health record.
func ChangeType(ts topo.Server, tabletAlias topo.TabletAlias, newType topo.TabletType, health map[string]string, runHooks bool) error {
	tablet, err := ts.GetTablet(tabletAlias)
	if err != nil {
		return err
	}

	if !topo.IsTrivialTypeChange(tablet.Type, newType) || !topo.IsValidTypeChange(tablet.Type, newType) {
		return fmt.Errorf("cannot change tablet type %v -> %v %v", tablet.Type, newType, tabletAlias)
	}

	if runHooks {
		// Only run the preflight_serving_type hook when
		// transitioning from non-serving to serving.
		if !topo.IsInServingGraph(tablet.Type) && topo.IsInServingGraph(newType) {
			if err := hook.NewSimpleHook("preflight_serving_type").ExecuteOptional(); err != nil {
				return err
			}
		}
	}

	tablet.Type = newType
	if newType == topo.TYPE_IDLE {
		if tablet.Parent.IsZero() {
			si, err := ts.GetShard(tablet.Keyspace, tablet.Shard)
			if err != nil {
				return err
			}
			rec := concurrency.AllErrorRecorder{}
			wg := sync.WaitGroup{}
			for _, cell := range si.Cells {
				wg.Add(1)
				go func(cell string) {
					defer wg.Done()
					sri, err := ts.GetShardReplication(cell, tablet.Keyspace, tablet.Shard)
					if err != nil {
						log.Warningf("Cannot check cell %v for extra replication paths, assuming it's good", cell)
						return
					}
					for _, rl := range sri.ReplicationLinks {
						if rl.Parent == tabletAlias {
							rec.RecordError(fmt.Errorf("Still have a ReplicationLink in cell %v", cell))
						}
					}
				}(cell)
			}
			wg.Wait()
			if rec.HasErrors() {
				return rec.Error()
			}
		}
		tablet.Parent = topo.TabletAlias{}
		tablet.Keyspace = ""
		tablet.Shard = ""
		tablet.KeyRange = key.KeyRange{}
		tablet.Health = health
	}
	if health != nil {
		if len(health) == 0 {
			tablet.Health = nil
		} else {
			tablet.Health = health
		}
	}
	return topo.UpdateTablet(ts, tablet)
}
