#!/usr/bin/env python3
"""GTK/WebKit fixture capture; explicitly not a Wails/native-binding test."""
import argparse
import json
import os
import pathlib
import sys
import time
import urllib.parse

from server import bridge_schema, serve

os.environ.setdefault('LIBGL_ALWAYS_SOFTWARE', '1')
os.environ.setdefault('WEBKIT_DISABLE_DMABUF_RENDERER', '1')
import gi
gi.require_version('Gtk', '3.0')
gi.require_version('Gdk', '3.0')
gi.require_version('WebKit2', '4.1')
from gi.repository import Gdk, GLib, Gtk, WebKit2

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--gui-root', type=pathlib.Path, required=True)
parser.add_argument('--out', type=pathlib.Path, required=True)
parser.add_argument('--scenario', default='target')
parser.add_argument('--theme', choices=['light','dark'], default='light')
parser.add_argument('--gtk-theme', default='')
parser.add_argument('--reduced-motion', action='store_true')
parser.add_argument('--width', type=int, default=1040)
parser.add_argument('--height', type=int, default=720)
parser.add_argument('--zoom', type=float, default=1)
parser.add_argument('--journey', default='')
parser.add_argument('--demo', choices=['build', 'picker-list', 'export'], default='')
parser.add_argument('--timeout', type=int, default=30)
parser.add_argument('--revision', required=True)
parser.add_argument('--diff-sha256', default='clean')
parser.add_argument('--source-sha256', default='')
parser.add_argument('--engine-revision', required=True)
parser.add_argument('--engine-diff-sha256', required=True)
args = parser.parse_args()
gtk_settings = Gtk.Settings.get_default()
if args.gtk_theme:
    gtk_settings.set_property('gtk-theme-name', args.gtk_theme)
if args.reduced_motion:
    gtk_settings.set_property('gtk-enable-animations', False)
bridge_schema(args.gui_root)
args.out.parent.mkdir(parents=True, exist_ok=True)
server = serve(args.gui_root)
query = urllib.parse.urlencode({'scenario': args.scenario, 'theme': args.theme, 'journey': args.journey})
uri = f'http://127.0.0.1:{server.server_port}/ui-review/index.html?{query}'
if args.demo:
    uri = f'http://127.0.0.1:{server.server_port}/frontend/src/screens/{args.demo}.demo.html?autorun=1'
window = Gtk.Window(title='Debark — GTK fixture capture')
window.set_default_size(args.width,args.height)
window.move(60,60)
view = WebKit2.WebView()
view.set_zoom_level(args.zoom)
view.get_settings().set_enable_page_cache(False)
window.add(view)
window.connect('destroy', Gtk.main_quit)
window.show_all()
window.present()
view.load_uri(uri)
started = time.monotonic()
done = False
result_code = 1


def finish(report):
    global done, result_code
    if done:
        return
    done = True
    allocation = view.get_allocation()
    x,y = view.translate_coordinates(window,0,0)
    pixels = Gdk.pixbuf_get_from_window(window.get_window(),x,y,allocation.width,allocation.height)
    if pixels is None:
        raise RuntimeError('No drawable client area; verify Xvfb, window mapping, and activation.')
    pixels.savev(str(args.out), 'png', [], [])
    metadata = {
        'schema': 'debark.ui-review/v1', 'scenario': args.scenario,
        'source': {'gui_revision': args.revision, 'gui_diff_sha256': args.diff_sha256,
                   'gui_content_manifest_sha256': args.source_sha256,
                   'engine_revision': args.engine_revision, 'engine_diff_sha256': args.engine_diff_sha256},
        'evidence': 'fixture-gtk-webkit', 'live_backend': False, 'engine': 'WebKitGTK ' + '.'.join(str(x()) for x in [WebKit2.get_major_version,WebKit2.get_minor_version,WebKit2.get_micro_version]),
        'os': dict(line.split('=', 1) for line in pathlib.Path('/etc/os-release').read_text().splitlines() if '=' in line).get('PRETTY_NAME', '').strip('"'),
        'capture_command': sys.argv,
        'theme': args.theme, 'gtk_preferences': {'theme': args.gtk_theme, 'reduced_motion': args.reduced_motion},
        'client': {'width': allocation.width,'height': allocation.height},
        'window': {'width': window.get_size().width,'height':window.get_size().height,'position':list(window.get_position())},
        'scaling': {'webkit_zoom': args.zoom, 'gdk_scale': window.get_scale_factor()},
        'inspection': {'status': 'pending-human-inspection', 'outcome': ''}, 'observed': report,
    }
    args.out.with_suffix('.json').write_text(json.dumps(metadata,indent=2),encoding='utf-8')
    problems = report.get('errors',[]) + report.get('fixtureErrors',[])
    result_code = 0 if report.get('ready') and not problems else 1
    print(json.dumps({'image':str(args.out),'ready':report.get('ready'),'errors':problems,
                      'helperWords':report.get('measurements',{}).get('helperWords')}),flush=True)
    Gtk.main_quit()


def received(webview, task, _data=None):
    try:
        if hasattr(webview, 'evaluate_javascript_finish'):
            result = webview.evaluate_javascript_finish(task)
        else:
            result = webview.run_javascript_finish(task).get_js_value()
        text = result.to_string()
        report = json.loads(text) if text and text != 'undefined' else {}
        if report.get('ready'):
            finish(report)
        elif time.monotonic()-started > args.timeout:
            finish({'ready':False,'errors':['Timed out waiting for actual modules to finish rendering.'],'observed':report})
    except Exception as exc:
        if time.monotonic()-started > args.timeout:
            finish({'ready':False,'errors':[str(exc)]})


def poll():
    if done:
        return False
    expression = 'JSON.stringify(window.__UI_REVIEW_INSPECT__ ? window.__UI_REVIEW_INSPECT__() : window.__UI_REVIEW__)'
    if args.demo == 'build':
        expression = 'JSON.stringify(window.__BUILD_CHECKS__ && ({ready:window.__BUILD_CHECKS__.finished, errors:window.__BUILD_CHECKS__.errors, checks:window.__BUILD_CHECKS__}))'
    elif args.demo == 'export':
        expression = 'JSON.stringify(window.__EXPORT_CHECKS__ && ({ready:window.__EXPORT_CHECKS__.finished, errors:window.__EXPORT_CHECKS__.errors, checks:window.__EXPORT_CHECKS__}))'
    elif args.demo == 'picker-list':
        expression = 'JSON.stringify(window.__BENCH_DONE__ && ({ready:true, errors:Object.entries(window.__BENCH__.results.verdict || {}).filter(([,ok]) => ok !== true).map(([name]) => "Failed picker verdict: " + name), checks:window.__BENCH__.results}))'
    if hasattr(view, 'evaluate_javascript'):
        view.evaluate_javascript(expression,-1,None,None,None,received,None)
    else:
        view.run_javascript(expression,None,received,None)
    return True


GLib.timeout_add(200,poll)
Gtk.main()
server.shutdown()
sys.exit(result_code)
