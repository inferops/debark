#!/usr/bin/env python3
"""Run installed Orca, ignoring unreapable zombie processes in this test container.

Only process discovery changes. Speech, event handling, AT-SPI and settings
are the unmodified installed Orca implementation. No live process is hidden.
"""
from pathlib import Path
import runpy
launcher = runpy.run_path('/usr/bin/orca', run_name='orca_test_launcher')
original = launcher['otherOrcas']
def live_orcas():
    result = []
    for pid in original():
        try:
            stat = (Path('/proc') / str(pid) / 'stat').read_text()
        except FileNotFoundError:
            continue
        if stat[stat.rindex(')') + 2:].split()[0] != 'Z':
            result.append(pid)
    return result
launcher['main'].__globals__['otherOrcas'] = live_orcas
raise SystemExit(launcher['main']())
