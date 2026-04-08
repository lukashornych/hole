#!/bin/bash
set -euo pipefail
curl -fsSL https://claude.ai/install.sh | bash

# initial claude setup to pickup login state from .claude/ and do not prompt user with same questions for each sandbox
printf '{
  "bypassPermissionsModeAccepted": true,
  "hasCompletedOnboarding": true,
  "projects": {
    "/": {
      "hasTrustDialogAccepted": true
    }
  }
}' > "${HOME}/.claude.json"