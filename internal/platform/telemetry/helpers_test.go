package telemetry_test

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// must unwraps constructor results for constant test fixtures; a failure is
// a broken test, not a behaviour under test.
func must[T any](value T, err error) T {
	if err != nil {
		panic(err)
	}
	return value
}

func shutdown(t *testing.T, closer interface{ Shutdown(context.Context) error }) {
	t.Helper()
	if err := closer.Shutdown(context.Background()); err != nil {
		t.Errorf("shutdown: %v", err)
	}
}

// metricValue returns the counter value (or histogram sample count) of the
// series of family name whose labels include want, or 0 when absent.
func metricValue(t *testing.T, gatherer prometheus.Gatherer, name string, want map[string]string) float64 {
	t.Helper()
	families, err := gatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	total := 0.0
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			if !labelsMatch(metric, want) {
				continue
			}
			switch {
			case metric.GetCounter() != nil:
				total += metric.GetCounter().GetValue()
			case metric.GetHistogram() != nil:
				total += float64(metric.GetHistogram().GetSampleCount())
			}
		}
	}
	return total
}

func labelsMatch(metric *dto.Metric, want map[string]string) bool {
	matched := 0
	for _, pair := range metric.GetLabel() {
		if value, ok := want[pair.GetName()]; ok {
			if value != pair.GetValue() {
				return false
			}
			matched++
		}
	}
	return matched == len(want)
}

// rollbackGuardianFamilies maps every rollback_guardian_* family to its
// sorted label names, and returns every label value seen.
func rollbackGuardianFamilies(t *testing.T, gatherer prometheus.Gatherer) (map[string]string, []string) {
	t.Helper()
	families, err := gatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	labels := map[string]string{}
	var values []string
	for _, family := range families {
		if !strings.HasPrefix(family.GetName(), "rollback_guardian_") {
			continue
		}
		for _, metric := range family.GetMetric() {
			var names []string
			for _, pair := range metric.GetLabel() {
				names = append(names, pair.GetName())
				values = append(values, pair.GetValue())
			}
			sort.Strings(names)
			labels[family.GetName()] = strings.Join(names, ",")
		}
	}
	return labels, values
}
