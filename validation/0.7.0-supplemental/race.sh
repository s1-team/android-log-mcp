#!/bin/bash
cd /workspace/alm-wt/v070-test/server
for p in store ipc service analysis device mcpserver; do
  s=$(date +%s); echo "START $p $(date +%T)" >> /workspace/ev/race-progress.txt
  nice -n 19 go test -race -count=20 -p 1 -timeout 150m ./internal/$p > /workspace/ev/race-$p.log 2>&1; rc=$?
  echo "END $p rc=$rc secs=$(( $(date +%s)-s )) $(tail -1 /workspace/ev/race-$p.log | cut -c1-120)" >> /workspace/ev/race-progress.txt
done
echo ALLDONE >> /workspace/ev/race-progress.txt
