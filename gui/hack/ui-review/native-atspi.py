#!/usr/bin/env python3
"""Read or operate accessible controls in a disposable native desktop session.

Uses AT-SPI controls, never evaluates JavaScript or calls Wails bindings.
Trusted keyboard input should be sent using xdotool after focus.
"""
import argparse
import gi
gi.require_version('Atspi', '2.0')
from gi.repository import Atspi

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('command', choices=['tree', 'activate', 'focus'])
parser.add_argument('name', nargs='?', default='')
parser.add_argument('--app', default='')
parser.add_argument('--role', default='')
parser.add_argument('--depth', type=int, default=22)
args = parser.parse_args()

def walk(node, depth=0):
    try:
        yield node, depth
        if depth < args.depth:
            for index in range(node.get_child_count()):
                child = node.get_child_at_index(index)
                if child is not None:
                    yield from walk(child, depth + 1)
    except gi.repository.GLib.Error:
        return

desktop = Atspi.get_desktop(0)
roots = [desktop.get_child_at_index(i) for i in range(desktop.get_child_count())]
if args.app:
    roots = [app for app in roots if args.app in app.get_name()]
if args.command != 'tree':
    active_roots = []
    for app in roots:
        windows = [app.get_child_at_index(i) for i in range(app.get_child_count())]
        active_roots.extend(window for window in windows if window is not None
                            and window.get_state_set().contains(Atspi.StateType.ACTIVE)
                            and window.get_state_set().contains(Atspi.StateType.SHOWING))
    if not active_roots:
        raise SystemExit('Activate the intended native window before operating its controls')
    roots = active_roots
matches = []
for root in roots:
    for node, depth in walk(root):
        try:
            role, name = node.get_role_name(), node.get_name()
        except gi.repository.GLib.Error:
            # Recycled WebKit nodes can disappear during native enumeration.
            # Exact showing-control matching below must still succeed.
            continue
        if args.command == 'tree':
            if not name and role in ['paragraph', 'text', 'entry', 'status bar']:
                try:
                    text = node.get_text_iface()
                    if text:
                        name = Atspi.Text.get_text(text, 0, -1)
                except gi.repository.GLib.Error:
                    pass
            print('  ' * depth + role + ' ' + name)
        elif name == args.name and (not args.role or role == args.role):
            if node.get_state_set().contains(Atspi.StateType.SHOWING):
                matches.append(node)
if args.command != 'tree':
    if len(matches) != 1:
        raise SystemExit(f'Expected one showing {args.role} control {args.name!r}; found {len(matches)}')
    node = matches[0]
    if args.command == 'focus':
        if not node.get_component_iface().grab_focus():
            raise SystemExit('Control refused focus')
    else:
        action = node.get_action_iface()
        if not action or not action.get_n_actions() or not action.do_action(0):
            raise SystemExit('Control refused its accessible action')
    print(args.command + ': ' + args.name)
