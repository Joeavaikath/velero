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
	"sort"

	"github.com/vmware-tanzu/velero/pkg/kuberesource"
)

// MaxDepth bounds how far a single parenting chain is followed, so a malformed
// or cyclic artifact cannot loop forever.
const MaxDepth = 32

// VolumeRef is the part of a backup's volume info the rollup rule needs.
//
// It is defined here rather than reusing internal/volume.BackupVolumeInfo
// because this package must stay a leaf: pkg/util/logging imports it for the
// per-item log hook, and internal/volume reaches pkg/util/logging, so importing
// volume would close a cycle. Callers map their volume info into this shape.
type VolumeRef struct {
	PVCNamespace string
	PVCName      string
	PVName       string
	// Failed reports whether the volume's own result was a failure. A volume that
	// was skipped by one path but captured by another is not a failure.
	Failed bool
}

// Key identifies an item within one artifact. It is the same flattened shape as
// ItemOutcome and ItemRef, so rows can be joined against parents without
// converting between types.
type Key struct {
	GroupResource string
	Namespace     string
	Name          string
}

// KeyOf returns the key identifying item.
func KeyOf(item ItemOutcome) Key {
	return Key{GroupResource: item.GroupResource, Namespace: item.Namespace, Name: item.Name}
}

// KeyOfRef returns the key identifying ref, for callers joining a row against
// its parent.
func KeyOfRef(ref *ItemRef) Key {
	return Key{GroupResource: ref.GroupResource, Namespace: ref.Namespace, Name: ref.Name}
}

// OwnerSummary is the rolled-up outcome of one root item and everything the
// artifact records beneath it.
type OwnerSummary struct {
	// Owner is the root of the chain: the item no other item names as a parent.
	Owner ItemRef `json:"owner"`
	// Outcome is the most significant outcome among the rows that actually
	// represent trouble, per the rule in IsFailure. A row that is merely Skipped
	// does not, on its own, raise the summary.
	Outcome Outcome `json:"outcome"`
	// Counts is how many rows sit in this chain, the root included.
	Counts map[Outcome]int `json:"counts"`
	// Failed lists the rows that made the summary worth reading.
	Failed []ItemRef `json:"failed,omitempty"`
}

// Rollup resolves every row to the root of its parenting chain and summarizes
// what happened under each root. Volume info is consulted because an item's own
// outcome is not enough to tell a benign skip from a lost disk: a PVC that
// Velero declined to back up by pod volume because a CSI snapshot was taking it
// records OutcomeSkipped, and only the volume result says the data was captured.
//
// volInfo may be nil, in which case the volume join cannot confirm that a skip
// was benign. That is not treated as a failure: guessing would re-introduce the
// false positive this rule exists to remove, and the row still appears in the
// owner's Counts. A caller that cannot load volume info should say so, so a
// reader knows a Skipped count is unexplained.
func Rollup(items *ItemOutcomes, vols []VolumeRef) []OwnerSummary {
	if items == nil {
		return nil
	}

	summaries := Summarize(items, vols)
	parentOf := ParentMap(IndexByKey(items))

	// ResolveRoot rather than "has no parent": a malformed artifact can hold a
	// cycle, and a cycle has no root at all. Asking each row for its root keeps
	// the result non-empty there instead of silently summarizing nothing.
	roots := map[Key]bool{}
	for key := range summaries {
		roots[ResolveRoot(parentOf, key)] = true
	}

	result := make([]OwnerSummary, 0, len(roots))
	for root := range roots {
		result = append(result, summaries[root])
	}

	sort.Slice(result, func(i, j int) bool {
		a, b := result[i], result[j]
		if precedence[a.Outcome] != precedence[b.Outcome] {
			return precedence[a.Outcome] > precedence[b.Outcome]
		}
		if a.Owner.Namespace != b.Owner.Namespace {
			return a.Owner.Namespace < b.Owner.Namespace
		}
		if a.Owner.Name != b.Owner.Name {
			return a.Owner.Name < b.Owner.Name
		}
		return a.Owner.GroupResource < b.Owner.GroupResource
	})

	return result
}

// Summarize returns one summary per row, each covering that row and everything
// the artifact records beneath it. A row carries the worst outcome anywhere in
// its subtree, so an ancestor shows the trouble its descendants caused — which
// is what lets a nested view put a verdict on every level rather than only on
// the root. Rollup is the roots-only projection of this.
//
// Counting a row at every level means a row appears in more than one summary.
// That is the point: the same disk failure is part of the PVC's summary, the
// pod's, the VMI's and the VM's, and a reader at any depth sees it.
func Summarize(items *ItemOutcomes, vols []VolumeRef) map[Key]OwnerSummary {
	if items == nil {
		return nil
	}

	byID := IndexByKey(items)
	parentOf := ParentMap(byID)
	index := IndexVolumes(vols)

	summaries := make(map[Key]OwnerSummary, len(byID))
	notable := map[Key][]ItemOutcome{}
	visited := map[Key]bool{}

	for _, item := range items.Items {
		failure := IsFailure(item, index, byID)

		// Walk from the row up its chain, counting it at every level. visited
		// bounds the walk so a cycle counts a row once instead of inflating
		// every level it loops through.
		clear(visited)
		for node, depth := KeyOf(item), 0; depth <= MaxDepth && !visited[node]; depth++ {
			visited[node] = true

			summary, seen := summaries[node]
			if !seen {
				summary = OwnerSummary{
					Owner:   ItemRef(node),
					Outcome: OutcomeBackedUp,
					Counts:  map[Outcome]int{},
				}
			}

			summary.Counts[item.Outcome]++
			if failure {
				notable[node] = append(notable[node], item)
				if raised := failureOutcome(item); precedence[raised] > precedence[summary.Outcome] {
					summary.Outcome = raised
				}
			}
			summaries[node] = summary

			parent, ok := parentOf[node]
			if !ok {
				break
			}
			node = parent
		}
	}

	for node, summary := range summaries {
		// Report the rows at the summary's own severity. A Failed owner should not
		// be explained by a list of warnings. The comparison is on failureOutcome,
		// the same value that raised the summary: a row whose failing volume
		// raised it to Failed must appear in Failed, or Outcome and Failed would
		// contradict each other.
		for _, item := range notable[node] {
			if precedence[failureOutcome(item)] != precedence[summary.Outcome] {
				continue
			}
			summary.Failed = append(summary.Failed, ItemRef{
				GroupResource: item.GroupResource, Namespace: item.Namespace, Name: item.Name,
			})
		}
		sort.Slice(summary.Failed, func(i, j int) bool {
			return summary.Failed[i].GroupResource+summary.Failed[i].Name <
				summary.Failed[j].GroupResource+summary.Failed[j].Name
		})
		summaries[node] = summary
	}

	return summaries
}

// failureOutcome is the outcome a summary should take on for a row that
// IsFailure judged to be trouble.
//
// It is normally the row's own outcome. The exception is a row that reads
// BackedUp but whose volume failed: the item was captured, and its disk was
// not, so the failure is real and the row's verdict understates it. Left as
// BackedUp the summary would name the row in Failed while its Outcome stayed
// clean, and every caller that asks "is this chain in trouble?" by reading
// Outcome alone -- the item-outcomes command's listing gate does exactly that --
// would report the backup as clean.
//
// Nothing is fabricated: the failure is what IsFailure already decided, and the
// row itself keeps its own verdict. Only the rollup's summary is raised.
func failureOutcome(item ItemOutcome) Outcome {
	if item.Outcome == OutcomeBackedUp {
		return OutcomeFailed
	}
	return item.Outcome
}

// IndexByKey maps each row to its key so parenting links can be followed.
func IndexByKey(items *ItemOutcomes) map[Key]ItemOutcome {
	byID := make(map[Key]ItemOutcome, len(items.Items))
	for _, item := range items.Items {
		byID[KeyOf(item)] = item
	}
	return byID
}

// ParentMap maps a child's key to its parent's key, dropping references to
// parents that are not in this backup's list. A dangling parent is not an error:
// a cluster-scoped owner can be outside a namespace-scoped backup's scope, and a
// row referencing one is simply treated as a root.
func ParentMap(byID map[Key]ItemOutcome) map[Key]Key {
	parentOf := make(map[Key]Key, len(byID))
	for key, item := range byID {
		if item.Parent == nil {
			continue
		}
		parentKey := KeyOfRef(item.Parent)
		if _, ok := byID[parentKey]; !ok {
			continue
		}
		parentOf[key] = parentKey
	}
	return parentOf
}

// ChildrenMap inverts a parent map.
func ChildrenMap(parentOf map[Key]Key) map[Key][]Key {
	childrenOf := make(map[Key][]Key, len(parentOf))
	for child, parent := range parentOf {
		childrenOf[parent] = append(childrenOf[parent], child)
	}
	return childrenOf
}

// Descendants returns an item and everything below it, breadth-first. The
// visited set and the size cap keep a malformed artifact from looping.
func Descendants(childrenOf map[Key][]Key, root Key) []Key {
	visited := map[Key]bool{root: true}
	result := []Key{root}
	queue := []Key{root}

	for len(queue) > 0 && len(result) < MaxDepth*MaxDepth {
		current := queue[0]
		queue = queue[1:]
		for _, child := range childrenOf[current] {
			if visited[child] {
				continue
			}
			visited[child] = true
			result = append(result, child)
			queue = append(queue, child)
		}
	}

	return result
}

// ResolveRoot walks up from key to the first item nothing names as a child.
func ResolveRoot(parentOf map[Key]Key, key Key) Key {
	for range MaxDepth {
		parent, ok := parentOf[key]
		if !ok {
			return key
		}
		key = parent
	}
	return key
}

// VolumeIndex maps a PVC or PV to the volume info recorded for it, so a row can
// be checked against what actually happened to its data.
type VolumeIndex struct {
	byPVC map[volumeItemKey]VolumeRef
	byPV  map[string]VolumeRef
}

type volumeItemKey struct {
	namespace string
	name      string
}

// IndexVolumes builds a VolumeIndex from a backup's volume info. A nil or empty
// slice yields an empty index, never a nil one, so callers need no nil checks.
func IndexVolumes(infos []VolumeRef) *VolumeIndex {
	index := &VolumeIndex{
		byPVC: map[volumeItemKey]VolumeRef{},
		byPV:  map[string]VolumeRef{},
	}
	for _, info := range infos {
		if info.PVCName != "" {
			index.byPVC[volumeItemKey{namespace: info.PVCNamespace, name: info.PVCName}] = info
		}
		if info.PVName != "" {
			index.byPV[info.PVName] = info
		}
	}
	return index
}

// IsFailure reports whether item represents real trouble, as opposed to a row
// that merely sounds like it. Failed and Warning always count.
//
// Skipped does not count on its own. Velero records OutcomeSkipped when it
// declines one path to a volume in favor of another — for example a PVC left to
// a CSI snapshot rather than copied by pod volume backup. Rolling that up would
// report a virtual machine as skipped while every one of its disks succeeded, so
// a skipped volume row only counts when volume info says the volume failed.
func IsFailure(item ItemOutcome, vols *VolumeIndex, byID map[Key]ItemOutcome) bool {
	if item.Outcome == OutcomeFailed || item.Outcome == OutcomeWarning {
		return true
	}

	if !showsVolume(item, byID) {
		return false
	}
	for _, info := range vols.ForItem(item) {
		if info.Failed {
			return true
		}
	}
	return false
}

// showsVolume reports whether the volume line belongs to this row. A PV whose
// PVC is also in the artifact defers to the PVC: both resolve to the same volume
// info, and counting both would double-report one failure.
func showsVolume(item ItemOutcome, byID map[Key]ItemOutcome) bool {
	if item.GroupResource != kuberesource.PersistentVolumes.String() || item.Parent == nil {
		return true
	}
	_, pvcPresent := byID[KeyOfRef(item.Parent)]
	return !pvcPresent
}

// ForItem returns the volume info recorded for item, if item is a PVC or PV.
func (v *VolumeIndex) ForItem(item ItemOutcome) []VolumeRef {
	if v == nil {
		return nil
	}

	switch item.GroupResource {
	case kuberesource.PersistentVolumeClaims.String():
		if info, ok := v.byPVC[volumeItemKey{namespace: item.Namespace, name: item.Name}]; ok {
			return []VolumeRef{info}
		}
	case kuberesource.PersistentVolumes.String():
		if info, ok := v.byPV[item.Name]; ok {
			return []VolumeRef{info}
		}
	}

	return nil
}
