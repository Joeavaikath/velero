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
	"testing"

	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	corev1api "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/vmware-tanzu/velero/internal/itemoutcome"
	"github.com/vmware-tanzu/velero/pkg/discovery/mocks"
	"github.com/vmware-tanzu/velero/pkg/kuberesource"
	"github.com/vmware-tanzu/velero/pkg/plugin/velero"
	ptr "k8s.io/utils/ptr"
)

var (
	dataVolumeGR = schema.GroupResource{Group: "cdi.kubevirt.io", Resource: "datavolumes"}
	nodeGR       = schema.GroupResource{Resource: "nodes"}
	pvcID        = velero.ResourceIdentifier{GroupResource: kuberesource.PersistentVolumeClaims, Namespace: "ns1", Name: "disk1"}
)

// pvcOwnedByDataVolume is the CDI shape: a DataVolume creates a PVC and takes a
// controller reference on it, which is the edge nothing else records.
func pvcOwnedByDataVolume() *corev1api.PersistentVolumeClaim {
	p := &corev1api.PersistentVolumeClaim{}
	p.Namespace = "ns1"
	p.Name = "disk1"
	p.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "cdi.kubevirt.io/v1beta1",
		Kind:       "DataVolume",
		Name:       "disk1",
		Controller: ptr.To(true),
	}}
	return p
}

func newOwnerTestBackupper(helper *mocks.Helper) *itemBackupper {
	return &itemBackupper{
		backupRequest:   &Request{ItemOutcomes: itemoutcome.NewRecorder()},
		discoveryHelper: helper,
	}
}

func parentOf(t *testing.T, ib *itemBackupper, id velero.ResourceIdentifier) *itemoutcome.ItemRef {
	t.Helper()
	for _, item := range ib.backupRequest.ItemOutcomes.Result().Items {
		if item.GroupResource == id.GroupResource.String() && item.Namespace == id.Namespace && item.Name == id.Name {
			return item.Parent
		}
	}
	t.Fatalf("no row for %s/%s", id.Namespace, id.Name)
	return nil
}

func TestRecordOwnerParentUsesControllerReference(t *testing.T) {
	helper := new(mocks.Helper)
	helper.On("KindFor", schema.GroupVersionKind{Group: "cdi.kubevirt.io", Version: "v1beta1", Kind: "DataVolume"}).
		Return(dataVolumeGR.WithVersion("v1beta1"), metav1.APIResource{Name: "datavolumes", Namespaced: true}, nil)
	ib := newOwnerTestBackupper(helper)

	ib.recordOwnerParent(pvcOwnedByDataVolume(), pvcID)

	require.NotNil(t, parentOf(t, ib, pvcID))
	assert.Equal(t, itemoutcome.ItemRef{GroupResource: "datavolumes.cdi.kubevirt.io", Namespace: "ns1", Name: "disk1"},
		*parentOf(t, ib, pvcID))
}

// A cluster-scoped owner is not in the child's namespace, so inheriting it would
// name an item that cannot exist.
func TestRecordOwnerParentLeavesClusterScopedOwnerUnnamespaced(t *testing.T) {
	helper := new(mocks.Helper)
	helper.On("KindFor", schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Node"}).
		Return(nodeGR.WithVersion("v1"), metav1.APIResource{Name: "nodes", Namespaced: false}, nil)
	ib := newOwnerTestBackupper(helper)

	pod := &corev1api.Pod{}
	pod.Namespace = "ns1"
	pod.Name = "pod1"
	pod.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "v1", Kind: "Node", Name: "node1", Controller: ptr.To(true),
	}}
	podID := velero.ResourceIdentifier{GroupResource: kuberesource.Pods, Namespace: "ns1", Name: "pod1"}

	ib.recordOwnerParent(pod, podID)

	require.NotNil(t, parentOf(t, ib, podID))
	assert.Equal(t, "", parentOf(t, ib, podID).Namespace)
	assert.Equal(t, "node1", parentOf(t, ib, podID).Name)
}

// ReplicaSets own the API server's pods, and Velero does not list them as items.
// Failing discovery for the kind must drop the edge, not the backup.
func TestRecordOwnerParentSkipsUnresolvableOwner(t *testing.T) {
	helper := new(mocks.Helper)
	helper.On("KindFor", mock.Anything).
		Return(schema.GroupVersionResource{}, metav1.APIResource{}, errors.New("no mapping"))
	ib := newOwnerTestBackupper(helper)

	pod := &corev1api.Pod{}
	pod.Namespace = "ns1"
	pod.Name = "pod1"
	pod.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "pod1-abc", Controller: ptr.To(true),
	}}
	podID := velero.ResourceIdentifier{GroupResource: kuberesource.Pods, Namespace: "ns1", Name: "pod1"}

	ib.recordOwnerParent(pod, podID)

	ib.backupRequest.ItemOutcomes.Observe(podID)
	assert.Nil(t, parentOf(t, ib, podID))
}

func TestRecordOwnerParentIgnoresNonControllerReferences(t *testing.T) {
	helper := new(mocks.Helper)
	ib := newOwnerTestBackupper(helper)

	pvc := &corev1api.PersistentVolumeClaim{}
	pvc.Namespace = "ns1"
	pvc.Name = "disk1"
	pvc.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "cdi.kubevirt.io/v1beta1", Kind: "DataVolume", Name: "disk1",
		// Controller unset: the object is merely associated with the DataVolume.
	}}

	ib.recordOwnerParent(pvc, pvcID)
	ib.backupRequest.ItemOutcomes.Observe(pvcID)

	assert.Nil(t, parentOf(t, ib, pvcID))
}

func TestRecordOwnerParentRefusesSelfEdge(t *testing.T) {
	helper := new(mocks.Helper)
	helper.On("KindFor", mock.Anything).
		Return(dataVolumeGR.WithVersion("v1beta1"), metav1.APIResource{Name: "datavolumes", Namespaced: true}, nil)
	ib := newOwnerTestBackupper(helper)

	dv := &corev1api.PersistentVolumeClaim{}
	dv.Namespace = "ns1"
	dv.Name = "disk1"
	dv.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "cdi.kubevirt.io/v1beta1", Kind: "DataVolume", Name: "disk1", Controller: ptr.To(true),
	}}

	// Force the owner to resolve to the child's own identity.
	self := velero.ResourceIdentifier{GroupResource: dataVolumeGR, Namespace: "ns1", Name: "disk1"}
	ib.recordOwnerParent(dv, self)
	ib.backupRequest.ItemOutcomes.Observe(self)

	assert.Nil(t, parentOf(t, ib, self))
}

func TestRecordOwnerParentIsNoOpWithoutRecorder(t *testing.T) {
	ib := &itemBackupper{backupRequest: &Request{}}
	helper := new(mocks.Helper)
	ib.discoveryHelper = helper

	// ItemOutcomes is nil when the backup did not opt in; this must not panic.
	ib.recordOwnerParent(pvcOwnedByDataVolume(), pvcID)
	helper.AssertNotCalled(t, "KindFor", mock.Anything)
}
