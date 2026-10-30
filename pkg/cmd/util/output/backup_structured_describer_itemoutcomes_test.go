/*
Copyright the Velero contributors.

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

package output

import (
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kbclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/vmware-tanzu/velero/internal/itemoutcome"
	"github.com/vmware-tanzu/velero/internal/volume"
	velerov1api "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	"github.com/vmware-tanzu/velero/pkg/builder"
	cmdtest "github.com/vmware-tanzu/velero/pkg/cmd/test"
	velerotest "github.com/vmware-tanzu/velero/pkg/test"
)

// describeBackupInSFForTest runs the structured describer for one backup
// against a fake client that serves the named artifacts, and returns the
// decoded "status" section.
//
// The download path is the real one: describeBackup*InSF creates a
// DownloadRequest and waits for a URL, so the harness plays the controller and
// patches one in, exactly as the CLI tests do. Anything not in artifacts 404s,
// which is how a backup without a given artifact is modeled.
func describeBackupInSFForTest(t *testing.T, backup *velerov1api.Backup, artifacts map[velerov1api.DownloadTargetKind]string, details bool) map[string]any {
	t.Helper()

	kbClient := velerotest.NewFakeControllerRuntimeClient(t, backup)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Sign a URL for every request, as the download controller does, and let
		// the download itself 404. That is how a backup without a given artifact
		// behaves, and how downloadrequest turns it into ErrNotFound.
		content, ok := artifacts[velerov1api.DownloadTargetKind(r.URL.Query().Get("kind"))]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		gzipWriter := gzip.NewWriter(w)
		defer gzipWriter.Close()
		gzipWriter.Write([]byte(content))
	}))
	defer server.Close()

	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case <-done:
				return
			case <-time.After(20 * time.Millisecond):
			}

			list := &velerov1api.DownloadRequestList{}
			if err := kbClient.List(t.Context(), list, &kbclient.ListOptions{Namespace: cmdtest.VeleroNameSpace}); err != nil {
				continue
			}
			for _, dr := range list.Items {
				if dr.Status.DownloadURL != "" {
					continue
				}
				dr.Status.DownloadURL = server.URL + "?kind=" + string(dr.Spec.Target.Kind)
				dr.Status.Phase = velerov1api.DownloadRequestPhaseProcessed
				kbClient.Update(t.Context(), &dr)
			}
		}
	}()

	d := &StructuredDescriber{output: make(map[string]any), format: ""}
	DescribeBackupStatusInSF(t.Context(), kbClient, d, backup, details, false, "", nil)

	return d.output["status"].(map[string]any)
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}

func TestDescribeBackupItemOutcomesInSF(t *testing.T) {
	backup := builder.ForBackup(cmdtest.VeleroNameSpace, "test-backup").Result()
	// DescribeBackupStatusInSF reads Status.Expiration unconditionally, so a
	// processed backup needs one.
	backup.Status.Expiration = &metav1.Time{Time: time.Now().Add(24 * time.Hour)}

	// A PVC carries the rollup with it, so the volume join has something to work
	// on. Reads Skipped but its volume failed: exactly the case the join exists
	// to stop reporting as benign.
	outcomes := itemoutcome.ItemOutcomes{
		SchemaVersion: itemoutcome.SchemaVersion,
		Items: []itemoutcome.ItemOutcome{{
			GroupResource: "persistentvolumeclaims",
			Namespace:     "ns1",
			Name:          "data",
			Outcome:       itemoutcome.OutcomeSkipped,
			Messages:      []string{"skipped by pod volume backup"},
		}},
	}
	volInfos := []volume.BackupVolumeInfo{{
		PVCNamespace: "ns1",
		PVCName:      "data",
		PVName:       "pv-data",
		Result:       volume.VolumeResultFailed,
	}}

	t.Run("rollup is reported with the volume verdict applied", func(t *testing.T) {
		status := describeBackupInSFForTest(t, backup, map[velerov1api.DownloadTargetKind]string{
			velerov1api.DownloadTargetKindBackupItemOutcomes: mustJSON(t, outcomes),
			velerov1api.DownloadTargetKindBackupVolumeInfos:  mustJSON(t, volInfos),
		}, true)

		sf, ok := status["itemOutcomes"].(map[string]any)
		require.True(t, ok, "status must carry an itemOutcomes section")

		// The row says Skipped, and the summary repeats the row's own verdict
		// rather than upgrading it. What makes it a failure is appearing in
		// Failed at all: without the volume join this skip would be judged
		// benign and leave Failed empty.
		rollup := sf["items"].([]itemoutcome.OwnerSummary)
		require.Len(t, rollup, 1)
		assert.Equal(t, itemoutcome.OutcomeSkipped, rollup[0].Outcome)
		require.Len(t, rollup[0].Failed, 1, "a skip whose volume failed must be named as a failure")
		assert.Equal(t, "data", rollup[0].Failed[0].Name)
	})

	t.Run("an absent artifact is reported as not collected", func(t *testing.T) {
		status := describeBackupInSFForTest(t, backup, map[velerov1api.DownloadTargetKind]string{
			velerov1api.DownloadTargetKindBackupVolumeInfos: mustJSON(t, volInfos),
		}, true)

		sf := status["itemOutcomes"].(map[string]any)
		assert.Equal(t, "<item outcomes not collected for this backup>", sf["errorGettingItemOutcomes"])
		assert.NotContains(t, sf, "items", "absence must not look like an empty artifact")
	})

	t.Run("unreadable volume info degrades the rollup and says so", func(t *testing.T) {
		status := describeBackupInSFForTest(t, backup, map[velerov1api.DownloadTargetKind]string{
			velerov1api.DownloadTargetKindBackupItemOutcomes: mustJSON(t, outcomes),
			// Not valid JSON: Load reports a decode error.
			velerov1api.DownloadTargetKindBackupVolumeInfos: "not json",
		}, true)

		sf := status["itemOutcomes"].(map[string]any)
		assert.Contains(t, sf, "warningGettingVolumeInfo",
			"a reader must be told the skip could not be checked against volume info")

		// Without volume info the join cannot confirm the skip was benign, so it
		// is not treated as a failure: guessing would re-introduce the false
		// positive the rule exists to remove. The row still appears in Counts,
		// so it is not lost -- only unexplained.
		rollup := sf["items"].([]itemoutcome.OwnerSummary)
		require.Len(t, rollup, 1)
		assert.Equal(t, itemoutcome.OutcomeBackedUp, rollup[0].Outcome)
		assert.Empty(t, rollup[0].Failed)
		assert.Equal(t, 1, rollup[0].Counts[itemoutcome.OutcomeSkipped])
	})

	t.Run("an unreadable artifact keeps its own key", func(t *testing.T) {
		status := describeBackupInSFForTest(t, backup, map[velerov1api.DownloadTargetKind]string{
			velerov1api.DownloadTargetKindBackupItemOutcomes: mustJSON(t, outcomes),
			// Not valid JSON: the fetch succeeds and the decode does not.
			velerov1api.DownloadTargetKindBackupVolumeInfos: "not json",
		}, true)

		volumes := status["backupVolumes"].(map[string]any)
		assert.Contains(t, volumes, "errorReadBackupVolumeInfo",
			"an unreadable artifact must not be reported as a fetch failure; the two keys have always differed")
		assert.NotContains(t, volumes, "errorGetBackupVolumeInfo")
	})

	t.Run("the section is only printed under --details", func(t *testing.T) {
		status := describeBackupInSFForTest(t, backup, map[velerov1api.DownloadTargetKind]string{
			velerov1api.DownloadTargetKindBackupItemOutcomes: mustJSON(t, outcomes),
			velerov1api.DownloadTargetKindBackupVolumeInfos:  mustJSON(t, volInfos),
		}, false)

		assert.NotContains(t, status, "itemOutcomes")
	})
}

// TestDescribeBackupInSFReadsVolumeInfoOnce pins the reason DescribeBackupStatusInSF
// resolves the cacert and loads volume info at the top instead of inside the two
// sections: with --details both would otherwise download the same bytes.
func TestDescribeBackupInSFReadsVolumeInfoOnce(t *testing.T) {
	backup := builder.ForBackup(cmdtest.VeleroNameSpace, "test-backup").Result()
	backup.Status.Expiration = &metav1.Time{Time: time.Now().Add(24 * time.Hour)}

	requests := make(chan string, 16)
	kbClient := velerotest.NewFakeControllerRuntimeClient(t, backup)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.URL.Query().Get("kind")
		gzipWriter := gzip.NewWriter(w)
		defer gzipWriter.Close()
		gzipWriter.Write([]byte("{}"))
	}))
	defer server.Close()

	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case <-done:
				return
			case <-time.After(20 * time.Millisecond):
			}

			list := &velerov1api.DownloadRequestList{}
			if err := kbClient.List(t.Context(), list, &kbclient.ListOptions{Namespace: cmdtest.VeleroNameSpace}); err != nil {
				continue
			}
			for _, dr := range list.Items {
				if dr.Status.DownloadURL != "" {
					continue
				}
				dr.Status.DownloadURL = server.URL + "?kind=" + string(dr.Spec.Target.Kind)
				dr.Status.Phase = velerov1api.DownloadRequestPhaseProcessed
				kbClient.Update(t.Context(), &dr)
			}
		}
	}()

	d := &StructuredDescriber{output: make(map[string]any), format: ""}
	DescribeBackupStatusInSF(t.Context(), kbClient, d, backup, true, false, "", nil)

	close(requests)
	volumeReads := 0
	for kind := range requests {
		if kind == string(velerov1api.DownloadTargetKindBackupVolumeInfos) {
			volumeReads++
		}
	}

	assert.Equal(t, 1, volumeReads, "volume info must be downloaded once per description, not once per section")
}
