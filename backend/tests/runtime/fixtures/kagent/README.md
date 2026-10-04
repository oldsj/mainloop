# kagent offline contracts

`remotemcpserver-crd.yaml` is the generated, configuration-free RemoteMCPServer CRD from
`api.kagent.dev/v1alpha3` in the companion a2 candidate, base `5662c609`. It contains only
public schema/defaults and no cluster instances, status values or credentials. Tests use
this local fixture to check served group/version and required fields offline.

`session-credential.hex` pins the shared credentials-only CreateSessionRequest contract.
Its strings are DNS/object names and a synthetic binding id; it contains no Secret value.
