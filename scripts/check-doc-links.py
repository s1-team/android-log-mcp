#!/usr/bin/env python3
"""Check relative links and #anchors in the Markdown docs (no dependencies).

Usage: scripts/check-doc-links.py [FILE.md ...]   (default: README, AGENTS,
CHANGELOG, docs/*.md, server/README.md, validation/README.md)
Anchors follow GitHub's slug rules. External (http/https/mailto) links are not checked.
"""
import glob, os, re, sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
os.chdir(ROOT)
files = sys.argv[1:] or (["README.md", "AGENTS.md", "CHANGELOG.md", "server/README.md", "validation/README.md"] + sorted(glob.glob("docs/*.md")))

def slug(h):
    h = re.sub(r"[`*_]", lambda m: "_" if m.group() == "_" else "", h.strip().lower())
    h = re.sub(r"\[([^\]]*)\]\([^)]*\)", r"\1", h)
    h = re.sub(r"[^\w\- ]", "", h, flags=re.U)
    return h.replace(" ", "-")

def anchors(path):
    out, seen, fence = set(), {}, False
    for line in open(path, encoding="utf-8"):
        if line.startswith("```"):
            fence = not fence
        m = None if fence else re.match(r"#{1,6}\s+(.*?)\s*#*\s*$", line)
        if m:
            s = slug(m.group(1))
            n = seen.get(s, 0)
            seen[s] = n + 1
            out.add(s if n == 0 else f"{s}-{n}")
    return out

cache, bad = {}, 0
for f in files:
    fence = False
    for no, line in enumerate(open(f, encoding="utf-8"), 1):
        if line.startswith("```"):
            fence = not fence
        if fence:
            continue
        line = re.sub(r"`[^`]*`", "", line)
        for target in re.findall(r"\]\(([^)\s]+)\)", line):
            if re.match(r"[a-z]+:", target):
                continue
            path, _, frag = target.partition("#")
            dest = f if path == "" else os.path.normpath(os.path.join(os.path.dirname(f), path))
            if not os.path.exists(dest):
                print(f"{f}:{no}: missing file {target}"); bad += 1; continue
            if frag and dest.endswith(".md"):
                if dest not in cache:
                    cache[dest] = anchors(dest)
                if frag not in cache[dest]:
                    print(f"{f}:{no}: missing anchor {target}"); bad += 1
print("links ok" if not bad else f"{bad} broken link(s)")
sys.exit(1 if bad else 0)
