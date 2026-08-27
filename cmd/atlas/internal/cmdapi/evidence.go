// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

package cmdapi

// This is the default local evidence store for the canary workflow. It is an
// intentionally small filesystem implementation: the directory is expected to
// be exported/read-only to verifiers by the deployment layer. Records are
// write-once, contain only the configs protocol fields, and never contain SQL
// or connection material.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const evidenceVersion = 1

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
var redactedTargetPattern = regexp.MustCompile(`^target-[0-9a-f]{12}$`)

type AtlasEvidence struct {
	ContractVersion          int               `json:"contract_version"`
	RunID                    string            `json:"run_id"`
	Identity                 EvidenceIdentity  `json:"identity"`
	ArtifactDigests          EvidenceArtifacts `json:"artifact_digests"`
	Status                   string            `json:"status"`
	GlobalResult             string            `json:"global_result"`
	Cohort                   EvidenceCohort    `json:"cohort"`
	TenantResultCounts       EvidenceCounts    `json:"tenant_result_counts"`
	AtlasGeneration          int64             `json:"atlas_generation"`
	ObservedGeneration       int64             `json:"observed_generation"`
	NormalizedSchemaIdentity string            `json:"normalized_schema_identity"`
	CurrentSchemaIdentity    string            `json:"current_schema_identity,omitempty"`
	NoTenantFanout           bool              `json:"no_tenant_fanout,omitempty"`
	OriginalApprovedTargets  []string          `json:"original_approved_targets,omitempty"`
	OriginalFailedTargets    []string          `json:"original_failed_targets,omitempty"`
	RetryOf                  string            `json:"retry_of,omitempty"`
	CreatedAt                time.Time         `json:"created_at"`
	ExpiresAt                time.Time         `json:"expires_at"`
}
type EvidenceIdentity struct {
	ImageDigest    string `json:"image_digest"`
	ContractDigest string `json:"contract_digest"`
	Environment    string `json:"environment"`
	CohortID       string `json:"cohort_id"`
}
type EvidenceArtifacts struct {
	Global string `json:"global"`
	Tenant string `json:"tenant"`
}
type EvidenceCohort struct {
	ID              string   `json:"id"`
	EligibleCount   int      `json:"eligible_count"`
	ApprovedTargets []string `json:"approved_targets"`
	FailedTargets   []string `json:"failed_targets"`
}
type EvidenceCounts struct {
	Eligible  int `json:"eligible"`
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
}

type FileEvidenceStore struct {
	Root string
}

func schemaEvidenceCmd() *cobra.Command {
	var root, runID string
	cmd := &cobra.Command{Use: "evidence", Short: "Inspect immutable Atlas readiness evidence (read-only)"}
	inspect := &cobra.Command{Use: "inspect", Args: cobra.NoArgs, RunE: RunE(func(cmd *cobra.Command, _ []string) error {
		if root == "" || runID == "" {
			return errors.New("--evidence-dir and --run-id are required")
		}
		e, err := (FileEvidenceStore{Root: root}).Inspect(runID)
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(e)
	})}
	inspect.Flags().StringVar(&root, "evidence-dir", "", "evidence store directory")
	inspect.Flags().StringVar(&runID, "run-id", "", "evidence run identifier")
	cmd.AddCommand(inspect)
	var f fanoutFlags
	publish := &cobra.Command{Use: "publish --report FILE", Short: "Publish evidence from a completed fan-out report without applying", Args: cobra.NoArgs, RunE: RunE(func(cmd *cobra.Command, args []string) error {
		if f.report == "" || f.evidenceDir == "" {
			return errors.New("--report and --evidence-dir are required")
		}
		if err := validateEvidenceInputs(f); err != nil {
			return err
		}
		b, err := os.ReadFile(f.report)
		if err != nil {
			return fmt.Errorf("read completed fan-out report: %w", err)
		}
		var report fanoutReport
		if err := json.Unmarshal(b, &report); err != nil {
			return fmt.Errorf("decode completed fan-out report: %w", err)
		}
		if report.Version != "atlas.schema.apply.fanout/v1" {
			return fmt.Errorf("unsupported fan-out report version %q", report.Version)
		}
		if report.RunID == "" {
			return errors.New("completed fan-out report is missing run id")
		}
		for _, p := range report.Plans {
			if p.Status == "" || p.Status == "planned" {
				return errors.New("cannot publish evidence from an incomplete fan-out report")
			}
		}
		e, err := evidenceFromFanout(f, report, report.Plans)
		if err != nil {
			return err
		}
		path, err := (FileEvidenceStore{Root: f.evidenceDir}).Publish(cmd.Context(), e)
		if err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), path)
		return nil
	})}
	publish.Flags().StringVar(&f.report, "report", "", "completed fan-out report")
	publish.Flags().StringVar(&f.evidenceDir, "evidence-dir", "", "evidence store directory")
	publish.Flags().StringVar(&f.releaseImageDigest, "release-image-digest", "", "immutable release image digest")
	publish.Flags().StringVar(&f.contractDigest, "contract-digest", "", "schema contract digest")
	publish.Flags().IntVar(&f.contractVersion, "contract-version", 0, "schema contract version")
	publish.Flags().StringVar(&f.contractDescriptor, "contract-descriptor", "", "canonical release-contract JSON")
	publish.Flags().StringVar(&f.globalArtifactDigest, "global-artifact-digest", "", "global artifact digest")
	publish.Flags().StringVar(&f.tenantArtifactDigest, "tenant-artifact-digest", "", "tenant artifact digest")
	publish.Flags().StringVar(&f.normalizedSchemaIdentity, "normalized-schema-identity", "", "normalized Atlas schema identity")
	publish.Flags().StringVar(&f.currentSchemaIdentity, "current-schema-identity", "", "current Atlas schema identity")
	publish.Flags().Int64Var(&f.atlasGeneration, "atlas-generation", 0, "Atlas resource generation")
	publish.Flags().Int64Var(&f.observedGeneration, "observed-generation", 0, "Atlas observed generation")
	publish.Flags().BoolVar(&f.noTenantFanout, "no-tenant-fanout", false, "explicitly declare no tenant fan-out")
	cmd.AddCommand(publish)
	var cleanupRoot string
	var protected []string
	cleanup := &cobra.Command{Use: "cleanup", Short: "Remove expired evidence, preserving protected run IDs", Args: cobra.NoArgs, RunE: RunE(func(cmd *cobra.Command, _ []string) error {
		if cleanupRoot == "" {
			return errors.New("--evidence-dir is required")
		}
		keep := make(map[string]bool, len(protected))
		for _, id := range protected {
			keep[id] = true
		}
		return (FileEvidenceStore{Root: cleanupRoot}).Cleanup(cmd.Context(), time.Now().UTC(), keep)
	})}
	cleanup.Flags().StringVar(&cleanupRoot, "evidence-dir", "", "evidence store directory")
	cleanup.Flags().StringSliceVar(&protected, "protected-run-id", nil, "run ID referenced by a pending promotion (repeatable)")
	cmd.AddCommand(cleanup)
	return cmd
}

func validateEvidenceInputs(f fanoutFlags) error {
	if !digestPattern.MatchString(f.releaseImageDigest) || !digestPattern.MatchString(f.contractDigest) {
		return errors.New("--evidence-dir requires valid sha256: release and contract digests")
	}
	if f.contractVersion != evidenceVersion {
		return fmt.Errorf("unsupported evidence contract version %d", f.contractVersion)
	}
	if f.globalArtifactDigest == "" || f.tenantArtifactDigest == "" {
		return errors.New("--evidence-dir requires global and tenant artifact digests")
	}
	for _, d := range []string{f.globalArtifactDigest, f.tenantArtifactDigest} {
		if len(d) != 64 || strings.Trim(d, "0123456789abcdef") != "" {
			return errors.New("artifact digests must be lowercase hexadecimal")
		}
	}
	b, err := os.ReadFile(f.contractDescriptor)
	if err != nil {
		return fmt.Errorf("read contract descriptor: %w", err)
	}
	var descriptor any
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.UseNumber()
	if err := decoder.Decode(&descriptor); err != nil {
		return fmt.Errorf("decode contract descriptor: %w", err)
	}
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(descriptor); err != nil {
		return fmt.Errorf("canonicalize contract descriptor: %w", err)
	}
	canonical := bytes.TrimSuffix(encoded.Bytes(), []byte("\n"))
	h := sha256.Sum256(canonical)
	if "sha256:"+hex.EncodeToString(h[:]) != f.contractDigest {
		return errors.New("contract descriptor digest does not match --contract-digest")
	}
	return nil
}

func newRunID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func evidenceFromFanout(f fanoutFlags, report fanoutReport, plans []fanoutPlan) (AtlasEvidence, error) {
	run := report.RunID
	if run == "" {
		var err error
		run, err = newRunID()
		if err != nil {
			return AtlasEvidence{}, err
		}
	}
	now := time.Now().UTC()
	cohortID := report.Cohort
	approvedTargets := f.originalApprovedTargets
	if f.originalCohort != "" {
		cohortID = f.originalCohort
	}
	if len(approvedTargets) == 0 {
		for _, p := range plans {
			approvedTargets = append(approvedTargets, p.Target)
		}
	}
	if len(plans) == 0 && !f.noTenantFanout {
		return AtlasEvidence{}, errors.New("empty cohort requires --no-tenant-fanout")
	}
	if approvedTargets == nil {
		approvedTargets = []string{}
	}
	e := AtlasEvidence{ContractVersion: f.contractVersion, RunID: run, Identity: EvidenceIdentity{f.releaseImageDigest, f.contractDigest, "canary", cohortID}, ArtifactDigests: EvidenceArtifacts{f.globalArtifactDigest, f.tenantArtifactDigest}, Cohort: EvidenceCohort{ID: cohortID, ApprovedTargets: approvedTargets, FailedTargets: []string{}}, AtlasGeneration: f.atlasGeneration, ObservedGeneration: f.observedGeneration, NormalizedSchemaIdentity: f.normalizedSchemaIdentity, CurrentSchemaIdentity: f.currentSchemaIdentity, NoTenantFanout: f.noTenantFanout, OriginalApprovedTargets: append([]string(nil), f.originalApprovedTargets...), OriginalFailedTargets: append([]string(nil), f.originalFailedTargets...), RetryOf: f.retryOf, CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour)}
	for _, p := range plans {
		switch p.Status {
		case "success", "no-op":
			e.TenantResultCounts.Succeeded++
		case "failed", "drifted", "canceled", "cancelled":
			e.TenantResultCounts.Failed++
			e.Cohort.FailedTargets = append(e.Cohort.FailedTargets, p.Target)
		case "planned", "":
			return AtlasEvidence{}, errors.New("cannot publish evidence for an incomplete target")
		default:
			return AtlasEvidence{}, fmt.Errorf("cannot publish evidence for unknown target status %q", p.Status)
		}
	}
	e.Cohort.EligibleCount = len(e.Cohort.ApprovedTargets)
	e.TenantResultCounts.Eligible = e.Cohort.EligibleCount
	if f.originalCohort != "" {
		// A retry report contains only the previously failed subset. Preserve a
		// complete cohort result by carrying forward the targets that already
		// succeeded in the original approved batch.
		e.TenantResultCounts.Succeeded = e.TenantResultCounts.Eligible - e.TenantResultCounts.Failed
	}
	var drifted, cancelled bool
	for _, p := range plans {
		drifted = drifted || p.Status == "drifted"
		cancelled = cancelled || p.Status == "canceled" || p.Status == "cancelled"
	}
	switch {
	case cancelled && e.TenantResultCounts.Succeeded == 0 && !drifted:
		e.Status, e.GlobalResult = "cancelled", "cancelled"
	case drifted && e.TenantResultCounts.Succeeded == 0 && !cancelled:
		e.Status, e.GlobalResult = "drift", "drift"
	case e.TenantResultCounts.Failed > 0:
		e.Status, e.GlobalResult = "partial_failure", "partial_failure"
	case e.TenantResultCounts.Succeeded == 0 || allNoOp(plans):
		e.Status, e.GlobalResult = "no-op", "no-op"
	default:
		e.Status, e.GlobalResult = "success", "success"
	}
	return e, nil
}
func allNoOp(ps []fanoutPlan) bool {
	for _, p := range ps {
		if p.Status != "no-op" {
			return false
		}
	}
	return len(ps) > 0
}

func (s FileEvidenceStore) Publish(ctx context.Context, e AtlasEvidence) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := validateEvidence(e); err != nil {
		return "", err
	}
	if s.Root == "" {
		return "", errors.New("evidence store directory is empty")
	}
	prefix := e.Identity.ImageDigest + "|" + e.Identity.ContractDigest + "|" + fmt.Sprint(e.ContractVersion)
	h := sha256.Sum256([]byte(prefix))
	dir := filepath.Join(s.Root, hex.EncodeToString(h[:]))
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	dir = filepath.Join(dir, e.Identity.Environment, e.Identity.CohortID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	// The lock makes the write-once operation safe for concurrent publishers;
	// run IDs ensure retries never replace an earlier attempt.
	lock := filepath.Join(dir, ".publish.lock")
	var f *os.File
	var err error
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		f, err = os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("evidence publication: %w", err)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-deadline.C:
			return "", errors.New("evidence publication timeout")
		case <-time.After(time.Millisecond):
		}
	}
	f.Close()
	defer os.Remove(lock)
	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return "", err
	}
	b = append(b, '\n')
	tmp, err := os.CreateTemp(dir, ".evidence-*")
	if err != nil {
		return "", err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err = tmp.Chmod(0600); err == nil {
		_, err = tmp.Write(b)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	dst := filepath.Join(dir, e.RunID+".json")
	meta := []byte(fmt.Sprintf("expires_at=%s\nretention=24h\n", e.ExpiresAt.Format(time.RFC3339)))
	retentionTmp, err := os.CreateTemp(dir, ".retention-*")
	if err != nil {
		return "", err
	}
	retentionName := retentionTmp.Name()
	defer os.Remove(retentionName)
	if err = retentionTmp.Chmod(0600); err == nil {
		_, err = retentionTmp.Write(meta)
	}
	if err == nil {
		err = retentionTmp.Sync()
	}
	if closeErr := retentionTmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", fmt.Errorf("write evidence retention metadata: %w", err)
	}
	if err = os.Link(retentionName, dst+".retention"); err != nil {
		return "", fmt.Errorf("write-once evidence retention metadata: %w", err)
	}
	if err = os.Link(name, dst); err != nil {
		_ = os.Remove(dst + ".retention")
		return "", fmt.Errorf("write-once evidence: %w", err)
	}
	return dst, nil
}

func validateEvidence(e AtlasEvidence) error {
	if e.ContractVersion != evidenceVersion || e.RunID == "" || strings.ContainsAny(e.RunID, `/\\`) {
		return errors.New("invalid evidence identity")
	}
	if !digestPattern.MatchString(e.Identity.ImageDigest) || !digestPattern.MatchString(e.Identity.ContractDigest) || e.Identity.Environment != "canary" || e.Identity.CohortID == "" {
		return errors.New("invalid evidence identity")
	}
	if !isArtifactDigest(e.ArtifactDigests.Global) || !isArtifactDigest(e.ArtifactDigests.Tenant) {
		return errors.New("invalid evidence artifact digest")
	}
	validStatus := map[string]bool{"success": true, "no-op": true, "partial_failure": true, "drift": true, "cancelled": true}
	if !validStatus[e.Status] || !validStatus[e.GlobalResult] || e.Status != e.GlobalResult {
		return errors.New("invalid evidence status")
	}
	if e.CreatedAt.IsZero() || e.ExpiresAt.IsZero() || !e.CreatedAt.Before(e.ExpiresAt) || e.ExpiresAt.Sub(e.CreatedAt) > 24*time.Hour {
		return errors.New("invalid evidence freshness window")
	}
	if e.Cohort.ID == "" || strings.ContainsAny(e.Cohort.ID, `/\\`) || e.Cohort.ID != e.Identity.CohortID || e.Cohort.EligibleCount != len(e.Cohort.ApprovedTargets) || e.TenantResultCounts.Eligible != e.Cohort.EligibleCount || e.TenantResultCounts.Succeeded < 0 || e.TenantResultCounts.Failed < 0 || e.TenantResultCounts.Failed != len(e.Cohort.FailedTargets) || e.TenantResultCounts.Succeeded+e.TenantResultCounts.Failed != e.TenantResultCounts.Eligible {
		return errors.New("inconsistent evidence cohort counts")
	}
	if e.Status == "success" && e.TenantResultCounts.Failed != 0 {
		return errors.New("inconsistent successful evidence")
	}
	if e.TenantResultCounts.Eligible == 0 && !e.NoTenantFanout {
		return errors.New("empty cohort requires explicit no-tenant-fanout declaration")
	}
	if e.AtlasGeneration < 0 || e.ObservedGeneration < 0 || e.NormalizedSchemaIdentity == "" {
		return errors.New("evidence is not from a current Atlas reconciliation")
	}
	// These values are opaque identities. Rejecting connection strings and SQL
	// here makes accidental leakage fail closed even if a caller is changed.
	for _, value := range []string{e.NormalizedSchemaIdentity, e.CurrentSchemaIdentity, e.Identity.CohortID, e.Cohort.ID} {
		if strings.Contains(value, "://") || strings.ContainsAny(value, "\r\n") || strings.Contains(strings.ToUpper(value), "SELECT ") || strings.Contains(strings.ToUpper(value), "CREATE ") || strings.Contains(strings.ToUpper(value), "ALTER ") {
			return errors.New("evidence contains unredacted sensitive identity")
		}
	}
	allTargets := append(append(append([]string{}, e.Cohort.ApprovedTargets...), e.Cohort.FailedTargets...), e.OriginalApprovedTargets...)
	allTargets = append(allTargets, e.OriginalFailedTargets...)
	for _, target := range allTargets {
		if !redactedTargetPattern.MatchString(target) {
			return errors.New("evidence contains an unredacted target identity")
		}
	}
	return nil
}

func isArtifactDigest(value string) bool {
	return len(value) == 64 && strings.Trim(value, "0123456789abcdef") == ""
}

func (s FileEvidenceStore) Inspect(runID string) (AtlasEvidence, error) {
	if runID == "" || strings.ContainsAny(runID, `/\\`) {
		return AtlasEvidence{}, errors.New("invalid run id")
	}
	var found string
	err := filepath.WalkDir(s.Root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && d.Name() == runID+".json" {
			found = path
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		return AtlasEvidence{}, err
	}
	if found == "" {
		return AtlasEvidence{}, os.ErrNotExist
	}
	b, err := os.ReadFile(found)
	var e AtlasEvidence
	if err == nil {
		err = json.Unmarshal(b, &e)
	}
	return e, err
}

// Cleanup removes only expired records. Callers pass run IDs referenced by a
// pending promotion; those records are always retained, even after expiry.
func (s FileEvidenceStore) Cleanup(ctx context.Context, now time.Time, protected map[string]bool) error {
	return filepath.WalkDir(s.Root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}
		id := strings.TrimSuffix(d.Name(), ".json")
		if protected[id] {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		var e AtlasEvidence
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err = json.Unmarshal(b, &e); err != nil {
			return err
		}
		if !now.Before(e.ExpiresAt) {
			if err = os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			_ = os.Remove(path + ".retention")
		}
		return nil
	})
}
