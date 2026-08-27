// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

package cmdapi

// This file contains the OSS, local-only orchestration for schema apply when
// an HCL environment expands to more than one environment block.  Keeping
// orchestration here (rather than in the HCL evaluator) is intentional: HCL
// expansion is frozen before any target is inspected or changed.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"ariga.io/atlas/sql/migrate"
	"ariga.io/atlas/sql/schema"
	"ariga.io/atlas/sql/sqlclient"
	"github.com/spf13/cobra"
)

const fanoutSummaryLimit = 100

type fanoutFlags struct {
	allowRisk                []string
	report                   string
	retryReport              string
	evidenceDir              string
	releaseImageDigest       string
	contractDigest           string
	contractVersion          int
	globalArtifactDigest     string
	tenantArtifactDigest     string
	normalizedSchemaIdentity string
	currentSchemaIdentity    string
	atlasGeneration          int64
	observedGeneration       int64
	noTenantFanout           bool
	contractDescriptor       string
	retryOf                  string
	originalCohort           string
	originalApprovedTargets  []string
	originalFailedTargets    []string
}

type fanoutPlan struct {
	Target        string            `json:"target"`
	Environment   string            `json:"environment"`
	CurrentHash   string            `json:"current_hash"`
	DesiredHash   string            `json:"desired_hash"`
	ArtifactHash  string            `json:"artifact_hash,omitempty"`
	SQL           []string          `json:"sql"`
	Diagnostics   []string          `json:"diagnostics,omitempty"`
	PlanHash      string            `json:"plan_hash"`
	Risk          []string          `json:"risk,omitempty"`
	Status        string            `json:"status"`
	Error         string            `json:"error,omitempty"`
	Warning       string            `json:"warning,omitempty"`
	Changes       []schema.Change   `json:"-"`
	MigrationPlan *migrate.Plan     `json:"-"`
	client        *sqlclient.Client `json:"-"`
	devURL        string            `json:"-"`
	toURLs        []string          `json:"-"`
	schemas       []string          `json:"-"`
	exclude       []string          `json:"-"`
	include       []string          `json:"-"`
	lockName      string            `json:"-"`
	vars          Vars              `json:"-"`
	targetURL     string            `json:"-"`
}

type fanoutReport struct {
	Version string       `json:"version"`
	Cohort  string       `json:"cohort"`
	RunID   string       `json:"run_id,omitempty"`
	Plans   []fanoutPlan `json:"plans"`
}

func addFanoutFlags(cmd *cobra.Command, f *fanoutFlags) {
	cmd.Flags().StringArrayVar(&f.allowRisk, "allow-risk", nil, "allow a typed risk class (repeatable)")
	cmd.Flags().StringVar(&f.report, "report", "", "write the complete fan-out report to a local JSON file")
	cmd.Flags().StringVar(&f.retryReport, "retry-report", "", "retry only targets recorded in a previous fan-out report")
	cmd.Flags().StringVar(&f.evidenceDir, "evidence-dir", "", "publish redacted readiness evidence to this local immutable store")
	cmd.Flags().StringVar(&f.releaseImageDigest, "release-image-digest", "", "immutable release image digest (required with --evidence-dir)")
	cmd.Flags().StringVar(&f.contractDigest, "contract-digest", "", "schema contract digest (required with --evidence-dir)")
	cmd.Flags().IntVar(&f.contractVersion, "contract-version", 0, "schema contract version (required with --evidence-dir)")
	cmd.Flags().StringVar(&f.globalArtifactDigest, "global-artifact-digest", "", "global artifact digest for evidence")
	cmd.Flags().StringVar(&f.tenantArtifactDigest, "tenant-artifact-digest", "", "tenant artifact digest for evidence")
	cmd.Flags().StringVar(&f.normalizedSchemaIdentity, "normalized-schema-identity", "", "normalized Atlas schema identity")
	cmd.Flags().StringVar(&f.currentSchemaIdentity, "current-schema-identity", "", "current Atlas schema identity")
	cmd.Flags().Int64Var(&f.atlasGeneration, "atlas-generation", 0, "Atlas resource generation")
	cmd.Flags().Int64Var(&f.observedGeneration, "observed-generation", 0, "Atlas observed generation")
	cmd.Flags().BoolVar(&f.noTenantFanout, "no-tenant-fanout", false, "explicitly declare that this release requires no tenant fan-out")
	cmd.Flags().StringVar(&f.contractDescriptor, "contract-descriptor", "", "canonical release-contract JSON used to verify --contract-digest")
}

var fanoutRiskClasses = map[string]bool{
	"destructive": true, "index": true, "foreign-key": true, "column-rewrite": true,
	"data-dependent": true,
}

func fanoutPlanHash(p fanoutPlan) string {
	b, _ := json.Marshal(struct {
		Target, CurrentHash, DesiredHash, ArtifactHash string
		SQL, Risk                                      []string
	}{p.Target, p.CurrentHash, p.DesiredHash, p.ArtifactHash, p.SQL, p.Risk})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func realmHash(r *schema.Realm) string {
	b, _ := json.Marshal(canonicalFingerprint(reflect.ValueOf(r), make(map[uintptr]bool)))
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func canonicalFingerprint(v reflect.Value, seen map[uintptr]bool) any {
	if !v.IsValid() {
		return nil
	}
	if v.Kind() == reflect.Interface {
		if v.IsNil() {
			return nil
		}
		return canonicalFingerprint(v.Elem(), seen)
	}
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return nil
		}
		ptr := v.Pointer()
		if seen[ptr] {
			return "<cycle>"
		}
		seen[ptr] = true
		return canonicalFingerprint(v.Elem(), seen)
	}
	switch v.Kind() {
	case reflect.Struct:
		out := make(map[string]any)
		for i := 0; i < v.NumField(); i++ {
			name := v.Type().Field(i).Name
			if name == "Realm" || name == "Schema" || name == "Refs" || name == "Deps" {
				continue
			}
			out[name] = canonicalFingerprint(v.Field(i), seen)
		}
		return out
	case reflect.Slice, reflect.Array:
		out := make([]any, v.Len())
		for i := range out {
			out[i] = canonicalFingerprint(v.Index(i), seen)
		}
		return out
	case reflect.Map:
		out := make(map[string]any)
		for _, key := range v.MapKeys() {
			out[fmt.Sprint(key.Interface())] = canonicalFingerprint(v.MapIndex(key), seen)
		}
		return out
	default:
		if v.CanInterface() {
			return v.Interface()
		}
		return fmt.Sprint(v)
	}
}

func redactedTarget(u string) string {
	h := sha256.Sum256([]byte(u))
	return "target-" + hex.EncodeToString(h[:])[:12]
}

func riskClasses(sqls []string) []string {
	var out []string
	for _, s := range sqls {
		u := strings.ToUpper(strings.TrimSpace(s))
		if strings.HasPrefix(u, "UPDATE ") || strings.HasPrefix(u, "INSERT ") || strings.HasPrefix(u, "DELETE ") {
			out = append(out, "data-transformation")
		}
		if strings.HasPrefix(u, "DROP ") || strings.Contains(u, " DROP ") || strings.HasPrefix(u, "TRUNCATE ") || strings.Contains(u, " RENAME ") || strings.HasPrefix(u, "RENAME ") {
			out = append(out, "destructive")
		}
		if strings.Contains(u, " ADD INDEX ") || strings.Contains(u, " ADD UNIQUE INDEX ") || strings.HasPrefix(u, "CREATE INDEX") || strings.HasPrefix(u, "CREATE UNIQUE INDEX") {
			out = append(out, "index")
		}
		if strings.Contains(u, "FOREIGN KEY") {
			out = append(out, "foreign-key")
		}
		if strings.Contains(u, " MODIFY ") || strings.Contains(u, " CHANGE ") {
			out = append(out, "column-rewrite")
		}
		if strings.HasPrefix(u, "ALTER TABLE") && (strings.Contains(u, " NOT NULL") || strings.Contains(u, " UNIQUE ")) {
			out = append(out, "data-dependent")
		}
	}
	sort.Strings(out)
	return uniqueStrings(out)
}

func uniqueStrings(in []string) []string {
	var out []string
	for _, s := range in {
		if len(out) == 0 || out[len(out)-1] != s {
			out = append(out, s)
		}
	}
	return out
}

func fanoutRun(cmd *cobra.Command, flags schemaApplyFlags, envs []*Env, ff fanoutFlags) error {
	if len(envs) < 2 {
		return schemaApplyRun(cmd, flags, envs[0])
	}
	if flags.logFormat != "" {
		return errors.New("--format/--log are not supported for fan-out; use --report")
	}
	if ff.evidenceDir != "" {
		if err := validateEvidenceInputs(ff); err != nil {
			return err
		}
		if ff.contractDescriptor == "" {
			return errors.New("--evidence-dir requires --contract-descriptor")
		}
		if ff.report == "" {
			return errors.New("--evidence-dir requires --report so the completed fan-out is retained")
		}
		if flags.dryRun {
			return errors.New("--evidence-dir requires a completed apply; use --report for planning")
		}
	}
	if flags.autoApprove && len(ff.allowRisk) > 0 {
		return errors.New("--auto-approve cannot be combined with --allow-risk")
	}
	for _, risk := range ff.allowRisk {
		if !fanoutRiskClasses[risk] {
			return fmt.Errorf("unknown fan-out risk class %q", risk)
		}
	}
	if len(ff.allowRisk) > 0 && ff.report == "" {
		return errors.New("--allow-risk requires --report so the override is bound to the saved cohort and plan hashes")
	}
	type retryBinding struct {
		planHash, artifactHash string
		drifted                bool
	}
	var retry map[string]retryBinding
	if ff.retryReport != "" {
		var previous fanoutReport
		b, err := os.ReadFile(ff.retryReport)
		if err != nil {
			return fmt.Errorf("read retry report: %w", err)
		}
		if err := json.Unmarshal(b, &previous); err != nil {
			return fmt.Errorf("decode retry report: %w", err)
		}
		if previous.Version != "atlas.schema.apply.fanout/v1" {
			return fmt.Errorf("unsupported retry report version %q", previous.Version)
		}
		retry = make(map[string]retryBinding, len(previous.Plans))
		for _, p := range previous.Plans {
			if p.Status == "" || p.Status == "planned" || p.Status == "failed" || p.Status == "drifted" || p.Status == "canceled" || p.Status == "cancelled" {
				retry[p.Target] = retryBinding{planHash: p.PlanHash, artifactHash: p.ArtifactHash, drifted: p.Status == "drifted"}
			}
		}
		if len(retry) == 0 {
			return errors.New("retry report contains no failed or drifted targets")
		}
		ff.originalCohort = previous.Cohort
		ff.retryOf = previous.RunID
		for _, p := range previous.Plans {
			ff.originalApprovedTargets = append(ff.originalApprovedTargets, p.Target)
			if p.Status == "failed" || p.Status == "drifted" || p.Status == "canceled" || p.Status == "cancelled" {
				ff.originalFailedTargets = append(ff.originalFailedTargets, p.Target)
			}
		}
	}
	plans := make([]fanoutPlan, 0, len(envs))
	defer func() { closeFanoutPlans(plans) }()
	seen := make(map[string]struct{}, len(envs))
	seenDev := make(map[string]string, len(envs))
	for _, env := range envs {
		perTarget := flags
		// Env expansion supplies the target and desired state. Explicit command
		// line values remain authoritative, matching single-target apply.
		if perTarget.url == "" {
			perTarget.url = env.URL
		}
		if perTarget.devURL == "" {
			perTarget.devURL = env.DevURL
		}
		if len(perTarget.toURLs) == 0 {
			srcs, err := env.Sources()
			if err != nil {
				return err
			}
			perTarget.toURLs = fixFileURLs(srcs)
		}
		if len(perTarget.schemas) == 0 {
			perTarget.schemas = env.Schemas
		}
		if len(perTarget.exclude) == 0 {
			perTarget.exclude = env.Exclude
		}
		if len(perTarget.include) == 0 {
			perTarget.include = env.Include
		}
		if _, ok := seen[perTarget.url]; ok {
			return fmt.Errorf("duplicate fan-out target %q", redactedTarget(perTarget.url))
		}
		if perTarget.devURL != "" {
			if previous, ok := seenDev[perTarget.devURL]; ok {
				return fmt.Errorf("fan-out dev database %q is shared by %s and %s; use a distinct resettable dev database per target", redactedTarget(perTarget.devURL), redactedTarget(previous), redactedTarget(perTarget.url))
			}
			seenDev[perTarget.devURL] = perTarget.url
		}
		if err := perTarget.check(env); err != nil {
			return err
		}
		seen[perTarget.url] = struct{}{}
		env = cloneEnvWithURL(env, perTarget.url)
		env.DevURL = perTarget.devURL
		env.Schemas = perTarget.schemas
		p, err := fanoutPlanOne(cmd.Context(), perTarget, env)
		if err != nil {
			return fmt.Errorf("plan %s: %w", redactedTarget(env.URL), err)
		}
		plans = append(plans, *p)
	}
	if retry != nil {
		filtered := plans[:0]
		for _, p := range plans {
			if expected, ok := retry[p.Target]; ok {
				if expected.drifted {
					if expected.artifactHash != p.ArtifactHash {
						closeFanoutPlans(plans)
						return fmt.Errorf("retry artifact changed for %s", p.Target)
					}
				} else if expected.planHash != p.PlanHash {
					closeFanoutPlans(plans)
					return fmt.Errorf("retry plan hash changed for %s", p.Target)
				}
				filtered = append(filtered, p)
			}
		}
		if len(filtered) != len(retry) {
			closeFanoutPlans(plans)
			return errors.New("retry report targets no longer match the expanded cohort")
		}
		plans = filtered
	}
	sort.SliceStable(plans, func(i, j int) bool { return plans[i].Target < plans[j].Target })
	cohort := sha256.Sum256([]byte(strings.Join(func() []string {
		r := make([]string, len(plans))
		for i := range plans {
			r[i] = plans[i].PlanHash
		}
		return r
	}(), "\n")))
	report := fanoutReport{Version: "atlas.schema.apply.fanout/v1", Cohort: hex.EncodeToString(cohort[:]), Plans: plans}
	if ff.report != "" {
		runID, err := newRunID()
		if err != nil {
			return err
		}
		report.RunID = runID
	}
	finishCanceled := func(reason string) error {
		for i := range plans {
			plans[i].Status, plans[i].Error = "canceled", reason
		}
		closeFanoutPlans(plans)
		if ff.report != "" {
			if err := writeFanoutReport(ff.report, fanoutReport{Version: report.Version, Cohort: report.Cohort, RunID: report.RunID, Plans: plans}); err != nil {
				return fmt.Errorf("write canceled fan-out report: %w", err)
			}
		}
		if ff.evidenceDir != "" {
			e, err := evidenceFromFanout(ff, report, plans)
			if err != nil {
				return err
			}
			path, err := (FileEvidenceStore{Root: ff.evidenceDir}).Publish(context.Background(), e)
			if err != nil {
				return fmt.Errorf("publish canceled evidence: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Evidence: %s\n", path)
		}
		return nil
	}
	if ff.report != "" {
		if err := writeFanoutReport(ff.report, report); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Fan-out report: %s\n", ff.report)
	}
	for i, p := range plans {
		if i < fanoutSummaryLimit {
			fmt.Fprintf(cmd.OutOrStdout(), "%s: %d change(s)%s\n", p.Target, len(p.SQL), riskSuffix(p.Risk))
		}
	}
	if len(plans) > fanoutSummaryLimit {
		fmt.Fprintf(cmd.OutOrStdout(), "... %d more targets (see report)\n", len(plans)-fanoutSummaryLimit)
	}
	if flags.dryRun {
		closeFanoutPlans(plans)
		return nil
	}
	if !flags.autoApprove && !promptUser(cmd) {
		return finishCanceled("batch approval declined")
	}
	hasRisk := false
	for _, p := range plans {
		if len(p.Risk) > 0 {
			hasRisk = true
			break
		}
	}
	if hasRisk && len(ff.allowRisk) > 0 && !promptUser(cmd) {
		if err := finishCanceled("risk confirmation aborted"); err != nil {
			return err
		}
		return AbortErrorf("risk confirmation aborted")
	}
	var failed []string
	for i := range plans {
		p := &plans[i]
		if err := cmd.Context().Err(); err != nil {
			p.Status, p.Error = "canceled", err.Error()
			failed = append(failed, p.Target+": canceled")
			continue
		}
		if contains(p.Risk, "data-transformation") {
			p.Status, p.Error = "failed", "data transformation forbidden"
			failed = append(failed, p.Target+" (data transformation forbidden)")
			continue
		}
		if len(p.Risk) > 0 && !containsAll(ff.allowRisk, p.Risk) {
			p.Status, p.Error = "failed", "risk rejected"
			failed = append(failed, p.Target+" (risk rejected)")
			continue
		}
		unlock, err := acquireFanoutLock(cmd.Context(), p, flags.lockTimeout)
		if err != nil {
			p.Status, p.Error = "failed", "lock: "+err.Error()
			failed = append(failed, p.Target+": lock: "+err.Error())
			continue
		}
		current, err := targetRealm(cmd.Context(), p)
		if err != nil {
			_ = unlock()
			p.Status, p.Error = "failed", "inspect: "+err.Error()
			failed = append(failed, p.Target+": inspect: "+err.Error())
			continue
		}
		if reason := fingerprintDrift(p, current, nil, ""); reason != "" {
			_ = unlock()
			p.Status, p.Error = "drifted", reason
			failed = append(failed, p.Target+" (drifted; retryable)")
			continue
		}
		desired, err := desiredRealm(cmd.Context(), p)
		if err != nil {
			_ = unlock()
			p.Status, p.Error = "failed", "desired-state check: "+err.Error()
			failed = append(failed, p.Target+": desired-state check: "+err.Error())
			continue
		}
		artifact, err := desiredArtifactHash(p.toURLs, desired)
		if err != nil {
			_ = unlock()
			p.Status, p.Error = "failed", "artifact check: "+err.Error()
			failed = append(failed, p.Target+": artifact check: "+err.Error())
			continue
		}
		if reason := fingerprintDrift(p, current, desired, artifact); reason != "" {
			_ = unlock()
			p.Status, p.Error = "drifted", reason
			failed = append(failed, p.Target+" (desired artifact changed; retryable)")
			continue
		}
		if len(p.Changes) == 0 {
			if err := unlock(); err != nil {
				p.Warning = "unlock after no-op: " + err.Error()
			}
			p.Status = "no-op"
			continue
		}
		if err := applyChanges(cmd.Context(), p.client, p.Changes, flags.txMode); err != nil {
			_ = unlock()
			p.Status, p.Error = "failed", err.Error()
			if cmd.Context().Err() != nil {
				p.Status = "canceled"
			}
			failed = append(failed, p.Target+": "+err.Error())
			continue
		}
		if err := unlock(); err != nil {
			p.Warning = "unlock after committed apply: " + err.Error()
		}
		p.Status = "success"
	}
	closeFanoutPlans(plans)
	if ff.report != "" {
		if err := writeFanoutReport(ff.report, fanoutReport{Version: report.Version, Cohort: report.Cohort, RunID: report.RunID, Plans: plans}); err != nil {
			return fmt.Errorf("write fan-out report: %w", err)
		}
	}
	warnings := 0
	for _, p := range plans {
		if p.Warning != "" {
			warnings++
		}
	}
	if warnings > 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "Fan-out completed with %d warning(s); see the report for details.\n", warnings)
	}
	var runErr error
	if len(failed) > 0 {
		runErr = fmt.Errorf("fan-out failed targets: %s", strings.Join(failed, ", "))
	}
	if ff.evidenceDir != "" {
		// Publication is deliberately after the final unlock and report write. A
		// failed/partial/cancelled run is still recorded, but can never validate
		// as successful evidence.
		e, err := evidenceFromFanout(ff, report, plans)
		if err != nil {
			return errors.Join(runErr, err)
		}
		store := FileEvidenceStore{Root: ff.evidenceDir}
		// Preserve a cancellation record even though the operation context is
		// cancelled; publication itself is local and bounded by the filesystem.
		path, err := store.Publish(context.Background(), e)
		if err != nil {
			return errors.Join(runErr, fmt.Errorf("publish evidence: %w", err))
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Evidence: %s\n", path)
	}
	if runErr != nil {
		return runErr
	}
	return nil
}

func acquireFanoutLock(ctx context.Context, p *fanoutPlan, timeout time.Duration) (schema.UnlockFunc, error) {
	unlock, err := p.client.Driver.Lock(ctx, p.lockName, timeout)
	if err == nil {
		return unlock, nil
	}
	// TiDB 5 exposes GET_LOCK as a documented no-op and returns an unsupported
	// feature error. Fingerprint revalidation remains mandatory; continue with
	// that guard rather than making all supported TiDB versions unusable.
	lower := strings.ToLower(err.Error())
	if strings.Contains(lower, "get_lock") && (strings.Contains(lower, "noop") || strings.Contains(lower, "unsupported")) {
		return func() error { return nil }, nil
	}
	return nil, err
}

func fingerprintDrift(p *fanoutPlan, current, desired *schema.Realm, artifact string) string {
	if current != nil && realmHash(current) != p.CurrentHash {
		return "target changed after planning"
	}
	if desired != nil && (realmHash(desired) != p.DesiredHash || artifact != p.ArtifactHash) {
		return "desired artifact changed after planning"
	}
	return ""
}

func cloneEnvWithURL(env *Env, u string) *Env {
	copy := *env
	copy.URL = u
	return &copy
}

func riskSuffix(r []string) string {
	if len(r) == 0 {
		return ""
	}
	return " [risk: " + strings.Join(r, ",") + "]"
}
func containsAll(allowed, required []string) bool {
	for _, r := range required {
		found := false
		for _, a := range allowed {
			if a == r {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}
func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
func closeFanoutPlans(ps []fanoutPlan) {
	for _, p := range ps {
		if p.client != nil {
			_ = p.client.Close()
		}
	}
}

func writeFanoutReport(path string, report fanoutReport) error {
	b, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0600)
}

func desiredArtifactHash(urls []string, fallback *schema.Realm) (string, error) {
	h := sha256.New()
	for _, raw := range urls {
		u, err := url.Parse(raw)
		if err != nil {
			return "", err
		}
		if u.Scheme != "file" {
			h := sha256.Sum256([]byte("atlas-artifact:" + strings.Join(urls, "\x00") + "\x00" + realmHash(fallback)))
			return hex.EncodeToString(h[:]), nil
		}
		path := filepath.Join(u.Host, u.Path)
		info, err := os.Stat(path)
		if err != nil {
			return "", err
		}
		if info.IsDir() {
			var files []string
			err = filepath.WalkDir(path, func(name string, entry fs.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if !entry.IsDir() {
					files = append(files, name)
				}
				return nil
			})
			if err != nil {
				return "", err
			}
			sort.Strings(files)
			for _, name := range files {
				b, err := os.ReadFile(name)
				if err != nil {
					return "", err
				}
				rel, err := filepath.Rel(path, name)
				if err != nil {
					return "", err
				}
				_, _ = h.Write([]byte(raw + "/" + rel + "\x00"))
				_, _ = h.Write(b)
				_, _ = h.Write([]byte("\x00"))
			}
		} else {
			b, err := os.ReadFile(path)
			if err != nil {
				return "", err
			}
			_, _ = h.Write([]byte(raw + "\x00"))
			_, _ = h.Write(b)
			_, _ = h.Write([]byte("\x00"))
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func fanoutPlanOne(ctx context.Context, flags schemaApplyFlags, env *Env) (*fanoutPlan, error) {
	from, err := stateReader(ctx, env, &stateReaderConfig{urls: []string{env.URL}, schemas: flags.schemas, exclude: flags.exclude, include: flags.include})
	if err != nil {
		return nil, err
	}
	client, ok := from.Closer.(*sqlclient.Client)
	if !ok {
		return nil, errors.New("fan-out target is not a database connection")
	}
	dev, err := openDev(ctx, env.DevURL)
	if err != nil {
		client.Close()
		return nil, err
	}
	to, err := stateReader(ctx, env, &stateReaderConfig{urls: flags.toURLs, dev: dev, client: client, schemas: flags.schemas, exclude: flags.exclude, include: flags.include, vars: env.Vars()})
	if err != nil {
		if dev != nil {
			dev.Close()
		}
		client.Close()
		return nil, err
	}
	d, err := computeDiff(ctx, client, from, to, diffOptions(nil, env)...)
	if dev != nil {
		dev.Close()
	}
	if err != nil {
		from.Close()
		to.Close()
		return nil, err
	}
	// Keep the target client open until the aggregate has been approved.
	to.Close()
	pl, err := client.PlanChanges(ctx, "", d.changes, planOptions(client)...)
	if err != nil {
		client.Close()
		return nil, err
	}
	status := "planned"
	if len(pl.Changes) == 0 {
		status = "no-op"
	}
	p := &fanoutPlan{Target: redactedTarget(env.URL), Environment: env.Name, Status: status, SQL: make([]string, len(pl.Changes)), Changes: d.changes, MigrationPlan: pl, client: client}
	for i, c := range pl.Changes {
		p.SQL[i] = c.Cmd
	}
	p.Risk = riskClasses(p.SQL)
	p.CurrentHash = realmHash(d.from)
	p.DesiredHash = realmHash(d.to)
	p.ArtifactHash, err = desiredArtifactHash(flags.toURLs, d.to)
	if err != nil {
		client.Close()
		return nil, err
	}
	for _, risk := range p.Risk {
		p.Diagnostics = append(p.Diagnostics, fmt.Sprintf("%s risk detected in planned SQL", risk))
	}
	p.lockName = "atlas-schema-apply-fanout"
	p.targetURL = env.URL
	p.devURL, p.toURLs, p.schemas, p.exclude, p.include, p.vars = env.DevURL, append([]string(nil), flags.toURLs...), append([]string(nil), flags.schemas...), append([]string(nil), flags.exclude...), append([]string(nil), flags.include...), env.Vars()
	p.PlanHash = fanoutPlanHash(*p)
	return p, nil
}

func targetRealm(ctx context.Context, p *fanoutPlan) (*schema.Realm, error) {
	r, err := stateReader(ctx, &Env{}, &stateReaderConfig{urls: []string{p.targetURL}, schemas: p.schemas, exclude: p.exclude, include: p.include})
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return r.ReadState(ctx)
}

func desiredRealm(ctx context.Context, p *fanoutPlan) (*schema.Realm, error) {
	dev, err := openDev(ctx, p.devURL)
	if err != nil {
		return nil, err
	}
	if dev != nil {
		defer dev.Close()
	}
	to, err := stateReader(ctx, &Env{}, &stateReaderConfig{
		urls: p.toURLs, dev: dev, client: p.client, schemas: p.schemas, exclude: p.exclude, include: p.include, vars: p.vars,
	})
	if err != nil {
		return nil, err
	}
	defer to.Close()
	return to.ReadState(ctx)
}

func openDev(ctx context.Context, u string) (*sqlclient.Client, error) {
	if u == "" {
		return nil, nil
	}
	return sqlclient.Open(ctx, u)
}
