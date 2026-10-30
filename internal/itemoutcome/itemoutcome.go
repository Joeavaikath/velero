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

// Package itemoutcome defines the per-item backup outcomes artifact and the
// in-memory recorder used to build it during a backup.
package itemoutcome

import (
	"fmt"
	"sort"
	"sync"

	"github.com/vmware-tanzu/velero/pkg/plugin/velero"
)

// SchemaVersion is the current schema version of the ItemOutcomes artifact.
const SchemaVersion = "v1"

// Outcome is the result of processing a single item during a backup.
type Outcome string

const (
	// OutcomeBackedUp means the item was processed with no error or warning.
	OutcomeBackedUp Outcome = "BackedUp"
	// OutcomeWarning means the item was processed but produced one or more warnings.
	OutcomeWarning Outcome = "Warning"
	// OutcomeFailed means the item errored, synchronously or during an async operation.
	OutcomeFailed Outcome = "Failed"
	// OutcomeSkipped means the item was excluded or skipped (e.g. a skipped PVC).
	OutcomeSkipped Outcome = "Skipped"
	// OutcomeNamed means another item's backup named this one as its child, but
	// nothing ever processed it: it was not observed, warned, failed or skipped.
	// It is the only outcome that is not a verdict, and it exists so that a row
	// created by an ownership edge cannot claim a success nobody established.
	OutcomeNamed Outcome = "Named"
)

// precedence ranks outcomes from least to most significant, so that repeated
// writes for one item collapse deterministically: Failed > Skipped > Warning > BackedUp > Named.
// Named sits below BackedUp because any real verdict outranks the absence of one.
var precedence = map[Outcome]int{
	OutcomeNamed:    -1,
	OutcomeBackedUp: 0,
	OutcomeWarning:  1,
	OutcomeSkipped:  2,
	OutcomeFailed:   3,
}

// MaxMessagesPerItem bounds how many of an item's messages reach the artifact.
// Five is enough to act on; past that the artifact stops being readable and
// starts being a log. A counted marker replaces the rest rather than the tail
// being dropped silently, so a reader can tell a truncated row from a clean one.
const MaxMessagesPerItem = 5

// capMessages returns msgs bounded to MaxMessagesPerItem, with a marker standing
// in for what it replaced. The marker is itself one of the returned entries, so
// the result never exceeds the cap.
//
// It is applied on output rather than on each append: every writer (Warn, Fail,
// Skip) appends unconditionally, and capping here bounds all of them, including
// a row whose messages arrive from more than one of them.
func capMessages(msgs []string) []string {
	if len(msgs) <= MaxMessagesPerItem {
		return msgs
	}

	shown := make([]string, 0, MaxMessagesPerItem)
	shown = append(shown, msgs[:MaxMessagesPerItem-1]...)
	shown = append(shown, fmt.Sprintf("... and %d more", len(msgs)-MaxMessagesPerItem+1))
	return shown
}

// ParentSource says where a child-to-parent edge came from, so a consumer can
// tell an ownership edge that Kubernetes records on the object from an edge a
// backup item action inferred, and can rank them accordingly.
type ParentSource string

const (
	// ParentSourceOwnerReference is an edge taken from the child's
	// metadata.ownerReferences. The object itself declares the relationship.
	ParentSourceOwnerReference ParentSource = "ownerReference"
	// ParentSourceAdditionalItem is an edge taken from a backup item action's
	// AdditionalItems. A plugin named the child while backing up the parent, so
	// the plugin's view of the relationship outranks the object's own metadata.
	ParentSourceAdditionalItem ParentSource = "additionalItem"
)

// precedence ranks parent sources from weakest to strongest. When two sources
// name different parents for one child the stronger one wins, which makes the
// resulting tree independent of the order concurrent item blocks happen to
// finish in.
var parentSourcePrecedence = map[ParentSource]int{
	ParentSourceOwnerReference: 0,
	ParentSourceAdditionalItem: 1,
}

// ItemOutcome is the outcome of a single backed-up item.
type ItemOutcome struct {
	GroupResource string        `json:"groupResource"` // e.g. "deployments.apps"
	Namespace     string        `json:"namespace,omitempty"`
	Name          string        `json:"name"`
	Outcome       Outcome       `json:"outcome"`
	Messages      []string      `json:"messages,omitempty"` // errors/warnings/skip reasons attributed to this item
	Parent        *ItemRef      `json:"parent,omitempty"`   // set when this item was backed up as a sub-item of another (e.g. a VM's PVC)
	ParentSource  *ParentSource `json:"parentSource,omitempty"`
}

// ItemRef identifies an item using the same flattened shape as ItemOutcome,
// so Parent reads consistently with the rest of the artifact. The plugin API's
// ResourceIdentifier is deliberately not used here: it has no json tags, so it
// would serialize as capitalized Group/Resource fields.
type ItemRef struct {
	GroupResource string `json:"groupResource"`
	Namespace     string `json:"namespace,omitempty"`
	Name          string `json:"name"`
}

func newItemRef(id velero.ResourceIdentifier) *ItemRef {
	return &ItemRef{
		GroupResource: id.GroupResource.String(),
		Namespace:     id.Namespace,
		Name:          id.Name,
	}
}

func newParentRef(id *velero.ResourceIdentifier) *ItemRef {
	if id == nil {
		return nil
	}
	return newItemRef(*id)
}

// ItemOutcomes is the top-level artifact, a versioned list.
type ItemOutcomes struct {
	SchemaVersion string        `json:"schemaVersion"` // "v1"
	Items         []ItemOutcome `json:"items"`
}

// Recorder accumulates per-item outcomes for one backup run, keyed by
// velero.ResourceIdentifier so repeated writes for one item collapse into a
// single row.
type Recorder struct {
	mu   sync.Mutex
	rows map[velero.ResourceIdentifier]*itemOutcomeState
}

type itemOutcomeState struct {
	outcome      Outcome
	messages     []string
	parent       *velero.ResourceIdentifier
	parentSource ParentSource
}

// NewRecorder returns an initialized, empty Recorder.
func NewRecorder() *Recorder {
	return &Recorder{
		rows: make(map[velero.ResourceIdentifier]*itemOutcomeState),
	}
}

// rowFor returns the row for id, minting it at OutcomeBackedUp if it has none.
// Callers that are about to record a verdict use this; callers that are only
// learning that the item exists use edgeRowFor.
func (r *Recorder) rowFor(id velero.ResourceIdentifier) *itemOutcomeState {
	return r.ensureRow(id, OutcomeBackedUp)
}

// edgeRowFor returns the row for id, minting it at OutcomeNamed if it has none.
// A row created by an ownership edge records that something named the item, not
// that anything processed it, so it must not start at OutcomeBackedUp.
func (r *Recorder) edgeRowFor(id velero.ResourceIdentifier) *itemOutcomeState {
	return r.ensureRow(id, OutcomeNamed)
}

func (r *Recorder) ensureRow(id velero.ResourceIdentifier, mint Outcome) *itemOutcomeState {
	row, ok := r.rows[id]
	if !ok {
		row = &itemOutcomeState{outcome: mint}
		r.rows[id] = row
	}
	return row
}

// upgrade moves a row's outcome to the given outcome if it is more
// significant than the row's current outcome, per the precedence order
// Failed > Skipped > Warning > BackedUp > Named.
func (row *itemOutcomeState) upgrade(outcome Outcome) {
	if precedence[outcome] > precedence[row.outcome] {
		row.outcome = outcome
	}
}

// Observe records that an item began processing, promoting a row that an
// ownership edge minted at OutcomeNamed to OutcomeBackedUp and minting one at
// OutcomeBackedUp if there is no existing row.
func (r *Recorder) Observe(id velero.ResourceIdentifier) {
	if r == nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// rowFor mints an absent row at BackedUp; the upgrade then promotes a row an
	// ownership edge already minted at Named. It is a no-op for every other
	// outcome, since upgrade only ever raises a severity.
	r.rowFor(id).upgrade(OutcomeBackedUp)
}

// Warn upgrades an item's outcome to OutcomeWarning, unless it is already
// OutcomeFailed, and appends msg to its messages.
func (r *Recorder) Warn(id velero.ResourceIdentifier, msg string) {
	if r == nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	row := r.rowFor(id)
	row.upgrade(OutcomeWarning)
	row.messages = append(row.messages, msg)
}

// Fail upgrades an item's outcome to OutcomeFailed and appends msg to its
// messages.
func (r *Recorder) Fail(id velero.ResourceIdentifier, msg string) {
	if r == nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	row := r.rowFor(id)
	row.upgrade(OutcomeFailed)
	row.messages = append(row.messages, msg)
}

// Skip upgrades an item's outcome to OutcomeSkipped and appends reason to its
// messages.
func (r *Recorder) Skip(id velero.ResourceIdentifier, reason string) {
	if r == nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	row := r.rowFor(id)
	row.upgrade(OutcomeSkipped)
	row.messages = append(row.messages, reason)
}

// SetParent records that child was backed up as a sub-item of parent, as named
// by a backup item action's AdditionalItems. A plugin recording the child while
// backing up the parent is a stronger claim than the child's own
// metadata.ownerReferences, so this overrides any owner-reference parent set by
// SetOwnerParent.
func (r *Recorder) SetParent(child, parent velero.ResourceIdentifier) {
	r.setParent(child, parent, ParentSourceAdditionalItem)
}

// SetOwnerParent records that child names parent in its metadata.ownerReferences.
// It is a weaker claim than SetParent and never displaces a parent a backup item
// action has already named.
func (r *Recorder) SetOwnerParent(child, parent velero.ResourceIdentifier) {
	r.setParent(child, parent, ParentSourceOwnerReference)
}

// setParent installs child's parent if the claim is at least as strong as the
// one already recorded. Comparing on source rather than arrival order keeps the
// tree stable when two item blocks back up a child and its parent concurrently:
// whichever finishes first, the surviving edge is the same.
//
// The row is minted at OutcomeNamed rather than OutcomeBackedUp. An edge is set
// before the child is fetched or backed up, so at this point nothing has
// confirmed the child is in the backup; when the child is processed, Observe
// promotes the row. A row still reading Named in a finished artifact is one a
// plugin named and nothing resolved.
func (r *Recorder) setParent(child, parent velero.ResourceIdentifier, source ParentSource) {
	if r == nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	row := r.edgeRowFor(child)
	if row.parent != nil && parentSourcePrecedence[source] <= parentSourcePrecedence[row.parentSource] {
		return
	}

	p := parent
	row.parent = &p
	row.parentSource = source
}

// Result returns the recorded outcomes as a sorted, deterministic
// ItemOutcomes artifact.
func (r *Recorder) Result() *ItemOutcomes {
	result := &ItemOutcomes{
		SchemaVersion: SchemaVersion,
		Items:         []ItemOutcome{},
	}

	if r == nil {
		return result
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	for id, row := range r.rows {
		item := ItemOutcome{
			GroupResource: id.GroupResource.String(),
			Namespace:     id.Namespace,
			Name:          id.Name,
			Outcome:       row.outcome,
			Messages:      capMessages(row.messages),
			Parent:        newParentRef(row.parent),
		}
		if item.Parent != nil {
			source := row.parentSource
			item.ParentSource = &source
		}
		result.Items = append(result.Items, item)
	}

	sort.Slice(result.Items, func(i, j int) bool {
		a, b := result.Items[i], result.Items[j]
		if a.GroupResource != b.GroupResource {
			return a.GroupResource < b.GroupResource
		}
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Name < b.Name
	})

	return result
}
