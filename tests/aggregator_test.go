package tests

import (
	"testing"

	"miniav/pkg/aggregator"
	"miniav/pkg/protocol"
)

func TestAggregateAppliesAnyMaliciousPolicy(t *testing.T) {
	tests := []struct {
		name     string
		verdicts []protocol.Verdict
		want     aggregator.Verdict
	}{
		{name: "single clean", verdicts: []protocol.Verdict{protocol.VerdictClean}, want: aggregator.VerdictClean},
		{name: "all clean", verdicts: []protocol.Verdict{protocol.VerdictClean, protocol.VerdictClean}, want: aggregator.VerdictClean},
		{name: "error", verdicts: []protocol.Verdict{protocol.VerdictError}, want: aggregator.VerdictInconclusive},
		{name: "timeout", verdicts: []protocol.Verdict{protocol.VerdictTimeout}, want: aggregator.VerdictInconclusive},
		{name: "unavailable", verdicts: []protocol.Verdict{protocol.VerdictUnavailable}, want: aggregator.VerdictInconclusive},
		{name: "clean and failures", verdicts: []protocol.Verdict{protocol.VerdictClean, protocol.VerdictError, protocol.VerdictTimeout, protocol.VerdictUnavailable}, want: aggregator.VerdictInconclusive},
		{name: "malware first", verdicts: []protocol.Verdict{protocol.VerdictMalware, protocol.VerdictClean, protocol.VerdictError, protocol.VerdictTimeout, protocol.VerdictUnavailable}, want: aggregator.VerdictMalware},
		{name: "malware last", verdicts: []protocol.Verdict{protocol.VerdictClean, protocol.VerdictError, protocol.VerdictTimeout, protocol.VerdictUnavailable, protocol.VerdictMalware}, want: aggregator.VerdictMalware},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := aggregator.Aggregate(test.verdicts)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("Aggregate() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestAggregateRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name     string
		verdicts []protocol.Verdict
	}{
		{name: "nil"},
		{name: "empty", verdicts: []protocol.Verdict{}},
		{name: "pending", verdicts: []protocol.Verdict{protocol.VerdictClean, ""}},
		{name: "combined verdict", verdicts: []protocol.Verdict{protocol.Verdict("INCONCLUSIVE")}},
		{name: "unknown", verdicts: []protocol.Verdict{protocol.Verdict("UNKNOWN")}},
		{name: "malware does not hide invalid", verdicts: []protocol.Verdict{protocol.VerdictMalware, protocol.Verdict("UNKNOWN")}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := aggregator.Aggregate(test.verdicts)
			if err == nil {
				t.Fatalf("Aggregate() = %q, want an error", got)
			}
			if got != "" {
				t.Fatalf("Aggregate() verdict = %q after error, want empty", got)
			}
		})
	}
}
