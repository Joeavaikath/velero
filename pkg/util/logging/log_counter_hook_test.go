package logging

import (
	"errors"
	"fmt"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/vmware-tanzu/velero/internal/itemoutcome"
	"github.com/vmware-tanzu/velero/pkg/plugin/velero"
	"github.com/vmware-tanzu/velero/pkg/util/results"
)

func TestLogHook_Fire(t *testing.T) {
	hook := NewLogHook(nil)

	entry := &logrus.Entry{
		Level: logrus.ErrorLevel,
		Data: logrus.Fields{
			"namespace": "test-namespace",
			"error":     errors.New("test-error"),
			"resource":  "test-resource",
			"name":      "test-name",
		},
		Message: "test-message",
	}

	err := hook.Fire(entry)
	require.NoError(t, err)

	// Verify the counts
	assert.Equal(t, 1, hook.counts[logrus.ErrorLevel])

	// Verify the entries
	expectedResult := &results.Result{}
	expectedResult.Add("test-namespace", fmt.Errorf(" resource: /test-resource name: /test-name message: /test-message error: /%v", entry.Data["error"]))
	assert.Equal(t, expectedResult, hook.entries[logrus.ErrorLevel])

	entry1 := &logrus.Entry{
		Level: logrus.ErrorLevel,
		Data: logrus.Fields{
			"error.message": errors.New("test-error"),
			"resource":      "test-resource",
			"name":          "test-name",
		},
		Message: "test-message",
	}

	err = hook.Fire(entry1)
	require.NoError(t, err)

	// Verify the counts
	assert.Equal(t, 2, hook.counts[logrus.ErrorLevel])

	// Verify the entries
	expectedResult = &results.Result{}
	expectedResult.Add("test-namespace", fmt.Errorf(" resource: /test-resource name: /test-name message: /test-message error: /%v", entry.Data["error"]))
	expectedResult.AddVeleroError(fmt.Errorf(" resource: /test-resource name: /test-name message: /test-message error: /%v", entry1.Data["error.message"]))
	assert.Equal(t, expectedResult, hook.entries[logrus.ErrorLevel])
}

func TestLogHook_Fire_RecordsStructuredOutcomes(t *testing.T) {
	recorder := itemoutcome.NewRecorder()
	hook := NewLogHook(recorder)

	errEntry := &logrus.Entry{
		Level: logrus.ErrorLevel,
		Data: logrus.Fields{
			"namespace": "ns1",
			"resource":  "pods",
			"name":      "pod1",
			"error":     errors.New("boom"),
		},
		Message: "backup item failed",
	}
	require.NoError(t, hook.Fire(errEntry))

	warnEntry := &logrus.Entry{
		Level: logrus.WarnLevel,
		Data: logrus.Fields{
			"resource": "persistentvolumeclaims",
			"name":     "pvc1",
		},
		Message: "backup item warning",
	}
	require.NoError(t, hook.Fire(warnEntry))

	result := recorder.Result()
	byName := map[string]itemoutcome.ItemOutcome{}
	for _, item := range result.Items {
		byName[item.Name] = item
	}

	require.Contains(t, byName, "pod1")
	assert.Equal(t, itemoutcome.OutcomeFailed, byName["pod1"].Outcome)
	assert.Equal(t, "ns1", byName["pod1"].Namespace)
	assert.Equal(t, "pods", byName["pod1"].GroupResource)

	require.Contains(t, byName, "pvc1")
	assert.Equal(t, itemoutcome.OutcomeWarning, byName["pvc1"].Outcome)
	assert.Empty(t, byName["pvc1"].Namespace)
}

// Attribution requires a resource as well as a name. A line carrying only a name
// is still counted and still filed against the backup, but nothing ties it to an
// item. This is the shape the pre/post hook failure lines in pkg/backup had, and
// it meant a failed hook was recorded against no item at all.
func TestLogHook_Fire_NameWithoutResourceIsNotAttributed(t *testing.T) {
	recorder := itemoutcome.NewRecorder()
	hook := NewLogHook(recorder)

	require.NoError(t, hook.Fire(&logrus.Entry{
		Level: logrus.ErrorLevel,
		Data: logrus.Fields{
			"namespace": "ns1",
			"name":      "pod1",
			"error":     errors.New("hook failed"),
		},
		Message: "Error running pre hooks for pod",
	}))

	assert.Empty(t, recorder.Result().Items,
		"a name without a resource must not mint an item outcome row")

	// The event is not lost: it is still counted and still filed against the
	// backup, just not against an item. Because the line carries a namespace it
	// lands in that namespace's bucket rather than the Velero-wide one.
	assert.Equal(t, 1, hook.GetCount(logrus.ErrorLevel))
	entries := hook.GetEntries(logrus.ErrorLevel)
	assert.Empty(t, entries.Velero)
	assert.Len(t, entries.Namespaces["ns1"], 1)
}

// The same event, with the resource the hook lines now log, lands on the item.
// It merges with the row Observe already minted for that pod rather than
// creating a second one, which is why the fields must match Observe's exactly.
func TestLogHook_Fire_HookFailureAttributesToObservedItem(t *testing.T) {
	recorder := itemoutcome.NewRecorder()

	id := velero.ResourceIdentifier{
		GroupResource: schema.GroupResource{Resource: "pods"},
		Namespace:     "ns1",
		Name:          "pod1",
	}
	recorder.Observe(id)

	hook := NewLogHook(recorder)
	require.NoError(t, hook.Fire(&logrus.Entry{
		Level: logrus.ErrorLevel,
		Data: logrus.Fields{
			"resource":  "pods",
			"namespace": "ns1",
			"name":      "pod1",
			"error":     errors.New("hook failed"),
		},
		Message: "Error running pre hooks for pod",
	}))

	result := recorder.Result()
	require.Len(t, result.Items, 1, "the failure must merge with the observed row, not add one")
	assert.Equal(t, itemoutcome.OutcomeFailed, result.Items[0].Outcome)
	assert.Equal(t, "pod1", result.Items[0].Name)
}

func TestLogHook_Fire_NilRecorderIsNoOp(t *testing.T) {
	hook := NewLogHook(nil)

	entry := &logrus.Entry{
		Level: logrus.ErrorLevel,
		Data: logrus.Fields{
			"resource": "pods",
			"name":     "pod1",
		},
		Message: "failure",
	}

	assert.NotPanics(t, func() {
		require.NoError(t, hook.Fire(entry))
	})
}

func TestLogHook_Fire_IgnoresInfoLevel(t *testing.T) {
	recorder := itemoutcome.NewRecorder()
	hook := NewLogHook(recorder)

	entry := &logrus.Entry{
		Level: logrus.InfoLevel,
		Data: logrus.Fields{
			"resource": "pods",
			"name":     "pod1",
		},
		Message: "just FYI",
	}
	require.NoError(t, hook.Fire(entry))

	result := recorder.Result()
	assert.Empty(t, result.Items)
}

func TestLogHook_Levels(t *testing.T) {
	hook := NewLogHook(nil)

	levels := hook.Levels()

	expectedLevels := []logrus.Level{
		logrus.PanicLevel,
		logrus.FatalLevel,
		logrus.ErrorLevel,
		logrus.WarnLevel,
		logrus.InfoLevel,
		logrus.DebugLevel,
		logrus.TraceLevel,
	}

	assert.Equal(t, expectedLevels, levels)
}

func TestLogHook_GetCount(t *testing.T) {
	hook := NewLogHook(nil)

	// Set up test data
	hook.counts[logrus.ErrorLevel] = 5
	hook.counts[logrus.WarnLevel] = 10

	// Test GetCount for ErrorLevel
	count := hook.GetCount(logrus.ErrorLevel)
	assert.Equal(t, 5, count)

	// Test GetCount for WarnLevel
	count = hook.GetCount(logrus.WarnLevel)
	assert.Equal(t, 10, count)

	// Test GetCount for other levels
	count = hook.GetCount(logrus.InfoLevel)
	assert.Equal(t, 0, count)

	count = hook.GetCount(logrus.DebugLevel)
	assert.Equal(t, 0, count)
}

func TestLogHook_GetEntries(t *testing.T) {
	hook := NewLogHook(nil)

	// Set up test data
	entry := &logrus.Entry{
		Level: logrus.ErrorLevel,
		Data: logrus.Fields{
			"namespace": "test-namespace",
			"error":     errors.New("test-error"),
			"resource":  "test-resource",
			"name":      "test-name",
		},
		Message: "test-message",
	}
	expectedResult := &results.Result{}
	expectedResult.Add("test-namespace", fmt.Errorf(" resource: /test-resource name: /test-name message: /test-message error: /%v", entry.Data["error"]))
	hook.entries[logrus.ErrorLevel] = expectedResult

	// Test GetEntries for ErrorLevel
	result := hook.GetEntries(logrus.ErrorLevel)
	assert.Equal(t, *expectedResult, result)

	// Test GetEntries for WarnLevel
	result = hook.GetEntries(logrus.WarnLevel)
	assert.Equal(t, results.Result{}, result)

	// Test GetEntries for other levels
	result = hook.GetEntries(logrus.InfoLevel)
	assert.Equal(t, results.Result{}, result)

	result = hook.GetEntries(logrus.DebugLevel)
	assert.Equal(t, results.Result{}, result)
}
