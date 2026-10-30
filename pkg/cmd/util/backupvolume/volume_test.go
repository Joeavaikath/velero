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

package backupvolume

import (
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kbclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/vmware-tanzu/velero/internal/volume"
	velerov1api "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	"github.com/vmware-tanzu/velero/pkg/builder"
	cmdtest "github.com/vmware-tanzu/velero/pkg/cmd/test"
	"github.com/vmware-tanzu/velero/pkg/test"
)

// serveArtifact stands up a fake object store that serves body for the volume
// info artifact, and a fake client whose DownloadRequest gets pointed at it.
// A body of nil means the artifact is absent, which the object store signals
// with a 404.
func serveArtifact(t *testing.T, body []byte) kbclient.Client {
	t.Helper()

	kbClient := test.NewFakeControllerRuntimeClient(t)
	backup := builder.ForBackup(cmdtest.VeleroNameSpace, "backup-1").Phase(velerov1api.BackupPhaseCompleted).Result()
	require.NoError(t, kbClient.Create(t.Context(), backup, &kbclient.CreateOptions{}))

	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if body == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		gzipWriter := gzip.NewWriter(w)
		defer gzipWriter.Close()
		_, _ = gzipWriter.Write(body)
	}))
	t.Cleanup(mockServer.Close)

	// The download request is created by Load and its URL filled in out of band,
	// as the controller would, so the patcher has to keep looking.
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
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
				dr.Status.DownloadURL = mockServer.URL
				dr.Status.Phase = velerov1api.DownloadRequestPhaseProcessed
				_ = kbClient.Update(t.Context(), &dr)
			}
		}
	}()

	return kbClient
}

// An absent artifact is reported as Missing, not as an error and not as an empty
// list. The describer branches on exactly this to decide whether to reconstruct
// snapshots from Backup.Status, so conflating it with "present but empty" would
// silently break pre-artifact backups.
func TestLoadReportsMissingArtifact(t *testing.T) {
	kbClient := serveArtifact(t, nil)

	res, err := Load(t.Context(), kbClient, backupRef(), time.Second, false, "", "")
	require.NoError(t, err)
	assert.True(t, res.Missing, "an absent artifact must be reported as Missing")
	assert.Empty(t, res.Volumes)
}

// An artifact that decodes to no volumes is present, and must not be reported as
// Missing: that backup recorded no volumes, it did not predate the artifact.
func TestLoadDoesNotConfuseEmptyWithMissing(t *testing.T) {
	kbClient := serveArtifact(t, mustJSON(t, []volume.BackupVolumeInfo{}))

	res, err := Load(t.Context(), kbClient, backupRef(), time.Second, false, "", "")
	require.NoError(t, err)
	assert.False(t, res.Missing, "an empty artifact is present, not missing")
	assert.Empty(t, res.Volumes)
}

func TestLoadDecodesVolumes(t *testing.T) {
	kbClient := serveArtifact(t, mustJSON(t, []volume.BackupVolumeInfo{
		{PVCNamespace: "ns-1", PVCName: "disk-0", PVName: "pv-0", BackupMethod: volume.CSISnapshot, Result: volume.VolumeResultFailed},
		{PVCNamespace: "ns-1", PVCName: "disk-1", PVName: "pv-1", BackupMethod: volume.CSISnapshot, Result: volume.VolumeResultSucceeded},
	}))

	res, err := Load(t.Context(), kbClient, backupRef(), time.Second, false, "", "")
	require.NoError(t, err)
	assert.False(t, res.Missing)
	require.Len(t, res.Volumes, 2)
	assert.Equal(t, "disk-0", res.Volumes[0].PVCName)
}

// A malformed artifact is an error: neither caller can say anything useful about
// the volumes, and silently reporting no volumes would imply the backup had none.
func TestLoadRejectsUndecodableArtifact(t *testing.T) {
	kbClient := serveArtifact(t, []byte("not json"))

	_, err := Load(t.Context(), kbClient, backupRef(), time.Second, false, "", "")
	require.Error(t, err)
}

// Only an explicit failure is a failure. A volume skipped by one path may have
// been captured by another, so it must not be projected as failed.
func TestToRollupRefsCarriesOnlyExplicitFailures(t *testing.T) {
	refs := ToRollupRefs([]volume.BackupVolumeInfo{
		{PVCNamespace: "ns-1", PVCName: "disk-0", PVName: "pv-0", Result: volume.VolumeResultFailed},
		{PVCNamespace: "ns-1", PVCName: "disk-1", PVName: "pv-1", Result: volume.VolumeResultSucceeded},
		// A skipped volume carries no Result: Skipped is its own field, not a
		// result value.
		{PVCNamespace: "ns-1", PVCName: "disk-2", PVName: "pv-2", Skipped: true, SkippedReason: "backed up by CSI snapshot"},
	})

	require.Len(t, refs, 3)
	assert.True(t, refs[0].Failed)
	assert.False(t, refs[1].Failed)
	assert.False(t, refs[2].Failed, "a skip is not a failure; it may have been captured by another path")

	// The fields the rollup joins on must survive the projection intact.
	assert.Equal(t, "ns-1", refs[0].PVCNamespace)
	assert.Equal(t, "disk-0", refs[0].PVCName)
	assert.Equal(t, "pv-0", refs[0].PVName)
}

func TestToRollupRefsOnEmptyInput(t *testing.T) {
	assert.Empty(t, ToRollupRefs(nil))
}

func backupRef() *velerov1api.Backup {
	return builder.ForBackup(cmdtest.VeleroNameSpace, "backup-1").Phase(velerov1api.BackupPhaseCompleted).Result()
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}
