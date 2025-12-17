#!/bin/bash
set -e

# This script keeps the container running and ready to accept commands
# In the future, this will be replaced with an HTTP server or similar
# that the backend can communicate with to execute Claude Code CLI commands

echo "Claude agent container started"
echo "Waiting for commands..."

# Keep container running
tail -f /dev/null
