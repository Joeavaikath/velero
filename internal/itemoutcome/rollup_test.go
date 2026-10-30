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

package itemoutcome

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	vmGR  = "virtualmachines.kubevirt.io"
	pvcGR = "persistentvolumeclaims"
	pvGR  = "persistentvolumes"
	dvGR  = "datavolumes.cdi.kubevirt.io"
)

func row(gr, ns, name string, outcome Outcome, parent *ItemRef) ItemOutcome {
	return ItemOutcome{GroupResource: gr, Namespace: ns, Name: name, Outcome: outcome, Parent: parent}
}

func ref(gr, ns, name string) *ItemRef {
	return &ItemRef{GroupResource: gr, Namespace: ns, Name: name}
}

// kubeVirtChain is the shape a KubeVirt backup produces: a VM owning a
// DataVolume and a VMI, which owns the virt-launcher pod, which mounts the disk.
func kubeVirtChain(pvcOutcome Outcome) *ItemOutcomes {
	return &ItemOutcomes{
		SchemaVersion: SchemaVersion,
		Items: []ItemOutcome{
			row(vmGR, "ns", "vm1", OutcomeBackedUp, nil),
			row(dvGR, "ns", "disk1", OutcomeBackedUp, ref(vmGR, "ns", "vm1")),
			row("virtualmachineinstances.kubevirt.io", "ns", "vm1", OutcomeBackedUp, ref(vmGR, "ns", "vm1")),
			row("pods", "ns", "virt-launcher-vm1", OutcomeBackedUp, ref("virtualmachineinstances.kubevirt.io", "ns", "vm1")),
			row(pvcGR, "ns", "disk1", pvcOutcome, ref("pods", "ns", "virt-launcher-vm1")),
			row(pvGR, "", "pv-1", OutcomeBackedUp, ref(pvcGR, "ns", "disk1")),
		},
	}
}

// The bug this rule exists to prevent: Velero records Skipped on a PVC when it
// declines pod-volume backup because a CSI snapshot is taking the volume. Both
// disks succeeded, so the VM must not read as skipped.
func TestRollupDoesNotFailOwnerForBenignSkip(t *testing.T) {
	outcomes := kubeVirtChain(OutcomeSkipped)
	vols := []VolumeRef{{PVCNamespace: "ns", PVCName: "disk1", PVName: "pv-1", Failed: false}}

	summaries := Rollup(outcomes, vols)

	require.Len(t, summaries, 1)
	assert.Equal(t, vmGR, summaries[0].Owner.GroupResource)
	assert.Equal(t, OutcomeBackedUp, summaries[0].Outcome,
		"a skipped volume that succeeded must not raise the owner's outcome")
	assert.Empty(t, summaries[0].Failed)
	assert.Equal(t, 1, summaries[0].Counts[OutcomeSkipped], "the row is still counted")
}

// A skipped PVC whose volume actually failed is real trouble and must surface.
func TestRollupSurfacesSkippedOwnerWhoseVolumeFailed(t *testing.T) {
	outcomes := kubeVirtChain(OutcomeSkipped)
	vols := []VolumeRef{{PVCNamespace: "ns", PVCName: "disk1", PVName: "pv-1", Failed: true}}

	summaries := Rollup(outcomes, vols)

	require.Len(t, summaries, 1)
	assert.Equal(t, OutcomeSkipped, summaries[0].Outcome)
	require.Len(t, summaries[0].Failed, 1)
	assert.Equal(t, "disk1", summaries[0].Failed[0].Name)
}

// Without volume info the join cannot confirm the skip was benign. Guessing
// either way would be worse than saying nothing: the row is still counted, but
// it does not raise the owner, keeping the command's answer identical to the
// describer's listing.
func TestRollupDoesNotGuessAtSkippedWithoutVolumeInfo(t *testing.T) {
	summaries := Rollup(kubeVirtChain(OutcomeSkipped), nil)

	require.Len(t, summaries, 1)
	assert.Equal(t, OutcomeBackedUp, summaries[0].Outcome)
	assert.Equal(t, 1, summaries[0].Counts[OutcomeSkipped])
	assert.Empty(t, summaries[0].Failed)
}

func TestRollupResolvesFailuresToTheRoot(t *testing.T) {
	outcomes := kubeVirtChain(OutcomeBackedUp)
	for i := range outcomes.Items {
		if outcomes.Items[i].GroupResource == pvcGR {
			outcomes.Items[i].Outcome = OutcomeFailed
			outcomes.Items[i].Messages = []string{"snapshot read failed"}
		}
	}

	summaries := Rollup(outcomes, nil)

	require.Len(t, summaries, 1)
	assert.Equal(t, vmGR, summaries[0].Owner.GroupResource, "the VM is the root, not the PVC")
	assert.Equal(t, OutcomeFailed, summaries[0].Outcome)
	require.Len(t, summaries[0].Failed, 1)
	assert.Equal(t, pvcGR, summaries[0].Failed[0].GroupResource)
}

// A dangling parent is out of the backup's scope, so the row is its own root
// rather than being dropped or invented as a child.
func TestRollupTreatsDanglingParentAsRoot(t *testing.T) {
	outcomes := &ItemOutcomes{
		SchemaVersion: SchemaVersion,
		Items: []ItemOutcome{
			row("customresourcedefinitions.apiextensions.k8s.io", "", "datavolumes.cdi.kubevirt.io",
				OutcomeFailed, ref("cdis.cdi.kubevirt.io", "", "operator")),
		},
	}

	summaries := Rollup(outcomes, nil)

	require.Len(t, summaries, 1)
	assert.Equal(t, "datavolumes.cdi.kubevirt.io", summaries[0].Owner.Name)
}

func TestRollupCRDAndCRSharingANameAreSeparateRoots(t *testing.T) {
	// A VirtualMachine and its VirtualMachineInstance share namespace and name.
	// Keying on those alone would make one the other's ancestor.
	outcomes := &ItemOutcomes{
		SchemaVersion: SchemaVersion,
		Items: []ItemOutcome{
			row(vmGR, "ns", "vm1", OutcomeFailed, nil),
			row("virtualmachineinstances.kubevirt.io", "ns", "vm1", OutcomeBackedUp, ref(vmGR, "ns", "vm1")),
		},
	}

	summaries := Rollup(outcomes, nil)

	require.Len(t, summaries, 1, "the VMI belongs to the VM's chain, not its own")
	assert.Equal(t, vmGR, summaries[0].Owner.GroupResource)
}

func TestRollupOrdersWorstFirst(t *testing.T) {
	outcomes := &ItemOutcomes{
		SchemaVersion: SchemaVersion,
		Items: []ItemOutcome{
			row("configmaps", "ns", "clean", OutcomeBackedUp, nil),
			row("configmaps", "ns", "warned", OutcomeWarning, nil),
			row("configmaps", "ns", "failed", OutcomeFailed, nil),
		},
	}

	summaries := Rollup(outcomes, nil)

	require.Len(t, summaries, 3)
	assert.Equal(t, OutcomeFailed, summaries[0].Outcome)
	assert.Equal(t, OutcomeWarning, summaries[1].Outcome)
	assert.Equal(t, OutcomeBackedUp, summaries[2].Outcome)
}

// A parented PV would otherwise double-report: its PVC and it resolve to the same
// volume info.
func TestRollupCountsOneFailedVolumeOnce(t *testing.T) {
	outcomes := kubeVirtChain(OutcomeBackedUp)
	vols := []VolumeRef{{PVCNamespace: "ns", PVCName: "disk1", PVName: "pv-1", Failed: true}}

	summaries := Rollup(outcomes, vols)

	require.Len(t, summaries, 1)
	assert.Equal(t, OutcomeBackedUp, summaries[0].Outcome,
		"the failed volume is attributed to the PVC, and the PVC row itself is BackedUp, so nothing is noteworthy here")
}

// A cycle must terminate rather than loop, and each row still lands in a chain.
func TestRollupTerminatesOnCyclicArtifact(t *testing.T) {
	a := row("configmaps", "ns", "a", OutcomeBackedUp, ref("configmaps", "ns", "b"))
	b := row("configmaps", "ns", "b", OutcomeBackedUp, ref("configmaps", "ns", "a"))
	outcomes := &ItemOutcomes{SchemaVersion: SchemaVersion, Items: []ItemOutcome{a, b}}

	summaries := Rollup(outcomes, nil)

	assert.NotEmpty(t, summaries, "a cycle must not hang or produce nothing")
}

func TestRollupHandlesNilArtifact(t *testing.T) {
	assert.Nil(t, Rollup(nil, nil))
}

func TestRollupEmptyArtifact(t *testing.T) {
	assert.Empty(t, Rollup(&ItemOutcomes{SchemaVersion: SchemaVersion}, nil))
}
