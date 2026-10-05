# Agent credentials

Native agents authenticate to their provider through the kagent installation: the Agent's
ModelConfig holds the provider credential, and Mainloop neither stores nor injects it. Mainloop
has no credential broker, seeding, or sign-in flow.

## Attention and recovery

The native turn path does not check credentials or raise a sign-in attention item: turns go to
kagent, and a provider authentication failure surfaces as a failed task. A failed turn is not
replayed automatically. Surfacing credential health from the kagent ModelConfig condition is a
later change.

## Tool access

The `mainloop` MCP server each native agent uses is authenticated per binding with a token whose
hash Mainloop keeps on the binding; terminal or archived bindings lose tool access. See
`docs/architecture.md`.
