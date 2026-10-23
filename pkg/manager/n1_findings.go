package manager

import (
	"context"
	"encoding/json"
	"llamactl/pkg/database"
	"reflect"
)

func (im *instanceManager) n1Analyze(ctx context.Context, n *n1Coordinator, r database.ExecutionRun, body []byte) (int, any, error) {
	var f N1Findings
	if err := n1Decode(body, &f); err != nil {
		return 0, nil, err
	}
	if r.Kind != "evaluation" || r.FrozenAt == nil || f.Schema != 1 || f.RunID != r.ID || f.Version < 1 || f.Key == "" || f.Policy == "" || f.Evaluator == "" || f.ManifestDigest != database.N1Digest(r.Manifest) || f.Conclusion != "synthetic_only" {
		return 0, nil, protocolError("invalid_findings")
	}
	if f.StopReason != "declared_coverage" && f.StopReason != "no_practical_gain" && f.StopReason != "budget_exhausted" && f.StopReason != "cancelled" && f.StopReason != "deadline_exceeded" {
		return 0, nil, protocolError("invalid_findings")
	}
	events, err := n.store.Events(ctx, r.ID)
	if err != nil {
		return 0, nil, err
	}
	if f.ReceiptDigest != receiptSetDigest(events) {
		return 0, nil, protocolError("receipt_set_mismatch")
	}
	var eval n1Evaluation
	if json.Unmarshal(r.Payload, &eval) != nil {
		return 0, nil, protocolError("invalid_findings")
	}
	if len(f.Candidates) != len(eval.Candidates) {
		return 0, nil, protocolError("candidate_coverage_mismatch")
	}
	receipts := map[int]n1Receipt{}
	dispatched := map[int]string{}
	for _, e := range events {
		switch e.Type {
		case "dispatch":
			var d struct {
				Seq       int    `json:"seq"`
				Candidate string `json:"candidate_id"`
			}
			_ = json.Unmarshal(e.Payload, &d)
			dispatched[d.Seq] = d.Candidate
		case "receipt":
			var v n1Receipt
			if json.Unmarshal(e.Payload, &v) != nil {
				return 0, nil, protocolError("invalid_findings")
			}
			receipts[v.Attempt] = v
		}
	}
	seen := map[string]bool{}
	used := map[int]bool{}
	for _, fc := range f.Candidates {
		var candidate *N1Candidate
		for idx := range eval.Candidates {
			if eval.Candidates[idx].ID == fc.Candidate {
				candidate = &eval.Candidates[idx]
				break
			}
		}
		if candidate == nil || seen[fc.Candidate] || fc.ArtifactDigest != candidate.ArtifactDigest || fc.ParameterDigest != candidateDigest(*candidate) || fc.Reason == "" || len(fc.Reason) > 4096 || fc.Capacity != nil || !reflect.DeepEqual(fc.Tested, fc.Attempts) {
			return 0, nil, protocolError("invalid_findings")
		}
		seen[fc.Candidate] = true
		if fc.Decision != "shortlisted" && fc.Decision != "eliminated" && fc.Decision != "untested" {
			return 0, nil, protocolError("invalid_findings")
		}
		if len(fc.Attempts) == 0 {
			if fc.Quality != "untested" || fc.Decision != "untested" {
				return 0, nil, protocolError("invalid_findings")
			}
		} else {
			if fc.Quality != "synthetic_only" && fc.Quality != "failed" && fc.Quality != "unknown" {
				return 0, nil, protocolError("invalid_findings")
			}
		}
		for _, id := range fc.Attempts {
			receipt, ok := receipts[id]
			if !ok || used[id] || dispatched[id] != fc.Candidate || receipt.Candidate != fc.Candidate || receipt.ParameterDigest != fc.ParameterDigest || receipt.ArtifactDigest != fc.ArtifactDigest {
				return 0, nil, protocolError("invalid_attempt_reference")
			}
			used[id] = true
			if fc.Quality == "synthetic_only" && (receipt.State != "succeeded" || receipt.Generation.Truncated || receipt.OutputBytes == 0 || receipt.Quality != "synthetic_only") {
				return 0, nil, protocolError("invalid_quality_claim")
			}
		}
	}
	if len(used) != len(receipts) {
		return 0, nil, protocolError("receipt_coverage_mismatch")
	}
	if f.Version == 1 && f.Supersedes != nil {
		return 0, nil, protocolError("invalid_findings")
	}
	if f.Version > 1 {
		valid := false
		for _, e := range events {
			if e.AnalysisVersion != nil && *e.AnalysisVersion == f.Version-1 && f.Supersedes != nil && e.Seq == *f.Supersedes {
				valid = true
			}
		}
		if !valid {
			return 0, nil, protocolError("invalid_supersedes")
		}
	}
	event, _, err := n.store.Finalize(ctx, r.ID, f.Version, n1JSON(f))
	return 200, event, err
}
