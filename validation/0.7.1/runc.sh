#!/bin/bash
cd /workspace/ev/071
for s in upgrade071b hung071c anr071b upgrade-long071; do echo "START $s $(date +%T)" >> /workspace/alm-0.7.1-verify-progress.md; nice -n 10 bash $s.sh > $s-run.txt 2>&1; echo "END $s rc=$? $(date +%T)" >> /workspace/alm-0.7.1-verify-progress.md; done
bash /workspace/ev/071/rune.sh
