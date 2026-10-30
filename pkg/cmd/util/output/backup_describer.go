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
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	corev1api "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cockroachdb/errors"
	snapshotv1api "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"

	"github.com/fatih/color"
	kbclient "sigs.k8s.io/controller-runtime/pkg/client"

	veleroapishared "github.com/vmware-tanzu/velero/pkg/apis/velero/shared"
	velerov1api "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	"github.com/vmware-tanzu/velero/pkg/cmd/util/cacert"
	"github.com/vmware-tanzu/velero/pkg/cmd/util/downloadrequest"
	"github.com/vmware-tanzu/velero/pkg/itemoperation"

	"github.com/vmware-tanzu/velero/internal/itemoutcome"
	"github.com/vmware-tanzu/velero/internal/resourcepolicies"
	"github.com/vmware-tanzu/velero/internal/volume"
	"github.com/vmware-tanzu/velero/pkg/kuberesource"
	"github.com/vmware-tanzu/velero/pkg/util/collections"
	"github.com/vmware-tanzu/velero/pkg/util/results"
)

// DescribeBackup describes a backup in human-readable format.
func DescribeBackup(
	ctx context.Context,
	kbClient kbclient.Client,
	backup *velerov1api.Backup,
	deleteRequests []velerov1api.DeleteBackupRequest,
	podVolumeBackups []velerov1api.PodVolumeBackup,
	details bool,
	insecureSkipTLSVerify bool,
	caCertFile string,
) string {
	return Describe(func(d *Describer) {
		d.DescribeMetadata(backup.ObjectMeta)

		d.Println()
		phase := backup.Status.Phase
		if phase == "" {
			phase = velerov1api.BackupPhaseNew
		}
		phaseString := string(phase)
		switch phase {
		case velerov1api.BackupPhaseFailedValidation, velerov1api.BackupPhasePartiallyFailed, velerov1api.BackupPhaseFailed:
			phaseString = color.RedString(phaseString)
		case velerov1api.BackupPhaseCompleted:
			phaseString = color.GreenString(phaseString)
		case velerov1api.BackupPhaseDeleting:
		case velerov1api.BackupPhaseWaitingForPluginOperations, velerov1api.BackupPhaseWaitingForPluginOperationsPartiallyFailed:
		case velerov1api.BackupPhaseFinalizing, velerov1api.BackupPhaseFinalizingPartiallyFailed:
		case velerov1api.BackupPhaseInProgress:
		case velerov1api.BackupPhaseNew:
		case velerov1api.BackupPhaseQueued, velerov1api.BackupPhaseReadyToStart:
		}

		logsNote := ""
		if backup.Status.Phase == velerov1api.BackupPhaseFailed || backup.Status.Phase == velerov1api.BackupPhasePartiallyFailed {
			logsNote = fmt.Sprintf(" (run `velero backup logs %s` for more information)", backup.Name)
		}

		d.Printf("Phase:\t%s%s\n", phaseString, logsNote)
		if phase == velerov1api.BackupPhaseQueued {
			d.Printf("Queue position:\t%v\n", backup.Status.QueuePosition)
		}

		if backup.Spec.ResourcePolicy != nil {
			d.Println()
			DescribeResourcePolicies(d, backup.Spec.ResourcePolicy)
		}

		DescribeGlobalVolumePolicy(d, backup)

		if backup.Spec.UploaderConfig != nil && backup.Spec.UploaderConfig.ParallelFilesUpload > 0 {
			d.Println()
			DescribeUploaderConfigForBackup(d, backup.Spec)
		}

		status := backup.Status
		if len(status.ValidationErrors) > 0 {
			d.Println()
			d.Printf("Validation errors:")
			for _, ve := range status.ValidationErrors {
				d.Printf("\t%s\n", color.RedString(ve))
			}
		}

		d.Println()
		DescribeBackupResults(ctx, kbClient, d, backup, insecureSkipTLSVerify, caCertFile)

		d.Println()
		DescribeBackupSpec(d, backup.Spec)

		d.Println()
		DescribeBackupStatus(ctx, kbClient, d, backup, details, insecureSkipTLSVerify, caCertFile, podVolumeBackups)

		if len(deleteRequests) > 0 {
			d.Println()
			DescribeDeleteBackupRequests(d, deleteRequests)
		}
	})
}

// DescribeResourcePolicies describes resource policies in human-readable format
func DescribeResourcePolicies(d *Describer, resPolicies *corev1api.TypedLocalObjectReference) {
	d.Printf("Resource policies:\n")
	d.Printf("\tType:\t%s\n", resPolicies.Kind)
	d.Printf("\tName:\t%s\n", resPolicies.Name)
}

// DescribeGlobalVolumePolicy describes the cluster-wide global backup volume policies
// ConfigMap that contributed to the backup, if any.
func DescribeGlobalVolumePolicy(d *Describer, backup *velerov1api.Backup) {
	name := backup.Annotations[velerov1api.GlobalBackupVolumePolicyConfigMapAnnotation]
	if name == "" {
		return
	}
	d.Println()
	d.Printf("Global volume policies:\n")
	d.Printf("\tType:\t%s\n", resourcepolicies.ConfigmapRefType)
	d.Printf("\tName:\t%s\n", name)
}

// DescribeUploaderConfigForBackup describes uploader config in human-readable format
func DescribeUploaderConfigForBackup(d *Describer, spec velerov1api.BackupSpec) {
	d.Printf("Uploader config:\n")
	d.Printf("\tParallel files upload:\t%d\n", spec.UploaderConfig.ParallelFilesUpload)
}

// DescribeBackupSpec describes a backup spec in human-readable format.
func DescribeBackupSpec(d *Describer, spec velerov1api.BackupSpec) {
	d.Printf("Namespaces:\n")
	d.Printf("\tIncluded:\t%s\n", JoinStringWithFallback(spec.IncludedNamespaces, "*"))
	d.Printf("\tExcluded:\t%s\n", JoinStringWithFallback(spec.ExcludedNamespaces, emptyDisplay))

	d.Println()
	d.Printf("Resources:\n")
	if collections.UseOldResourceFilters(spec) {
		d.Printf("\tIncluded:\t%s\n", JoinStringWithFallback(spec.IncludedResources, "*"))
		d.Printf("\tExcluded:\t%s\n", JoinStringWithFallback(spec.ExcludedResources, emptyDisplay))
		d.Printf("\tCluster-scoped:\t%s\n", BoolPointerString(spec.IncludeClusterResources, "excluded", "included", "auto"))
	} else {
		d.Printf("\tIncluded cluster-scoped:\t%s\n", JoinStringWithFallback(spec.IncludedClusterScopedResources, emptyDisplay))
		d.Printf("\tExcluded cluster-scoped:\t%s\n", JoinStringWithFallback(spec.ExcludedClusterScopedResources, emptyDisplay))

		d.Printf("\tIncluded namespace-scoped:\t%s\n", JoinStringWithFallback(spec.IncludedNamespaceScopedResources, "*"))
		d.Printf("\tExcluded namespace-scoped:\t%s\n", JoinStringWithFallback(spec.ExcludedNamespaceScopedResources, emptyDisplay))
	}

	d.Println()
	s := emptyDisplay
	if spec.LabelSelector != nil {
		s = metav1.FormatLabelSelector(spec.LabelSelector)
	}
	d.Printf("Label selector:\t%s\n", s)

	d.Println()
	if len(spec.OrLabelSelectors) == 0 {
		s = emptyDisplay
	} else {
		orLabelSelectors := []string{}
		for _, v := range spec.OrLabelSelectors {
			orLabelSelectors = append(orLabelSelectors, metav1.FormatLabelSelector(v))
		}
		s = strings.Join(orLabelSelectors, " or ")
	}
	d.Printf("Or label selector:\t%s\n", s)

	d.Println()
	d.Printf("Storage Location:\t%s\n", spec.StorageLocation)

	d.Println()
	d.Printf("Velero-Native Snapshot PVs:\t%s\n", BoolPointerString(spec.SnapshotVolumes, "false", "true", "auto"))
	if spec.DefaultVolumesToFsBackup != nil {
		d.Printf("File System Backup (Default):\t%s\n", BoolPointerString(spec.DefaultVolumesToFsBackup, "false", "true", ""))
	}
	d.Printf("Snapshot Move Data:\t%s\n", BoolPointerString(spec.SnapshotMoveData, "false", "true", "auto"))
	if len(spec.DataMover) == 0 {
		s = defaultDataMover
	} else {
		s = spec.DataMover
	}
	d.Printf("Data Mover:\t%s\n", s)

	if string(spec.BackupType) != "" {
		d.Printf("Backup Type:\t%s\n", spec.BackupType)
	}

	d.Println()
	d.Printf("TTL:\t%s\n", spec.TTL.Duration)

	d.Println()
	d.Printf("CSISnapshotTimeout:\t%s\n", spec.CSISnapshotTimeout.Duration)
	d.Printf("ItemOperationTimeout:\t%s\n", spec.ItemOperationTimeout.Duration)

	d.Println()
	if len(spec.Hooks.Resources) == 0 {
		d.Printf("Hooks:\t" + emptyDisplay + "\n")
	} else {
		d.Printf("Hooks:\n")
		d.Printf("\tResources:\n")
		for _, backupResourceHookSpec := range spec.Hooks.Resources {
			d.Printf("\t\t%s:\n", backupResourceHookSpec.Name)
			d.Printf("\t\t\tNamespaces:\n")
			d.Printf("\t\t\t\tIncluded:\t%s\n", JoinStringWithFallback(backupResourceHookSpec.IncludedNamespaces, "*"))
			d.Printf("\t\t\t\tExcluded:\t%s\n", JoinStringWithFallback(backupResourceHookSpec.ExcludedNamespaces, emptyDisplay))

			d.Println()
			d.Printf("\t\t\tResources:\n")
			d.Printf("\t\t\t\tIncluded:\t%s\n", JoinStringWithFallback(backupResourceHookSpec.IncludedResources, "*"))
			d.Printf("\t\t\t\tExcluded:\t%s\n", JoinStringWithFallback(backupResourceHookSpec.ExcludedResources, emptyDisplay))

			d.Println()
			s = emptyDisplay
			if backupResourceHookSpec.LabelSelector != nil {
				s = metav1.FormatLabelSelector(backupResourceHookSpec.LabelSelector)
			}
			d.Printf("\t\t\tLabel selector:\t%s\n", s)

			for _, hook := range backupResourceHookSpec.PreHooks {
				if hook.Exec != nil {
					d.Println()
					d.Printf("\t\t\tPre Exec Hook:\n")
					d.Printf("\t\t\t\tContainer:\t%s\n", hook.Exec.Container)
					d.Printf("\t\t\t\tCommand:\t%s\n", strings.Join(hook.Exec.Command, " "))
					d.Printf("\t\t\t\tOn Error:\t%s\n", hook.Exec.OnError)
					d.Printf("\t\t\t\tTimeout:\t%s\n", hook.Exec.Timeout.Duration)
				}
			}

			for _, hook := range backupResourceHookSpec.PostHooks {
				if hook.Exec != nil {
					d.Println()
					d.Printf("\t\t\tPost Exec Hook:\n")
					d.Printf("\t\t\t\tContainer:\t%s\n", hook.Exec.Container)
					d.Printf("\t\t\t\tCommand:\t%s\n", strings.Join(hook.Exec.Command, " "))
					d.Printf("\t\t\t\tOn Error:\t%s\n", hook.Exec.OnError)
					d.Printf("\t\t\t\tTimeout:\t%s\n", hook.Exec.Timeout.Duration)
				}
			}
		}
	}

	if spec.OrderedResources != nil {
		d.Println()
		d.Printf("OrderedResources:\n")
		for key, value := range spec.OrderedResources {
			d.Printf("\t%s: %s\n", key, value)
		}
	}
}

// DescribeBackupStatus describes a backup status in human-readable format.
func DescribeBackupStatus(ctx context.Context,
	kbClient kbclient.Client,
	d *Describer,
	backup *velerov1api.Backup,
	details bool,
	insecureSkipTLSVerify bool,
	caCertPath string,
	podVolumeBackups []velerov1api.PodVolumeBackup) {
	status := backup.Status

	// Status.Version has been deprecated, use Status.FormatVersion
	d.Printf("Backup Format Version:\t%s\n", status.FormatVersion)

	d.Println()
	// "<n/a>" output should only be applicable for backups that failed validation
	if status.StartTimestamp == nil || status.StartTimestamp.Time.IsZero() {
		d.Printf("Started:\t%s\n", "<n/a>")
	} else {
		d.Printf("Started:\t%s\n", status.StartTimestamp.Time)
	}
	if status.CompletionTimestamp == nil || status.CompletionTimestamp.Time.IsZero() {
		d.Printf("Completed:\t%s\n", "<n/a>")
	} else {
		d.Printf("Completed:\t%s\n", status.CompletionTimestamp.Time)
	}

	d.Println()
	// Expiration can't be 0, it is always set to a 30-day default. It can be nil
	// if the controller hasn't processed this Backup yet, in which case this will
	// just display `<nil>`, though this should be temporary.
	d.Printf("Expiration:\t%s\n", status.Expiration)
	d.Println()

	if backup.Status.Progress != nil {
		if backup.Status.Phase == velerov1api.BackupPhaseInProgress {
			d.Printf("Estimated total items to be backed up:\t%d\n", backup.Status.Progress.TotalItems)
			d.Printf("Items backed up so far:\t%d\n", backup.Status.Progress.ItemsBackedUp)
		} else {
			d.Printf("Total items to be backed up:\t%d\n", backup.Status.Progress.TotalItems)
			d.Printf("Items backed up:\t%d\n", backup.Status.Progress.ItemsBackedUp)
		}

		d.Println()
	}

	describeBackupItemOperations(ctx, kbClient, d, backup, details, insecureSkipTLSVerify, caCertPath)

	if details {
		describeBackupResourceList(ctx, kbClient, d, backup, insecureSkipTLSVerify, caCertPath)
		d.Println()
	}

	describeBackupVolumes(ctx, kbClient, d, backup, details, insecureSkipTLSVerify, caCertPath, podVolumeBackups)

	if details {
		d.Println()
		describeBackupItemOutcomes(ctx, kbClient, d, backup, insecureSkipTLSVerify, caCertPath)
	}

	if status.HookStatus != nil {
		d.Println()
		d.Printf("HooksAttempted:\t%d\n", status.HookStatus.HooksAttempted)
		d.Printf("HooksFailed:\t%d\n", status.HookStatus.HooksFailed)
	}
}

func describeBackupItemOperations(ctx context.Context, kbClient kbclient.Client, d *Describer, backup *velerov1api.Backup, details bool, insecureSkipTLSVerify bool, caCertPath string) {
	status := backup.Status
	if status.BackupItemOperationsAttempted > 0 {
		if !details {
			d.Printf("Backup Item Operations:\t%d of %d completed successfully, %d failed (specify --details for more information)\n", status.BackupItemOperationsCompleted, status.BackupItemOperationsAttempted, status.BackupItemOperationsFailed)
			return
		}

		// Get BSL cacert if available
		bslCACert, err := cacert.GetCACertFromBackup(ctx, kbClient, backup.Namespace, backup)
		if err != nil {
			// Log the error but don't fail - we can still try to download without the BSL cacert
			d.Printf("WARNING: Error getting cacert from BSL: %v\n", err)
			bslCACert = ""
		}

		buf := new(bytes.Buffer)
		if err := downloadrequest.StreamWithBSLCACert(ctx, kbClient, backup.Namespace, backup.Name, velerov1api.DownloadTargetKindBackupItemOperations, buf, downloadRequestTimeout, insecureSkipTLSVerify, caCertPath, bslCACert); err != nil {
			d.Printf("Backup Item Operations:\t<error getting operation info: %v>\n", err)
			return
		}

		var operations []*itemoperation.BackupOperation
		if err := json.NewDecoder(buf).Decode(&operations); err != nil {
			d.Printf("Backup Item Operations:\t<error reading operation info: %v>\n", err)
			return
		}

		d.Printf("Backup Item Operations:\n")
		for _, operation := range operations {
			describeBackupItemOperation(d, operation)
		}
	}
}

// describeBackupItemOutcomes prints a compact summary of the per-item backup
// outcomes artifact: counts by outcome, plus the failed/warning rows, with
// sub-items indented under their Parent.
func describeBackupItemOutcomes(ctx context.Context, kbClient kbclient.Client, d *Describer, backup *velerov1api.Backup, insecureSkipTLSVerify bool, caCertPath string) {
	// Get BSL cacert if available
	bslCACert, err := cacert.GetCACertFromBackup(ctx, kbClient, backup.Namespace, backup)
	if err != nil {
		// Log the error but don't fail - we can still try to download without the BSL cacert
		d.Printf("WARNING: Error getting cacert from BSL: %v\n", err)
		bslCACert = ""
	}

	buf := new(bytes.Buffer)
	if err := downloadrequest.StreamWithBSLCACert(ctx, kbClient, backup.Namespace, backup.Name, velerov1api.DownloadTargetKindBackupItemOutcomes, buf, downloadRequestTimeout, insecureSkipTLSVerify, caCertPath, bslCACert); err != nil {
		if err == downloadrequest.ErrNotFound {
			d.Println("Item Outcomes:\t<not collected for this backup>")
		} else {
			d.Printf("Item Outcomes:\t<error getting item outcomes: %v>\n", err)
		}
		return
	}

	var outcomes itemoutcome.ItemOutcomes
	if err := json.NewDecoder(buf).Decode(&outcomes); err != nil {
		d.Printf("Item Outcomes:\t<error reading item outcomes: %v>\n", err)
		return
	}

	// Item outcomes answer "what was touched", but carry no volume verdict: a
	// PVC row can read BackedUp while its snapshot failed. The per-volume
	// result lives in the separate volumeinfo artifact, joined here on
	// (namespace, PVC name), falling back to PV name. Best-effort: a failure to
	// read volume info degrades the report rather than dropping it.
	volumeByItem := backupVolumeResultsByItem(ctx, kbClient, backup, insecureSkipTLSVerify, caCertPath, bslCACert)

	describeItemOutcomeListing(d, &outcomes, volumeByItem)
}

// describeItemOutcomeListing writes the outcome counts and the rows worth
// reading. It takes no context or client so the rendering can be exercised
// directly, with the artifacts already decoded.
func describeItemOutcomeListing(d *Describer, outcomes *itemoutcome.ItemOutcomes, volumeByItem *volumeResultsIndex) {
	if len(outcomes.Items) == 0 {
		d.Println("Item Outcomes:\t<none>")
		return
	}

	counts := map[itemoutcome.Outcome]int{}
	for _, item := range outcomes.Items {
		counts[item.Outcome]++
	}
	d.Printf("Item Outcomes:\t%d BackedUp, %d Warning, %d Failed, %d Skipped\n",
		counts[itemoutcome.OutcomeBackedUp], counts[itemoutcome.OutcomeWarning], counts[itemoutcome.OutcomeFailed], counts[itemoutcome.OutcomeSkipped])
	// Named is reported rather than folded into BackedUp, for the same reason
	// Skipped is: neither is a success, and a reader who sums the four older
	// numbers would otherwise believe every row had a verdict. Named is the
	// weaker of the two — Skipped is a decision, Named is the absence of one.
	if n := counts[itemoutcome.OutcomeNamed]; n > 0 {
		d.Printf("  %d item(s) were named by another item's backup and never resolved\n", n)
	}

	byID := itemoutcome.IndexByKey(outcomes)
	parentOf := itemoutcome.ParentMap(byID)
	childrenOf := itemoutcome.ChildrenMap(parentOf)

	// The counts above come from item outcomes alone, so a backup whose only
	// problem is a failed snapshot would print "0 Failed" directly above the row
	// reporting that failure. Call the discrepancy out rather than letting the
	// two statements contradict each other.
	if n := countFailedVolumes(volumeByItem, byID); n > 0 && counts[itemoutcome.OutcomeFailed] == 0 {
		d.Printf("  %d volume(s) failed; see below\n", n)
	}

	// Report at the root of each chain rather than at the row that failed. The
	// parenting chain for a VM is VM -> VMI -> virt-launcher pod -> PVC, so a
	// failed disk hangs several levels below the object a user names. Anchoring
	// on the root is what turns a failure recorded against
	// "virt-launcher-cirros-abc" into a failure reported under the VM.
	anchors := map[itemoutcome.Key][]itemoutcome.ItemOutcome{}
	chains := map[itemoutcome.Key][]itemoutcome.ItemOutcome{}
	var anchorOrder []itemoutcome.Key

	for _, item := range outcomes.Items {
		if !isNoteworthyOutcome(item, volumeByItem, byID) {
			continue
		}

		root := itemoutcome.ResolveRoot(parentOf, itemoutcome.KeyOf(item))
		if _, seen := anchors[root]; !seen {
			anchorOrder = append(anchorOrder, root)
			for _, key := range itemoutcome.Descendants(childrenOf, root) {
				chains[root] = append(chains[root], byID[key])
			}
		}
		anchors[root] = append(anchors[root], item)
	}

	sort.Slice(anchorOrder, func(i, j int) bool {
		if anchorOrder[i].Namespace != anchorOrder[j].Namespace {
			return anchorOrder[i].Namespace < anchorOrder[j].Namespace
		}
		if anchorOrder[i].Name != anchorOrder[j].Name {
			return anchorOrder[i].Name < anchorOrder[j].Name
		}
		return anchorOrder[i].GroupResource < anchorOrder[j].GroupResource
	})

	// Rows are indented with spaces, not tabs. The counts line above uses a tab
	// as a column separator, and the describer's tabwriter would then pad every
	// tab-indented row out to that column's width.
	for _, root := range anchorOrder {
		rows := anchors[root]
		rootItem := byID[root]

		// A single chain is reported as a plain row: a header followed by the one
		// row it names would only repeat itself.
		if len(rows) == 1 && itemoutcome.KeyOf(rows[0]) == root {
			printItemOutcomeRow(d, volumeByItem, rows[0], "  ", byID)
			continue
		}

		// Summarise the whole chain, not just its noteworthy rows: "1 of 2 volumes
		// failed" needs the clean disk counted too.
		summary := summariseVolumes(volumeByItem, chains[root], byID)
		if summary != "" {
			d.Printf("  %s (%s):\n", outcomeRef(rootItem.GroupResource, rootItem.Namespace, rootItem.Name), summary)
		} else {
			d.Printf("  %s:\n", outcomeRef(rootItem.GroupResource, rootItem.Namespace, rootItem.Name))
		}

		sort.SliceStable(rows, func(i, j int) bool {
			return outcomeRef(rows[i].GroupResource, rows[i].Namespace, rows[i].Name) <
				outcomeRef(rows[j].GroupResource, rows[j].Namespace, rows[j].Name)
		})
		for _, row := range rows {
			printItemOutcomeRow(d, volumeByItem, row, "    ", byID)
		}
	}
}

func printItemOutcomeRow(d *Describer, index *volumeResultsIndex, item itemoutcome.ItemOutcome, indent string, byID map[itemoutcome.Key]itemoutcome.ItemOutcome) {
	d.Printf("%s%s: %s\n", indent, outcomeRef(item.GroupResource, item.Namespace, item.Name), item.Outcome)
	for _, msg := range item.Messages {
		d.Printf("%s%s\n", indent+"  ", msg)
	}
	if shouldShowVolume(item, byID) {
		if volumes := volumesForItem(index, item); len(volumes) > 0 {
			d.Printf("%svolume: %s\n", indent+"  ", describeVolumeResults(volumes))
		}
	}
}

// shouldShowVolume suppresses the volume line on a PV row whose PVC is also in
// the artifact: both rows resolve to the same volume info, and the PVC is the
// row a reader looks at. The PV row keeps its volume line when its PVC was not
// part of the backup.
func shouldShowVolume(item itemoutcome.ItemOutcome, byID map[itemoutcome.Key]itemoutcome.ItemOutcome) bool {
	if item.GroupResource != kuberesource.PersistentVolumes.String() || item.Parent == nil {
		return true
	}
	_, pvcPresent := byID[itemoutcome.KeyOfRef(item.Parent)]
	return !pvcPresent
}

func outcomeRef(groupResource, namespace, name string) string {
	if namespace == "" {
		return fmt.Sprintf("%s %s", groupResource, name)
	}
	return fmt.Sprintf("%s %s/%s", groupResource, namespace, name)
}

// countFailedVolumes returns how many distinct volumes the backup recorded as
// failed. Both a volume's PVC row and its PV row resolve to the same volume, so
// each volume is counted once.
func countFailedVolumes(index *volumeResultsIndex, byID map[itemoutcome.Key]itemoutcome.ItemOutcome) int {
	seen := map[string]bool{}
	count := 0
	for _, item := range byID {
		if !shouldShowVolume(item, byID) {
			continue
		}
		for _, info := range volumesForItem(index, item) {
			if info.Result != volume.VolumeResultFailed {
				continue
			}
			key := info.PVCNamespace + "/" + info.PVCName + "/" + info.PVName
			if seen[key] {
				continue
			}
			seen[key] = true
			count++
		}
	}
	return count
}

// summariseVolumes condenses the volumes touched anywhere in one reported chain
// into a one-line verdict, e.g. "1 of 3 volumes failed", or "" when the chain
// owns no volume.
func summariseVolumes(index *volumeResultsIndex, rows []itemoutcome.ItemOutcome, byID map[itemoutcome.Key]itemoutcome.ItemOutcome) string {
	// A volume's PVC row and its PV row both resolve to the same volume info, so
	// count each volume once.
	seen := map[string]bool{}
	var total, failed, skipped int
	for _, row := range rows {
		if !shouldShowVolume(row, byID) {
			continue
		}
		for _, info := range volumesForItem(index, row) {
			key := info.PVCNamespace + "/" + info.PVCName + "/" + info.PVName
			if seen[key] {
				continue
			}
			seen[key] = true

			total++
			switch {
			case info.Result == volume.VolumeResultFailed:
				failed++
			case info.Skipped:
				skipped++
			}
		}
	}

	if total == 0 {
		return ""
	}

	parts := []string{fmt.Sprintf("%d of %d volumes failed", failed, total)}
	if skipped > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped", skipped))
	}
	return strings.Join(parts, ", ")
}

// volumeItemKey identifies the k8s object a volume belongs to, so item
// outcomes and volume info can be joined without either artifact knowing about
// the other.
type volumeItemKey struct {
	namespace string
	name      string
}

func itemKey(namespace, name string) volumeItemKey {
	return volumeItemKey{namespace: namespace, name: name}
}

// volumeResultsIndex maps a PVC or PV to the volume info recorded for it. A
// PVC is the natural join key (both artifacts name the volume by its PVC), and
// the PV name is indexed too so a PersistentVolume row resolves as well.
type volumeResultsIndex struct {
	byPVC map[volumeItemKey]volume.BackupVolumeInfo
	byPV  map[string]volume.BackupVolumeInfo
}

// backupVolumeResultsByItem reads the backup's volume info artifact and indexes
// it for joining against item outcomes. Best-effort by design: item outcomes are
// still worth printing without volume detail, so every failure here returns an
// empty index rather than an error.
func backupVolumeResultsByItem(ctx context.Context, kbClient kbclient.Client, backup *velerov1api.Backup, insecureSkipTLSVerify bool, caCertPath string, bslCACert string) *volumeResultsIndex {
	index := &volumeResultsIndex{
		byPVC: map[volumeItemKey]volume.BackupVolumeInfo{},
		byPV:  map[string]volume.BackupVolumeInfo{},
	}

	buf := new(bytes.Buffer)
	if err := downloadrequest.StreamWithBSLCACert(ctx, kbClient, backup.Namespace, backup.Name, velerov1api.DownloadTargetKindBackupVolumeInfos, buf, downloadRequestTimeout, insecureSkipTLSVerify, caCertPath, bslCACert); err != nil {
		// A pre-1.11 backup, a backup with no volumes, or a missing artifact all
		// land here. None of them should stop item outcomes from being printed.
		return index
	}

	var volumeInfos []volume.BackupVolumeInfo
	if err := json.NewDecoder(buf).Decode(&volumeInfos); err != nil {
		return index
	}

	for _, info := range volumeInfos {
		if info.PVCName != "" {
			index.byPVC[itemKey(info.PVCNamespace, info.PVCName)] = info
		}
		if info.PVName != "" {
			index.byPV[info.PVName] = info
		}
	}

	return index
}

// volumesForItem returns the volume info belonging to one item outcome row. Only
// a PVC or PV row resolves: those are the objects volume info is keyed by. Other
// resources (a VM, a pod) own their volumes indirectly, through the rows
// parented to them, and summariseVolumes rolls those up instead.
func volumesForItem(index *volumeResultsIndex, item itemoutcome.ItemOutcome) []volume.BackupVolumeInfo {
	if index == nil {
		return nil
	}

	switch item.GroupResource {
	case kuberesource.PersistentVolumeClaims.String():
		if info, ok := index.byPVC[itemKey(item.Namespace, item.Name)]; ok {
			return []volume.BackupVolumeInfo{info}
		}
	case kuberesource.PersistentVolumes.String():
		if info, ok := index.byPV[item.Name]; ok {
			return []volume.BackupVolumeInfo{info}
		}
	}

	return nil
}

// isNoteworthyOutcome decides which rows are listed below the counts. The rule
// itself lives in itemoutcome.IsFailure so the rollup command and this listing
// cannot disagree about what counts as trouble; this only adapts the describer's
// volume index to the shape that rule takes.
func isNoteworthyOutcome(item itemoutcome.ItemOutcome, index *volumeResultsIndex, byID map[itemoutcome.Key]itemoutcome.ItemOutcome) bool {
	return itemoutcome.IsFailure(item, index.asRollupVolumes(), byID)
}

// asRollupVolumes projects the describer's volume index onto the minimal shape
// itemoutcome.IsFailure takes. The descriptor keeps the full volume info because
// it prints snapshot IDs and backup methods; the rule needs only the verdict.
func (index *volumeResultsIndex) asRollupVolumes() *itemoutcome.VolumeIndex {
	if index == nil {
		return itemoutcome.IndexVolumes(nil)
	}

	refs := make([]itemoutcome.VolumeRef, 0, len(index.byPVC)+len(index.byPV))
	seen := map[string]bool{}
	add := func(info volume.BackupVolumeInfo) {
		key := info.PVCNamespace + "/" + info.PVCName + "/" + info.PVName
		if seen[key] {
			return
		}
		seen[key] = true
		refs = append(refs, itemoutcome.VolumeRef{
			PVCNamespace: info.PVCNamespace,
			PVCName:      info.PVCName,
			PVName:       info.PVName,
			Failed:       info.Result == volume.VolumeResultFailed,
		})
	}
	for _, info := range index.byPVC {
		add(info)
	}
	for _, info := range index.byPV {
		add(info)
	}

	return itemoutcome.IndexVolumes(refs)
}

// describeVolumeResults renders the volume verdicts for one item row.
func describeVolumeResults(infos []volume.BackupVolumeInfo) string {
	parts := make([]string, 0, len(infos))
	for _, info := range infos {
		parts = append(parts, describeVolumeResult(info))
	}
	return strings.Join(parts, "; ")
}

func describeVolumeResult(info volume.BackupVolumeInfo) string {
	ref := info.PVCName
	if ref == "" {
		ref = info.PVName
	}
	if info.PVName != "" && info.PVCName != "" {
		ref = fmt.Sprintf("%s (pv=%s)", info.PVCName, info.PVName)
	}

	method := string(info.BackupMethod)
	if method == "" {
		method = "unknown method"
	}

	verdict := string(info.Result)
	switch {
	case info.Skipped:
		verdict = "skipped"
		if info.SkippedReason != "" {
			verdict = fmt.Sprintf("skipped: %s", info.SkippedReason)
		}
	case verdict == "":
		verdict = "unknown result"
	}

	if handle := snapshotHandle(info); handle != "" {
		return fmt.Sprintf("%s %s %s (%s)", ref, method, verdict, handle)
	}
	return fmt.Sprintf("%s %s %s", ref, method, verdict)
}

// snapshotHandle returns the provider-specific snapshot identifier, if the
// backup method recorded one. Which field holds it depends on the method.
func snapshotHandle(info volume.BackupVolumeInfo) string {
	switch {
	case info.CSISnapshotInfo != nil && info.CSISnapshotInfo.SnapshotHandle != "":
		return info.CSISnapshotInfo.SnapshotHandle
	case info.NativeSnapshotInfo != nil && info.NativeSnapshotInfo.SnapshotHandle != "":
		return info.NativeSnapshotInfo.SnapshotHandle
	case info.PVBInfo != nil && info.PVBInfo.SnapshotHandle != "":
		return info.PVBInfo.SnapshotHandle
	case info.SnapshotDataMovementInfo != nil && info.SnapshotDataMovementInfo.SnapshotHandle != "":
		return info.SnapshotDataMovementInfo.SnapshotHandle
	}
	return ""
}

func describeBackupResourceList(ctx context.Context, kbClient kbclient.Client, d *Describer, backup *velerov1api.Backup, insecureSkipTLSVerify bool, caCertPath string) {
	// Get BSL cacert if available
	bslCACert, err := cacert.GetCACertFromBackup(ctx, kbClient, backup.Namespace, backup)
	if err != nil {
		// Log the error but don't fail - we can still try to download without the BSL cacert
		d.Printf("WARNING: Error getting cacert from BSL: %v\n", err)
		bslCACert = ""
	}

	buf := new(bytes.Buffer)
	if err := downloadrequest.StreamWithBSLCACert(ctx, kbClient, backup.Namespace, backup.Name, velerov1api.DownloadTargetKindBackupResourceList, buf, downloadRequestTimeout, insecureSkipTLSVerify, caCertPath, bslCACert); err != nil {
		if err == downloadrequest.ErrNotFound {
			// the backup resource list could be missing if (other reasons may exist as well):
			//	- the backup was taken prior to v1.1; or
			//	- the backup hasn't completed yet; or
			//	- there was an error uploading the file; or
			//	- the file was manually deleted after upload
			d.Println("Resource List:\t<backup resource list not found>")
		} else {
			d.Printf("Resource List:\t<error getting backup resource list: %v>\n", err)
		}
		return
	}

	var resourceList map[string][]string
	if err := json.NewDecoder(buf).Decode(&resourceList); err != nil {
		d.Printf("Resource List:\t<error reading backup resource list: %v>\n", err)
		return
	}

	d.Println("Resource List:")

	// Sort GVKs in output
	gvks := make([]string, 0, len(resourceList))
	for gvk := range resourceList {
		gvks = append(gvks, gvk)
	}
	sort.Strings(gvks)

	for _, gvk := range gvks {
		d.Printf("\t%s:\n\t\t- %s\n", gvk, strings.Join(resourceList[gvk], "\n\t\t- "))
	}
}

func describeBackupVolumes(
	ctx context.Context,
	kbClient kbclient.Client,
	d *Describer,
	backup *velerov1api.Backup,
	details bool,
	insecureSkipTLSVerify bool,
	caCertPath string,
	podVolumeBackupCRs []velerov1api.PodVolumeBackup,
) {
	d.Println("Backup Volumes:")

	// Get BSL cacert if available
	bslCACert, err := cacert.GetCACertFromBackup(ctx, kbClient, backup.Namespace, backup)
	if err != nil {
		// Log the error but don't fail - we can still try to download without the BSL cacert
		d.Printf("WARNING: Error getting cacert from BSL: %v\n", err)
		bslCACert = ""
	}

	nativeSnapshots := []*volume.BackupVolumeInfo{}
	csiSnapshots := []*volume.BackupVolumeInfo{}
	legacyInfoSource := false

	buf := new(bytes.Buffer)
	err = downloadrequest.StreamWithBSLCACert(ctx, kbClient, backup.Namespace, backup.Name, velerov1api.DownloadTargetKindBackupVolumeInfos, buf, downloadRequestTimeout, insecureSkipTLSVerify, caCertPath, bslCACert)
	if err == downloadrequest.ErrNotFound {
		nativeSnapshots, err = retrieveNativeSnapshotLegacy(ctx, kbClient, backup, insecureSkipTLSVerify, caCertPath, bslCACert)
		if err != nil {
			d.Printf("\t<error concluding native snapshot info: %v>\n", err)
			return
		}

		csiSnapshots, err = retrieveCSISnapshotLegacy(ctx, kbClient, backup, insecureSkipTLSVerify, caCertPath, bslCACert)
		if err != nil {
			d.Printf("\t<error concluding CSI snapshot info: %v>\n", err)
			return
		}

		legacyInfoSource = true
	} else if err != nil {
		d.Printf("\t<error getting backup volume info: %v>\n", err)
		return
	} else {
		var volumeInfos []volume.BackupVolumeInfo
		if err := json.NewDecoder(buf).Decode(&volumeInfos); err != nil {
			d.Printf("\t<error reading backup volume info: %v>\n", err)
			return
		}

		for i := range volumeInfos {
			switch volumeInfos[i].BackupMethod {
			case volume.NativeSnapshot:
				nativeSnapshots = append(nativeSnapshots, &volumeInfos[i])
			case volume.CSISnapshot:
				csiSnapshots = append(csiSnapshots, &volumeInfos[i])
			}
		}
	}

	describeNativeSnapshots(d, details, nativeSnapshots)
	d.Println()

	describeCSISnapshots(d, details, csiSnapshots, legacyInfoSource)
	d.Println()

	describePodVolumeBackups(d, details, podVolumeBackupCRs)
}

func retrieveNativeSnapshotLegacy(ctx context.Context, kbClient kbclient.Client, backup *velerov1api.Backup, insecureSkipTLSVerify bool, caCertPath string, bslCACert string) ([]*volume.BackupVolumeInfo, error) {
	status := backup.Status
	nativeSnapshots := []*volume.BackupVolumeInfo{}

	if status.VolumeSnapshotsAttempted == 0 {
		return nativeSnapshots, nil
	}

	buf := new(bytes.Buffer)
	if err := downloadrequest.StreamWithBSLCACert(ctx, kbClient, backup.Namespace, backup.Name, velerov1api.DownloadTargetKindBackupVolumeSnapshots, buf, downloadRequestTimeout, insecureSkipTLSVerify, caCertPath, bslCACert); err != nil {
		return nativeSnapshots, errors.Wrapf(err, "error to download native snapshot info")
	}

	var snapshots []*volume.Snapshot
	if err := json.NewDecoder(buf).Decode(&snapshots); err != nil {
		return nativeSnapshots, errors.Wrapf(err, "error to decode native snapshot info")
	}

	for _, snap := range snapshots {
		volumeInfo := volume.BackupVolumeInfo{
			PVName: snap.Spec.PersistentVolumeName,
			NativeSnapshotInfo: &volume.NativeSnapshotInfo{
				SnapshotHandle: snap.Status.ProviderSnapshotID,
				VolumeType:     snap.Spec.VolumeType,
				VolumeAZ:       snap.Spec.VolumeAZ,
			},
		}

		if snap.Spec.VolumeIOPS != nil {
			volumeInfo.NativeSnapshotInfo.IOPS = strconv.FormatInt(*snap.Spec.VolumeIOPS, 10)
		}

		nativeSnapshots = append(nativeSnapshots, &volumeInfo)
	}

	return nativeSnapshots, nil
}

func retrieveCSISnapshotLegacy(ctx context.Context, kbClient kbclient.Client, backup *velerov1api.Backup, insecureSkipTLSVerify bool, caCertPath string, bslCACert string) ([]*volume.BackupVolumeInfo, error) {
	status := backup.Status
	csiSnapshots := []*volume.BackupVolumeInfo{}

	if status.CSIVolumeSnapshotsAttempted == 0 {
		return csiSnapshots, nil
	}

	vsBuf := new(bytes.Buffer)
	err := downloadrequest.StreamWithBSLCACert(ctx, kbClient, backup.Namespace, backup.Name, velerov1api.DownloadTargetKindCSIBackupVolumeSnapshots, vsBuf, downloadRequestTimeout, insecureSkipTLSVerify, caCertPath, bslCACert)
	if err != nil {
		return csiSnapshots, errors.Wrapf(err, "error to download vs list")
	}

	var vsList []snapshotv1api.VolumeSnapshot
	if err := json.NewDecoder(vsBuf).Decode(&vsList); err != nil {
		return csiSnapshots, errors.Wrapf(err, "error to decode vs list")
	}

	vscBuf := new(bytes.Buffer)
	err = downloadrequest.StreamWithBSLCACert(ctx, kbClient, backup.Namespace, backup.Name, velerov1api.DownloadTargetKindCSIBackupVolumeSnapshotContents, vscBuf, downloadRequestTimeout, insecureSkipTLSVerify, caCertPath, bslCACert)
	if err != nil {
		return csiSnapshots, errors.Wrapf(err, "error to download vsc list")
	}

	var vscList []snapshotv1api.VolumeSnapshotContent
	if err := json.NewDecoder(vscBuf).Decode(&vscList); err != nil {
		return csiSnapshots, errors.Wrapf(err, "error to decode vsc list")
	}

	for _, vsc := range vscList {
		volInfo := volume.BackupVolumeInfo{
			PreserveLocalSnapshot: true,
			CSISnapshotInfo: &volume.CSISnapshotInfo{
				VSCName: vsc.Name,
				Driver:  vsc.Spec.Driver,
			},
		}

		if vsc.Status != nil && vsc.Status.SnapshotHandle != nil {
			volInfo.CSISnapshotInfo.SnapshotHandle = *vsc.Status.SnapshotHandle
		}

		if vsc.Status != nil && vsc.Status.RestoreSize != nil {
			volInfo.CSISnapshotInfo.Size = *vsc.Status.RestoreSize
		}

		for _, vs := range vsList {
			if vs.Status.BoundVolumeSnapshotContentName == nil {
				continue
			}

			if vs.Spec.Source.PersistentVolumeClaimName == nil {
				continue
			}

			if *vs.Status.BoundVolumeSnapshotContentName == vsc.Name {
				volInfo.PVCName = *vs.Spec.Source.PersistentVolumeClaimName
				volInfo.PVCNamespace = vs.Namespace
			}
		}

		if volInfo.PVCName == "" {
			volInfo.PVCName = "<PVC name not found>"
		}

		csiSnapshots = append(csiSnapshots, &volInfo)
	}

	return csiSnapshots, nil
}

func describeNativeSnapshots(d *Describer, details bool, infos []*volume.BackupVolumeInfo) {
	if len(infos) == 0 {
		d.Printf("\tVelero-Native Snapshots: <none included>\n")
		return
	}

	d.Println("\tVelero-Native Snapshots:")
	for _, info := range infos {
		describNativeSnapshot(d, details, info)
	}
}

func describNativeSnapshot(d *Describer, details bool, info *volume.BackupVolumeInfo) {
	if details {
		d.Printf("\t\t%s:\n", info.PVName)
		d.Printf("\t\t\tSnapshot ID:\t%s\n", info.NativeSnapshotInfo.SnapshotHandle)
		d.Printf("\t\t\tType:\t%s\n", info.NativeSnapshotInfo.VolumeType)
		d.Printf("\t\t\tAvailability Zone:\t%s\n", info.NativeSnapshotInfo.VolumeAZ)
		d.Printf("\t\t\tIOPS:\t%s\n", info.NativeSnapshotInfo.IOPS)
		d.Printf("\t\t\tResult:\t%s\n", info.Result)
	} else {
		d.Printf("\t\t%s: specify --details for more information\n", info.PVName)
	}
}

func describeCSISnapshots(d *Describer, details bool, infos []*volume.BackupVolumeInfo, legacyInfoSource bool) {
	if len(infos) == 0 {
		if legacyInfoSource {
			d.Printf("\tCSI Snapshots: <none included or not detectable>\n")
		} else {
			d.Printf("\tCSI Snapshots: <none included>\n")
		}
		return
	}

	d.Println("\tCSI Snapshots:")
	for _, info := range infos {
		describeCSISnapshot(d, details, info)
	}
}

func describeCSISnapshot(d *Describer, details bool, info *volume.BackupVolumeInfo) {
	d.Printf("\t\t%s:\n", fmt.Sprintf("%s/%s", info.PVCNamespace, info.PVCName))

	describeLocalSnapshot(d, details, info)
	describeDataMovement(d, details, info)
}

func describeLocalSnapshot(d *Describer, details bool, info *volume.BackupVolumeInfo) {
	if !info.PreserveLocalSnapshot {
		return
	}

	if details {
		d.Printf("\t\t\tSnapshot:\n")
		if !info.SnapshotDataMoved && info.CSISnapshotInfo.OperationID != "" {
			d.Printf("\t\t\t\tOperation ID: %s\n", info.CSISnapshotInfo.OperationID)
		}

		d.Printf("\t\t\t\tSnapshot Content Name: %s\n", info.CSISnapshotInfo.VSCName)
		d.Printf("\t\t\t\tStorage Snapshot ID: %s\n", info.CSISnapshotInfo.SnapshotHandle)
		d.Printf("\t\t\t\tSnapshot Size (bytes): %d\n", info.CSISnapshotInfo.Size)
		d.Printf("\t\t\t\tCSI Driver: %s\n", info.CSISnapshotInfo.Driver)
		d.Printf("\t\t\t\tResult: %s\n", info.Result)
	} else {
		d.Printf("\t\t\tSnapshot: %s\n", "included, specify --details for more information")
	}
}

func describeDataMovement(d *Describer, details bool, info *volume.BackupVolumeInfo) {
	if !info.SnapshotDataMoved {
		return
	}

	if details {
		d.Printf("\t\t\tData Movement:\n")
		d.Printf("\t\t\t\tOperation ID: %s\n", info.SnapshotDataMovementInfo.OperationID)

		dataMover := "velero"
		if info.SnapshotDataMovementInfo.DataMover != "" {
			dataMover = info.SnapshotDataMovementInfo.DataMover
		}
		d.Printf("\t\t\t\tData Mover: %s\n", dataMover)

		if info.BackupType != "" {
			backupType := string(info.BackupType)
			if info.FallbackFull {
				backupType += " (fallen back to Full)"
			}

			d.Printf("\t\t\t\tBackup Type: %s\n", backupType)
		}

		d.Printf("\t\t\t\tUploader Type: %s\n", info.SnapshotDataMovementInfo.UploaderType)
		d.Printf("\t\t\t\tMoved data Size (bytes): %d\n", info.SnapshotDataMovementInfo.Size)
		// Print whenever the uploader measured a figure, including zero. A zero-delta
		// incremental transfers nothing, which is the whole point of CBT; hiding it
		// leaves only the volume size on display and makes the best possible result
		// indistinguishable from a full transfer.
		if info.SnapshotDataMovementInfo.IncrementalSize != nil {
			d.Printf("\t\t\t\tIncremental data Size (bytes): %d\n", *info.SnapshotDataMovementInfo.IncrementalSize)
		}

		d.Printf("\t\t\t\tResult: %s\n", info.Result)
	} else {
		d.Printf("\t\t\tData Movement: %s\n", "included, specify --details for more information")
	}
}

func describeBackupItemOperation(d *Describer, operation *itemoperation.BackupOperation) {
	d.Printf("\tOperation for %s %s/%s:\n", operation.Spec.ResourceIdentifier, operation.Spec.ResourceIdentifier.Namespace, operation.Spec.ResourceIdentifier.Name)
	d.Printf("\t\tBackup Item Action Plugin:\t%s\n", operation.Spec.BackupItemAction)
	d.Printf("\t\tOperation ID:\t%s\n", operation.Spec.OperationID)
	if len(operation.Spec.PostOperationItems) > 0 {
		d.Printf("\t\tItems to Update:\n")
	}
	for _, item := range operation.Spec.PostOperationItems {
		d.Printf("\t\t\t%s %s/%s\n", item, item.Namespace, item.Name)
	}
	d.Printf("\t\tPhase:\t%s\n", operation.Status.Phase)
	if operation.Status.Error != "" {
		d.Printf("\t\tOperation Error:\t%s\n", operation.Status.Error)
	}
	if operation.Status.NTotal > 0 || operation.Status.NCompleted > 0 {
		d.Printf("\t\tProgress:\t%v of %v complete (%s)\n",
			operation.Status.NCompleted,
			operation.Status.NTotal,
			operation.Status.OperationUnits)
	}
	if operation.Status.Description != "" {
		d.Printf("\t\tProgress description:\t%s\n", operation.Status.Description)
	}
	if operation.Status.Created != nil {
		d.Printf("\t\tCreated:\t%s\n", operation.Status.Created.String())
	}
	if operation.Status.Started != nil {
		d.Printf("\t\tStarted:\t%s\n", operation.Status.Started.String())
	}
	if operation.Status.Updated != nil {
		d.Printf("\t\tUpdated:\t%s\n", operation.Status.Updated.String())
	}
}

// DescribeDeleteBackupRequests describes delete backup requests in human-readable format.
func DescribeDeleteBackupRequests(d *Describer, requests []velerov1api.DeleteBackupRequest) {
	d.Printf("Deletion Attempts")
	if count := failedDeletionCount(requests); count > 0 {
		d.Printf(" (%d failed)", count)
	}
	d.Println(":")

	started := false
	for _, req := range requests {
		if !started {
			started = true
		} else {
			d.Println()
		}

		d.Printf("\t%s: %s\n", req.CreationTimestamp.String(), req.Status.Phase)
		if len(req.Status.Errors) > 0 {
			d.Printf("\tErrors:\n")
			for _, err := range req.Status.Errors {
				d.Printf("\t\t%s\n", err)
			}
		}
	}
}

func failedDeletionCount(requests []velerov1api.DeleteBackupRequest) int {
	var count int
	for _, req := range requests {
		if req.Status.Phase == velerov1api.DeleteBackupRequestPhaseProcessed && len(req.Status.Errors) > 0 {
			count++
		}
	}
	return count
}

// describePodVolumeBackups describes pod volume backups in human-readable format.
func describePodVolumeBackups(d *Describer, details bool, podVolumeBackups []velerov1api.PodVolumeBackup) {
	// Get the type of pod volume uploader. Since the uploader only comes from a single source, we can
	// take the uploader type from the first element of the array.
	var uploaderType string
	if len(podVolumeBackups) > 0 {
		uploaderType = podVolumeBackups[0].Spec.UploaderType
	} else {
		d.Printf("\tPod Volume Backups: <none included>\n")
		return
	}

	if details {
		d.Printf("\tPod Volume Backups - %s:\n", uploaderType)
	} else {
		d.Printf("\tPod Volume Backups - %s (specify --details for more information):\n", uploaderType)
	}

	// separate backups by phase (combining <none> and New into a single group)
	backupsByPhase := groupByPhase(podVolumeBackups)

	// go through phases in a specific order
	for _, phase := range []string{
		string(velerov1api.PodVolumeBackupPhaseCompleted),
		string(velerov1api.PodVolumeBackupPhaseFailed),
		string(velerov1api.PodVolumeBackupPhaseCanceled),
		"In Progress",
		string(velerov1api.PodVolumeBackupPhaseCanceling),
		string(velerov1api.PodVolumeBackupPhasePrepared),
		string(velerov1api.PodVolumeBackupPhaseAccepted),
		string(velerov1api.PodVolumeBackupPhaseNew),
	} {
		if len(backupsByPhase[phase]) == 0 {
			continue
		}

		// if we're not printing details, just report the phase and count
		if !details {
			d.Printf("\t\t%s:\t%d\n", phase, len(backupsByPhase[phase]))
			continue
		}

		// group the backups in the current phase by pod (i.e. "ns/name")
		backupsByPod := new(volumesByPod)

		for _, backup := range backupsByPhase[phase] {
			backupsByPod.Add(backup.Spec.Pod.Namespace, backup.Spec.Pod.Name, backup.Spec.Volume, phase, backup.Status.Progress, backup.Status.IncrementalBytes)
		}

		d.Printf("\t\t%s:\n", phase)
		for _, backupGroup := range backupsByPod.Sorted() {
			sort.Strings(backupGroup.volumes)

			// print volumes backed up for this pod
			d.Printf("\t\t\t%s: %s\n", backupGroup.label, strings.Join(backupGroup.volumes, ", "))
		}
	}
}

func groupByPhase(backups []velerov1api.PodVolumeBackup) map[string][]velerov1api.PodVolumeBackup {
	backupsByPhase := make(map[string][]velerov1api.PodVolumeBackup)

	phaseToGroup := map[velerov1api.PodVolumeBackupPhase]string{
		velerov1api.PodVolumeBackupPhaseCompleted:  string(velerov1api.PodVolumeBackupPhaseCompleted),
		velerov1api.PodVolumeBackupPhaseFailed:     string(velerov1api.PodVolumeBackupPhaseFailed),
		velerov1api.PodVolumeBackupPhaseCanceled:   string(velerov1api.PodVolumeBackupPhaseCanceled),
		velerov1api.PodVolumeBackupPhaseInProgress: "In Progress",
		velerov1api.PodVolumeBackupPhaseCanceling:  string(velerov1api.PodVolumeBackupPhaseCanceling),
		velerov1api.PodVolumeBackupPhasePrepared:   string(velerov1api.PodVolumeBackupPhasePrepared),
		velerov1api.PodVolumeBackupPhaseAccepted:   string(velerov1api.PodVolumeBackupPhaseAccepted),
		velerov1api.PodVolumeBackupPhaseNew:        string(velerov1api.PodVolumeBackupPhaseNew),
		"":                                         string(velerov1api.PodVolumeBackupPhaseNew),
	}

	for _, backup := range backups {
		group := phaseToGroup[backup.Status.Phase]
		backupsByPhase[group] = append(backupsByPhase[group], backup)
	}

	return backupsByPhase
}

type podVolumeGroup struct {
	label   string
	volumes []string
}

// volumesByPod stores podVolumeGroups, where the grouping
// label is "namespace/name".
type volumesByPod struct {
	volumesByPodMap   map[string]*podVolumeGroup
	volumesByPodSlice []*podVolumeGroup
}

// Add adds a pod volume with the specified pod namespace, name
// and volume to the appropriate group.
// Used for both backup and restore
func (v *volumesByPod) Add(namespace, name, volume, phase string, progress veleroapishared.DataMoveOperationProgress, incrementalBytes *int64) {
	if v.volumesByPodMap == nil {
		v.volumesByPodMap = make(map[string]*podVolumeGroup)
	}

	key := fmt.Sprintf("%s/%s", namespace, name)

	// append backup progress percentage if backup is in progress
	if phase == "In Progress" && progress.TotalBytes != 0 {
		volume = fmt.Sprintf("%s (%.2f%%)", volume, float64(progress.BytesDone)/float64(progress.TotalBytes)*100)
	} else if phase == string(velerov1api.PodVolumeBackupPhaseCompleted) && incrementalBytes != nil {
		// Report the incremental figure whenever it was measured, including zero. Zero is
		// the best possible outcome - nothing changed, so nothing was transferred - and
		// suppressing it leaves only the volume size on display, which reads as a full
		// transfer.
		volume = fmt.Sprintf("%s (size: %v, incremental size: %v)", volume, progress.TotalBytes, *incrementalBytes)
	} else if (phase == string(velerov1api.PodVolumeBackupPhaseCompleted) ||
		phase == string(velerov1api.PodVolumeRestorePhaseCompleted)) &&
		progress.TotalBytes > 0 {
		volume = fmt.Sprintf("%s (size: %v)", volume, progress.TotalBytes)
	}

	if group, ok := v.volumesByPodMap[key]; !ok {
		group := &podVolumeGroup{
			label:   key,
			volumes: []string{volume},
		}

		v.volumesByPodMap[key] = group
		v.volumesByPodSlice = append(v.volumesByPodSlice, group)
	} else {
		group.volumes = append(group.volumes, volume)
	}
}

// Sorted returns a slice of all pod volume groups, ordered by
// label.
func (v *volumesByPod) Sorted() []*podVolumeGroup {
	sort.Slice(v.volumesByPodSlice, func(i, j int) bool {
		return v.volumesByPodSlice[i].label <= v.volumesByPodSlice[j].label
	})

	return v.volumesByPodSlice
}

// DescribeBackupResults describes errors and warnings in human-readable format.
func DescribeBackupResults(ctx context.Context, kbClient kbclient.Client, d *Describer, backup *velerov1api.Backup, insecureSkipTLSVerify bool, caCertPath string) {
	if backup.Status.Warnings == 0 && backup.Status.Errors == 0 {
		return
	}

	// Get BSL cacert if available
	bslCACert, err := cacert.GetCACertFromBackup(ctx, kbClient, backup.Namespace, backup)
	if err != nil {
		// Log the error but don't fail - we can still try to download without the BSL cacert
		d.Printf("WARNING: Error getting cacert from BSL: %v\n", err)
		bslCACert = ""
	}

	var buf bytes.Buffer
	var resultMap map[string]results.Result

	// If err 'ErrNotFound' occurs, it means the backup bundle in the bucket has already been there before the backup-result file is introduced.
	// We only display the count of errors and warnings in this case.
	err = downloadrequest.StreamWithBSLCACert(ctx, kbClient, backup.Namespace, backup.Name, velerov1api.DownloadTargetKindBackupResults, &buf, downloadRequestTimeout, insecureSkipTLSVerify, caCertPath, bslCACert)
	if err == downloadrequest.ErrNotFound {
		d.Printf("Errors:\t%d\n", backup.Status.Errors)
		d.Printf("Warnings:\t%d\n", backup.Status.Warnings)
		return
	} else if err != nil {
		d.Printf("Warnings:\t<error getting warnings: %v>\n\nErrors:\t<error getting errors: %v>\n", err, err)
		return
	}

	if err := json.NewDecoder(&buf).Decode(&resultMap); err != nil {
		d.Printf("Warnings:\t<error decoding warnings: %v>\n\nErrors:\t<error decoding errors: %v>\n", err, err)
		return
	}

	if backup.Status.Warnings > 0 {
		d.Println()
		describeResult(d, "Warnings", resultMap["warnings"])
	}
	if backup.Status.Errors > 0 {
		d.Println()
		describeResult(d, "Errors", resultMap["errors"])
	}
}
