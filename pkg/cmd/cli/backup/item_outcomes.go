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
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	kbclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/vmware-tanzu/velero/internal/itemoutcome"
	velerov1api "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	"github.com/vmware-tanzu/velero/pkg/client"
	"github.com/vmware-tanzu/velero/pkg/cmd"
	"github.com/vmware-tanzu/velero/pkg/cmd/cli"
	"github.com/vmware-tanzu/velero/pkg/cmd/util/backupvolume"
	"github.com/vmware-tanzu/velero/pkg/cmd/util/cacert"
	"github.com/vmware-tanzu/velero/pkg/cmd/util/downloadrequest"
)

type ItemOutcomesOptions struct {
	Timeout               time.Duration
	InsecureSkipTLSVerify bool
	CaCertFile            string
	Raw                   bool
	ByOwner               bool
	All                   bool
	Output                string
	Client                kbclient.Client
	BackupName            string
	Namespace             string
}

func NewItemOutcomesOptions() ItemOutcomesOptions {
	config, err := client.LoadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "WARNING: Error reading config file: %v\n", err)
	}

	return ItemOutcomesOptions{
		Timeout:               time.Minute,
		InsecureSkipTLSVerify: false,
		CaCertFile:            config.CACertFile(),
		Output:                "table",
	}
}

func (o *ItemOutcomesOptions) BindFlags(flags *pflag.FlagSet) {
	flags.DurationVar(&o.Timeout, "timeout", o.Timeout, "How long to wait to receive the item outcomes artifact.")
	flags.BoolVar(&o.InsecureSkipTLSVerify, "insecure-skip-tls-verify", o.InsecureSkipTLSVerify, "If true, the object store's TLS certificate will not be checked for validity. This is insecure and susceptible to man-in-the-middle attacks. Not recommended for production.")
	flags.StringVar(&o.CaCertFile, "cacert", o.CaCertFile, "Path to a certificate bundle to use when verifying TLS connections.")
	flags.BoolVar(&o.Raw, "raw", o.Raw, "Print the compact, un-indented JSON as stored in object storage, instead of pretty-printing it.")
	flags.BoolVar(&o.ByOwner, "by-owner", o.ByOwner, "Roll the items up to the object that owns them, so each backup item's outcome is reported against the root of its parenting chain. Consult the backup's volume info, so a volume that was skipped by one method but captured by another does not read as a failure.")
	flags.BoolVar(&o.All, "all", o.All, "With --by-owner, show every item in table output instead of only the chains that failed or warned. JSON and tree output are always unfiltered.")
	flags.StringVarP(&o.Output, "output", "o", o.Output, "Output format for --by-owner: table, json or tree.")
}

func (o *ItemOutcomesOptions) Run(c *cobra.Command, f client.Factory) error {
	backup := new(velerov1api.Backup)
	err := o.Client.Get(context.Background(), kbclient.ObjectKey{Namespace: f.Namespace(), Name: o.BackupName}, backup)
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("backup %q does not exist", o.BackupName)
	} else if err != nil {
		return fmt.Errorf("error checking for backup %q: %v", o.BackupName, err)
	}

	// Get BSL cacert if available
	bslCACert, err := cacert.GetCACertFromBackup(context.Background(), o.Client, f.Namespace(), backup)
	if err != nil {
		// Log the error but don't fail - we can still try to download without the BSL cacert
		fmt.Fprintf(os.Stderr, "WARNING: Error getting cacert from BSL: %v\n", err)
		bslCACert = ""
	}

	buf := new(bytes.Buffer)
	err = downloadrequest.StreamWithBSLCACert(context.Background(), o.Client, f.Namespace(), o.BackupName, velerov1api.DownloadTargetKindBackupItemOutcomes, buf, o.Timeout, o.InsecureSkipTLSVerify, o.CaCertFile, bslCACert)
	if err == downloadrequest.ErrNotFound {
		return fmt.Errorf("item outcomes were not collected for backup %q", o.BackupName)
	}
	if err != nil {
		return err
	}

	if o.Raw {
		_, err = os.Stdout.Write(buf.Bytes())
		return err
	}

	var outcomes itemoutcome.ItemOutcomes
	if err := json.Unmarshal(buf.Bytes(), &outcomes); err != nil {
		return fmt.Errorf("error reading item outcomes: %v", err)
	}

	if o.ByOwner {
		return o.printRollup(&outcomes, bslCACert, backup)
	}

	// Without --by-owner the command prints the artifact as stored, so an -o or
	// --all given on its own would be silently ignored. Say so instead.
	if c.Flags().Changed("output") || o.Output != "table" {
		return fmt.Errorf("--output %q requires --by-owner: the artifact is printed as JSON otherwise", o.Output)
	}
	if o.All {
		return fmt.Errorf("--all requires --by-owner")
	}

	pretty, err := json.MarshalIndent(outcomes, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(pretty))
	return nil
}

// printRollup reports each backup item's outcome against the root of its
// parenting chain. It reads the volume info artifact because an item's own
// outcome cannot distinguish a benign skip from a lost disk; see
// itemoutcome.IsFailure.
func (o *ItemOutcomesOptions) printRollup(outcomes *itemoutcome.ItemOutcomes, bslCACert string, backup *velerov1api.Backup) error {
	vols := o.volumeRefs(backup, bslCACert)
	summaries := itemoutcome.Rollup(outcomes, vols)

	// The artifact only covers items that were processed. A backup can fail while
	// listing resources, before any item exists, and then every item still reads
	// BackedUp. Say so rather than reporting the backup as clean.
	if backup != nil && backup.Status.Errors > 0 {
		fmt.Fprintf(os.Stderr, "WARNING: backup %q recorded %d error(s), which need not belong to a listed item; "+
			"see `velero backup logs %s`.\n", backup.Name, backup.Status.Errors, backup.Name)
	}

	switch o.Output {
	case "json":
		out, err := json.MarshalIndent(summaries, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(out))
		return nil
	case "tree":
		return o.printTree(outcomes, itemoutcome.IndexVolumes(vols), summaries)
	case "", "table":
	default:
		return fmt.Errorf("unsupported output format %q: use table, json or tree", o.Output)
	}

	// Without --all only chains that hold trouble are printed: a clean backup
	// produces one line per object, which buries the ones that failed. With --all
	// every row is printed, so the shape of the backup is visible too.
	troubled := 0
	for _, s := range summaries {
		if s.Outcome != itemoutcome.OutcomeBackedUp {
			troubled++
		}
	}
	if troubled == 0 && !o.All {
		fmt.Printf("No backup item failed or warned across %d owner(s).\n", len(summaries))
		// A skip is not trouble when another method captured the data, but saying
		// nothing was skipped would contradict the artifact.
		benignSkips := 0
		for _, s := range summaries {
			benignSkips += s.Counts[itemoutcome.OutcomeSkipped]
		}
		if benignSkips > 0 {
			fmt.Printf("%d item(s) were skipped because another backup method captured the data.\n", benignSkips)
		}
		return nil
	}

	// The table carries the same forest the tree does, indented inside the same
	// columns. A flat one-row-per-owner listing hides the chain that explains a
	// verdict: a VM reported Skipped says nothing until the disk beneath it is
	// visible. Each row's counts are its own subtree, so an ancestor carries the
	// trouble its descendants caused.
	byID := itemoutcome.IndexByKey(outcomes)
	childrenOf := itemoutcome.ChildrenMap(itemoutcome.ParentMap(byID))
	nodes := itemoutcome.Summarize(outcomes, vols)

	w := tabwriter.NewWriter(os.Stdout, 0, 8, 2, ' ', 0)
	header := []string{"GROUP/RESOURCE", "NAMESPACE", "NAME", "OUTCOME", "ITEMS"}
	if o.All {
		header = append(header, "BACKEDUP", "WARNING", "FAILED", "SKIPPED", "NAMED")
	}
	fmt.Fprintln(w, strings.Join(header, "\t"))

	var writeRows func(key itemoutcome.Key, depth int)
	writeRows = func(key itemoutcome.Key, depth int) {
		// parentOf can only drop dangling references, not cycles, and a cyclic
		// artifact would make this recurse forever. The rollup tolerates one, so
		// the renderer must too.
		if depth > itemoutcome.MaxDepth {
			return
		}

		summary := nodes[key]
		// Pruning on the subtree verdict keeps every ancestor of a failure while
		// dropping the siblings that are fine.
		if !o.All && summary.Outcome == itemoutcome.OutcomeBackedUp {
			return
		}
		writeTableRow(w, byID[key], summary, depth, o.All)

		children := append([]itemoutcome.Key(nil), childrenOf[key]...)
		sortTreeKeys(children, byID)
		for _, child := range children {
			writeRows(child, depth+1)
		}
	}

	// Summaries are worst-first, so a broken VM is listed before a namespace that
	// merely exists.
	for _, s := range summaries {
		writeRows(itemoutcome.KeyOfRef(&s.Owner), 0)
	}
	return w.Flush()
}

// writeTableRow prints one item and the rollup of everything beneath it. The
// item is named rather than the summary's owner, because a nested row is not a
// root.
func writeTableRow(w io.Writer, item itemoutcome.ItemOutcome, summary itemoutcome.OwnerSummary, depth int, all bool) {
	total := 0
	for _, n := range summary.Counts {
		total += n
	}

	fmt.Fprintf(w, "%s%s\t%s\t%s\t%s\t%d",
		strings.Repeat("  ", depth), item.GroupResource, tableNamespace(item.Namespace), item.Name, summary.Outcome, total)
	if all {
		fmt.Fprintf(w, "\t%d\t%d\t%d\t%d\t%d",
			summary.Counts[itemoutcome.OutcomeBackedUp], summary.Counts[itemoutcome.OutcomeWarning],
			summary.Counts[itemoutcome.OutcomeFailed], summary.Counts[itemoutcome.OutcomeSkipped],
			summary.Counts[itemoutcome.OutcomeNamed])
	}
	fmt.Fprintln(w)
}

// printTree renders the parenting forest itself, which the table and JSON
// formats both flatten. Rollup is used only for its ordering: it ranks owners
// worst-first, so a broken VM appears before a namespace that merely exists.
// The tree itself comes from the artifact, because OwnerSummary keeps counts
// and failures but not the shape that produced them.
func (o *ItemOutcomesOptions) printTree(outcomes *itemoutcome.ItemOutcomes, vols *itemoutcome.VolumeIndex, summaries []itemoutcome.OwnerSummary) error {
	if outcomes == nil || len(outcomes.Items) == 0 {
		fmt.Println("No items were recorded for this backup.")
		return nil
	}

	byID := itemoutcome.IndexByKey(outcomes)
	childrenOf := itemoutcome.ChildrenMap(itemoutcome.ParentMap(byID))
	ambiguous := resourceGroups(outcomes)

	w := bufio.NewWriter(os.Stdout)
	defer w.Flush()

	// Summaries are already worst-first and cover every root exactly once, so
	// they give both the order and the root set without a second walk.
	for _, summary := range summaries {
		root := itemoutcome.KeyOfRef(&summary.Owner)
		writeTreeRow(w, byID[root], 0, vols, byID, ambiguous)
		writeTreeChildren(w, childrenOf, root, 1, vols, byID, ambiguous)
	}
	return nil
}

func writeTreeChildren(w io.Writer, childrenOf map[itemoutcome.Key][]itemoutcome.Key, key itemoutcome.Key, depth int, vols *itemoutcome.VolumeIndex, byID map[itemoutcome.Key]itemoutcome.ItemOutcome, ambiguous map[string][]string) {
	// A cyclic artifact leaves no true root, but ResolveRoot still names one, so
	// following children from it would loop. The rollup tolerates a cycle, so the
	// renderer must stop rather than recurse forever.
	if depth > itemoutcome.MaxDepth {
		return
	}

	children := childrenOf[key]
	// Sorted so two runs against one artifact print the same thing; the map the
	// children come from has no order of its own.
	sortTreeKeys(children, byID)
	for _, child := range children {
		writeTreeRow(w, byID[child], depth, vols, byID, ambiguous)
		writeTreeChildren(w, childrenOf, child, depth+1, vols, byID, ambiguous)
	}
}

func writeTreeRow(w io.Writer, item itemoutcome.ItemOutcome, depth int, vols *itemoutcome.VolumeIndex, byID map[itemoutcome.Key]itemoutcome.ItemOutcome, ambiguous map[string][]string) {
	fmt.Fprintf(w, "%s%s %s", strings.Repeat("  ", depth), shortResource(item.GroupResource, ambiguous), item.Name)
	if item.Namespace != "" {
		fmt.Fprintf(w, " (%s)", item.Namespace)
	}

	// Only rows that raise an owner are flagged. Printing "[BackedUp]" on every
	// line would bury the ones that matter, which is the whole reason the tree
	// exists in this format.
	if itemoutcome.IsFailure(item, vols, byID) {
		fmt.Fprintf(w, "  [%s]", item.Outcome)
		for _, msg := range item.Messages {
			fmt.Fprintf(w, "\n%s- %s", strings.Repeat("  ", depth+1), msg)
		}
	}
	fmt.Fprintln(w)
}

// sortTreeKeys orders siblings by resource then name, so a tree does not reshuffle
// between runs.
func sortTreeKeys(keys []itemoutcome.Key, byID map[itemoutcome.Key]itemoutcome.ItemOutcome) {
	sort.Slice(keys, func(i, j int) bool {
		a, b := byID[keys[i]], byID[keys[j]]
		if a.GroupResource != b.GroupResource {
			return a.GroupResource < b.GroupResource
		}
		return a.Name < b.Name
	})
}

// resourceGroups maps each resource name to the API groups it appeared under in
// one artifact. It is built before rendering so a resource that is ambiguous
// prints the same way on every row, rather than depending on which row came first.
func resourceGroups(items *itemoutcome.ItemOutcomes) map[string][]string {
	seen := map[string]map[string]bool{}
	for _, item := range items.Items {
		i := strings.Index(item.GroupResource, ".")
		if i <= 0 {
			continue
		}
		resource := item.GroupResource[:i]
		if seen[resource] == nil {
			seen[resource] = map[string]bool{}
		}
		seen[resource][item.GroupResource[i+1:]] = true
	}

	groups := make(map[string][]string, len(seen))
	for resource, set := range seen {
		if len(set) < 2 {
			continue
		}
		for group := range set {
			groups[resource] = append(groups[resource], group)
		}
		sort.Strings(groups[resource])
	}
	return groups
}

// shortResource trims a GroupResource for the tree. The group is dropped only
// when the resource name alone identifies the kind: a cluster can hold
// rolebindings.authorization.openshift.io and rolebindings.rbac.authorization.k8s.io
// under the same names, so stripping both groups would print two identical lines
// for two different objects. Lenient because nesting already shows ownership, and
// a full group at every depth pushes the names off the screen.
func shortResource(groupResource string, ambiguous map[string][]string) string {
	i := strings.Index(groupResource, ".")
	if i <= 0 {
		return groupResource
	}
	resource := groupResource[:i]
	groups := ambiguous[resource]
	if len(groups) == 0 {
		return resource
	}
	// Keep the shortest suffix that tells this group apart from its siblings.
	group := groupResource[i+1:]
	for _, other := range groups {
		if other == group {
			continue
		}
		return resource + " (" + shortestDistinguishingSuffix(group, other) + ")"
	}
	return resource
}

// shortestDistinguishingSuffix returns the shortest leading dotted segment of
// group that other does not share, falling back to trailing segments. The
// leading part is preferred because it names the API: "authorization" reads
// better than "openshift.io" when the sibling group is "rbac.authorization.k8s.io".
func shortestDistinguishingSuffix(group, other string) string {
	parts := strings.Split(group, ".")
	otherParts := strings.Split(other, ".")
	suffix := ""
	for n := 1; n <= len(parts); n++ {
		suffix = strings.Join(parts[:n], ".")
		if n > len(otherParts) || strings.Join(otherParts[:n], ".") != suffix {
			return suffix
		}
	}
	return group
}

// tableNamespace prints a cluster-scoped item's empty namespace as "-", so an
// empty cell is not mistaken for a column that failed to print.
func tableNamespace(namespace string) string {
	if namespace == "" {
		return "-"
	}
	return namespace
}

// volumeRefs reads the backup's volume info and projects it onto the shape the
// rollup rule takes. A unreadable or absent artifact yields no refs rather than
// an error: the rollup is still useful, it just cannot distinguish a benign skip
// from a failed volume.
//
// A nil backup yields no refs: the artifact is addressed through the backup, so
// there is nothing to read, and printRollup treats a nil backup as a case to
// tolerate rather than a programmer error.
func (o *ItemOutcomesOptions) volumeRefs(backup *velerov1api.Backup, bslCACert string) []itemoutcome.VolumeRef {
	if backup == nil {
		return nil
	}
	vols, err := backupvolume.Load(context.Background(), o.Client, backup, o.Timeout, o.InsecureSkipTLSVerify, o.CaCertFile, bslCACert)
	if err != nil {
		fmt.Fprintf(os.Stderr, "WARNING: could not read volume info, so a skipped volume cannot be checked against its result: %v\n", err)
		return nil
	}
	return backupvolume.ToRollupRefs(vols.Volumes)
}

func (o *ItemOutcomesOptions) Complete(args []string, f client.Factory) error {
	if len(args) > 0 {
		o.BackupName = args[0]
	}

	kbClient, err := f.KubebuilderClient()
	if err != nil {
		return err
	}
	o.Client = kbClient
	o.Namespace = f.Namespace()
	return nil
}

func NewItemOutcomesCommand(f client.Factory) *cobra.Command {
	o := NewItemOutcomesOptions()

	c := &cobra.Command{
		Use:   "item-outcomes BACKUP",
		Short: "Get the per-item backup outcomes artifact, as JSON",
		Args:  cobra.ExactArgs(1),
		Run: func(c *cobra.Command, args []string) {
			err := o.Complete(args, f)
			cmd.CheckError(err)

			err = o.Run(c, f)
			cmd.CheckError(err)
		},
	}

	c.ValidArgsFunction = cli.CompleteBackupNames(f)
	o.BindFlags(c.Flags())

	return c
}
