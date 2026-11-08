#!/usr/bin/env python3
"""Check that docs/notes/retro-coverage.md covers every P0 and P1 finding of the
browser-checker retro aggregate (design docs/design/agentic-parity.md, 7.9 acceptance 3).

Usage: check-retro-coverage.py [aggregate.md] [matrix.md]

A finding is identified by its severity, run id, owner and category; the aggregate
can list several findings with the same key, so the check compares counts. Every
matrix row must be a finding of the aggregate, and its disposition must start with
"solved by ", "constrained by " or "owned elsewhere (". A "solved by" row must name
at least one repository path (internal/..., cmd/..., skills/...), and every such path
must exist in the checkout; the check cannot judge whether the code bounds the failure.
Exit 1 lists each difference.
"""
import collections
import re
import sys
from pathlib import Path

AGGREGATE = '/home/devkit/.pi/artifacts/retro/browser-checker-20261106/aggregate.md'
MATRIX = Path(__file__).resolve().parent.parent / 'docs/notes/retro-coverage.md'
FORMS = ('solved by ', 'constrained by ', 'owned elsewhere (')
REPO = Path(__file__).resolve().parent.parent
# A repository path in a disposition: internal/..., cmd/..., skills/... up to a
# separator; a trailing dot or colon ends the sentence and is not part of it.
REPO_PATH = re.compile(r'(?<![\w/.])(?:internal|cmd|skills)/[\w./-]*[\w/]')


def aggregate_findings(text):
    found = collections.Counter()
    sev, owner = None, None
    for line in text.splitlines():
        if line.startswith('## '):
            sev = 'P0' if line.startswith('## P0 findings') else 'P1' if line.startswith('## P1 findings') else None
            owner = None
            continue
        if sev == 'P1' and line.startswith('### '):
            owner = re.match(r'### (\S+)', line).group(1)
            continue
        if sev == 'P0':
            m = re.match(r'- \[(\d+)\] \(([^/]+)/([^)]+)\)', line)
            if m:
                found[('P0', m.group(1), f'{m.group(2)}/{m.group(3)}')] += 1
        elif sev == 'P1' and owner:
            m = re.match(r'- \[(\d+)\] \(([^)]+)\)', line)
            if m:
                found[('P1', m.group(1), f'{owner}/{m.group(2)}')] += 1
    return found


def matrix_rows(text):
    rows, bad = collections.Counter(), []
    for n, line in enumerate(text.splitlines(), 1):
        m = re.match(r'\| (P[01]) \| \[(\d+)\] \| ([^|]+?) \| [^|]* \| ([^|]+?) \|$', line)
        if not m:
            if re.match(r'\| P[01] \|', line):
                bad.append(f'line {n}: malformed row')
            continue
        rows[(m.group(1), m.group(2), m.group(3))] += 1
        disposition = m.group(4)
        if not disposition.startswith(FORMS):
            bad.append(f'line {n}: disposition "{disposition[:60]}" is not solved by, constrained by or owned elsewhere')
        if disposition.startswith('solved by '):
            paths = REPO_PATH.findall(disposition)
            if not paths:
                bad.append(f'line {n}: "solved by" names no repository path (internal/, cmd/ or skills/)')
            bad.extend(f'line {n}: "solved by" names {p}, which does not exist' for p in paths if not (REPO / p).exists())
    return rows, bad


def main(argv):
    agg = Path(argv[1] if len(argv) > 1 else AGGREGATE).read_text()
    want = aggregate_findings(agg)
    got, bad = matrix_rows(Path(argv[2] if len(argv) > 2 else MATRIX).read_text())
    if sum(want.values()) == 0:
        bad.append('the aggregate has no P0 or P1 finding; wrong file?')
    for k in sorted(want.keys() | got.keys()):
        if want[k] > got[k]:
            bad.append(f'missing in the matrix: {want[k] - got[k]} x {k[0]} [{k[1]}] {k[2]}')
        elif got[k] > want[k]:
            bad.append(f'not in the aggregate: {got[k] - want[k]} x {k[0]} [{k[1]}] {k[2]}')
    for b in bad:
        print(b)
    print(f'{"FAIL" if bad else "OK"}: {sum(want.values())} P0 and P1 findings in the aggregate, {sum(got.values())} matrix rows')
    return 1 if bad else 0


if __name__ == '__main__':
    sys.exit(main(sys.argv))
