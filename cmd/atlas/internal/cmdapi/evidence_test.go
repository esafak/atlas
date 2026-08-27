// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

package cmdapi

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func evidenceFlagsForTest(dir string) fanoutFlags {
	return fanoutFlags{evidenceDir: dir, releaseImageDigest: "sha256:" + strings.Repeat("1", 64), contractDigest: "sha256:" + strings.Repeat("2", 64), contractVersion: 1, globalArtifactDigest: strings.Repeat("3", 64), tenantArtifactDigest: strings.Repeat("4", 64), normalizedSchemaIdentity: "normalized-v1", atlasGeneration: 7, observedGeneration: 7}
}

func TestEvidenceIsRedactedAndWriteOnce(t *testing.T) {
	dir := t.TempDir()
	f := evidenceFlagsForTest(dir)
	report := fanoutReport{Version: "atlas.schema.apply.fanout/v1", Cohort: "cohort-1"}
	e, err := evidenceFromFanout(f, report, []fanoutPlan{{Target: "target-abcdef123456", Status: "success", SQL: []string{"CREATE TABLE users (password text)"}}})
	require.NoError(t, err)
	store := FileEvidenceStore{Root: dir}
	path, err := store.Publish(context.Background(), e)
	require.NoError(t, err)
	_, err = store.Publish(context.Background(), e)
	require.Error(t, err, "a run identifier is immutable")
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NotContains(t, string(b), "CREATE TABLE")
	require.NotContains(t, string(b), "password")
	require.NotContains(t, string(b), "mysql://")
	got, err := store.Inspect(e.RunID)
	require.NoError(t, err)
	require.Equal(t, e.Identity, got.Identity)
	require.FileExists(t, path+".retention")
}

func TestEvidenceFailedStatesNeverClaimSuccess(t *testing.T) {
	f := evidenceFlagsForTest(t.TempDir())
	for _, status := range []string{"failed", "drifted", "cancelled"} {
		e, err := evidenceFromFanout(f, fanoutReport{Cohort: status}, []fanoutPlan{{Target: "target-abcdef123456", Status: status}})
		require.NoError(t, err)
		require.NotEqual(t, "success", e.Status)
		require.NotEqual(t, "success", e.GlobalResult)
	}
}

func TestEvidenceEmptyNoFanoutUsesJSONArrays(t *testing.T) {
	f := evidenceFlagsForTest(t.TempDir())
	f.noTenantFanout = true
	e, err := evidenceFromFanout(f, fanoutReport{Cohort: "empty"}, nil)
	require.NoError(t, err)
	b, err := json.Marshal(e)
	require.NoError(t, err)
	require.Contains(t, string(b), `"approved_targets":[]`)
	require.Contains(t, string(b), `"failed_targets":[]`)
}

func TestEvidenceConcurrentPublication(t *testing.T) {
	dir := t.TempDir()
	store := FileEvidenceStore{Root: dir}
	f := evidenceFlagsForTest(dir)
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e, err := evidenceFromFanout(f, fanoutReport{Cohort: "same"}, []fanoutPlan{{Target: "target-abcdef123456", Status: "no-op"}})
			if err == nil {
				_, err = store.Publish(context.Background(), e)
			}
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	for err := range results {
		require.NoError(t, err)
	}
	files, err := filepath.Glob(filepath.Join(dir, "*", "canary", "same", "*.json"))
	require.NoError(t, err)
	require.Len(t, files, 8)
}

func TestEvidenceRejectsUnredactedValues(t *testing.T) {
	f := evidenceFlagsForTest(t.TempDir())
	e, err := evidenceFromFanout(f, fanoutReport{Cohort: "safe"}, []fanoutPlan{{Target: "mysql://user:secret@db", Status: "success"}})
	require.NoError(t, err)
	_, err = (FileEvidenceStore{Root: f.evidenceDir}).Publish(context.Background(), e)
	require.Error(t, err)

	e, err = evidenceFromFanout(f, fanoutReport{Cohort: "safe"}, []fanoutPlan{{Target: "target-abcdef123456", Status: "success"}})
	require.NoError(t, err)
	e.NormalizedSchemaIdentity = "SELECT password FROM users"
	_, err = (FileEvidenceStore{Root: f.evidenceDir}).Publish(context.Background(), e)
	require.Error(t, err)
}

func TestEvidenceRejectsCancelledContextAndInvalidInspection(t *testing.T) {
	f := evidenceFlagsForTest(t.TempDir())
	e, err := evidenceFromFanout(f, fanoutReport{Cohort: "x"}, []fanoutPlan{{Target: "target-abcdef123456", Status: "success"}})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = (FileEvidenceStore{Root: f.evidenceDir}).Publish(ctx, e)
	require.ErrorIs(t, err, context.Canceled)
	_, err = (FileEvidenceStore{Root: f.evidenceDir}).Inspect("../secret")
	require.Error(t, err)
}

func TestEvidenceCleanupHonorsPendingPromotion(t *testing.T) {
	dir := t.TempDir()
	f := evidenceFlagsForTest(dir)
	e, err := evidenceFromFanout(f, fanoutReport{Cohort: "x"}, []fanoutPlan{{Target: "target-abcdef123456", Status: "no-op"}})
	require.NoError(t, err)
	store := FileEvidenceStore{Root: dir}
	_, err = store.Publish(context.Background(), e)
	require.NoError(t, err)
	now := e.ExpiresAt.Add(time.Hour)
	require.NoError(t, store.Cleanup(context.Background(), now, map[string]bool{e.RunID: true}))
	_, err = store.Inspect(e.RunID)
	require.NoError(t, err)
	require.NoError(t, store.Cleanup(context.Background(), now, nil))
	_, err = store.Inspect(e.RunID)
	require.ErrorIs(t, err, os.ErrNotExist)
}
