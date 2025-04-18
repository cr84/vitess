/*
Copyright 2022 The Vitess Authors.

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

package vdiff

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/prototext"

	"vitess.io/vitess/go/mysql/collations"
	"vitess.io/vitess/go/sqltypes"
	"vitess.io/vitess/go/vt/binlog/binlogplayer"
	"vitess.io/vitess/go/vt/key"
	"vitess.io/vitess/go/vt/log"
	"vitess.io/vitess/go/vt/schema"
	"vitess.io/vitess/go/vt/sqlparser"
	"vitess.io/vitess/go/vt/topo"
	"vitess.io/vitess/go/vt/vtctl/schematools"
	"vitess.io/vitess/go/vt/vterrors"
	"vitess.io/vitess/go/vt/vtgate/vindexes"
	vttablet "vitess.io/vitess/go/vt/vttablet/common"
	"vitess.io/vitess/go/vt/vttablet/tabletmanager/vreplication"

	binlogdatapb "vitess.io/vitess/go/vt/proto/binlogdata"
	tabletmanagerdatapb "vitess.io/vitess/go/vt/proto/tabletmanagerdata"
	vtrpcpb "vitess.io/vitess/go/vt/proto/vtrpc"
)

// workflowDiffer has metadata and state for the vdiff of a single workflow on this tablet
// only one vdiff can be running for a workflow at any time.
type workflowDiffer struct {
	ct *controller

	tableDiffers map[string]*tableDiffer // key is table name
	opts         *tabletmanagerdatapb.VDiffOptions

	collationEnv   *collations.Environment
	WorkflowConfig **vttablet.VReplicationConfig
}

func newWorkflowDiffer(ct *controller, opts *tabletmanagerdatapb.VDiffOptions, collationEnv *collations.Environment) (*workflowDiffer, error) {
	vttablet.InitVReplicationConfigDefaults()
	wd := &workflowDiffer{
		ct:             ct,
		opts:           opts,
		tableDiffers:   make(map[string]*tableDiffer, 1),
		collationEnv:   collationEnv,
		WorkflowConfig: &vttablet.DefaultVReplicationConfig,
	}
	return wd, nil
}

// reconcileExtraRows compares the extra rows in the source and target tables. If there are any matching rows, they are
// removed from the extra rows. The number of extra rows to compare is limited by vdiff option maxExtraRowsToCompare.
func (wd *workflowDiffer) reconcileExtraRows(dr *DiffReport, maxExtraRowsToCompare int64, maxReportSampleRows int64) error {
	err := wd.reconcileReferenceTables(dr)
	if err != nil {
		return err
	}

	return wd.doReconcileExtraRows(dr, maxExtraRowsToCompare, maxReportSampleRows)
}

func (wd *workflowDiffer) reconcileReferenceTables(dr *DiffReport) error {
	if dr.MismatchedRows == 0 {
		// Get the VSchema on the target and source keyspaces. We can then use this
		// for handling additional edge cases, such as adjusting results for reference
		// tables when the shard count is different between the source and target as
		// then there will be a extra rows reported on the side with more shards.
		srcvschema, err := wd.ct.ts.GetVSchema(wd.ct.vde.ctx, wd.ct.sourceKeyspace)
		if err != nil {
			return err
		}
		tgtvschema, err := wd.ct.ts.GetVSchema(wd.ct.vde.ctx, wd.ct.vde.thisTablet.Keyspace)
		if err != nil {
			return err
		}
		svt, sok := srcvschema.Tables[dr.TableName]
		tvt, tok := tgtvschema.Tables[dr.TableName]
		if dr.ExtraRowsSource > 0 && sok && svt.Type == vindexes.TypeReference && dr.ExtraRowsSource%dr.MatchingRows == 0 {
			// We have a reference table with no mismatched rows and the number of
			// extra rows on the source is a multiple of the matching rows. This
			// means that there's no actual diff.
			dr.ExtraRowsSource = 0
			dr.ExtraRowsSourceDiffs = nil
		}
		if dr.ExtraRowsTarget > 0 && tok && tvt.Type == vindexes.TypeReference && dr.ExtraRowsTarget%dr.MatchingRows == 0 {
			// We have a reference table with no mismatched rows and the number of
			// extra rows on the target is a multiple of the matching rows. This
			// means that there's no actual diff.
			dr.ExtraRowsTarget = 0
			dr.ExtraRowsTargetDiffs = nil
		}
	}
	return nil
}

func (wd *workflowDiffer) doReconcileExtraRows(dr *DiffReport, maxExtraRowsToCompare int64, maxReportSampleRows int64) error {
	if dr.ExtraRowsSource == 0 || dr.ExtraRowsTarget == 0 {
		return nil
	}
	matchedSourceDiffs := make([]bool, int(dr.ExtraRowsSource))
	matchedTargetDiffs := make([]bool, int(dr.ExtraRowsTarget))
	matchedDiffs := int64(0)

	maxRows := int(dr.ExtraRowsSource)
	if maxRows > int(maxExtraRowsToCompare) {
		maxRows = int(maxExtraRowsToCompare)
	}
	log.Infof("Reconciling extra rows for table %s in vdiff %s, extra source rows %d, extra target rows %d, max rows %d",
		dr.TableName, wd.ct.uuid, dr.ExtraRowsSource, dr.ExtraRowsTarget, maxRows)

	// Find the matching extra rows
	for i := 0; i < maxRows; i++ {
		for j := 0; j < int(dr.ExtraRowsTarget); j++ {
			if matchedTargetDiffs[j] {
				// previously matched
				continue
			}
			if reflect.DeepEqual(dr.ExtraRowsSourceDiffs[i], dr.ExtraRowsTargetDiffs[j]) {
				matchedSourceDiffs[i] = true
				matchedTargetDiffs[j] = true
				matchedDiffs++
				break
			}
		}
	}

	if matchedDiffs == 0 {
		log.Infof("No matching extra rows found for table %s in vdiff %s, checked %d rows",
			dr.TableName, maxRows, wd.ct.uuid)
	} else {
		// Now remove the matching extra rows
		newExtraRowsSourceDiffs := make([]*RowDiff, 0, dr.ExtraRowsSource-matchedDiffs)
		newExtraRowsTargetDiffs := make([]*RowDiff, 0, dr.ExtraRowsTarget-matchedDiffs)
		for i := 0; i < int(dr.ExtraRowsSource); i++ {
			if !matchedSourceDiffs[i] {
				newExtraRowsSourceDiffs = append(newExtraRowsSourceDiffs, dr.ExtraRowsSourceDiffs[i])
			}
			if len(newExtraRowsSourceDiffs) >= maxRows {
				break
			}
		}
		for i := 0; i < int(dr.ExtraRowsTarget); i++ {
			if !matchedTargetDiffs[i] {
				newExtraRowsTargetDiffs = append(newExtraRowsTargetDiffs, dr.ExtraRowsTargetDiffs[i])
			}
			if len(newExtraRowsTargetDiffs) >= maxRows {
				break
			}
		}
		dr.ExtraRowsSourceDiffs = newExtraRowsSourceDiffs
		dr.ExtraRowsTargetDiffs = newExtraRowsTargetDiffs

		// Update the counts
		dr.ExtraRowsSource = int64(len(dr.ExtraRowsSourceDiffs))
		dr.ExtraRowsTarget = int64(len(dr.ExtraRowsTargetDiffs))
		dr.MatchingRows += matchedDiffs
		dr.MismatchedRows -= matchedDiffs
		dr.ProcessedRows += matchedDiffs
		log.Infof("Reconciled extra rows for table %s in vdiff %s, matching rows %d, extra source rows %d, extra target rows %d. Max compared rows %d",
			dr.TableName, wd.ct.uuid, matchedDiffs, dr.ExtraRowsSource, dr.ExtraRowsTarget, maxRows)
	}

	// Trim the extra rows diffs to the maxReportSampleRows value. Note we need to do this after updating
	// the slices and counts above, since maxExtraRowsToCompare can be greater than maxVDiffReportSampleRows.
	if int64(len(dr.ExtraRowsSourceDiffs)) > maxReportSampleRows && maxReportSampleRows > 0 {
		dr.ExtraRowsSourceDiffs = dr.ExtraRowsSourceDiffs[:maxReportSampleRows-1]
	}
	if int64(len(dr.ExtraRowsTargetDiffs)) > maxReportSampleRows && maxReportSampleRows > 0 {
		dr.ExtraRowsTargetDiffs = dr.ExtraRowsTargetDiffs[:maxReportSampleRows-1]
	}
	return nil
}

func (wd *workflowDiffer) diffTable(ctx context.Context, dbClient binlogplayer.DBClient, td *tableDiffer) error {
	cancelShardStreams := func() {
		if td.shardStreamsCancel != nil {
			td.shardStreamsCancel()
		}
		// Wait for all the shard streams to finish before returning.
		td.wgShardStreamers.Wait()
	}
	defer func() {
		cancelShardStreams()
	}()

	var (
		diffTimer  *time.Timer
		diffReport *DiffReport
		diffErr    error
	)
	defer func() {
		if diffTimer != nil {
			if !diffTimer.Stop() {
				select {
				case <-diffTimer.C:
				default:
				}
			}
		}
	}()

	maxDiffRuntime := time.Duration(24 * time.Hour * 365) // 1 year (effectively forever)
	if wd.ct.options.CoreOptions.MaxDiffSeconds > 0 {
		// Restart the diff if it takes longer than the specified max diff time.
		maxDiffRuntime = time.Duration(wd.ct.options.CoreOptions.MaxDiffSeconds) * time.Second
	}

	log.Infof("Starting differ on table %s for vdiff %s", td.table.Name, wd.ct.uuid)
	if err := td.updateTableState(ctx, dbClient, StartedState); err != nil {
		return err
	}

	for {
		select {
		case <-ctx.Done():
			return vterrors.Errorf(vtrpcpb.Code_CANCELED, "context has expired")
		case <-wd.ct.done:
			return ErrVDiffStoppedByUser
		default:
		}

		if diffTimer != nil { // We're restarting the diff
			if !diffTimer.Stop() {
				select {
				case <-diffTimer.C:
				default:
				}
			}
			diffTimer = nil
			cancelShardStreams()
			// Give the underlying resources (mainly MySQL) a moment to catch up
			// before we pick up where we left off (but with new database snapshots).
			time.Sleep(30 * time.Second)
		}
		if err := td.initialize(ctx); err != nil { // Setup the consistent snapshots
			return err
		}
		log.Infof("Table initialization done on table %s for vdiff %s", td.table.Name, wd.ct.uuid)
		diffTimer = time.NewTimer(maxDiffRuntime)
		diffReport, diffErr = td.diff(ctx, wd.opts.CoreOptions, wd.opts.ReportOptions, diffTimer.C)
		if diffErr == nil { // We finished the diff successfully
			break
		}
		log.Errorf("Encountered an error diffing table %s for vdiff %s: %v", td.table.Name, wd.ct.uuid, diffErr)
		if !errors.Is(diffErr, ErrMaxDiffDurationExceeded) { // We only want to retry if we hit the max-diff-duration
			return diffErr
		}
	}
	log.Infof("Table diff done on table %s for vdiff %s with report: %+v", td.table.Name, wd.ct.uuid, diffReport)

	if diffReport.ExtraRowsSource > 0 || diffReport.ExtraRowsTarget > 0 {
		if err := wd.reconcileExtraRows(diffReport, wd.opts.CoreOptions.MaxExtraRowsToCompare, wd.opts.ReportOptions.MaxSampleRows); err != nil {
			log.Errorf("Encountered an error reconciling extra rows found for table %s for vdiff %s: %v", td.table.Name, wd.ct.uuid, err)
			return vterrors.Wrap(err, "failed to reconcile extra rows")
		}
	}

	if diffReport.MismatchedRows > 0 || diffReport.ExtraRowsTarget > 0 || diffReport.ExtraRowsSource > 0 {
		if err := updateTableMismatch(dbClient, wd.ct.id, td.table.Name); err != nil {
			return err
		}
	}

	log.Infof("Completed reconciliation on table %s for vdiff %s with updated report: %+v", td.table.Name, wd.ct.uuid, diffReport)
	if err := td.updateTableStateAndReport(ctx, dbClient, CompletedState, diffReport); err != nil {
		return err
	}
	return nil
}

func (wd *workflowDiffer) diff(ctx context.Context) (err error) {
	defer func() {
		if err != nil {
			globalStats.ErrorCount.Add(1)
			wd.ct.Errors.Add(err.Error(), 1)
		}
	}()
	dbClient := wd.ct.dbClientFactory()
	if err := dbClient.Connect(); err != nil {
		return err
	}
	defer dbClient.Close()

	select {
	case <-ctx.Done():
		return vterrors.Errorf(vtrpcpb.Code_CANCELED, "context has expired")
	case <-wd.ct.done:
		return ErrVDiffStoppedByUser
	default:
	}

	filter := wd.ct.filter
	req := &tabletmanagerdatapb.GetSchemaRequest{}
	schm, err := schematools.GetSchema(ctx, wd.ct.ts, wd.ct.tmc, wd.ct.vde.thisTablet.Alias, req)
	if err != nil {
		return vterrors.Wrap(err, "GetSchema")
	}
	if err = wd.buildPlan(dbClient, filter, schm); err != nil {
		return vterrors.Wrap(err, "buildPlan")
	}
	if err := wd.initVDiffTables(dbClient); err != nil {
		return err
	}
	for _, td := range wd.tableDiffers {
		select {
		case <-ctx.Done():
			return vterrors.Errorf(vtrpcpb.Code_CANCELED, "context has expired")
		case <-wd.ct.done:
			return ErrVDiffStoppedByUser
		default:
		}
		query, err := sqlparser.ParseAndBind(sqlGetVDiffTable,
			sqltypes.Int64BindVariable(wd.ct.id),
			sqltypes.StringBindVariable(td.table.Name),
		)
		if err != nil {
			return err
		}
		qr, err := dbClient.ExecuteFetch(query, 1)
		if err != nil {
			return err
		}
		if len(qr.Rows) == 0 {
			return fmt.Errorf("no vdiff table found for %s on tablet %v",
				td.table.Name, wd.ct.vde.thisTablet.Alias)
		}

		log.Infof("Starting diff of table %s for vdiff %s", td.table.Name, wd.ct.uuid)
		if err := wd.diffTable(ctx, dbClient, td); err != nil {
			if err := td.updateTableState(ctx, dbClient, ErrorState); err != nil {
				return err
			}
			insertVDiffLog(ctx, dbClient, wd.ct.id, fmt.Sprintf("Table %s Error: %s", td.table.Name, err))
			return err
		}
		if err := td.updateTableState(ctx, dbClient, CompletedState); err != nil {
			return err
		}
		log.Infof("Completed diff of table %s for vdiff %s", td.table.Name, wd.ct.uuid)
	}
	if err := wd.markIfCompleted(ctx, dbClient); err != nil {
		return err
	}
	return nil
}

func (wd *workflowDiffer) markIfCompleted(ctx context.Context, dbClient binlogplayer.DBClient) error {
	query, err := sqlparser.ParseAndBind(sqlGetIncompleteTables, sqltypes.Int64BindVariable(wd.ct.id))
	if err != nil {
		return err
	}
	qr, err := dbClient.ExecuteFetch(query, -1)
	if err != nil {
		return err
	}

	// Double check to be sure all of the individual table diffs completed without error
	// before marking the vdiff as completed.
	if len(qr.Rows) == 0 {
		if err := wd.ct.updateState(dbClient, CompletedState, nil); err != nil {
			return err
		}
	}
	return nil
}

func (wd *workflowDiffer) buildPlan(dbClient binlogplayer.DBClient, filter *binlogdatapb.Filter, schm *tabletmanagerdatapb.SchemaDefinition) error {
	var specifiedTables []string
	optTables := strings.TrimSpace(wd.opts.CoreOptions.Tables)
	if optTables != "" {
		specifiedTables = strings.Split(optTables, ",")
	}

	for _, table := range schm.TableDefinitions {
		// if user specified tables explicitly only use those, otherwise diff all tables in workflow
		if len(specifiedTables) != 0 && !slices.Contains(specifiedTables, table.Name) {
			continue
		}
		if schema.IsInternalOperationTableName(table.Name) && !schema.IsOnlineDDLTableName(table.Name) {
			continue
		}
		rule, err := vreplication.MatchTable(table.Name, filter)
		if err != nil {
			return err
		}
		if rule == nil || rule.Filter == "exclude" {
			continue
		}
		sourceQuery := rule.Filter
		switch {
		case rule.Filter == "":
			buf := sqlparser.NewTrackedBuffer(nil)
			buf.Myprintf("select * from %v", sqlparser.NewIdentifierCS(table.Name))
			sourceQuery = buf.String()
		case key.IsValidKeyRange(rule.Filter):
			buf := sqlparser.NewTrackedBuffer(nil)
			buf.Myprintf("select * from %v where in_keyrange(%v)", sqlparser.NewIdentifierCS(table.Name), sqlparser.NewStrLiteral(rule.Filter))
			sourceQuery = buf.String()
		}

		td := newTableDiffer(wd, table, sourceQuery)
		lastPK, err := wd.getTableLastPK(dbClient, table.Name)
		if err != nil {
			return err
		}
		if lastPK != nil {
			td.lastSourcePK = lastPK.Source
			td.lastTargetPK = lastPK.Target
		}
		wd.tableDiffers[table.Name] = td
		if _, err := td.buildTablePlan(dbClient, wd.ct.vde.dbName, wd.collationEnv); err != nil {
			return err
		}
		// We get the PK columns from the source schema as well as they can differ
		// and they determine the proper position to use when saving our progress.
		if err := td.getSourcePKCols(); err != nil {
			return vterrors.Wrapf(err, "could not get the primary key columns from the %s source keyspace",
				wd.ct.sourceKeyspace)
		}
	}
	if len(wd.tableDiffers) == 0 {
		return fmt.Errorf("no tables found to diff, %s:%s, on tablet %v",
			optTables, specifiedTables, wd.ct.vde.thisTablet.Alias)
	}
	return nil
}

// getTableLastPK gets the lastPK protobuf message for a given vdiff table.
func (wd *workflowDiffer) getTableLastPK(dbClient binlogplayer.DBClient, tableName string) (*tabletmanagerdatapb.VDiffTableLastPK, error) {
	query, err := sqlparser.ParseAndBind(sqlGetVDiffTable,
		sqltypes.Int64BindVariable(wd.ct.id),
		sqltypes.StringBindVariable(tableName),
	)
	if err != nil {
		return nil, err
	}
	qr, err := dbClient.ExecuteFetch(query, 1)
	if err != nil {
		return nil, err
	}
	if len(qr.Rows) == 1 {
		var lastpk []byte
		if lastpk, err = qr.Named().Row().ToBytes("lastpk"); err != nil {
			return nil, err
		}
		if len(lastpk) != 0 {
			lastPK := &tabletmanagerdatapb.VDiffTableLastPK{}
			if err := prototext.Unmarshal(lastpk, lastPK); err != nil {
				return nil, vterrors.Wrapf(err, "failed to unmarshal lastpk value of %s for the %s table",
					string(lastpk), tableName)
			}
			if lastPK.Source == nil { // Then it's the same as the target
				lastPK.Source = lastPK.Target
			}
			return lastPK, nil
		}
	}
	return nil, nil
}

func (wd *workflowDiffer) initVDiffTables(dbClient binlogplayer.DBClient) error {
	tableIn := strings.Builder{}
	n := 0
	for tableName := range wd.tableDiffers {
		// Update the table statistics for each table if requested.
		if wd.opts.CoreOptions.UpdateTableStats {
			stmt := sqlparser.BuildParsedQuery(sqlAnalyzeTable,
				wd.ct.vde.dbName,
				tableName,
			)
			log.Infof("Updating the table stats for %s.%s using: %q", wd.ct.vde.dbName, tableName, stmt.Query)
			if _, err := dbClient.ExecuteFetch(stmt.Query, -1); err != nil {
				return err
			}
			log.Infof("Finished updating the table stats for %s.%s", wd.ct.vde.dbName, tableName)
		}
		tableIn.WriteString(encodeString(tableName))
		if n++; n < len(wd.tableDiffers) {
			tableIn.WriteByte(',')
		}
	}
	query := sqlparser.BuildParsedQuery(sqlGetAllTableRows,
		encodeString(wd.ct.vde.dbName),
		tableIn.String(),
	)
	isqr, err := dbClient.ExecuteFetch(query.Query, -1)
	if err != nil {
		return err
	}
	for _, row := range isqr.Named().Rows {
		tableName, _ := row.ToString("table_name")
		tableRows, _ := row.ToInt64("table_rows")

		query, err := sqlparser.ParseAndBind(sqlGetVDiffTable,
			sqltypes.Int64BindVariable(wd.ct.id),
			sqltypes.StringBindVariable(tableName),
		)
		if err != nil {
			return err
		}
		qr, err := dbClient.ExecuteFetch(query, -1)
		if err != nil {
			return err
		}
		if len(qr.Rows) == 0 {
			query, err = sqlparser.ParseAndBind(sqlNewVDiffTable,
				sqltypes.Int64BindVariable(wd.ct.id),
				sqltypes.StringBindVariable(tableName),
				sqltypes.Int64BindVariable(tableRows),
			)
			if err != nil {
				return err
			}
		} else if len(qr.Rows) == 1 {
			query, err = sqlparser.ParseAndBind(sqlUpdateTableRows,
				sqltypes.Int64BindVariable(tableRows),
				sqltypes.Int64BindVariable(wd.ct.id),
				sqltypes.StringBindVariable(tableName),
			)
			if err != nil {
				return err
			}
		} else {
			return fmt.Errorf("invalid state found for vdiff table %s for vdiff_id %d on tablet %s",
				tableName, wd.ct.id, wd.ct.vde.thisTablet.Alias)
		}
		if _, err := dbClient.ExecuteFetch(query, 1); err != nil {
			return err
		}
	}
	return nil
}

// getSourceTopoServer returns the source topo server as for Mount+Migrate the
// source tablets will be in a different Vitess cluster with its own TopoServer.
func (wd *workflowDiffer) getSourceTopoServer() (*topo.Server, error) {
	if wd.ct.externalCluster == "" {
		return wd.ct.ts, nil
	}
	ctx, cancel := context.WithTimeout(wd.ct.vde.ctx, topo.RemoteOperationTimeout)
	defer cancel()
	return wd.ct.ts.OpenExternalVitessClusterServer(ctx, wd.ct.externalCluster)
}
