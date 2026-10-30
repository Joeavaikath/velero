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

package backup

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	flag "github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kbclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/vmware-tanzu/velero/internal/itemoutcome"
	velerov1api "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	"github.com/vmware-tanzu/velero/pkg/builder"
	factorymocks "github.com/vmware-tanzu/velero/pkg/client/mocks"
	cmdtest "github.com/vmware-tanzu/velero/pkg/cmd/test"
	velerotest "github.com/vmware-tanzu/velero/pkg/test"
)

func TestNewItemOutcomesCommand(t *testing.T) {
	t.Run("Flag test", func(t *testing.T) {
		o := NewItemOutcomesOptions()
		flags := new(flag.FlagSet)
		o.BindFlags(flags)

		timeout := "1m0s"
		insecureSkipTLSVerify := "true"
		caCertFile := "testing"

		flags.Parse([]string{"--timeout", timeout})
		flags.Parse([]string{"--insecure-skip-tls-verify", insecureSkipTLSVerify})
		flags.Parse([]string{"--cacert", caCertFile})

		require.Equal(t, timeout, o.Timeout.String())
		require.Equal(t, insecureSkipTLSVerify, strconv.FormatBool(o.InsecureSkipTLSVerify))
		require.Equal(t, caCertFile, o.CaCertFile)
	})

	t.Run("Backup not exist test", func(t *testing.T) {
		backupName := "not-exist"

		f := &factorymocks.Factory{}

		kbClient := velerotest.NewFakeControllerRuntimeClient(t)

		f.On("Namespace").Return(cmdtest.VeleroNameSpace)
		f.On("KubebuilderClient").Return(kbClient, nil)

		c := NewItemOutcomesCommand(f)
		assert.Equal(t, "Get the per-item backup outcomes artifact, as JSON", c.Short)

		o := NewItemOutcomesOptions()
		flags := new(flag.FlagSet)
		o.BindFlags(flags)
		err := o.Complete([]string{backupName}, f)
		require.NoError(t, err)

		err = o.Run(c, f)
		require.Error(t, err)
		require.Equal(t, fmt.Sprintf("backup %q does not exist", backupName), err.Error())
	})

	t.Run("Invalid client test", func(t *testing.T) {
		f := &factorymocks.Factory{}

		kbClient := velerotest.NewFakeControllerRuntimeClient(t)

		f.On("Namespace").Return(cmdtest.VeleroNameSpace)

		c := NewItemOutcomesCommand(f)
		assert.Equal(t, "Get the per-item backup outcomes artifact, as JSON", c.Short)

		o := NewItemOutcomesOptions()
		flags := new(flag.FlagSet)
		o.BindFlags(flags)

		f.On("KubebuilderClient").Return(kbClient, fmt.Errorf("test error"))
		err := o.Complete([]string{""}, f)
		require.Equal(t, "test error", err.Error())
	})

	// runAgainstArtifacts serves one mock object-store body per download target
	// kind, so a command that reads more than one artifact can be exercised.
	runAgainstArtifacts := func(t *testing.T, backupName string, artifacts map[velerov1api.DownloadTargetKind]string, configure func(*ItemOutcomesOptions)) (string, string) {
		t.Helper()

		f := &factorymocks.Factory{}

		kbClient := velerotest.NewFakeControllerRuntimeClient(t)
		backup := builder.ForBackup(cmdtest.VeleroNameSpace, backupName).Phase(velerov1api.BackupPhaseCompleted).Result()
		err := kbClient.Create(t.Context(), backup, &kbclient.CreateOptions{})
		require.NoError(t, err)

		f.On("Namespace").Return(cmdtest.VeleroNameSpace)
		f.On("KubebuilderClient").Return(kbClient, nil)

		c := NewItemOutcomesCommand(f)

		o := NewItemOutcomesOptions()
		flags := new(flag.FlagSet)
		o.BindFlags(flags)
		err = o.Complete([]string{backupName}, f)
		require.NoError(t, err)
		if configure != nil {
			configure(&o)
		}

		mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Every request names the artifact it wants, so one server can serve all
			// of them.
			content, ok := artifacts[velerov1api.DownloadTargetKind(r.URL.Query().Get("kind"))]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			gzipWriter := gzip.NewWriter(w)
			defer gzipWriter.Close()
			gzipWriter.Write([]byte(content))
		}))
		defer mockServer.Close()

		// A command that reads several artifacts creates them one after another, so
		// the patcher has to keep looking rather than sweep once.
		done := make(chan struct{})
		defer close(done)
		go func() {
			for {
				select {
				case <-done:
					return
				case <-time.After(20 * time.Millisecond):
				}

				downloadRequestList := &velerov1api.DownloadRequestList{}
				if err := kbClient.List(t.Context(), downloadRequestList, &kbclient.ListOptions{
					Namespace: cmdtest.VeleroNameSpace,
				}); err != nil {
					continue
				}
				for _, dr := range downloadRequestList.Items {
					if _, ok := artifacts[dr.Spec.Target.Kind]; !ok {
						continue
					}
					if dr.Spec.Target.Name != backupName {
						continue
					}
					if dr.Status.DownloadURL != "" {
						continue
					}
					dr.Status.DownloadURL = mockServer.URL + "?kind=" + string(dr.Spec.Target.Kind)
					dr.Status.Phase = velerov1api.DownloadRequestPhaseProcessed
					kbClient.Update(t.Context(), &dr)
				}
			}
		}()

		var stdout, stderr bytes.Buffer
		oldStdout, oldStderr := os.Stdout, os.Stderr
		rOut, wOut, _ := os.Pipe()
		rErr, wErr, _ := os.Pipe()
		os.Stdout, os.Stderr = wOut, wErr

		o.Timeout = 5 * time.Second
		err = o.Run(c, f)

		wOut.Close()
		wErr.Close()
		os.Stdout, os.Stderr = oldStdout, oldStderr
		io.Copy(&stdout, rOut)
		io.Copy(&stderr, rErr)

		require.NoError(t, err)
		return stdout.String(), stderr.String()
	}

	runAgainstMockArtifact := func(t *testing.T, backupName string, content string, configure func(*ItemOutcomesOptions)) string {
		t.Helper()
		stdout, _ := runAgainstArtifacts(t, backupName, map[velerov1api.DownloadTargetKind]string{
			velerov1api.DownloadTargetKindBackupItemOutcomes: content,
		}, configure)
		return stdout
	}

	t.Run("Pretty-prints the decompressed artifact by default", func(t *testing.T) {
		content := `{"schemaVersion":"v1","items":[{"groupResource":"configmaps","namespace":"ns","name":"cm","outcome":"BackedUp"}]}`

		output := runAgainstMockArtifact(t, "bk-item-outcomes-1", content, nil)

		assert.JSONEq(t, `{"schemaVersion":"v1","items":[{"groupResource":"configmaps","namespace":"ns","name":"cm","outcome":"BackedUp"}]}`, output)
		assert.Contains(t, output, "\n  \"schemaVersion\": \"v1\",", "output is indented, not compact")
	})

	t.Run("Streams the raw compact artifact with --raw", func(t *testing.T) {
		content := `{"schemaVersion":"v1","items":[{"groupResource":"configmaps","namespace":"ns","name":"cm","outcome":"BackedUp"}]}`

		output := runAgainstMockArtifact(t, "bk-item-outcomes-raw", content, func(o *ItemOutcomesOptions) {
			o.Raw = true
		})

		assert.Equal(t, content, output)
	})

	t.Run("Not collected for this backup", func(t *testing.T) {
		backupName := "bk-item-outcomes-not-collected"

		f := &factorymocks.Factory{}

		kbClient := velerotest.NewFakeControllerRuntimeClient(t)
		backup := builder.ForBackup(cmdtest.VeleroNameSpace, backupName).Phase(velerov1api.BackupPhaseCompleted).Result()
		err := kbClient.Create(t.Context(), backup, &kbclient.CreateOptions{})
		require.NoError(t, err)

		f.On("Namespace").Return(cmdtest.VeleroNameSpace)
		f.On("KubebuilderClient").Return(kbClient, nil)

		c := NewItemOutcomesCommand(f)

		o := NewItemOutcomesOptions()
		flags := new(flag.FlagSet)
		o.BindFlags(flags)
		err = o.Complete([]string{backupName}, f)
		require.NoError(t, err)

		mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer mockServer.Close()

		go func() {
			time.Sleep(50 * time.Millisecond)

			downloadRequestList := &velerov1api.DownloadRequestList{}
			if err := kbClient.List(t.Context(), downloadRequestList, &kbclient.ListOptions{
				Namespace: cmdtest.VeleroNameSpace,
			}); err == nil {
				for _, dr := range downloadRequestList.Items {
					if dr.Spec.Target.Kind == velerov1api.DownloadTargetKindBackupItemOutcomes &&
						dr.Spec.Target.Name == backupName {
						dr.Status.DownloadURL = mockServer.URL
						dr.Status.Phase = velerov1api.DownloadRequestPhaseProcessed
						kbClient.Update(t.Context(), &dr)
					}
				}
			}
		}()

		o.Timeout = 5 * time.Second
		err = o.Run(c, f)
		require.Error(t, err)
		assert.Equal(t, fmt.Sprintf("item outcomes were not collected for backup %q", backupName), err.Error())
	})

	// kubeVirtArtifact mirrors the shape a KubeVirt backup produces: the VM owns
	// a DataVolume and a VMI, the VMI owns the virt-launcher pod, and the pod
	// owns the disks the VM boots from.
	kubeVirtArtifact := `{"schemaVersion":"v1","items":[
		{"groupResource":"virtualmachines.kubevirt.io","namespace":"ns","name":"vm1","outcome":"BackedUp"},
		{"groupResource":"virtualmachineinstances.kubevirt.io","namespace":"ns","name":"vm1","outcome":"BackedUp",
			"parent":{"groupResource":"virtualmachines.kubevirt.io","namespace":"ns","name":"vm1"}},
		{"groupResource":"pods","namespace":"ns","name":"virt-launcher-vm1","outcome":"BackedUp",
			"parent":{"groupResource":"virtualmachineinstances.kubevirt.io","namespace":"ns","name":"vm1"}},
		{"groupResource":"persistentvolumeclaims","namespace":"ns","name":"disk1","outcome":"Skipped",
			"parent":{"groupResource":"pods","namespace":"ns","name":"virt-launcher-vm1"}}
	]}`

	volumeInfo := func(result string) string {
		return fmt.Sprintf(`[{"backupMethod":"CSISnapshot","pvcName":"disk1","pvcNamespace":"ns","pvName":"pv-1","result":%q}]`, result)
	}

	t.Run("--by-owner stays quiet when a skipped volume was captured anyway", func(t *testing.T) {
		stdout, _ := runAgainstArtifacts(t, "bk-by-owner-benign", map[velerov1api.DownloadTargetKind]string{
			velerov1api.DownloadTargetKindBackupItemOutcomes: kubeVirtArtifact,
			velerov1api.DownloadTargetKindBackupVolumeInfos:  volumeInfo("succeeded"),
		}, func(o *ItemOutcomesOptions) {
			o.ByOwner = true
		})

		// The PVC is Skipped because a CSI snapshot took the disk. The volume
		// succeeded, so reporting the VM as skipped would be a false alarm, but the
		// skip itself must not be denied either.
		assert.Equal(t, "No backup item failed or warned across 1 owner(s).\n1 item(s) were skipped because another backup method captured the data.\n", stdout)
	})

	t.Run("--by-owner reports the VM whose disk actually failed", func(t *testing.T) {
		stdout, _ := runAgainstArtifacts(t, "bk-by-owner-failed", map[velerov1api.DownloadTargetKind]string{
			velerov1api.DownloadTargetKindBackupItemOutcomes: kubeVirtArtifact,
			velerov1api.DownloadTargetKindBackupVolumeInfos:  volumeInfo("failed"),
		}, func(o *ItemOutcomesOptions) {
			o.ByOwner = true
		})

		// The failed disk rolls up to the VM, not to the PVC, so the reader sees
		// the object that is actually broken. The chain beneath it is indented
		// under the VM, because the verdict is only explicable once the disk is
		// visible.
		assert.Contains(t, stdout, "GROUP/RESOURCE")
		assert.Contains(t, stdout, "NAMESPACE")
		assert.Contains(t, stdout, "Skipped")
		assert.Regexp(t, `(?m)^virtualmachines\.kubevirt\.io\s+ns\s+vm1\s+Skipped\s+4$`, stdout)
		assert.Regexp(t, `(?m)^  virtualmachineinstances\.kubevirt\.io\s+ns\s+vm1\s+Skipped\s+3$`, stdout)
		assert.Regexp(t, `(?m)^    pods\s+ns\s+virt-launcher-vm1\s+Skipped\s+2$`, stdout)
		assert.Regexp(t, `(?m)^      persistentvolumeclaims\s+ns\s+disk1\s+Skipped\s+1$`, stdout)
	})

	t.Run("--by-owner -o json emits the summaries", func(t *testing.T) {
		stdout, _ := runAgainstArtifacts(t, "bk-by-owner-json", map[velerov1api.DownloadTargetKind]string{
			velerov1api.DownloadTargetKindBackupItemOutcomes: kubeVirtArtifact,
			velerov1api.DownloadTargetKindBackupVolumeInfos:  volumeInfo("failed"),
		}, func(o *ItemOutcomesOptions) {
			o.ByOwner = true
			o.Output = "json"
		})

		var summaries []itemoutcome.OwnerSummary
		require.NoError(t, json.Unmarshal([]byte(stdout), &summaries), "output must be valid JSON")
		require.Len(t, summaries, 1)
		assert.Equal(t, "vm1", summaries[0].Owner.Name)
		assert.Equal(t, itemoutcome.OutcomeSkipped, summaries[0].Outcome)
		assert.Equal(t, 4, summaries[0].Counts[itemoutcome.OutcomeBackedUp]+summaries[0].Counts[itemoutcome.OutcomeSkipped])
	})

	t.Run("--all lists owners a clean backup would hide", func(t *testing.T) {
		stdout, _ := runAgainstArtifacts(t, "bk-by-owner-all", map[velerov1api.DownloadTargetKind]string{
			velerov1api.DownloadTargetKindBackupItemOutcomes: kubeVirtArtifact,
			velerov1api.DownloadTargetKindBackupVolumeInfos:  volumeInfo("succeeded"),
		}, func(o *ItemOutcomesOptions) {
			o.ByOwner = true
			o.All = true
		})

		// The default view prints one line of prose here; --all has to show the VM
		// and, on its row, the two skipped disks it was forgiven for, with the
		// whole chain beneath it.
		assert.Contains(t, stdout, "SKIPPED", "the header names the per-outcome columns")
		assert.Regexp(t, `(?m)^virtualmachines\.kubevirt\.io\s+ns\s+vm1\s+BackedUp\s+4\s+3\s+0\s+0\s+1`, stdout)
		assert.Regexp(t, `(?m)^      persistentvolumeclaims\s+ns\s+disk1\s+BackedUp\s+1\s+0\s+0\s+0\s+1`, stdout,
			"the skipped disk is nested under its pod, not listed as a root")
		assert.NotContains(t, stdout, "No backup item failed", "the prose summary is replaced by the listing")
	})

	t.Run("--all keeps naming the rows behind a failure", func(t *testing.T) {
		stdout, _ := runAgainstArtifacts(t, "bk-by-owner-all-failed", map[velerov1api.DownloadTargetKind]string{
			velerov1api.DownloadTargetKindBackupItemOutcomes: kubeVirtArtifact,
			velerov1api.DownloadTargetKindBackupVolumeInfos:  volumeInfo("failed"),
		}, func(o *ItemOutcomesOptions) {
			o.ByOwner = true
			o.All = true
		})

		assert.Regexp(t, `virtualmachines\.kubevirt\.io\s+ns\s+vm1`, stdout)
		assert.Regexp(t, `  persistentvolumeclaims\s+ns\s+disk1`, stdout,
			"the failed child is still named, in the same columns")
	})

	// withoutAllSiblingsArtifact is a VM whose pod mounts two disks. Only one of
	// them failed, so the healthy sibling must not be printed in the default view.
	withoutAllSiblingsArtifact := `{"schemaVersion":"v1","items":[
		{"groupResource":"virtualmachines.kubevirt.io","namespace":"ns","name":"vm1","outcome":"BackedUp"},
		{"groupResource":"pods","namespace":"ns","name":"virt-launcher-vm1","outcome":"BackedUp",
			"parent":{"groupResource":"virtualmachines.kubevirt.io","namespace":"ns","name":"vm1"}},
		{"groupResource":"persistentvolumeclaims","namespace":"ns","name":"good-disk","outcome":"BackedUp",
			"parent":{"groupResource":"pods","namespace":"ns","name":"virt-launcher-vm1"}},
		{"groupResource":"persistentvolumeclaims","namespace":"ns","name":"bad-disk","outcome":"Failed",
			"parent":{"groupResource":"pods","namespace":"ns","name":"virt-launcher-vm1"}}
	]}`

	t.Run("the default view prints the path to a failure, not its siblings", func(t *testing.T) {
		stdout, _ := runAgainstArtifacts(t, "bk-by-owner-path", map[velerov1api.DownloadTargetKind]string{
			velerov1api.DownloadTargetKindBackupItemOutcomes: withoutAllSiblingsArtifact,
		}, func(o *ItemOutcomesOptions) {
			o.ByOwner = true
		})

		// Every ancestor of the failed disk is needed to explain it, so they are
		// all printed with the failure rolled up into their counts.
		assert.Regexp(t, `(?m)^virtualmachines\.kubevirt\.io\s+ns\s+vm1\s+Failed\s+4$`, stdout)
		assert.Regexp(t, `(?m)^  pods\s+ns\s+virt-launcher-vm1\s+Failed\s+3$`, stdout)
		assert.Regexp(t, `(?m)^    persistentvolumeclaims\s+ns\s+bad-disk\s+Failed\s+1$`, stdout)
		assert.NotContains(t, stdout, "good-disk", "a healthy sibling is not part of the path to the failure")
	})

	t.Run("--all prints the healthy sibling beside the failed one", func(t *testing.T) {
		stdout, _ := runAgainstArtifacts(t, "bk-by-owner-path-all", map[velerov1api.DownloadTargetKind]string{
			velerov1api.DownloadTargetKindBackupItemOutcomes: withoutAllSiblingsArtifact,
		}, func(o *ItemOutcomesOptions) {
			o.ByOwner = true
			o.All = true
		})

		assert.Regexp(t, `(?m)^    persistentvolumeclaims\s+ns\s+bad-disk\s+Failed\s+1`, stdout)
		assert.Regexp(t, `(?m)^    persistentvolumeclaims\s+ns\s+good-disk\s+BackedUp\s+1`, stdout)
	})

	t.Run("--all without --by-owner is rejected rather than ignored", func(t *testing.T) {
		f := &factorymocks.Factory{}
		kbClient := velerotest.NewFakeControllerRuntimeClient(t)
		backup := builder.ForBackup(cmdtest.VeleroNameSpace, "bk-all-without-by-owner").Phase(velerov1api.BackupPhaseCompleted).Result()
		require.NoError(t, kbClient.Create(t.Context(), backup, &kbclient.CreateOptions{}))

		f.On("Namespace").Return(cmdtest.VeleroNameSpace)
		f.On("KubebuilderClient").Return(kbClient, nil)

		o := NewItemOutcomesOptions()
		o.BindFlags(new(flag.FlagSet))
		require.NoError(t, o.Complete([]string{"bk-all-without-by-owner"}, f))
		o.All = true

		mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gzipWriter := gzip.NewWriter(w)
			defer gzipWriter.Close()
			gzipWriter.Write([]byte(`{"schemaVersion":"v1","items":[]}`))
		}))
		defer mockServer.Close()

		done := make(chan struct{})
		defer close(done)
		go func() {
			for {
				select {
				case <-done:
					return
				case <-time.After(20 * time.Millisecond):
				}
				downloadRequestList := &velerov1api.DownloadRequestList{}
				if err := kbClient.List(t.Context(), downloadRequestList, &kbclient.ListOptions{
					Namespace: cmdtest.VeleroNameSpace,
				}); err != nil {
					continue
				}
				for _, dr := range downloadRequestList.Items {
					dr.Status.DownloadURL = mockServer.URL
					dr.Status.Phase = velerov1api.DownloadRequestPhaseProcessed
					kbClient.Update(t.Context(), &dr)
				}
			}
		}()

		o.Timeout = 5 * time.Second
		err := o.Run(NewItemOutcomesCommand(f), f)
		require.Error(t, err)
		assert.Equal(t, "--all requires --by-owner", err.Error())
	})

	t.Run("--by-owner still works when volume info cannot be read", func(t *testing.T) {
		stdout, stderr := runAgainstArtifacts(t, "bk-by-owner-no-volinfo", map[velerov1api.DownloadTargetKind]string{
			velerov1api.DownloadTargetKindBackupItemOutcomes: kubeVirtArtifact,
			// No BackupVolumeInfos entry: the server answers 404 for it.
		}, func(o *ItemOutcomesOptions) {
			o.ByOwner = true
		})

		assert.Contains(t, stderr, "could not read volume info", "the gap must be stated, not silent")
		assert.Contains(t, stdout, "No backup item failed")
	})

	t.Run("--by-owner warns when the backup recorded errors no item shows", func(t *testing.T) {
		backupName := "bk-by-owner-unattributed"

		f := &factorymocks.Factory{}
		kbClient := velerotest.NewFakeControllerRuntimeClient(t)
		// A backup can fail while listing resources, before any item exists: every
		// item then reads BackedUp and only the status holds the error count.
		backup := builder.ForBackup(cmdtest.VeleroNameSpace, backupName).Phase(velerov1api.BackupPhasePartiallyFailed).Result()
		backup.Status.Errors = 7
		require.NoError(t, kbClient.Create(t.Context(), backup, &kbclient.CreateOptions{}))

		f.On("Namespace").Return(cmdtest.VeleroNameSpace)
		f.On("KubebuilderClient").Return(kbClient, nil)

		o := NewItemOutcomesOptions()
		flags := new(flag.FlagSet)
		o.BindFlags(flags)
		require.NoError(t, o.Complete([]string{backupName}, f))
		o.ByOwner = true

		mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gzipWriter := gzip.NewWriter(w)
			defer gzipWriter.Close()
			gzipWriter.Write([]byte(`{"schemaVersion":"v1","items":[{"groupResource":"configmaps","namespace":"ns","name":"cm","outcome":"BackedUp"}]}`))
		}))
		defer mockServer.Close()

		done := make(chan struct{})
		defer close(done)
		go func() {
			for {
				select {
				case <-done:
					return
				case <-time.After(20 * time.Millisecond):
				}
				downloadRequestList := &velerov1api.DownloadRequestList{}
				if err := kbClient.List(t.Context(), downloadRequestList, &kbclient.ListOptions{
					Namespace: cmdtest.VeleroNameSpace,
				}); err != nil {
					continue
				}
				for _, dr := range downloadRequestList.Items {
					dr.Status.DownloadURL = mockServer.URL
					dr.Status.Phase = velerov1api.DownloadRequestPhaseProcessed
					kbClient.Update(t.Context(), &dr)
				}
			}
		}()

		var stdout, stderr bytes.Buffer
		oldStdout, oldStderr := os.Stdout, os.Stderr
		rOut, wOut, _ := os.Pipe()
		rErr, wErr, _ := os.Pipe()
		os.Stdout, os.Stderr = wOut, wErr

		o.Timeout = 5 * time.Second
		err := o.Run(NewItemOutcomesCommand(f), f)

		wOut.Close()
		wErr.Close()
		os.Stdout, os.Stderr = oldStdout, oldStderr
		io.Copy(&stdout, rOut)
		io.Copy(&stderr, rErr)

		require.NoError(t, err)
		assert.Contains(t, stderr.String(), "recorded 7 error(s)")
		assert.Contains(t, stdout.String(), "No backup item failed")
	})

	t.Run("-o tree renders the forest with the failure at depth", func(t *testing.T) {
		stdout, _ := runAgainstArtifacts(t, "bk-tree", map[velerov1api.DownloadTargetKind]string{
			velerov1api.DownloadTargetKindBackupItemOutcomes: kubeVirtArtifact,
			velerov1api.DownloadTargetKindBackupVolumeInfos:  volumeInfo("failed"),
		}, func(o *ItemOutcomesOptions) {
			o.ByOwner = true
			o.Output = "tree"
		})

		// The shape is the point: the failed PVC sits four levels below the VM,
		// and only it is flagged.
		assert.Contains(t, stdout, "virtualmachines vm1 (ns)")
		assert.Contains(t, stdout, "  virtualmachineinstances vm1 (ns)")
		assert.Contains(t, stdout, "    pods virt-launcher-vm1 (ns)")
		assert.Contains(t, stdout, "      persistentvolumeclaims disk1 (ns)  [Skipped]")
		assert.NotContains(t, stdout, "virtualmachines vm1 (ns)  [", "a clean ancestor is not flagged")
	})

	t.Run("-o tree leaves a clean volume unflagged", func(t *testing.T) {
		stdout, _ := runAgainstArtifacts(t, "bk-tree-clean", map[velerov1api.DownloadTargetKind]string{
			velerov1api.DownloadTargetKindBackupItemOutcomes: kubeVirtArtifact,
			velerov1api.DownloadTargetKindBackupVolumeInfos:  volumeInfo("succeeded"),
		}, func(o *ItemOutcomesOptions) {
			o.ByOwner = true
			o.Output = "tree"
		})

		assert.Contains(t, stdout, "persistentvolumeclaims disk1 (ns)\n", "the skipped-but-captured disk is not flagged")
		assert.NotContains(t, stdout, "[Skipped]")
	})

	t.Run("-o tree disqualifies same-named kinds from different groups", func(t *testing.T) {
		// Two rolebindings share a name but not an API group. Dropping both groups
		// would print two identical lines for two different objects.
		artifact := `{"schemaVersion":"v1","items":[
			{"groupResource":"rolebindings.authorization.openshift.io","namespace":"ns","name":"system:deployers","outcome":"BackedUp"},
			{"groupResource":"rolebindings.rbac.authorization.k8s.io","namespace":"ns","name":"system:deployers","outcome":"BackedUp"}
		]}`
		stdout, _ := runAgainstArtifacts(t, "bk-tree-ambiguous", map[velerov1api.DownloadTargetKind]string{
			velerov1api.DownloadTargetKindBackupItemOutcomes: artifact,
			velerov1api.DownloadTargetKindBackupVolumeInfos:  `[]`,
		}, func(o *ItemOutcomesOptions) {
			o.ByOwner = true
			o.Output = "tree"
		})

		assert.Contains(t, stdout, "rolebindings (authorization) system:deployers (ns)")
		assert.Contains(t, stdout, "rolebindings (rbac) system:deployers (ns)")
	})

	t.Run("-o tree is stable across runs", func(t *testing.T) {
		render := func() string {
			out, _ := runAgainstArtifacts(t, "bk-tree-stable", map[velerov1api.DownloadTargetKind]string{
				velerov1api.DownloadTargetKindBackupItemOutcomes: kubeVirtArtifact,
				velerov1api.DownloadTargetKindBackupVolumeInfos:  volumeInfo("succeeded"),
			}, func(o *ItemOutcomesOptions) {
				o.ByOwner = true
				o.Output = "tree"
			})
			return out
		}

		// Children come out of a map, so without sorting the tree would reshuffle
		// between runs and be useless in a diff. Each call is a separate render of
		// the same artifact, which is what makes this a stability check.
		first := render()
		second := render()
		assert.Equal(t, first, second)
	})

	t.Run("--by-owner rejects an unknown output format", func(t *testing.T) {
		f := &factorymocks.Factory{}
		kbClient := velerotest.NewFakeControllerRuntimeClient(t)
		backup := builder.ForBackup(cmdtest.VeleroNameSpace, "bk-bad-format").Phase(velerov1api.BackupPhaseCompleted).Result()
		require.NoError(t, kbClient.Create(t.Context(), backup, &kbclient.CreateOptions{}))

		f.On("Namespace").Return(cmdtest.VeleroNameSpace)
		f.On("KubebuilderClient").Return(kbClient, nil)

		o := NewItemOutcomesOptions()
		flags := new(flag.FlagSet)
		o.BindFlags(flags)
		require.NoError(t, o.Complete([]string{"bk-bad-format"}, f))
		o.ByOwner = true
		o.Output = "yaml"

		mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gzipWriter := gzip.NewWriter(w)
			defer gzipWriter.Close()
			gzipWriter.Write([]byte(kubeVirtArtifact))
		}))
		defer mockServer.Close()

		go func() {
			time.Sleep(50 * time.Millisecond)
			downloadRequestList := &velerov1api.DownloadRequestList{}
			if err := kbClient.List(t.Context(), downloadRequestList, &kbclient.ListOptions{
				Namespace: cmdtest.VeleroNameSpace,
			}); err == nil {
				for _, dr := range downloadRequestList.Items {
					dr.Status.DownloadURL = mockServer.URL
					dr.Status.Phase = velerov1api.DownloadRequestPhaseProcessed
					kbClient.Update(t.Context(), &dr)
				}
			}
		}()

		o.Timeout = 5 * time.Second
		err := o.Run(NewItemOutcomesCommand(f), f)
		require.Error(t, err)
		assert.Equal(t, `unsupported output format "yaml": use table, json or tree`, err.Error())
	})
	t.Run("Rejects -o on its own, which would be ignored", func(t *testing.T) {
		content := `{"schemaVersion":"v1","items":[{"groupResource":"configmaps","namespace":"ns","name":"cm","outcome":"BackedUp"}]}`

		f := &factorymocks.Factory{}
		kbClient := velerotest.NewFakeControllerRuntimeClient(t)
		backup := builder.ForBackup(cmdtest.VeleroNameSpace, "bk-output-without-by-owner").Phase(velerov1api.BackupPhaseCompleted).Result()
		require.NoError(t, kbClient.Create(t.Context(), backup, &kbclient.CreateOptions{}))

		f.On("Namespace").Return(cmdtest.VeleroNameSpace)
		f.On("KubebuilderClient").Return(kbClient, nil)

		o := NewItemOutcomesOptions()
		flags := new(flag.FlagSet)
		o.BindFlags(flags)
		require.NoError(t, o.Complete([]string{"bk-output-without-by-owner"}, f))
		o.Output = "json"

		mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gzipWriter := gzip.NewWriter(w)
			defer gzipWriter.Close()
			gzipWriter.Write([]byte(content))
		}))
		defer mockServer.Close()

		done := make(chan struct{})
		defer close(done)
		go func() {
			for {
				select {
				case <-done:
					return
				case <-time.After(20 * time.Millisecond):
				}
				downloadRequestList := &velerov1api.DownloadRequestList{}
				if err := kbClient.List(t.Context(), downloadRequestList, &kbclient.ListOptions{
					Namespace: cmdtest.VeleroNameSpace,
				}); err != nil {
					continue
				}
				for _, dr := range downloadRequestList.Items {
					dr.Status.DownloadURL = mockServer.URL
					dr.Status.Phase = velerov1api.DownloadRequestPhaseProcessed
					kbClient.Update(t.Context(), &dr)
				}
			}
		}()

		o.Timeout = 5 * time.Second
		err := o.Run(NewItemOutcomesCommand(f), f)
		require.Error(t, err)
		assert.Equal(t, `--output "json" requires --by-owner: the artifact is printed as JSON otherwise`, err.Error())
	})

	t.Run("Rejects an explicit -o table on its own too", func(t *testing.T) {
		// table is the default, so only the flag's changed state distinguishes
		// "-o table given and ignored" from "-o not given at all".
		f := &factorymocks.Factory{}
		kbClient := velerotest.NewFakeControllerRuntimeClient(t)
		backup := builder.ForBackup(cmdtest.VeleroNameSpace, "bk-table-without-by-owner").Phase(velerov1api.BackupPhaseCompleted).Result()
		require.NoError(t, kbClient.Create(t.Context(), backup, &kbclient.CreateOptions{}))

		f.On("Namespace").Return(cmdtest.VeleroNameSpace)
		f.On("KubebuilderClient").Return(kbClient, nil)

		c := NewItemOutcomesCommand(f)
		require.NoError(t, c.Flags().Parse([]string{"-o", "table", "bk-table-without-by-owner"}))

		o := NewItemOutcomesOptions()
		o.BindFlags(new(flag.FlagSet))
		require.NoError(t, o.Complete([]string{"bk-table-without-by-owner"}, f))

		mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gzipWriter := gzip.NewWriter(w)
			defer gzipWriter.Close()
			gzipWriter.Write([]byte(`{"schemaVersion":"v1","items":[]}`))
		}))
		defer mockServer.Close()

		done := make(chan struct{})
		defer close(done)
		go func() {
			for {
				select {
				case <-done:
					return
				case <-time.After(20 * time.Millisecond):
				}
				downloadRequestList := &velerov1api.DownloadRequestList{}
				if err := kbClient.List(t.Context(), downloadRequestList, &kbclient.ListOptions{
					Namespace: cmdtest.VeleroNameSpace,
				}); err != nil {
					continue
				}
				for _, dr := range downloadRequestList.Items {
					dr.Status.DownloadURL = mockServer.URL
					dr.Status.Phase = velerov1api.DownloadRequestPhaseProcessed
					kbClient.Update(t.Context(), &dr)
				}
			}
		}()

		o.Timeout = 5 * time.Second
		err := o.Run(c, f)
		require.Error(t, err)
		assert.Equal(t, `--output "table" requires --by-owner: the artifact is printed as JSON otherwise`, err.Error())
	})
}

func TestNewItemOutcomesOptionsSetsTableOutput(t *testing.T) {
	assert.Equal(t, "table", NewItemOutcomesOptions().Output)
}
