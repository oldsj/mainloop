package dbstate

// DevloopOperationKind is devloop_operations.kind: which Kubernetes- or GitHub-reaching action the
// row drives.
type DevloopOperationKind string

// Devloop operation kinds.
const (
	DevloopSync   DevloopOperationKind = "sync"
	DevloopStatus DevloopOperationKind = "status"
)

// DevloopOperationState is devloop_operations.state. `unknown` covers a sync whose copy outcome is
// uncertain; the loop resolves it by safely re-running the copy, since overwriting the sync
// directory with the same commit's tree is idempotent.
type DevloopOperationState string

// Devloop operation states.
const (
	DevloopPending   DevloopOperationState = "pending"
	DevloopSucceeded DevloopOperationState = "succeeded"
	DevloopFailed    DevloopOperationState = "failed"
	DevloopUnknown   DevloopOperationState = "unknown"
)

// DevloopTargetKind is devloop_targets.kind: how to reach the workload's Kubernetes location.
type DevloopTargetKind string

// Devloop target kinds.
const (
	DevloopTargetNamespace DevloopTargetKind = "namespace"
	DevloopTargetVCluster  DevloopTargetKind = "vcluster"
)
