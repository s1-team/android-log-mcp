import subprocess, sys, time, os
p = subprocess.Popen(["adb", "track-devices"], stdout=subprocess.PIPE)
out = open(sys.argv[1], "a", buffering=1)
while True:
    b = os.read(p.stdout.fileno(), 4096)
    if not b: out.write(f"[{time.strftime('%H:%M:%S')}] <EOF rc={p.poll()}>\n"); break
    out.write(f"[{time.strftime('%H:%M:%S')}] {b!r}\n")
