// SPDX-License-Identifier: AGPL-3.0-only
// Provenance-includes-location: https://github.com/cortexproject/cortex/blob/master/integration/ruler_test.go
// Provenance-includes-license: Apache-2.0
// Provenance-includes-copyright: The Cortex Authors.
//go:build requires_docker
// +build requires_docker

package integration

import (
	"context"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/grafana/e2e"
	e2edb "github.com/grafana/e2e/db"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/rulefmt"
	"github.com/prometheus/prometheus/model/value"
	"github.com/prometheus/prometheus/prompb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/grafana/mimir/integration/ca"
	"github.com/grafana/mimir/integration/e2emimir"
)

func TestRulerAPI(t *testing.T) {
	var (
		namespaceOne = "test_/encoded_+namespace/?"
		namespaceTwo = "test_/encoded_+namespace/?/two"
		ruleGroup    = createTestRuleGroup(t)
	)

	s, err := e2e.NewScenario(networkName)
	require.NoError(t, err)
	defer s.Close()

	// Start dependencies.
	consul := e2edb.NewConsul()
	minio := e2edb.NewMinio(9000, bucketName, rulestoreBucketName)
	require.NoError(t, s.StartAndWaitReady(consul, minio))

	// Configure the ruler.
	rulerFlags := mergeFlags(BlocksStorageFlags(), RulerFlags())

	// Start Mimir components.
	ruler := e2emimir.NewRuler("ruler", consul.NetworkHTTPEndpoint(), rulerFlags, "")
	require.NoError(t, s.StartAndWaitReady(ruler))

	// Create a client with the ruler address configured
	c, err := e2emimir.NewClient("", "", "", ruler.HTTPEndpoint(), "user-1")
	require.NoError(t, err)

	// Set the rule group into the ruler
	require.NoError(t, c.SetRuleGroup(ruleGroup, namespaceOne))

	// Wait until the user manager is created
	require.NoError(t, ruler.WaitSumMetrics(e2e.Equals(1), "cortex_ruler_managers_total"))

	// Check to ensure the rules running in the ruler match what was set
	rgs, err := c.GetRuleGroups()
	require.NoError(t, err)

	retrievedNamespace, exists := rgs[namespaceOne]
	require.True(t, exists)
	require.Len(t, retrievedNamespace, 1)
	require.Equal(t, retrievedNamespace[0].Name, ruleGroup.Name)

	// Add a second rule group with a similar namespace
	require.NoError(t, c.SetRuleGroup(ruleGroup, namespaceTwo))
	require.NoError(t, ruler.WaitSumMetrics(e2e.Equals(2), "cortex_prometheus_rule_group_rules"))

	// Check to ensure the rules running in the ruler match what was set
	rgs, err = c.GetRuleGroups()
	require.NoError(t, err)

	retrievedNamespace, exists = rgs[namespaceOne]
	require.True(t, exists)
	require.Len(t, retrievedNamespace, 1)
	require.Equal(t, retrievedNamespace[0].Name, ruleGroup.Name)

	retrievedNamespace, exists = rgs[namespaceTwo]
	require.True(t, exists)
	require.Len(t, retrievedNamespace, 1)
	require.Equal(t, retrievedNamespace[0].Name, ruleGroup.Name)

	// Test compression by inspecting the response Headers
	req, err := http.NewRequest("GET", fmt.Sprintf("http://%s/api/prom/rules", ruler.HTTPEndpoint()), nil)
	require.NoError(t, err)

	req.Header.Set("X-Scope-OrgID", "user-1")
	req.Header.Set("Accept-Encoding", "gzip")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Execute HTTP request
	res, err := http.DefaultClient.Do(req.WithContext(ctx))
	require.NoError(t, err)

	defer res.Body.Close()
	// We assert on the Vary header as the minimum response size for enabling compression is 1500 bytes.
	// This is enough to know whenever the handler for compression is enabled or not.
	require.Equal(t, "Accept-Encoding", res.Header.Get("Vary"))

	// Delete the set rule groups
	require.NoError(t, c.DeleteRuleGroup(namespaceOne, ruleGroup.Name))
	require.NoError(t, c.DeleteRuleNamespace(namespaceTwo))

	// Get the rule group and ensure it returns a 404
	resp, err := c.GetRuleGroup(namespaceOne, ruleGroup.Name)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)

	// Wait until the users manager has been terminated
	require.NoError(t, ruler.WaitSumMetrics(e2e.Equals(0), "cortex_ruler_managers_total"))

	// Check to ensure the rule groups are no longer active
	groups, err := c.GetRuleGroups()
	require.NoError(t, err)
	require.Empty(t, groups)

	// Ensure no service-specific metrics prefix is used by the wrong service.
	assertServiceMetricsPrefixes(t, Ruler, ruler)
}

func TestRulerAPISingleBinary(t *testing.T) {
	s, err := e2e.NewScenario(networkName)
	require.NoError(t, err)
	defer s.Close()

	namespace := "ns"
	user := "fake"

	// Start dependencies.
	minio := e2edb.NewMinio(9000, bucketName)
	require.NoError(t, s.StartAndWaitReady(minio))

	flags := mergeFlags(
		BlocksStorageFlags(),
		map[string]string{
			"-ruler-storage.local.directory": filepath.Join(e2e.ContainerSharedDir, "ruler_configs"),
			"-ruler.poll-interval":           "2s",
			"-ruler.rule-path":               filepath.Join(e2e.ContainerSharedDir, "rule_tmp/"),
		},
	)

	// Start Mimir components.
	require.NoError(t, copyFileToSharedDir(s, "docs/sources/configuration/single-process-config-blocks.yaml", mimirConfigFile))
	require.NoError(t, writeFileToSharedDir(s, filepath.Join("ruler_configs", user, namespace), []byte(mimirRulerUserConfigYaml)))
	mimir := e2emimir.NewSingleBinaryWithConfigFile("mimir", mimirConfigFile, flags, "", 9009, 9095)
	require.NoError(t, s.StartAndWaitReady(mimir))

	// Create a client with the ruler address configured
	c, err := e2emimir.NewClient("", "", "", mimir.HTTPEndpoint(), "")
	require.NoError(t, err)

	// Wait until the user manager is created
	require.NoError(t, mimir.WaitSumMetrics(e2e.Equals(1), "cortex_ruler_managers_total"))

	// Check to ensure the rules running in the mimir match what was set
	rgs, err := c.GetRuleGroups()
	require.NoError(t, err)

	retrievedNamespace, exists := rgs[namespace]
	require.True(t, exists)
	require.Len(t, retrievedNamespace, 1)
	require.Equal(t, retrievedNamespace[0].Name, "rule")

	// Check to make sure prometheus engine metrics are available for both engine types
	require.NoError(t, mimir.WaitSumMetricsWithOptions(e2e.Equals(0), []string{"prometheus_engine_queries"}, e2e.WithLabelMatchers(
		labels.MustNewMatcher(labels.MatchEqual, "engine", "querier"))))

	require.NoError(t, mimir.WaitSumMetricsWithOptions(e2e.Equals(0), []string{"prometheus_engine_queries"}, e2e.WithLabelMatchers(
		labels.MustNewMatcher(labels.MatchEqual, "engine", "ruler"))))

	// Test Cleanup and Restart

	// Stop the running mimir
	require.NoError(t, mimir.Stop())

	// Restart Mimir with identical configs
	mimirRestarted := e2emimir.NewSingleBinaryWithConfigFile("mimir-restarted", mimirConfigFile, flags, "", 9009, 9095)
	require.NoError(t, s.StartAndWaitReady(mimirRestarted))

	// Wait until the user manager is created
	require.NoError(t, mimirRestarted.WaitSumMetrics(e2e.Equals(1), "cortex_ruler_managers_total"))
}

func TestRulerEvaluationDelay(t *testing.T) {
	s, err := e2e.NewScenario(networkName)
	require.NoError(t, err)
	defer s.Close()

	namespace := "ns"
	user := "fake"

	evaluationDelay := time.Minute * 5

	// Start dependencies.
	minio := e2edb.NewMinio(9000, bucketName)
	require.NoError(t, s.StartAndWaitReady(minio))

	flags := mergeFlags(
		BlocksStorageFlags(),
		map[string]string{
			"-ruler-storage.local.directory":   filepath.Join(e2e.ContainerSharedDir, "ruler_configs"),
			"-ruler.poll-interval":             "2s",
			"-ruler.rule-path":                 filepath.Join(e2e.ContainerSharedDir, "rule_tmp/"),
			"-ruler.evaluation-delay-duration": evaluationDelay.String(),
		},
	)

	// Start Mimir components.
	require.NoError(t, copyFileToSharedDir(s, "docs/sources/configuration/single-process-config-blocks.yaml", mimirConfigFile))
	require.NoError(t, writeFileToSharedDir(s, filepath.Join("ruler_configs", user, namespace), []byte(mimirRulerEvalStaleNanConfigYaml)))
	mimir := e2emimir.NewSingleBinaryWithConfigFile("mimir", mimirConfigFile, flags, "", 9009, 9095)
	require.NoError(t, s.StartAndWaitReady(mimir))

	// Create a client with the ruler address configured
	c, err := e2emimir.NewClient(mimir.HTTPEndpoint(), mimir.HTTPEndpoint(), "", mimir.HTTPEndpoint(), "")
	require.NoError(t, err)

	now := time.Now()

	// Generate series that includes stale nans
	var samplesToSend int = 10
	series := prompb.TimeSeries{
		Labels: []prompb.Label{
			{Name: "__name__", Value: "a_sometimes_stale_nan_series"},
			{Name: "instance", Value: "sometimes-stale"},
		},
	}
	series.Samples = make([]prompb.Sample, samplesToSend)
	posStale := 2

	// Create samples, that are delayed by the evaluation delay with increasing values.
	for pos := range series.Samples {
		series.Samples[pos].Timestamp = e2e.TimeToMilliseconds(now.Add(-evaluationDelay).Add(time.Duration(pos) * time.Second))
		series.Samples[pos].Value = float64(pos + 1)

		// insert staleness marker at the positions marked by posStale
		if pos == posStale {
			series.Samples[pos].Value = math.Float64frombits(value.StaleNaN)
		}
	}

	// Insert metrics
	res, err := c.Push([]prompb.TimeSeries{series})
	require.NoError(t, err)
	require.Equal(t, 200, res.StatusCode)

	// Get number of rule evaluations just after push
	ruleEvaluationsAfterPush, err := mimir.SumMetrics([]string{"cortex_prometheus_rule_evaluations_total"})
	require.NoError(t, err)

	// Wait until the rule is evaluated for the first time
	require.NoError(t, mimir.WaitSumMetrics(e2e.Greater(ruleEvaluationsAfterPush[0]), "cortex_prometheus_rule_evaluations_total"))

	// Query the timestamp of the latest result to ensure the evaluation is delayed
	result, err := c.Query("timestamp(stale_nan_eval)", now)
	require.NoError(t, err)
	require.Equal(t, model.ValVector, result.Type())

	vector := result.(model.Vector)
	require.Equal(t, 1, vector.Len(), "expect one sample returned")

	// 290 seconds gives 10 seconds of slack between the rule evaluation and the query
	// to account for CI latency, but ensures the latest evaluation was in the past.
	var maxDiff int64 = 290_000
	require.GreaterOrEqual(t, e2e.TimeToMilliseconds(time.Now())-int64(vector[0].Value)*1000, maxDiff)

	// Wait until all the pushed samples have been evaluated by the rule. This
	// ensures that rule results are successfully written even after a
	// staleness period.
	require.NoError(t, mimir.WaitSumMetrics(e2e.GreaterOrEqual(ruleEvaluationsAfterPush[0]+float64(samplesToSend)), "cortex_prometheus_rule_evaluations_total"))

	// query all results to verify rules have been evaluated correctly
	result, err = c.QueryRange("stale_nan_eval", now.Add(-evaluationDelay), now, time.Second)
	require.NoError(t, err)
	require.Equal(t, model.ValMatrix, result.Type())

	matrix := result.(model.Matrix)
	require.GreaterOrEqual(t, 1, matrix.Len(), "expect at least a series returned")

	// Iterate through the values recorded and ensure they exist as expected.
	inputPos := 0
	for _, m := range matrix {
		for _, v := range m.Values {
			// Skip values for stale positions
			if inputPos == posStale {
				inputPos++
			}

			expectedValue := model.SampleValue(2 * (inputPos + 1))
			require.Equal(t, expectedValue, v.Value)

			// Look for next value
			inputPos++

			// We have found all input values
			if inputPos >= len(series.Samples) {
				break
			}
		}
	}
	require.Equal(t, len(series.Samples), inputPos, "expect to have returned all evaluations")
}

func TestRulerSharding(t *testing.T) {
	const numRulesGroups = 100

	s, err := e2e.NewScenario(networkName)
	require.NoError(t, err)
	defer s.Close()

	// Generate multiple rule groups, with 1 rule each.
	ruleGroups := make([]rulefmt.RuleGroup, numRulesGroups)
	expectedNames := make([]string, numRulesGroups)
	for i := 0; i < numRulesGroups; i++ {
		var recordNode yaml.Node
		var exprNode yaml.Node

		recordNode.SetString(fmt.Sprintf("rule_%d", i))
		exprNode.SetString(strconv.Itoa(i))
		ruleName := fmt.Sprintf("test_%d", i)

		expectedNames[i] = ruleName
		ruleGroups[i] = rulefmt.RuleGroup{
			Name:     ruleName,
			Interval: 60,
			Rules: []rulefmt.RuleNode{{
				Record: recordNode,
				Expr:   exprNode,
			}},
		}
	}

	// Start dependencies.
	consul := e2edb.NewConsul()
	minio := e2edb.NewMinio(9000, rulestoreBucketName)
	require.NoError(t, s.StartAndWaitReady(consul, minio))

	// Configure the ruler.
	rulerFlags := mergeFlags(
		BlocksStorageFlags(),
		RulerFlags(),
		RulerShardingFlags(consul.NetworkHTTPEndpoint()),
		map[string]string{
			// Enable the bucket index so we can skip the initial bucket scan.
			"-blocks-storage.bucket-store.bucket-index.enabled": "true",
			// Disable rule group limit
			"-ruler.max-rule-groups-per-tenant": "0",
		},
	)

	// Start rulers.
	ruler1 := e2emimir.NewRuler("ruler-1", consul.NetworkHTTPEndpoint(), rulerFlags, "")
	ruler2 := e2emimir.NewRuler("ruler-2", consul.NetworkHTTPEndpoint(), rulerFlags, "")
	rulers := e2emimir.NewCompositeMimirService(ruler1, ruler2)
	require.NoError(t, s.StartAndWaitReady(ruler1, ruler2))

	// Upload rule groups to one of the rulers.
	c, err := e2emimir.NewClient("", "", "", ruler1.HTTPEndpoint(), "user-1")
	require.NoError(t, err)

	for _, ruleGroup := range ruleGroups {
		require.NoError(t, c.SetRuleGroup(ruleGroup, "test"))
	}

	// Wait until rulers have loaded all rules.
	require.NoError(t, rulers.WaitSumMetricsWithOptions(e2e.Equals(numRulesGroups), []string{"cortex_prometheus_rule_group_rules"}, e2e.WaitMissingMetrics))

	// Since rulers have loaded all rules, we expect that rules have been sharded
	// between the two rulers.
	require.NoError(t, ruler1.WaitSumMetrics(e2e.Less(numRulesGroups), "cortex_prometheus_rule_group_rules"))
	require.NoError(t, ruler2.WaitSumMetrics(e2e.Less(numRulesGroups), "cortex_prometheus_rule_group_rules"))

	// Fetch the rules and ensure they match the configured ones.
	actualGroups, err := c.GetPrometheusRules()
	require.NoError(t, err)

	var actualNames []string
	for _, group := range actualGroups {
		actualNames = append(actualNames, group.Name)
	}
	assert.ElementsMatch(t, expectedNames, actualNames)
}

func TestRulerAlertmanager(t *testing.T) {
	var namespaceOne = "test_/encoded_+namespace/?"
	ruleGroup := createTestRuleGroup(t)

	s, err := e2e.NewScenario(networkName)
	require.NoError(t, err)
	defer s.Close()

	// Start dependencies.
	consul := e2edb.NewConsul()
	minio := e2edb.NewMinio(9000, bucketName, rulestoreBucketName)
	require.NoError(t, s.StartAndWaitReady(consul, minio))

	// Have at least one alertmanager configuration.
	require.NoError(t, writeFileToSharedDir(s, "alertmanager_configs/user-1.yaml", []byte(mimirAlertmanagerUserConfigYaml)))

	// Start Alertmanagers.
	amFlags := mergeFlags(AlertmanagerFlags(), AlertmanagerLocalFlags())
	am1 := e2emimir.NewAlertmanager("alertmanager1", amFlags, "")
	am2 := e2emimir.NewAlertmanager("alertmanager2", amFlags, "")
	require.NoError(t, s.StartAndWaitReady(am1, am2))

	am1URL := "http://" + am1.HTTPEndpoint()
	am2URL := "http://" + am2.HTTPEndpoint()

	// Configure the ruler.
	rulerFlags := mergeFlags(
		BlocksStorageFlags(),
		RulerFlags(),
		map[string]string{
			// Connect the ruler to Alertmanagers
			"-ruler.alertmanager-url": strings.Join([]string{am1URL, am2URL}, ","),
		},
	)

	// Start Ruler.
	ruler := e2emimir.NewRuler("ruler", consul.NetworkHTTPEndpoint(), rulerFlags, "")
	require.NoError(t, s.StartAndWaitReady(ruler))

	// Create a client with the ruler address configured
	c, err := e2emimir.NewClient("", "", "", ruler.HTTPEndpoint(), "user-1")
	require.NoError(t, err)

	// Set the rule group into the ruler
	require.NoError(t, c.SetRuleGroup(ruleGroup, namespaceOne))

	// Wait until the user manager is created
	require.NoError(t, ruler.WaitSumMetrics(e2e.Equals(1), "cortex_ruler_managers_total"))

	//  Wait until we've discovered the alertmanagers.
	require.NoError(t, ruler.WaitSumMetricsWithOptions(e2e.Equals(2), []string{"cortex_prometheus_notifications_alertmanagers_discovered"}, e2e.WaitMissingMetrics))
}

func TestRulerAlertmanagerTLS(t *testing.T) {
	var namespaceOne = "test_/encoded_+namespace/?"
	ruleGroup := createTestRuleGroup(t)

	s, err := e2e.NewScenario(networkName)
	require.NoError(t, err)
	defer s.Close()

	// Start dependencies.
	consul := e2edb.NewConsul()
	minio := e2edb.NewMinio(9000, bucketName, rulestoreBucketName)
	require.NoError(t, s.StartAndWaitReady(consul, minio))

	// set the ca
	cert := ca.New("Ruler/Alertmanager Test")

	// Ensure the entire path of directories exist.
	require.NoError(t, os.MkdirAll(filepath.Join(s.SharedDir(), "certs"), os.ModePerm))

	require.NoError(t, cert.WriteCACertificate(filepath.Join(s.SharedDir(), caCertFile)))

	// server certificate
	require.NoError(t, cert.WriteCertificate(
		&x509.Certificate{
			Subject:     pkix.Name{CommonName: "client"},
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		},
		filepath.Join(s.SharedDir(), clientCertFile),
		filepath.Join(s.SharedDir(), clientKeyFile),
	))
	require.NoError(t, cert.WriteCertificate(
		&x509.Certificate{
			Subject:     pkix.Name{CommonName: "server"},
			DNSNames:    []string{"ruler.alertmanager-client"},
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		},
		filepath.Join(s.SharedDir(), serverCertFile),
		filepath.Join(s.SharedDir(), serverKeyFile),
	))

	// Have at least one alertmanager configuration.
	require.NoError(t, writeFileToSharedDir(s, "alertmanager_configs/user-1.yaml", []byte(mimirAlertmanagerUserConfigYaml)))

	// Start Alertmanagers.
	amFlags := mergeFlags(
		AlertmanagerFlags(),
		AlertmanagerLocalFlags(),
		getServerHTTPTLSFlags(),
	)
	am1 := e2emimir.NewAlertmanagerWithTLS("alertmanager1", amFlags, "")
	require.NoError(t, s.StartAndWaitReady(am1))

	// Configure the ruler.
	rulerFlags := mergeFlags(
		BlocksStorageFlags(),
		RulerFlags(),
		map[string]string{
			// Connect the ruler to the Alertmanager
			"-ruler.alertmanager-url": "https://" + am1.HTTPEndpoint(),
		},
	)

	// Start Ruler.
	ruler := e2emimir.NewRuler("ruler", consul.NetworkHTTPEndpoint(), rulerFlags, "")
	require.NoError(t, s.StartAndWaitReady(ruler))

	// Create a client with the ruler address configured
	c, err := e2emimir.NewClient("", "", "", ruler.HTTPEndpoint(), "user-1")
	require.NoError(t, err)

	// Set the rule group into the ruler
	require.NoError(t, c.SetRuleGroup(ruleGroup, namespaceOne))

	// Wait until the user manager is created
	require.NoError(t, ruler.WaitSumMetrics(e2e.Equals(1), "cortex_ruler_managers_total"))

	//  Wait until we've discovered the alertmanagers.
	require.NoError(t, ruler.WaitSumMetricsWithOptions(e2e.Equals(1), []string{"cortex_prometheus_notifications_alertmanagers_discovered"}, e2e.WaitMissingMetrics))
}

func TestRulerMetricsForInvalidQueries(t *testing.T) {
	s, err := e2e.NewScenario(networkName)
	require.NoError(t, err)
	defer s.Close()

	// Start dependencies.
	consul := e2edb.NewConsul()
	minio := e2edb.NewMinio(9000, bucketName, rulestoreBucketName)
	require.NoError(t, s.StartAndWaitReady(consul, minio))

	// Configure the ruler.
	flags := mergeFlags(
		BlocksStorageFlags(),
		RulerFlags(),
		map[string]string{
			// Enable the bucket index so we can skip the initial bucket scan.
			"-blocks-storage.bucket-store.bucket-index.enabled": "true",
			// Evaluate rules often, so that we don't need to wait for metrics to show up.
			"-ruler.evaluation-interval": "2s",
			"-ruler.poll-interval":       "2s",
			// No delay
			"-ruler.evaluation-delay-duration": "0",

			"-blocks-storage.tsdb.block-ranges-period":   "1h",
			"-blocks-storage.bucket-store.sync-interval": "1s",
			"-blocks-storage.tsdb.retention-period":      "2h",

			// We run single ingester only, no replication.
			"-distributor.replication-factor": "1",

			// Very low limit so that ruler hits it.
			"-querier.max-fetched-chunks-per-query": "5",
		},
	)

	const namespace = "test"
	const user = "user"

	distributor := e2emimir.NewDistributor("distributor", consul.NetworkHTTPEndpoint(), flags, "")
	ruler := e2emimir.NewRuler("ruler", consul.NetworkHTTPEndpoint(), flags, "")
	ingester := e2emimir.NewIngester("ingester", consul.NetworkHTTPEndpoint(), flags, "")
	require.NoError(t, s.StartAndWaitReady(distributor, ingester, ruler))

	// Wait until both the distributor and ruler have updated the ring. The querier will also watch
	// the store-gateway ring if blocks sharding is enabled.
	// The distributor should have 512 tokens for the ingester ring and 1 for the distributor ring
	require.NoError(t, distributor.WaitSumMetrics(e2e.Equals(512+1), "cortex_ring_tokens_total"))
	// Ruler will see 512 tokens for the ingester, and 128 for the ruler.
	require.NoError(t, ruler.WaitSumMetrics(e2e.Equals(512+128), "cortex_ring_tokens_total"))

	c, err := e2emimir.NewClient(distributor.HTTPEndpoint(), "", "", ruler.HTTPEndpoint(), user)
	require.NoError(t, err)

	// Push some series to Mimir -- enough so that we can hit some limits.
	for i := 0; i < 10; i++ {
		series, _ := generateSeries("metric", time.Now(), prompb.Label{Name: "foo", Value: fmt.Sprintf("%d", i)})

		res, err := c.Push(series)
		require.NoError(t, err)
		require.Equal(t, 200, res.StatusCode)
	}

	totalQueries, err := ruler.SumMetrics([]string{"cortex_ruler_queries_total"})
	require.NoError(t, err)

	// Verify that user-failures don't increase cortex_ruler_queries_failed_total
	for groupName, expression := range map[string]string{
		// Syntactically correct expression (passes check in ruler), but failing because of invalid regex. This fails in PromQL engine.
		"invalid_group": `label_replace(metric, "foo", "$1", "service", "[")`,

		// This one fails in querier code, because of limits.
		"too_many_chunks_group": `sum(metric)`,
	} {
		t.Run(groupName, func(t *testing.T) {
			require.NoError(t, c.SetRuleGroup(ruleGroupWithRule(groupName, "rule", expression), namespace))
			m := ruleGroupMatcher(user, namespace, groupName)

			// Wait until ruler has loaded the group.
			require.NoError(t, ruler.WaitSumMetricsWithOptions(e2e.Equals(1), []string{"cortex_prometheus_rule_group_rules"}, e2e.WithLabelMatchers(m), e2e.WaitMissingMetrics))

			// Wait until rule group has tried to evaluate the rule.
			require.NoError(t, ruler.WaitSumMetricsWithOptions(e2e.GreaterOrEqual(1), []string{"cortex_prometheus_rule_evaluations_total"}, e2e.WithLabelMatchers(m), e2e.WaitMissingMetrics))

			// Verify that evaluation of the rule failed.
			require.NoError(t, ruler.WaitSumMetricsWithOptions(e2e.GreaterOrEqual(1), []string{"cortex_prometheus_rule_evaluation_failures_total"}, e2e.WithLabelMatchers(m), e2e.WaitMissingMetrics))

			// But these failures were not reported as "failed queries"
			sum, err := ruler.SumMetrics([]string{"cortex_ruler_queries_failed_total"})
			require.NoError(t, err)
			require.Equal(t, float64(0), sum[0])

			// Delete rule before checkin "cortex_ruler_queries_total", as we want to reuse value for next test.
			require.NoError(t, c.DeleteRuleGroup(namespace, groupName))

			// Wait until ruler has unloaded the group. We don't use any matcher, so there should be no groups (in fact, metric disappears).
			require.NoError(t, ruler.WaitSumMetricsWithOptions(e2e.Equals(0), []string{"cortex_prometheus_rule_group_rules"}, e2e.SkipMissingMetrics))

			// Check that cortex_ruler_queries_total went up since last test.
			newTotalQueries, err := ruler.SumMetrics([]string{"cortex_ruler_queries_total"})
			require.NoError(t, err)
			require.Greater(t, newTotalQueries[0], totalQueries[0])

			// Remember totalQueries for next test.
			totalQueries = newTotalQueries
		})
	}

	// Now let's upload a non-failing rule, and make sure that it works.
	t.Run("real_error", func(t *testing.T) {
		const groupName = "good_rule"
		const expression = `sum(metric{foo=~"1|2"})`

		require.NoError(t, c.SetRuleGroup(ruleGroupWithRule(groupName, "rule", expression), namespace))
		m := ruleGroupMatcher(user, namespace, groupName)

		// Wait until ruler has loaded the group.
		require.NoError(t, ruler.WaitSumMetricsWithOptions(e2e.Equals(1), []string{"cortex_prometheus_rule_group_rules"}, e2e.WithLabelMatchers(m), e2e.WaitMissingMetrics))

		// Wait until rule group has tried to evaluate the rule, and succeeded.
		require.NoError(t, ruler.WaitSumMetricsWithOptions(e2e.GreaterOrEqual(1), []string{"cortex_prometheus_rule_evaluations_total"}, e2e.WithLabelMatchers(m), e2e.WaitMissingMetrics))
		require.NoError(t, ruler.WaitSumMetricsWithOptions(e2e.Equals(0), []string{"cortex_prometheus_rule_evaluation_failures_total"}, e2e.WithLabelMatchers(m), e2e.WaitMissingMetrics))

		// Still no failures.
		sum, err := ruler.SumMetrics([]string{"cortex_ruler_queries_failed_total"})
		require.NoError(t, err)
		require.Equal(t, float64(0), sum[0])

		// Now let's stop ingester, and recheck metrics. This should increase cortex_ruler_queries_failed_total failures.
		require.NoError(t, s.Stop(ingester))

		// We should start getting "real" failures now.
		require.NoError(t, ruler.WaitSumMetricsWithOptions(e2e.GreaterOrEqual(1), []string{"cortex_ruler_queries_failed_total"}))
	})
}

func TestRulerFederatedRules(t *testing.T) {
	type testCase struct {
		name               string
		tenantsWithMetrics []string // will generate series `metric{}` in each tenant
		ruleGroupOwner     string   // will create the federated rule under this tenant
		ruleExpression     string
		groupSourceTenants []string

		assertEvalResult func(model.Vector)
	}

	testCases := []testCase{
		{
			name:               "separate source tenants and destination tenant",
			tenantsWithMetrics: []string{"tenant-1", "tenant-2"},
			ruleGroupOwner:     "tenant-3",
			ruleExpression:     "count(sum_over_time(metric[1h]))",
			groupSourceTenants: []string{"tenant-1", "tenant-2"},
			assertEvalResult: func(evalResult model.Vector) {
				require.Len(t, evalResult, 1)
				require.Equal(t, evalResult[0].Value, model.SampleValue(2))
			},
		},
		{
			name:               "__tenant_id__ is added on all metrics for federated rules",
			tenantsWithMetrics: []string{"tenant-1", "tenant-2", "tenant-3"},
			ruleGroupOwner:     "tenant-3",
			ruleExpression:     "count(group by (__tenant_id__) (metric))", // count to number of different values of __tenant_id__
			groupSourceTenants: []string{"tenant-1", "tenant-2", "tenant-3"},
			assertEvalResult: func(evalResult model.Vector) {
				require.Len(t, evalResult, 1)
				require.Equal(t, evalResult[0].Value, model.SampleValue(3))
			},
		},
		{
			name:               "__tenant_id__ is present on metrics for federated rules when source tenants == owner",
			tenantsWithMetrics: []string{"tenant-1"},
			ruleGroupOwner:     "tenant-1",
			ruleExpression:     "count(group by (__tenant_id__) (metric))", // query to count to number of different values of __tenant_id__
			groupSourceTenants: []string{"tenant-1", "tenant-2", "tenant-3"},
			assertEvalResult: func(evalResult model.Vector) {
				require.Len(t, evalResult, 1)
				require.Equal(t, evalResult[0].Value, model.SampleValue(1))
			},
		},
	}

	s, err := e2e.NewScenario(networkName)
	require.NoError(t, err)
	t.Cleanup(s.Close)

	// Start dependencies.
	consul := e2edb.NewConsul()
	minio := e2edb.NewMinio(9000, bucketName, rulestoreBucketName)
	require.NoError(t, s.StartAndWaitReady(minio, consul))

	flags := mergeFlags(
		BlocksStorageFlags(),
		RulerFlags(),
		map[string]string{
			"-ruler.tenant-federation.enabled": "true",
			"-tenant-federation.enabled":       "true",
			"-distributor.replication-factor":  "1",
		},
	)

	// Start up services
	distributor := e2emimir.NewDistributor("distributor", consul.NetworkHTTPEndpoint(), flags, "")
	ruler := e2emimir.NewRuler("ruler", consul.NetworkHTTPEndpoint(), flags, "")
	ingester := e2emimir.NewIngester("ingester", consul.NetworkHTTPEndpoint(), flags, "")
	querier := e2emimir.NewQuerier("querier", consul.NetworkHTTPEndpoint(), flags, "")
	require.NoError(t, s.StartAndWaitReady(distributor, ingester, ruler, querier))

	// Wait until both the distributor and ruler are ready
	// The distributor should have 512 tokens for the ingester ring and 1 for the distributor rin
	require.NoError(t, distributor.WaitSumMetrics(e2e.Equals(512+1), "cortex_ring_tokens_total"))
	// Ruler will see 512 tokens from ingester, and 128 tokens from itself.
	require.NoError(t, ruler.WaitSumMetrics(e2e.Equals(512+128), "cortex_ring_tokens_total"))

	// isolatedTestCase prefixes all the tenant IDs in the testCase with "run-<n>-"
	// so we can ensure that the tenants in different test cases don't overlap
	isolatedTestCase := func(tc testCase, n int) testCase {
		prefixID := func(tenantID string) string {
			return fmt.Sprintf("run-%d-%s", n, tenantID)
		}

		tc.ruleGroupOwner = prefixID(tc.ruleGroupOwner)
		for i, t := range tc.tenantsWithMetrics {
			tc.tenantsWithMetrics[i] = prefixID(t)
		}
		for i, t := range tc.groupSourceTenants {
			tc.groupSourceTenants[i] = prefixID(t)
		}
		return tc
	}

	for i, tc := range testCases {
		tc = isolatedTestCase(tc, i)
		t.Run(tc.name, func(t *testing.T) {
			// Generate some series under different tenants
			sampleTime := time.Now()
			for _, tenantID := range tc.tenantsWithMetrics {
				client, err := e2emimir.NewClient(distributor.HTTPEndpoint(), "", "", "", tenantID)
				require.NoError(t, err)

				series, _ := generateSeries("metric", sampleTime)

				res, err := client.Push(series)
				require.NoError(t, err)
				require.Equal(t, 200, res.StatusCode)
			}

			// Create a client as owner tenant to upload groups and then make assertions
			c, err := e2emimir.NewClient(distributor.HTTPEndpoint(), querier.HTTPEndpoint(), "", ruler.HTTPEndpoint(), tc.ruleGroupOwner)
			require.NoError(t, err)

			// Obtain total series before rule evaluation
			totalSeriesBeforeEval, err := ingester.SumMetrics([]string{"cortex_ingester_memory_series"})
			require.NoError(t, err)

			// Create federated rule group
			namespace := "test_namespace"
			ruleName := "federated_rule_name"
			g := ruleGroupWithRule("x", ruleName, tc.ruleExpression)
			g.Interval = model.Duration(time.Second / 4)
			g.SourceTenants = tc.groupSourceTenants
			require.NoError(t, c.SetRuleGroup(g, namespace))

			// Wait until another user manager is created (i is one more since last time). This means the rule groups is loaded.
			require.NoError(t, ruler.WaitSumMetrics(e2e.Equals(float64(i+1)), "cortex_ruler_managers_total"))

			// Check to ensure the rules running in the ruler match what was set
			rgs, err := c.GetRuleGroups()
			retrievedNamespace, exists := rgs[namespace]
			require.NoError(t, err)
			require.True(t, exists)
			require.Len(t, retrievedNamespace, 1)
			require.ElementsMatch(t, retrievedNamespace[0].SourceTenants, tc.groupSourceTenants)

			// Wait until rule evaluation resulting series had been pushed
			require.NoError(t, ingester.WaitSumMetrics(e2e.Greater(totalSeriesBeforeEval[0]), "cortex_ingester_memory_series"))

			result, err := c.Query(ruleName, time.Now())
			require.NoError(t, err)
			tc.assertEvalResult(result.(model.Vector))
		})
	}
}

func ruleGroupMatcher(user, namespace, groupName string) *labels.Matcher {
	return labels.MustNewMatcher(labels.MatchEqual, "rule_group", fmt.Sprintf("/rules/%s/%s;%s", user, namespace, groupName))
}

func ruleGroupWithRule(groupName string, ruleName string, expression string) rulefmt.RuleGroup {
	// Prepare rule group with invalid rule.
	var recordNode = yaml.Node{}
	var exprNode = yaml.Node{}

	recordNode.SetString(ruleName)
	exprNode.SetString(expression)

	return rulefmt.RuleGroup{
		Name:     groupName,
		Interval: 10,
		Rules: []rulefmt.RuleNode{{
			Record: recordNode,
			Expr:   exprNode,
		}},
	}
}

func createTestRuleGroup(t *testing.T) rulefmt.RuleGroup {
	t.Helper()

	var (
		recordNode = yaml.Node{}
		exprNode   = yaml.Node{}
	)

	recordNode.SetString("test_rule")
	exprNode.SetString("up")
	return rulefmt.RuleGroup{
		Name:     "test_encoded_+\"+group_name/?",
		Interval: 100,
		Rules: []rulefmt.RuleNode{
			{
				Record: recordNode,
				Expr:   exprNode,
			},
		},
	}
}
