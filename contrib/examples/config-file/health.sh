#!/bin/sh
# Runs as root after apply. A failure here starts the rollback.
set -eu
sleep 2
systemctl is-active --quiet example-broker.service
