"""DBOS configuration and initialization."""

import os

from dbos import DBOS, DBOSConfig
from mainloop.config import settings

# DBOS configuration
# application_version prevents recovery of old workflows after code changes
# Bump this when workflow step order/logic changes to avoid DBOSUnexpectedStepError
WORKFLOW_VERSION = (
    "12"  # v12: Native Substrate sessions replace the removed session workers
)

dbos_config: DBOSConfig = {
    "name": "mainloop",
    "system_database_url": settings.database_url
    or os.environ.get("DBOS_SYSTEM_DATABASE_URL"),
    "application_database_url": settings.database_url,
    "application_version": WORKFLOW_VERSION,
}

# Initialize DBOS - must be done before defining workflows
DBOS(config=dbos_config)
