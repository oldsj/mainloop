# kagent offline contracts

`remotemcpserver-crd.yaml` is the generated, configuration-free RemoteMCPServer CRD from
`api.kagent.dev/v1alpha3` in the companion a2 candidate, base `5662c609`. It contains only
public schema/defaults and no cluster instances, status values or credentials. Tests use
this local fixture to check served group/version and required fields offline.

`session-credential.hex` pins the shared credentials-only CreateSessionRequest contract.
Its strings are DNS/object names and a synthetic binding id; it contains no Secret value.

`hitl-free-text-direct.json` and `hitl-free-text-propagated.json` are sanitized,
source-derived wire fixtures, not live captures. They follow kagent revision
`cc299b03ab84ad6be47661d16402ee5946ec0590`: `go/harness/runtime/a2a/executor.go`
encodes empty choices as a nil slice (`null`); `go/adk/pkg/a2a/hitl.go`'s
`VisibleTools` and `BuildHITLStatusMessage` wrap the same question with distinct
parent/child IDs for propagation. Task, agent and request IDs are synthetic.
