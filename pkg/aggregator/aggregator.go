package aggregator

import (
	"errors"
	"fmt"

	"miniav/pkg/protocol"
)

type Verdict string

const (
	VerdictClean        Verdict = "CLEAN"
	VerdictMalware      Verdict = "MALWARE"
	VerdictInconclusive Verdict = "INCONCLUSIVE"
)

func Aggregate(verdicts []protocol.Verdict) (Verdict, error) {
	if len(verdicts) == 0 {
		return "", errors.New("aggregation requires at least one verdict")
	}

	hasMalware := false
	hasFailure := false
	for index, verdict := range verdicts {
		switch verdict {
		case protocol.VerdictClean:
		case protocol.VerdictMalware:
			hasMalware = true
		case protocol.VerdictError, protocol.VerdictTimeout, protocol.VerdictUnavailable:
			hasFailure = true
		default:
			return "", fmt.Errorf("verdict %d = %q is invalid", index, verdict)
		}
	}

	if hasMalware {
		return VerdictMalware, nil
	}
	if hasFailure {
		return VerdictInconclusive, nil
	}
	return VerdictClean, nil
}
