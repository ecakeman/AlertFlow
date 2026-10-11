#!/usr/bin/env python3
"""POST a fixture to the agent webhook. Token and URL come from the environment."""

import json
import os
import urllib.request
from pathlib import Path

body = Path(__file__).resolve().parent.joinpath("scenarios", "incident_start.json").read_bytes()
request = urllib.request.Request(
    os.environ.get("AGENT_URL", "http://127.0.0.1:8000/webhooks/alertflow"),
    data=body,
    headers={"Authorization": "Bearer " + os.environ.get("AGENT_WEBHOOK_TOKEN", ""), "Content-Type": "application/json"},
    method="POST",
)
with urllib.request.urlopen(request) as response:
    print(response.status, response.read().decode())
