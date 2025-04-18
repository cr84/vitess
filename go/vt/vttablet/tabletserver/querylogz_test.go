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

package tabletserver

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"vitess.io/vitess/go/streamlog"
	"vitess.io/vitess/go/vt/callerid"
	"vitess.io/vitess/go/vt/sqlparser"
	"vitess.io/vitess/go/vt/vttablet/tabletserver/planbuilder"
	"vitess.io/vitess/go/vt/vttablet/tabletserver/tabletenv"
)

func TestQuerylogzHandler(t *testing.T) {
	req, _ := http.NewRequest("GET", "/querylogz?timeout=10&limit=1", nil)
	logStats := tabletenv.NewLogStats(context.Background(), "Execute", streamlog.NewQueryLogConfigForTest())
	logStats.PlanType = planbuilder.PlanSelect.String()
	logStats.OriginalSQL = "select name, 'inject <script>alert();</script>' from test_table limit 1000"
	logStats.RowsAffected = 1000
	logStats.NumberOfQueries = 1
	logStats.StartTime, _ = time.Parse("Jan 2 15:04:05", "Nov 29 13:33:09")
	logStats.MysqlResponseTime = 1 * time.Millisecond
	logStats.WaitingForConnection = 10 * time.Nanosecond
	logStats.TransactionID = 131
	logStats.ReservedID = 313
	logStats.Ctx = callerid.NewContext(
		context.Background(),
		callerid.NewEffectiveCallerID("effective-caller", "component", "subcomponent"),
		callerid.NewImmediateCallerID("immediate-caller"),
	)

	// fast query
	fastQueryPattern := []string{
		`<tr class="low">`,
		`<td>Execute</td>`,
		`<td></td>`,
		`<td>effective-caller</td>`,
		`<td>immediate-caller</td>`,
		`<td>Nov 29 13:33:09.000000</td>`,
		`<td>Nov 29 13:33:09.001000</td>`,
		`<td>0.001</td>`,
		`<td>0.001</td>`,
		`<td>1e-08</td>`,
		`<td>Select</td>`,
		regexp.QuoteMeta("<td>select name,\u200b &#39;inject &lt;script&gt;alert()\u200b;&lt;/script&gt;&#39; from test_table limit 1000</td>"),
		`<td>1</td>`,
		`<td>none</td>`,
		`<td>1000</td>`,
		`<td>0</td>`,
		`<td>131</td>`,
		`<td>313</td>`,
		`<td></td>`,
	}
	logStats.EndTime = logStats.StartTime.Add(1 * time.Millisecond)
	response := httptest.NewRecorder()
	ch := make(chan *tabletenv.LogStats, 1)
	ch <- logStats
	querylogzHandler(ch, response, req, sqlparser.NewTestParser())
	close(ch)
	body, _ := io.ReadAll(response.Body)
	checkQuerylogzHasStats(t, fastQueryPattern, logStats, body)

	// medium query
	mediumQueryPattern := []string{
		`<tr class="medium">`,
		`<td>Execute</td>`,
		`<td></td>`,
		`<td>effective-caller</td>`,
		`<td>immediate-caller</td>`,
		`<td>Nov 29 13:33:09.000000</td>`,
		`<td>Nov 29 13:33:09.020000</td>`,
		`<td>0.02</td>`,
		`<td>0.001</td>`,
		`<td>1e-08</td>`,
		`<td>Select</td>`,
		regexp.QuoteMeta("<td>select name,\u200b &#39;inject &lt;script&gt;alert()\u200b;&lt;/script&gt;&#39; from test_table limit 1000</td>"),
		`<td>1</td>`,
		`<td>none</td>`,
		`<td>1000</td>`,
		`<td>0</td>`,
		`<td>131</td>`,
		`<td>313</td>`,
		`<td></td>`,
	}
	logStats.EndTime = logStats.StartTime.Add(20 * time.Millisecond)
	response = httptest.NewRecorder()
	ch = make(chan *tabletenv.LogStats, 1)
	ch <- logStats
	querylogzHandler(ch, response, req, sqlparser.NewTestParser())
	close(ch)
	body, _ = io.ReadAll(response.Body)
	checkQuerylogzHasStats(t, mediumQueryPattern, logStats, body)

	// slow query
	slowQueryPattern := []string{
		`<tr class="high">`,
		`<td>Execute</td>`,
		`<td></td>`,
		`<td>effective-caller</td>`,
		`<td>immediate-caller</td>`,
		`<td>Nov 29 13:33:09.000000</td>`,
		`<td>Nov 29 13:33:09.500000</td>`,
		`<td>0.5</td>`,
		`<td>0.001</td>`,
		`<td>1e-08</td>`,
		`<td>Select</td>`,
		regexp.QuoteMeta("<td>select name,\u200b &#39;inject &lt;script&gt;alert()\u200b;&lt;/script&gt;&#39; from test_table limit 1000</td>"),
		`<td>1</td>`,
		`<td>none</td>`,
		`<td>1000</td>`,
		`<td>0</td>`,
		`<td>131</td>`,
		`<td>313</td>`,
		`<td></td>`,
	}
	logStats.EndTime = logStats.StartTime.Add(500 * time.Millisecond)
	ch = make(chan *tabletenv.LogStats, 1)
	ch <- logStats
	querylogzHandler(ch, response, req, sqlparser.NewTestParser())
	close(ch)
	body, _ = io.ReadAll(response.Body)
	checkQuerylogzHasStats(t, slowQueryPattern, logStats, body)

	// ensure querylogz is not affected by the filter tag
	logStats.Config.FilterTag = "XXX_SKIP_ME"
	ch = make(chan *tabletenv.LogStats, 1)
	ch <- logStats
	querylogzHandler(ch, response, req, sqlparser.NewTestParser())
	close(ch)
	body, _ = io.ReadAll(response.Body)
	checkQuerylogzHasStats(t, slowQueryPattern, logStats, body)
}

func checkQuerylogzHasStats(t *testing.T, pattern []string, logStats *tabletenv.LogStats, page []byte) {
	t.Helper()
	matcher := regexp.MustCompile(strings.Join(pattern, `\s*`))
	if !matcher.Match(page) {
		t.Fatalf("querylogz page does not contain stats: %v, pattern: %v, page: %s", logStats, pattern, string(page))
	}
}
