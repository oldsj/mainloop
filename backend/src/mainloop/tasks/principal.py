"""Server-resolved authority, never constructed from a tool payload."""

from dataclasses import dataclass


@dataclass(frozen=True, slots=True)
class TaskPrincipal:
    owner_id: str
    binding_id: str | None = None
    role: str = "owner"
    task_id: str | None = None
    attempt_id: str | None = None
    project_id: str | None = None
    root_task_id: str | None = None
    depth: int = 0

    @property
    def key(self) -> str:
        return f"binding:{self.binding_id}" if self.binding_id else "owner"

    def __post_init__(self):
        if self.role == "owner":
            valid = (
                self.binding_id is None
                and self.depth == 0
                and self.task_id is None
                and self.attempt_id is None
            )
        elif self.role == "main":
            valid = (
                self.binding_id is not None
                and self.depth == 0
                and self.task_id is None
                and self.attempt_id is None
            )
        else:
            valid = (self.role, self.depth) in (
                ("supervisor", 1),
                ("child", 2),
            ) and all(
                (self.binding_id, self.task_id, self.attempt_id, self.root_task_id)
            )
        if not valid:
            raise ValueError("invalid task principal")
