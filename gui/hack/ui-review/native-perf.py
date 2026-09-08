#!/usr/bin/env python3
"""External Wails startup/idle observations; no JS hooks or application calls.

startup launches only a caller-specified test binary in its own process group,
with a separate explicit profile. It stops only that owned group after each
sample. rss is entirely read-only and should target an already idle app.
"""
import argparse
import hashlib
import json
import math
import os
from pathlib import Path
import platform
import signal
import subprocess
import time


def distribution(samples):
    values = sorted(samples)
    return {'n': len(values), 'p50': values[max(0, math.ceil(.5 * len(values)) - 1)],
            'p95': values[max(0, math.ceil(.95 * len(values)) - 1)], 'max': values[-1]}


def processes():
    result = {}
    for path in Path('/proc').glob('[0-9]*/stat'):
        try:
            text = path.read_text()
            end = text.rindex(')')
            fields = text[end + 2:].split()
            result[int(path.parent.name)] = {'name': text[text.index('(') + 1:end],
                                             'state': fields[0], 'ppid': int(fields[1]), 'pgrp': int(fields[2])}
        except (OSError, ValueError, IndexError):
            continue
    return result


def stop_owned_group(proc):
    """Wait for the group, including children whose leader exits first."""
    try:
        os.killpg(proc.pid, signal.SIGTERM)
    except ProcessLookupError:
        pass
    deadline = time.monotonic() + 3
    remaining = []
    while time.monotonic() < deadline:
        remaining = [pid for pid, row in processes().items() if row['pgrp'] == proc.pid and row['state'] != 'Z']
        if not remaining:
            break
        time.sleep(.03)
    forced = bool(remaining)
    before_kill = remaining
    if forced:
        try:
            os.killpg(proc.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
    proc.wait(timeout=3)
    deadline = time.monotonic() + 1
    while time.monotonic() < deadline:
        remaining = [pid for pid, row in processes().items() if row['pgrp'] == proc.pid and row['state'] != 'Z']
        if not remaining:
            break
        time.sleep(.03)
    return {'forced_group_cleanup': forced, 'remaining_before_kill': before_kill,
            'remaining_after_cleanup': remaining, 'complete': not remaining}


def memory_sample(pid):
    table = processes()
    if pid not in table:
        raise RuntimeError('requested app PID is not alive')
    selected = {pid}
    changed = True
    while changed:
        additions = {p for p, row in table.items() if row['ppid'] in selected and p not in selected}
        selected.update(additions)
        changed = bool(additions)
    rows, excluded = [], []
    for current in sorted(selected):
        row = {'pid': current, **table[current]}
        # A launch shell can exec Wails after starting its desktop helpers,
        # making infrastructure descendants of the application by ancestry.
        if row['name'] in {'Xvfb', 'openbox', 'orca'}:
            excluded.append(row)
            continue
        try:
            rollup = (Path('/proc') / str(current) / 'smaps_rollup').read_text().splitlines()
            values = {line.split(':', 1)[0]: int(line.split()[1]) for line in rollup if ':' in line and line.split()[1].isdigit()}
            row.update({'rss_kib': values.get('Rss'), 'pss_kib': values.get('Pss')})
        except OSError as error:
            row['error'] = str(error)
        rows.append(row)
    return {'unix_seconds': time.time(), 'loadavg': list(os.getloadavg()), 'processes': rows,
            'excluded_infrastructure_processes': excluded,
            'rss_kib': sum(row.get('rss_kib', 0) or 0 for row in rows),
            'pss_kib': sum(row.get('pss_kib', 0) or 0 for row in rows),
            'complete': all('error' not in row for row in rows)}


def target_ready(pid, atspi):
    desktop = atspi.get_desktop(0)
    for index in range(desktop.get_child_count()):
        app = desktop.get_child_at_index(index)
        if app.get_process_id() != pid:
            continue
        stack, inspected = [app], 0
        while stack and inspected < 5000:
            item = stack.pop()
            inspected += 1
            state = item.get_state_set()
            if (item.get_name() == 'Continue' and
                    item.get_role() == atspi.Role.PUSH_BUTTON and
                    state.contains(atspi.StateType.SHOWING) and
                    state.contains(atspi.StateType.ENABLED)):
                return True
            for child_index in range(item.get_child_count()):
                stack.append(item.get_child_at_index(child_index))
    return False


parser = argparse.ArgumentParser()
parser.add_argument('mode', choices=['rss', 'startup'])
parser.add_argument('--pid', type=int)
parser.add_argument('--binary', type=Path)
parser.add_argument('--profile-root', type=Path)
parser.add_argument('--samples', type=int, default=5)
parser.add_argument('--interval', type=float, default=1)
parser.add_argument('--timeout', type=float, default=15)
parser.add_argument('--out', type=Path, required=True)
args = parser.parse_args()
if not 1 <= args.samples <= 50:
    parser.error('samples must be 1..50')
args.out.parent.mkdir(parents=True, exist_ok=True)
report = {'mode': args.mode, 'os': platform.platform(), 'display': os.environ.get('DISPLAY'),
          'started_unix_seconds': time.time(), 'sample_count': args.samples,
          'percentile_method': 'Nearest rank: sorted[ceil(p*n)-1].', 'samples': [],
          'limits': ['No physical display or truly cold filesystem page-cache claim.',
                     'RSS sums shared pages repeatedly; PSS varies with unrelated sharing.',
                     'Process-tree sample explicitly excludes named Xvfb/openbox/Orca infrastructure, including inherited launcher children.',
                     'No cache-load or parsing subphase timing is inferred from startup.']}
if args.mode == 'rss':
    if not args.pid:
        parser.error('rss requires --pid of an already idle Wails app')
    report['pid'] = args.pid
    report['method'] = 'Read-only /proc smaps_rollup for current app PID and descendants; no forced GC.'
    for index in range(args.samples):
        report['samples'].append(memory_sample(args.pid))
        if index + 1 < args.samples:
            time.sleep(args.interval)
    complete = [sample for sample in report['samples'] if sample['complete']]
    report['complete_samples'] = len(complete)
    report['all_samples_complete'] = len(complete) == args.samples
    report['rss_kib'] = distribution([sample['rss_kib'] for sample in complete]) if complete else None
    report['pss_kib'] = distribution([sample['pss_kib'] for sample in complete]) if complete else None
else:
    if not args.binary or not args.profile_root:
        parser.error('startup requires --binary and an isolated --profile-root')
    import gi
    gi.require_version('Atspi', '2.0')
    from gi.repository import Atspi
    binary = args.binary.resolve(strict=True)
    profile = args.profile_root.resolve()
    if not str(profile).startswith('/out/'):
        parser.error('test profile must be an explicit directory within /out')
    profile.mkdir(parents=True, exist_ok=True)
    env = dict(os.environ)
    for key, suffix in [('XDG_CONFIG_HOME', 'config'), ('XDG_CACHE_HOME', 'cache'), ('XDG_DATA_HOME', 'data')]:
        env[key] = str(profile / suffix)
        Path(env[key]).mkdir(parents=True, exist_ok=True)
    report['binary'] = str(binary)
    report['binary_sha256'] = hashlib.sha256(binary.read_bytes()).hexdigest()
    report['profile'] = str(profile)
    report['method'] = 'New process to native AT-SPI showing/enabled Continue button, polled at >=20ms; upper bound on accessible availability, not first paint.'
    report['limits'].append('AT-SPI query overhead is included; repeated launches reuse the test profile and warm shared-library/page cache.')
    for index in range(args.samples):
        log_path = args.out.with_name(args.out.stem + '-launch-' + str(index + 1) + '.log')
        with log_path.open('wb') as log:
            start = time.monotonic()
            proc = subprocess.Popen([str(binary)], env=env, stdout=log, stderr=subprocess.STDOUT, start_new_session=True)
            sample = {'pid': proc.pid, 'loadavg': list(os.getloadavg()), 'log': str(log_path), 'ready': False}
            try:
                while time.monotonic() - start < args.timeout:
                    if proc.poll() is not None:
                        sample['exit_code'] = proc.returncode
                        break
                    try:
                        if target_ready(proc.pid, Atspi):
                            sample['ready'] = True
                            sample['accessible_ready_ms'] = (time.monotonic() - start) * 1000
                            break
                    except Exception as error:
                        sample['last_observer_error'] = str(error)
                    time.sleep(.02)
                sample['observed_elapsed_ms'] = (time.monotonic() - start) * 1000
            finally:
                # Only this newly spawned, isolated process group is owned here.
                sample['cleanup'] = stop_owned_group(proc)
            report['samples'].append(sample)
            if not sample['cleanup']['complete']:
                report['cleanup_error'] = 'Owned process-group members survived cleanup; no further launches attempted.'
                break
            if index + 1 < args.samples:
                time.sleep(args.interval)
    values = [sample['accessible_ready_ms'] for sample in report['samples'] if sample['ready']]
    report['all_ready'] = len(values) == args.samples and not report.get('cleanup_error')
    report['accessible_ready_ms'] = distribution(values) if values else None
args.out.write_text(json.dumps(report, indent=2) + '\n')
print(json.dumps({'out': str(args.out), 'mode': args.mode, 'samples': len(report['samples']),
                  'all_ready': report.get('all_ready'), 'accessible_ready_ms': report.get('accessible_ready_ms'),
                  'rss_kib': report.get('rss_kib')}))
