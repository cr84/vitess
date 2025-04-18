/*
Copyright 2021 The Vitess Authors.

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

package reparentutil

import (
	"context"
	"sync"
	"time"

	"vitess.io/vitess/go/event"
	"vitess.io/vitess/go/mysql/replication"
	"vitess.io/vitess/go/mysql/sqlerror"
	"vitess.io/vitess/go/sets"
	"vitess.io/vitess/go/vt/concurrency"
	"vitess.io/vitess/go/vt/log"
	"vitess.io/vitess/go/vt/logutil"
	replicationdatapb "vitess.io/vitess/go/vt/proto/replicationdata"
	topodatapb "vitess.io/vitess/go/vt/proto/topodata"
	"vitess.io/vitess/go/vt/proto/vtrpc"
	"vitess.io/vitess/go/vt/topo"
	"vitess.io/vitess/go/vt/topo/topoproto"
	"vitess.io/vitess/go/vt/topotools"
	"vitess.io/vitess/go/vt/topotools/events"
	"vitess.io/vitess/go/vt/vtctl/reparentutil/policy"
	"vitess.io/vitess/go/vt/vterrors"
	"vitess.io/vitess/go/vt/vttablet/tmclient"
)

// FindPositionsOfAllCandidates will find candidates for an emergency
// reparent, and, if successful, return a mapping of those tablet aliases (as
// raw strings) to their replication positions for later comparison.
func FindPositionsOfAllCandidates(
	statusMap map[string]*replicationdatapb.StopReplicationStatus,
	primaryStatusMap map[string]*replicationdatapb.PrimaryStatus,
) (map[string]replication.Position, bool, error) {
	replicationStatusMap := make(map[string]*replication.ReplicationStatus, len(statusMap))
	positionMap := make(map[string]replication.Position)

	// Build out replication status list from proto types.
	for alias, statuspb := range statusMap {
		status := replication.ProtoToReplicationStatus(statuspb.After)
		replicationStatusMap[alias] = &status
	}

	// Determine if we're GTID-based. If we are, we'll need to look for errant
	// GTIDs below.
	var (
		isGTIDBased                bool
		isNonGTIDBased             bool
		emptyRelayPosErrorRecorder concurrency.FirstErrorRecorder
	)

	for alias, status := range replicationStatusMap {
		if _, ok := status.RelayLogPosition.GTIDSet.(replication.Mysql56GTIDSet); ok {
			isGTIDBased = true
		} else {
			isNonGTIDBased = true
		}

		if status.RelayLogPosition.IsZero() {
			// Potentially bail. If any other tablet is detected to have
			// GTID-based relay log positions, we will return the error recorded
			// here.
			emptyRelayPosErrorRecorder.RecordError(vterrors.Errorf(vtrpc.Code_UNAVAILABLE, "encountered tablet %v with no relay log position, when at least one other tablet in the status map has GTID based relay log positions", alias))
		}
	}

	if isGTIDBased && emptyRelayPosErrorRecorder.HasErrors() {
		return nil, false, emptyRelayPosErrorRecorder.Error()
	}

	if isGTIDBased && isNonGTIDBased {
		return nil, false, vterrors.Errorf(vtrpc.Code_FAILED_PRECONDITION, "encountered mix of GTID-based and non GTID-based relay logs")
	}

	// Store the final positions in the map.
	for alias, status := range replicationStatusMap {
		if !isGTIDBased {
			positionMap[alias] = status.Position

			continue
		}
		positionMap[alias] = status.RelayLogPosition
	}

	for alias, primaryStatus := range primaryStatusMap {
		executedPosition, err := replication.DecodePosition(primaryStatus.Position)
		if err != nil {
			return nil, false, vterrors.Wrapf(err, "could not decode a primary status executed position for tablet %v: %v", alias, err)
		}

		positionMap[alias] = executedPosition
	}

	return positionMap, isGTIDBased, nil
}

// ReplicaWasRunning returns true if a StopReplicationStatus indicates that the
// replica had running replication threads before being stopped. It returns an
// error if the Before state of replication is nil.
func ReplicaWasRunning(stopStatus *replicationdatapb.StopReplicationStatus) (bool, error) {
	if stopStatus == nil || stopStatus.Before == nil {
		return false, vterrors.Errorf(vtrpc.Code_INVALID_ARGUMENT, "could not determine Before state of StopReplicationStatus %v", stopStatus)
	}

	replStatus := replication.ProtoToReplicationStatus(stopStatus.Before)
	return (replStatus.IOState == replication.ReplicationStateRunning) ||
		(replStatus.SQLState == replication.ReplicationStateRunning), nil
}

// SetReplicationSource is used to set the replication source on the specified
// tablet to the current shard primary (if available). It also figures out if
// the tablet should be sending semi-sync ACKs or not and passes that to the
// tabletmanager RPC.
//
// It does not start the replication forcefully.
// If we are unable to find the shard primary of the tablet from the topo server
// we exit out without any error.
func SetReplicationSource(ctx context.Context, ts *topo.Server, tmc tmclient.TabletManagerClient, tablet *topodatapb.Tablet) error {
	shardPrimary, err := topotools.GetShardPrimaryForTablet(ctx, ts, tablet)
	if err != nil {
		// If we didn't find the shard primary, we return without any error
		return nil
	}

	durabilityName, err := ts.GetKeyspaceDurability(ctx, tablet.Keyspace)
	if err != nil {
		return err
	}
	log.Infof("Getting a new durability policy for %v", durabilityName)
	durability, err := policy.GetDurabilityPolicy(durabilityName)
	if err != nil {
		return err
	}

	isSemiSync := policy.IsReplicaSemiSync(durability, shardPrimary.Tablet, tablet)
	return tmc.SetReplicationSource(ctx, tablet, shardPrimary.Alias, 0, "", false, isSemiSync, 0)
}

// replicationSnapshot stores the status maps and the tablets that were reachable
// when trying to stopReplicationAndBuildStatusMaps.
type replicationSnapshot struct {
	statusMap          map[string]*replicationdatapb.StopReplicationStatus
	primaryStatusMap   map[string]*replicationdatapb.PrimaryStatus
	reachableTablets   []*topodatapb.Tablet
	tabletsBackupState map[string]bool
}

// stopReplicationAndBuildStatusMaps stops replication on all replicas, then
// collects and returns a mapping of TabletAlias (as string) to their current
// replication positions.
// Apart from the status maps, it also returns the tablets reached as a list
func stopReplicationAndBuildStatusMaps(
	ctx context.Context,
	tmc tmclient.TabletManagerClient,
	ev *events.Reparent,
	tabletMap map[string]*topo.TabletInfo,
	stopReplicationTimeout time.Duration,
	ignoredTablets sets.Set[string],
	tabletToWaitFor *topodatapb.TabletAlias,
	durability policy.Durabler,
	waitForAllTablets bool,
	logger logutil.Logger,
) (*replicationSnapshot, error) {
	event.DispatchUpdate(ev, "stop replication on all replicas")

	var (
		m          sync.Mutex
		errChan    = make(chan concurrency.Error)
		allTablets []*topodatapb.Tablet
		res        = &replicationSnapshot{
			statusMap:          map[string]*replicationdatapb.StopReplicationStatus{},
			primaryStatusMap:   map[string]*replicationdatapb.PrimaryStatus{},
			reachableTablets:   []*topodatapb.Tablet{},
			tabletsBackupState: map[string]bool{},
		}
	)

	groupCtx, groupCancel := context.WithTimeout(ctx, stopReplicationTimeout)
	defer groupCancel()

	fillStatus := func(alias string, tabletInfo *topo.TabletInfo, mustWaitForTablet bool) {
		var concurrencyErr concurrency.Error
		var err error
		defer func() {
			concurrencyErr.Err = err
			concurrencyErr.MustWaitFor = mustWaitForTablet
			errChan <- concurrencyErr
		}()

		logger.Infof("getting replication position from %v", alias)

		stopReplicationStatus, err := tmc.StopReplicationAndGetStatus(groupCtx, tabletInfo.Tablet, replicationdatapb.StopReplicationMode_IOTHREADONLY)
		if err != nil {
			sqlErr, isSQLErr := sqlerror.NewSQLErrorFromError(err).(*sqlerror.SQLError)
			if isSQLErr && sqlErr != nil && sqlErr.Number() == sqlerror.ERNotReplica {
				var primaryStatus *replicationdatapb.PrimaryStatus

				primaryStatus, err = tmc.DemotePrimary(groupCtx, tabletInfo.Tablet)
				if err != nil {
					msg := "replica %v thinks it's primary but we failed to demote it: %v"
					err = vterrors.Wrapf(err, msg, alias, err)

					logger.Warningf(msg, alias, err)
					return
				}

				m.Lock()
				res.primaryStatusMap[alias] = primaryStatus
				res.reachableTablets = append(res.reachableTablets, tabletInfo.Tablet)
				m.Unlock()
			} else {
				logger.Warningf("failed to get replication status from %v: %v", alias, err)
				err = vterrors.Wrapf(err, "error when getting replication status for alias %v: %v", alias, err)
			}
		} else {
			isTakingBackup := false

			// Prefer the most up-to-date information regarding whether the tablet is taking a backup from the After
			// replication status, but fall back to the Before status if After is nil.
			if stopReplicationStatus.After != nil {
				isTakingBackup = stopReplicationStatus.After.BackupRunning
			} else if stopReplicationStatus.Before != nil {
				isTakingBackup = stopReplicationStatus.Before.BackupRunning
			}

			m.Lock()
			res.tabletsBackupState[alias] = isTakingBackup
			res.statusMap[alias] = stopReplicationStatus
			res.reachableTablets = append(res.reachableTablets, tabletInfo.Tablet)
			m.Unlock()
		}
	}

	// For the tablets that we want to get a response from necessarily, we
	// get them to set the MustWaitFor boolean as part of the concurrency.Error message
	// that we send to the waitGroup below.
	//
	// numErrorsToWaitFor corresponds to how many such tablets there are. This is the number
	// of special messages with MustWaitFor set that the call errgroup.Wait will wait for.
	tabletAliasToWaitFor := ""
	numErrorsToWaitFor := 0
	if tabletToWaitFor != nil {
		tabletAliasToWaitFor = topoproto.TabletAliasString(tabletToWaitFor)
	}
	for alias, tabletInfo := range tabletMap {
		allTablets = append(allTablets, tabletInfo.Tablet)
		if !ignoredTablets.Has(alias) {
			mustWaitFor := tabletAliasToWaitFor == alias
			// If this is a tablet that we must wait for
			// we increment numErrorsToWaitFor and pass in this to the
			// fillStatus function to indicate we must send this with the boolean
			// MustWaitFor specified.
			if mustWaitFor {
				numErrorsToWaitFor++
			}
			go fillStatus(alias, tabletInfo, mustWaitFor)
		}
	}

	numGoRoutines := len(tabletMap) - ignoredTablets.Len()
	// In general we want to wait for n-1 tablets to respond, since we know the primary tablet is down.
	requiredSuccesses := numGoRoutines - 1
	if waitForAllTablets {
		// In the special case, where we are explicitly told to wait for all the tablets to return,
		// we set the required success to all the go-routines.
		requiredSuccesses = numGoRoutines
	}

	errgroup := concurrency.ErrorGroup{
		NumGoroutines:        numGoRoutines,
		NumRequiredSuccesses: requiredSuccesses,
		NumAllowedErrors:     len(tabletMap), // We set the number of allowed errors to a very high value, because we don't want to exit early
		// even in case of multiple failures. We rely on the revoke function below to determine if we have more failures than we can tolerate
		NumErrorsToWaitFor: numErrorsToWaitFor,
	}

	errRecorder := errgroup.Wait(groupCancel, errChan)
	if len(errRecorder.Errors) <= 1 {
		return res, nil
	}
	// check that the tablets we were able to reach are sufficient for us to guarantee that no new write will be accepted by any tablet
	revokeSuccessful := haveRevoked(durability, res.reachableTablets, allTablets)
	if !revokeSuccessful {
		return nil, vterrors.Wrapf(errRecorder.Error(), "could not reach sufficient tablets to guarantee safety: %v", errRecorder.Error())
	}

	return res, nil
}

// WaitForRelayLogsToApply blocks execution waiting for the given tablet's relay
// logs to apply, unless the specified context is canceled or exceeded.
// Typically a caller will set a timeout of WaitReplicasTimeout on a context and
// use that context with this function.
func WaitForRelayLogsToApply(ctx context.Context, tmc tmclient.TabletManagerClient, tabletInfo *topo.TabletInfo, status *replicationdatapb.StopReplicationStatus) error {
	switch status.After.RelayLogPosition {
	case "":
		return tmc.WaitForPosition(ctx, tabletInfo.Tablet, status.After.RelayLogSourceBinlogEquivalentPosition)
	default:
		return tmc.WaitForPosition(ctx, tabletInfo.Tablet, status.After.RelayLogPosition)
	}
}
