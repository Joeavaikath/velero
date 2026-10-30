// Package backupvolume loads a backup's volume info artifact and projects it
// onto the shapes the item-outcomes renderers need.
//
// It exists because two commands read the same artifact to answer two questions
// about the same volumes: "backup describe --details" prints what happened to
// each volume, and "backup item-outcomes" joins the volume verdict onto each
// item row so a PVC that reads BackedUp is not mistaken for a captured disk.
// Keeping the read and the projection here means the two cannot drift on what
// a volume's result means.
//
// The projection lands in itemoutcome.VolumeRef rather than in
// internal/volume.BackupVolumeInfo because internal/itemoutcome must stay a leaf
// package: pkg/util/logging imports it for the per-item log hook, and
// internal/volume reaches pkg/util/logging, so itemoutcome importing volume
// would close a cycle. That is why the projection is a copy of the three fields
// the rule needs rather than a reuse of the full struct.
package backupvolume

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	kbclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/vmware-tanzu/velero/internal/itemoutcome"
	"github.com/vmware-tanzu/velero/internal/volume"
	velerov1api "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	"github.com/vmware-tanzu/velero/pkg/cmd/util/downloadrequest"
)

// Result is the outcome of loading a backup's volume info.
//
// Missing distinguishes "this backup has no volume info artifact" from "the
// artifact is there and lists no volumes". Callers need the difference:
// describeBackupVolumes reconstructs snapshots from Backup.Status for pre-artifact
// backups, and must not do that for a backup that simply recorded no volumes.
type Result struct {
	// Volumes is the artifact contents, empty when the artifact is absent.
	Volumes []volume.BackupVolumeInfo
	// Missing reports that the artifact was not found, as opposed to being
	// unreadable or empty.
	Missing bool
}

// Load reads and decodes the volume info artifact for backup.
//
// timeout is the caller's, not this package's: the item-outcomes command exposes
// it as a flag, and the describer bounds every artifact download the same way.
// Hardcoding one here would quietly ignore the user's choice for this artifact
// while honoring it for the one beside it.
//
// A missing artifact is not an error: it is reported through Result.Missing, so
// callers that have a fallback can take it. A transport or decode failure is
// returned as an error because neither caller can say anything useful about the
// volumes without the bytes.
// DecodeError reports that the artifact was fetched but could not be read. The
// structured describer distinguishes it from a fetch failure because it has
// always reported the two under different keys, and a consumer may key on them.
var DecodeError = errors.New("backup volume info could not be decoded")

func Load(ctx context.Context, kbClient kbclient.Client, backup *velerov1api.Backup, timeout time.Duration, insecureSkipTLSVerify bool, caCertPath string, bslCACert string) (Result, error) {
	buf := new(bytes.Buffer)
	err := downloadrequest.StreamWithBSLCACert(ctx, kbClient, backup.Namespace, backup.Name,
		velerov1api.DownloadTargetKindBackupVolumeInfos, buf, timeout, insecureSkipTLSVerify, caCertPath, bslCACert)
	if err == downloadrequest.ErrNotFound {
		return Result{Missing: true}, nil
	}
	if err != nil {
		return Result{}, err
	}

	var infos []volume.BackupVolumeInfo
	if err := json.NewDecoder(buf).Decode(&infos); err != nil {
		return Result{}, fmt.Errorf("%w: %w", DecodeError, err)
	}

	return Result{Volumes: infos}, nil
}

// ToRollupRefs projects volume info onto the minimal shape the item-outcomes
// rollup rule takes.
func ToRollupRefs(infos []volume.BackupVolumeInfo) []itemoutcome.VolumeRef {
	refs := make([]itemoutcome.VolumeRef, 0, len(infos))
	for _, info := range infos {
		refs = append(refs, itemoutcome.VolumeRef{
			PVCNamespace: info.PVCNamespace,
			PVCName:      info.PVCName,
			PVName:       info.PVName,
			// Only an explicit failure counts. A volume skipped by one path but
			// captured by another is not a failure, and treating Result alone as
			// the verdict would report it as one.
			Failed: info.Result == volume.VolumeResultFailed,
		})
	}
	return refs
}
