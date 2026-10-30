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
	"bytes"
	"testing"
	"text/tabwriter"

	"github.com/stretchr/testify/assert"

	"github.com/vmware-tanzu/velero/internal/itemoutcome"
	"github.com/vmware-tanzu/velero/internal/volume"
	"github.com/vmware-tanzu/velero/pkg/kuberesource"
)

// newTestDescriber mirrors how the describe command builds its Describer, so
// assertions compare the exact text a user would see.
func newTestDescriber() *Describer {
	d := &Describer{
		Prefix: "",
		out:    &tabwriter.Writer{},
		buf:    &bytes.Buffer{},
	}
	d.out.Init(d.buf, 0, 8, 2, ' ', 0)
	return d
}

// flushed renders the describer's buffered output and returns it, so assertions
// compare the text a user would actually see.
func flushed(d *Describer) string {
	d.out.Flush()
	return d.buf.String()
}
func testVolumeIndex(infos ...volume.BackupVolumeInfo) *volumeResultsIndex {
	index := &volumeResultsIndex{
		byPVC: map[volumeItemKey]volume.BackupVolumeInfo{},
		byPV:  map[string]volume.BackupVolumeInfo{},
	}
	for _, info := range infos {
		if info.PVCName != "" {
			index.byPVC[itemKey(info.PVCNamespace, info.PVCName)] = info
		}
		if info.PVName != "" {
			index.byPV[info.PVName] = info
		}
	}
	return index
}

// testOutcomeIndex builds a one-row artifact index containing only the item
// under test, which is all isNoteworthyOutcome needs to decide whether a PV row
// should defer to its PVC.
func testOutcomeIndex(item itemoutcome.ItemOutcome) map[itemoutcome.Key]itemoutcome.ItemOutcome {
	return map[itemoutcome.Key]itemoutcome.ItemOutcome{itemoutcome.KeyOf(item): item}
}

func pvcOutcome(namespace, name string, outcome itemoutcome.Outcome, parent *itemoutcome.ItemRef) itemoutcome.ItemOutcome {
	return itemoutcome.ItemOutcome{
		GroupResource: kuberesource.PersistentVolumeClaims.String(),
		Namespace:     namespace,
		Name:          name,
		Outcome:       outcome,
		Parent:        parent,
	}
}

func TestVolumesForItem(t *testing.T) {
	index := testVolumeIndex(
		volume.BackupVolumeInfo{
			PVCNamespace: "vm-ns",
			PVCName:      "disk-0",
			PVName:       "pv-disk-0",
			BackupMethod: volume.CSISnapshot,
			Result:       volume.VolumeResultFailed,
		},
	)

	tests := []struct {
		name string
		item itemoutcome.ItemOutcome
		want int
	}{
		{
			name: "pvc row resolves by namespace and name",
			item: pvcOutcome("vm-ns", "disk-0", itemoutcome.OutcomeBackedUp, nil),
			want: 1,
		},
		{
			name: "pvc row in another namespace does not resolve",
			item: pvcOutcome("other-ns", "disk-0", itemoutcome.OutcomeBackedUp, nil),
			want: 0,
		},
		{
			name: "pv row resolves by pv name",
			item: itemoutcome.ItemOutcome{
				GroupResource: kuberesource.PersistentVolumes.String(),
				Name:          "pv-disk-0",
				Outcome:       itemoutcome.OutcomeBackedUp,
			},
			want: 1,
		},
		{
			name: "a vm row owns no volume directly",
			item: itemoutcome.ItemOutcome{
				GroupResource: "virtualmachines.kubevirt.io",
				Namespace:     "vm-ns",
				Name:          "my-vm",
				Outcome:       itemoutcome.OutcomeBackedUp,
			},
			want: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Len(t, volumesForItem(index, tc.item), tc.want)
		})
	}
}

func TestVolumesForItemNilIndex(t *testing.T) {
	assert.Nil(t, volumesForItem(nil, pvcOutcome("vm-ns", "disk-0", itemoutcome.OutcomeBackedUp, nil)))
}

// A PVC records BackedUp at the item level even when its snapshot failed: the
// volume verdict is the only place the failure shows up. The row must not be
// filtered out of the listing.
func TestIsNoteworthyOutcome(t *testing.T) {
	index := testVolumeIndex(
		volume.BackupVolumeInfo{
			PVCNamespace: "vm-ns",
			PVCName:      "disk-0",
			BackupMethod: volume.CSISnapshot,
			Result:       volume.VolumeResultFailed,
		},
		volume.BackupVolumeInfo{
			PVCNamespace: "vm-ns",
			PVCName:      "disk-1",
			BackupMethod: volume.CSISnapshot,
			Result:       volume.VolumeResultSucceeded,
		},
	)

	tests := []struct {
		name string
		item itemoutcome.ItemOutcome
		want bool
	}{
		{
			name: "failed item",
			item: pvcOutcome("vm-ns", "disk-0", itemoutcome.OutcomeFailed, nil),
			want: true,
		},
		{
			name: "warning item",
			item: pvcOutcome("vm-ns", "disk-0", itemoutcome.OutcomeWarning, nil),
			want: true,
		},
		{
			name: "backed up item whose volume failed",
			item: pvcOutcome("vm-ns", "disk-0", itemoutcome.OutcomeBackedUp, nil),
			want: true,
		},
		{
			name: "cleanly backed up item",
			item: pvcOutcome("vm-ns", "disk-1", itemoutcome.OutcomeBackedUp, nil),
			want: false,
		},
		{
			name: "skipped item with no volume failure",
			item: pvcOutcome("vm-ns", "disk-1", itemoutcome.OutcomeSkipped, nil),
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isNoteworthyOutcome(tc.item, index, testOutcomeIndex(tc.item)))
		})
	}
}

func TestSummariseVolumes(t *testing.T) {
	vmParent := &itemoutcome.ItemRef{
		GroupResource: "virtualmachines.kubevirt.io",
		Namespace:     "vm-ns",
		Name:          "my-vm",
	}

	index := testVolumeIndex(
		volume.BackupVolumeInfo{
			PVCNamespace: "vm-ns",
			PVCName:      "disk-0",
			BackupMethod: volume.CSISnapshot,
			Result:       volume.VolumeResultFailed,
		},
		volume.BackupVolumeInfo{
			PVCNamespace: "vm-ns",
			PVCName:      "disk-1",
			BackupMethod: volume.CSISnapshot,
			Result:       volume.VolumeResultSucceeded,
		},
		volume.BackupVolumeInfo{
			PVCNamespace:  "vm-ns",
			PVCName:       "disk-2",
			BackupMethod:  volume.CSISnapshot,
			Skipped:       true,
			SkippedReason: "CSI: skipped for PodVolumeBackup",
		},
	)

	children := []itemoutcome.ItemOutcome{
		pvcOutcome("vm-ns", "disk-0", itemoutcome.OutcomeBackedUp, vmParent),
		pvcOutcome("vm-ns", "disk-1", itemoutcome.OutcomeBackedUp, vmParent),
		pvcOutcome("vm-ns", "disk-2", itemoutcome.OutcomeSkipped, vmParent),
	}

	assert.Equal(t, "1 of 3 volumes failed, 1 skipped", summariseVolumes(index, children, map[itemoutcome.Key]itemoutcome.ItemOutcome{}))
}

func TestSummariseVolumesNoVolumes(t *testing.T) {
	children := []itemoutcome.ItemOutcome{
		pvcOutcome("vm-ns", "disk-0", itemoutcome.OutcomeBackedUp, nil),
	}
	assert.Equal(t, "", summariseVolumes(testVolumeIndex(), children, map[itemoutcome.Key]itemoutcome.ItemOutcome{}))
}

func TestDescribeVolumeResult(t *testing.T) {
	tests := []struct {
		name string
		info volume.BackupVolumeInfo
		want string
	}{
		{
			name: "failed csi snapshot names the pvc, pv, method and handle",
			info: volume.BackupVolumeInfo{
				PVCNamespace: "vm-ns",
				PVCName:      "disk-0",
				PVName:       "pv-disk-0",
				BackupMethod: volume.CSISnapshot,
				Result:       volume.VolumeResultFailed,
				CSISnapshotInfo: &volume.CSISnapshotInfo{
					SnapshotHandle: "snap-0abc",
				},
			},
			want: "disk-0 (pv=pv-disk-0) CSISnapshot failed (snap-0abc)",
		},
		{
			name: "skipped volume reports its reason",
			info: volume.BackupVolumeInfo{
				PVCNamespace:  "vm-ns",
				PVCName:       "disk-1",
				BackupMethod:  volume.CSISnapshot,
				Skipped:       true,
				SkippedReason: "CSI: skipped for PodVolumeBackup",
			},
			want: "disk-1 CSISnapshot skipped: CSI: skipped for PodVolumeBackup",
		},
		{
			name: "pvc-only reference",
			info: volume.BackupVolumeInfo{
				PVCNamespace: "vm-ns",
				PVCName:      "disk-2",
				BackupMethod: volume.PodVolumeBackup,
				Result:       volume.VolumeResultSucceeded,
			},
			want: "disk-2 PodVolumeBackup succeeded",
		},
		{
			name: "missing method and result stay legible",
			info: volume.BackupVolumeInfo{
				PVCNamespace: "vm-ns",
				PVCName:      "disk-3",
			},
			want: "disk-3 unknown method unknown result",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, describeVolumeResult(tc.info))
		})
	}
}

func TestDescribeVolumeResultNativeSnapshotHandle(t *testing.T) {
	info := volume.BackupVolumeInfo{
		PVCNamespace: "vm-ns",
		PVCName:      "disk-0",
		PVName:       "pv-disk-0",
		BackupMethod: volume.NativeSnapshot,
		Result:       volume.VolumeResultSucceeded,
		NativeSnapshotInfo: &volume.NativeSnapshotInfo{
			SnapshotHandle: "vol-0123",
		},
	}
	assert.Equal(t, "disk-0 (pv=pv-disk-0) NativeSnapshot succeeded (vol-0123)", describeVolumeResult(info))
}

// The scenario this feature exists for: a VM whose own row reads BackedUp, one
// of whose two disks failed its snapshot. A KubeVirt VM's disks hang four levels
// below it (VM -> VMI -> virt-launcher pod -> PVC), so the listing must climb to
// the VM and report the failure there, not under the pod.
func TestDescribeItemOutcomeListingVMFailedDisk(t *testing.T) {
	outcomes := &itemoutcome.ItemOutcomes{
		SchemaVersion: itemoutcome.SchemaVersion,
		Items:         kubeVirtChain("cirros-multipvc-test", "cirros-multipvc-test", itemoutcome.OutcomeBackedUp),
	}

	index := testVolumeIndex(
		volume.BackupVolumeInfo{
			PVCNamespace: "cirros-multipvc-test",
			PVCName:      "cirros-multipvc-test-disk",
			PVName:       "pv-disk",
			BackupMethod: volume.CSISnapshot,
			Result:       volume.VolumeResultFailed,
			CSISnapshotInfo: &volume.CSISnapshotInfo{
				SnapshotHandle: "snap-0cb3dc480464a34b8",
			},
		},
		volume.BackupVolumeInfo{
			PVCNamespace: "cirros-multipvc-test",
			PVCName:      "cirros-multipvc-test-datadisk",
			PVName:       "pv-datadisk",
			BackupMethod: volume.CSISnapshot,
			Result:       volume.VolumeResultSucceeded,
			CSISnapshotInfo: &volume.CSISnapshotInfo{
				SnapshotHandle: "snap-0fdd33540d741f1d6",
			},
		},
	)

	d := newTestDescriber()
	describeItemOutcomeListing(d, outcomes, index)

	expected := `Item Outcomes:  6 BackedUp, 0 Warning, 0 Failed, 0 Skipped
  1 volume(s) failed; see below
  virtualmachines.kubevirt.io cirros-multipvc-test/cirros-multipvc-test (1 of 2 volumes failed):
    persistentvolumeclaims cirros-multipvc-test/cirros-multipvc-test-disk: BackedUp
      volume: cirros-multipvc-test-disk (pv=pv-disk) CSISnapshot failed (snap-0cb3dc480464a34b8)
`
	assert.Equal(t, expected, flushed(d))
}

// kubeVirtChain builds the parenting shape a KubeVirt VM backup actually
// produces, as observed in a real item-outcomes artifact.
func kubeVirtChain(namespace, vmName string, outcome itemoutcome.Outcome) []itemoutcome.ItemOutcome {
	vmRef := &itemoutcome.ItemRef{
		GroupResource: "virtualmachines.kubevirt.io",
		Namespace:     namespace,
		Name:          vmName,
	}
	vmiName := vmName
	podName := "virt-launcher-" + vmName + "-cz5xs"

	return []itemoutcome.ItemOutcome{
		{
			GroupResource: "virtualmachines.kubevirt.io",
			Namespace:     namespace,
			Name:          vmName,
			Outcome:       outcome,
		},
		{
			GroupResource: "virtualmachineinstances.kubevirt.io",
			Namespace:     namespace,
			Name:          vmiName,
			Outcome:       outcome,
			Parent:        vmRef,
		},
		{
			GroupResource: "pods",
			Namespace:     namespace,
			Name:          podName,
			Outcome:       outcome,
			Parent: &itemoutcome.ItemRef{
				GroupResource: "virtualmachineinstances.kubevirt.io",
				Namespace:     namespace,
				Name:          vmiName,
			},
		},
		{
			GroupResource: kuberesource.PersistentVolumeClaims.String(),
			Namespace:     namespace,
			Name:          vmName + "-disk",
			Outcome:       outcome,
			Parent: &itemoutcome.ItemRef{
				GroupResource: "pods",
				Namespace:     namespace,
				Name:          podName,
			},
		},
		{
			GroupResource: kuberesource.PersistentVolumeClaims.String(),
			Namespace:     namespace,
			Name:          vmName + "-datadisk",
			Outcome:       outcome,
			Parent: &itemoutcome.ItemRef{
				GroupResource: "pods",
				Namespace:     namespace,
				Name:          podName,
			},
		},
		{
			GroupResource: kuberesource.PersistentVolumes.String(),
			Name:          "pv-disk",
			Outcome:       outcome,
			Parent: &itemoutcome.ItemRef{
				GroupResource: kuberesource.PersistentVolumeClaims.String(),
				Namespace:     namespace,
				Name:          vmName + "-disk",
			},
		},
	}
}

// A failure recorded on the pod deep in the chain still surfaces under the VM.
func TestDescribeItemOutcomeListingAnchorsFailureAtVM(t *testing.T) {
	items := kubeVirtChain("vm-ns", "my-vm", itemoutcome.OutcomeBackedUp)
	for i := range items {
		if items[i].GroupResource == "pods" {
			items[i].Outcome = itemoutcome.OutcomeFailed
			items[i].Messages = []string{"pod volume backup failed"}
		}
	}

	d := newTestDescriber()
	describeItemOutcomeListing(d, &itemoutcome.ItemOutcomes{Items: items}, testVolumeIndex())

	expected := `Item Outcomes:  5 BackedUp, 0 Warning, 1 Failed, 0 Skipped
  virtualmachines.kubevirt.io vm-ns/my-vm:
    pods vm-ns/virt-launcher-my-vm-cz5xs: Failed
      pod volume backup failed
`
	assert.Equal(t, expected, flushed(d))
}

// A parent whose children are all clean produces no header, so a successful
// backup stays quiet.
func TestDescribeItemOutcomeListingCleanStaysQuiet(t *testing.T) {
	index := testVolumeIndex(
		volume.BackupVolumeInfo{
			PVCNamespace: "cirros-multipvc-test",
			PVCName:      "cirros-multipvc-test-disk",
			BackupMethod: volume.CSISnapshot,
			Result:       volume.VolumeResultSucceeded,
		},
		volume.BackupVolumeInfo{
			PVCNamespace: "cirros-multipvc-test",
			PVCName:      "cirros-multipvc-test-datadisk",
			BackupMethod: volume.CSISnapshot,
			Result:       volume.VolumeResultSucceeded,
		},
	)

	outcomes := &itemoutcome.ItemOutcomes{
		Items: kubeVirtChain("cirros-multipvc-test", "cirros-multipvc-test", itemoutcome.OutcomeBackedUp),
	}

	d := newTestDescriber()
	describeItemOutcomeListing(d, outcomes, index)

	assert.Equal(t, "Item Outcomes:  6 BackedUp, 0 Warning, 0 Failed, 0 Skipped\n", flushed(d))
}

// Failures recorded against the item itself are still surfaced, independent of
// any volume detail. A backup with no volume artifact must behave exactly as it
// did before the join existed.
func TestDescribeItemOutcomeListingWithoutVolumeInfo(t *testing.T) {
	outcomes := &itemoutcome.ItemOutcomes{
		Items: []itemoutcome.ItemOutcome{
			{
				GroupResource: kuberesource.PersistentVolumeClaims.String(),
				Namespace:     "vm-ns",
				Name:          "disk-0",
				Outcome:       itemoutcome.OutcomeFailed,
				Messages:      []string{"snapshot failed"},
			},
		},
	}

	d := newTestDescriber()
	describeItemOutcomeListing(d, outcomes, testVolumeIndex())

	expected := `Item Outcomes:  0 BackedUp, 0 Warning, 1 Failed, 0 Skipped
  persistentvolumeclaims vm-ns/disk-0: Failed
    snapshot failed
`
	assert.Equal(t, expected, flushed(d))
}

// A malformed artifact with a parenting cycle must not hang the describer.
func TestDescribeItemOutcomeListingCycleIsBounded(t *testing.T) {
	outcomes := &itemoutcome.ItemOutcomes{
		Items: []itemoutcome.ItemOutcome{
			{
				GroupResource: "pods",
				Namespace:     "ns",
				Name:          "a",
				Outcome:       itemoutcome.OutcomeFailed,
				Parent:        &itemoutcome.ItemRef{GroupResource: "pods", Namespace: "ns", Name: "b"},
			},
			{
				GroupResource: "pods",
				Namespace:     "ns",
				Name:          "b",
				Outcome:       itemoutcome.OutcomeFailed,
				Parent:        &itemoutcome.ItemRef{GroupResource: "pods", Namespace: "ns", Name: "a"},
			},
		},
	}

	d := newTestDescriber()
	assert.NotPanics(t, func() {
		describeItemOutcomeListing(d, outcomes, testVolumeIndex())
	})
	assert.Contains(t, flushed(d), "2 Failed")
}

func TestDescribeItemOutcomeListingNone(t *testing.T) {
	d := newTestDescriber()
	describeItemOutcomeListing(d, &itemoutcome.ItemOutcomes{}, testVolumeIndex())
	assert.Equal(t, "Item Outcomes:  <none>\n", flushed(d))
}
