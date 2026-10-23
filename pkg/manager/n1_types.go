package manager

import (
	"encoding/json"
	"llamactl/pkg/database"
	"llamactl/pkg/instance"
)

type n1Envelope struct {
	Boot     string  `json:"boot_id"`
	Revision *uint64 `json:"expected_revision"`
	Key      string  `json:"idempotency_key"`
}
type N1Candidate struct {
	ID             string                     `json:"candidate_id"`
	ArtifactID     string                     `json:"artifact_id"`
	ArtifactDigest string                     `json:"artifact_manifest_digest"`
	Deployment     map[string]json.RawMessage `json:"deployment"`
	Request        instance.RequestContract   `json:"request"`
}
type n1Evaluation struct {
	n1Envelope
	Candidates     []N1Candidate       `json:"candidates"`
	WallSeconds    int                 `json:"wall_seconds"`
	AttemptSeconds int                 `json:"attempt_seconds"`
	CleanupSeconds int                 `json:"cleanup_seconds"`
	Budget         database.RunBudget  `json:"budget"`
	Notice         string              `json:"notice_ref"`
	NotBefore      int64               `json:"not_before"`
	FixtureVersion string              `json:"fixture_version"`
	Purpose        string              `json:"purpose"`
	Ranges         map[string][2]int64 `json:"parameter_ranges"`
	Retain         bool                `json:"retain_outputs"`
}
type n1Attempt struct {
	n1Envelope
	Seq           int             `json:"seq"`
	Candidate     string          `json:"candidate_id"`
	Fixture       string          `json:"fixture_id"`
	FixtureDigest string          `json:"fixture_digest"`
	Messages      json.RawMessage `json:"messages"`
}
type n1Use struct {
	n1Envelope
	RecipeID    string `json:"recipe_id"`
	Version     int    `json:"version"`
	IdleSeconds int    `json:"idle_seconds"`
}
type RecipeEvidence struct {
	RunID       string `json:"run_id"`
	FindingsSeq int64  `json:"findings_seq"`
	Digest      string `json:"findings_digest"`
	Candidate   string `json:"candidate_id"`
}
type N1Recipe struct {
	ID              string                     `json:"recipe_id,omitempty"`
	ExpectedVersion int                        `json:"expected_version,omitempty"`
	Description     string                     `json:"description"`
	Tags            []string                   `json:"task_tags"`
	ArtifactID      string                     `json:"artifact_id"`
	ArtifactDigest  string                     `json:"artifact_manifest_digest"`
	Engine          string                     `json:"engine_identity"`
	Deployment      map[string]json.RawMessage `json:"deployment"`
	Request         instance.RequestContract   `json:"request"`
	Conclusion      string                     `json:"conclusion_level"`
	Evidence        *RecipeEvidence            `json:"evidence"`
	PortableMembers []string                   `json:"portable_members,omitempty"`
	Foreign         bool                       `json:"foreign,omitempty"`
	SourceRef       string                     `json:"source_ref,omitempty"`
}
type n1Receipt struct {
	Attempt         int                      `json:"attempt_id"`
	Candidate       string                   `json:"candidate_id"`
	ArtifactDigest  string                   `json:"artifact_manifest_digest"`
	ParameterDigest string                   `json:"parameter_digest"`
	FixtureDigest   string                   `json:"fixture_digest"`
	InputDigest     string                   `json:"input_digest"`
	State           string                   `json:"state"`
	Quality         string                   `json:"quality"`
	ErrorCode       string                   `json:"error_code,omitempty"`
	Cleanup         string                   `json:"cleanup_state"`
	OutputDigest    string                   `json:"output_digest"`
	OutputBytes     int                      `json:"output_bytes"`
	Generation      instance.N1Generation    `json:"generation"`
	Identity        instance.ControlSnapshot `json:"identity"`
}
type N1FindingsCandidate struct {
	Candidate       string    `json:"candidate_id"`
	ArtifactDigest  string    `json:"artifact_manifest_digest"`
	ParameterDigest string    `json:"parameter_digest"`
	Attempts        []int     `json:"tested_attempt_ids"`
	Quality         string    `json:"quality_result"`
	Decision        string    `json:"decision"`
	Reason          string    `json:"reason"`
	Tested          []int     `json:"tested_range"`
	Untested        []int     `json:"untested_range"`
	Capacity        *[2]int64 `json:"capacity_interval"`
	Limitations     []string  `json:"limitations"`
}
type N1Findings struct {
	Schema         int                   `json:"schema_version"`
	RunID          string                `json:"run_id"`
	Version        int                   `json:"analysis_version"`
	Key            string                `json:"idempotency_key"`
	ManifestDigest string                `json:"manifest_digest"`
	ReceiptDigest  string                `json:"attempt_receipt_set_digest"`
	Policy         string                `json:"policy_version"`
	Evaluator      string                `json:"evaluator_version"`
	StopReason     string                `json:"stop_reason"`
	Conclusion     string                `json:"conclusion_level"`
	Supersedes     *int64                `json:"supersedes,omitempty"`
	Candidates     []N1FindingsCandidate `json:"candidates"`
}
