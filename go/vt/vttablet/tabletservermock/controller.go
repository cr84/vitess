/*
Copyright 2019 The Vitess Authors.

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

// Package tabletservermock provides mock interfaces for tabletserver.
package tabletservermock

import (
	"context"
	"sync"
	"time"

	"vitess.io/vitess/go/vt/dbconfigs"
	"vitess.io/vitess/go/vt/mysqlctl"
	"vitess.io/vitess/go/vt/proto/tabletmanagerdata"
	"vitess.io/vitess/go/vt/servenv"
	"vitess.io/vitess/go/vt/topo"
	"vitess.io/vitess/go/vt/vttablet/queryservice"
	"vitess.io/vitess/go/vt/vttablet/tabletserver/rules"
	"vitess.io/vitess/go/vt/vttablet/tabletserver/schema"
	"vitess.io/vitess/go/vt/vttablet/tabletserver/tabletenv"
	"vitess.io/vitess/go/vt/vttablet/tabletserver/throttle"

	querypb "vitess.io/vitess/go/vt/proto/query"
	topodatapb "vitess.io/vitess/go/vt/proto/topodata"
)

// BroadcastData is used by the mock Controller to send data
// so the tests can check what was sent.
type BroadcastData struct {
	// TERTimestamp stores the last broadcast timestamp.
	TERTimestamp int64

	// RealtimeStats stores the last broadcast stats.
	RealtimeStats querypb.RealtimeStats

	// Serving contains the QueryServiceEnabled flag
	Serving bool
}

// StateChange stores the state the controller changed to.
// Tests can use this to verify that the state changed as expected.
type StateChange struct {
	// Serving is true when the QueryService is enabled.
	Serving bool
	// TabletType is the type of tablet e.g. REPLICA.
	TabletType topodatapb.TabletType
}

// Controller is a mock tabletserver.Controller
type Controller struct {
	stats *tabletenv.Stats

	// BroadcastData is a channel where we send BroadcastHealth data.
	// Set at construction time.
	BroadcastData chan *BroadcastData

	// StateChanges has the list of state changes done by SetServingType().
	// Set at construction time.
	StateChanges chan *StateChange

	target *querypb.Target

	// SetServingTypeError is the return value for SetServingType.
	SetServingTypeError error

	// TS is the return value for TopoServer.
	TS *topo.Server

	// mu protects the next fields in this structure. They are
	// accessed by both the methods in this interface, and the
	// background health check.
	mu sync.Mutex

	// QueryServiceEnabled is a state variable.
	queryServiceEnabled bool

	// isInLameduck is a state variable.
	isInLameduck bool

	// queryRulesMap has the latest query rules.
	queryRulesMap map[string]*rules.Rules

	MethodCalled map[string]bool
}

// NewController returns a mock of tabletserver.Controller
func NewController() *Controller {
	return &Controller{
		stats:               tabletenv.NewStats(servenv.NewExporter("MockController", "Tablet")),
		queryServiceEnabled: false,
		BroadcastData:       make(chan *BroadcastData, 10),
		StateChanges:        make(chan *StateChange, 10),
		queryRulesMap:       make(map[string]*rules.Rules),
		MethodCalled:        make(map[string]bool),
	}
}

// Stats is part of the tabletserver.Controller interface
func (tqsc *Controller) Stats() *tabletenv.Stats {
	return tqsc.stats
}

// Register is part of the tabletserver.Controller interface
func (tqsc *Controller) Register() {
}

// AddStatusHeader is part of the tabletserver.Controller interface
func (tqsc *Controller) AddStatusHeader() {
}

// AddStatusPart is part of the tabletserver.Controller interface
func (tqsc *Controller) AddStatusPart() {
}

// InitDBConfig is part of the tabletserver.Controller interface
func (tqsc *Controller) InitDBConfig(target *querypb.Target, dbcfgs *dbconfigs.DBConfigs, _ mysqlctl.MysqlDaemon) error {
	tqsc.mu.Lock()
	defer tqsc.mu.Unlock()
	tqsc.target = target.CloneVT()
	return nil
}

// SetServingType is part of the tabletserver.Controller interface
func (tqsc *Controller) SetServingType(tabletType topodatapb.TabletType, ptsTime time.Time, serving bool, reason string) error {
	tqsc.mu.Lock()
	defer tqsc.mu.Unlock()

	if tqsc.SetServingTypeError == nil {
		tqsc.target.TabletType = tabletType
		tqsc.queryServiceEnabled = serving
	}
	tqsc.StateChanges <- &StateChange{
		Serving:    serving,
		TabletType: tabletType,
	}
	tqsc.isInLameduck = false
	return tqsc.SetServingTypeError
}

// IsServing is part of the tabletserver.Controller interface
func (tqsc *Controller) IsServing() bool {
	tqsc.mu.Lock()
	defer tqsc.mu.Unlock()

	return tqsc.queryServiceEnabled
}

// CurrentTarget returns the current target.
func (tqsc *Controller) CurrentTarget() *querypb.Target {
	tqsc.mu.Lock()
	defer tqsc.mu.Unlock()
	return tqsc.target.CloneVT()
}

// IsHealthy is part of the tabletserver.Controller interface
func (tqsc *Controller) IsHealthy() error {
	return nil
}

// ReloadSchema is part of the tabletserver.Controller interface
func (tqsc *Controller) ReloadSchema(ctx context.Context) error {
	return nil
}

// ClearQueryPlanCache is part of the tabletserver.Controller interface
func (tqsc *Controller) ClearQueryPlanCache() {
}

// RegisterQueryRuleSource is part of the tabletserver.Controller interface
func (tqsc *Controller) RegisterQueryRuleSource(ruleSource string) {
}

// UnRegisterQueryRuleSource is part of the tabletserver.Controller interface
func (tqsc *Controller) UnRegisterQueryRuleSource(ruleSource string) {
}

// SetQueryRules is part of the tabletserver.Controller interface
func (tqsc *Controller) SetQueryRules(ruleSource string, qrs *rules.Rules) error {
	tqsc.mu.Lock()
	defer tqsc.mu.Unlock()
	tqsc.queryRulesMap[ruleSource] = qrs
	return nil
}

// QueryService is part of the tabletserver.Controller interface
func (tqsc *Controller) QueryService() queryservice.QueryService {
	return nil
}

// SchemaEngine is part of the tabletserver.Controller interface
func (tqsc *Controller) SchemaEngine() *schema.Engine {
	return nil
}

// BroadcastHealth is part of the tabletserver.Controller interface
func (tqsc *Controller) BroadcastHealth() {
	tqsc.mu.Lock()
	defer tqsc.mu.Unlock()

	tqsc.BroadcastData <- &BroadcastData{
		Serving: tqsc.queryServiceEnabled && (!tqsc.isInLameduck),
	}
}

// TopoServer is part of the tabletserver.Controller interface.
func (tqsc *Controller) TopoServer() *topo.Server {
	return tqsc.TS
}

// CheckThrottler is part of the tabletserver.Controller interface
func (tqsc *Controller) CheckThrottler(ctx context.Context, appName string, flags *throttle.CheckFlags) *throttle.CheckResult {
	return nil
}

// GetThrottlerStatus is part of the tabletserver.Controller interface
func (tqsc *Controller) GetThrottlerStatus(ctx context.Context) *throttle.ThrottlerStatus {
	return nil
}

// RedoPreparedTransactions is part of the tabletserver.Controller interface
func (tqsc *Controller) RedoPreparedTransactions() {}

// SetTwoPCAllowed sets whether TwoPC is allowed or not. It also takes the reason of why it is being set.
// The reason should be an enum value defined in the tabletserver.
func (tqsc *Controller) SetTwoPCAllowed(int, bool) {
}

// UnresolvedTransactions is part of the tabletserver.Controller interface
func (tqsc *Controller) UnresolvedTransactions(context.Context, *querypb.Target, int64) ([]*querypb.TransactionMetadata, error) {
	tqsc.MethodCalled["UnresolvedTransactions"] = true
	return nil, nil
}

// ReadTransaction is part of the tabletserver.Controller interface
func (tqsc *Controller) ReadTransaction(ctx context.Context, target *querypb.Target, dtid string) (*querypb.TransactionMetadata, error) {
	tqsc.MethodCalled["ReadTransaction"] = true
	return nil, nil
}

// GetTransactionInfo is part of the tabletserver.Controller interface
func (tqsc *Controller) GetTransactionInfo(ctx context.Context, target *querypb.Target, dtid string) (*tabletmanagerdata.GetTransactionInfoResponse, error) {
	tqsc.MethodCalled["GetTransactionInfo"] = true
	return nil, nil
}

// ConcludeTransaction is part of the tabletserver.Controller interface
func (tqsc *Controller) ConcludeTransaction(context.Context, *querypb.Target, string) error {
	tqsc.MethodCalled["ConcludeTransaction"] = true
	return nil
}

// RollbackPrepared is part of the tabletserver.Controller interface
func (tqsc *Controller) RollbackPrepared(context.Context, *querypb.Target, string, int64) error {
	tqsc.MethodCalled["RollbackPrepared"] = true
	return nil
}

// WaitForPreparedTwoPCTransactions is part of the tabletserver.Controller interface
func (tqsc *Controller) WaitForPreparedTwoPCTransactions(context.Context) error {
	tqsc.MethodCalled["WaitForPreparedTwoPCTransactions"] = true
	return nil
}

// SetDemotePrimaryStalled is part of the tabletserver.Controller interface
func (tqsc *Controller) SetDemotePrimaryStalled(bool) {
	tqsc.MethodCalled["SetDemotePrimaryStalled"] = true
}

// IsDiskStalled is part of the tabletserver.Controller interface
func (tqsc *Controller) IsDiskStalled() bool {
	tqsc.MethodCalled["IsDiskStalled"] = true
	return false
}

// EnterLameduck implements tabletserver.Controller.
func (tqsc *Controller) EnterLameduck() {
	tqsc.mu.Lock()
	defer tqsc.mu.Unlock()

	tqsc.isInLameduck = true
}

// SetQueryServiceEnabledForTests can set queryServiceEnabled in tests.
func (tqsc *Controller) SetQueryServiceEnabledForTests(enabled bool) {
	tqsc.mu.Lock()
	defer tqsc.mu.Unlock()

	tqsc.queryServiceEnabled = enabled
}

// GetQueryRules allows a test to check what was set.
func (tqsc *Controller) GetQueryRules(ruleSource string) *rules.Rules {
	tqsc.mu.Lock()
	defer tqsc.mu.Unlock()
	return tqsc.queryRulesMap[ruleSource]
}
