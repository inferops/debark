#!/usr/bin/env python3
"""Serve actual modules from an explicit revision, never copied screen markup."""
import argparse
import functools
import hashlib
import http.server
import json
import pathlib
import re
import threading
import urllib.parse

HERE = pathlib.Path(__file__).resolve().parent


def bridge_schema(gui_root):
    model_bytes = (gui_root / 'frontend/wailsjs/go/models.ts').read_bytes()
    declaration_bytes = (gui_root / 'frontend/wailsjs/go/app/App.d.ts').read_bytes()
    models = model_bytes.decode('utf-8')
    decls = declaration_bytes.decode('utf-8')
    classes = {}
    for name, body in re.findall(r'export class (\w+) \{([\s\S]*?)\n\s*static createFrom', models):
        classes[name] = [{'name': n, 'optional': bool(o), 'type': t.strip()}
                         for n, o, t in re.findall(r'^\s+(\w+)(\?)?:\s*(.+?);\s*$', body, re.M)]
    methods = {name: {'args': args, 'ret': ret.strip()}
               for name, args, ret in re.findall(r'^export function (\w+)\((.*)\):Promise<(.+)>;\s*$', decls, re.M)}
    class_count = len(re.findall(r'export class ', models))
    method_count = len(re.findall(r'^export function ', decls, re.M))
    if not class_count or len(classes) != class_count or not method_count or len(methods) != method_count:
        raise ValueError('Generated bridge schema is incomplete; rebuild bindings before review.')
    return {'classes': classes, 'methods': methods, 'source': {
        'models_sha256': hashlib.sha256(model_bytes).hexdigest(),
        'declarations_sha256': hashlib.sha256(declaration_bytes).hexdigest(),
    }}


class Handler(http.server.SimpleHTTPRequestHandler):
    def __init__(self, *args, gui_root, **kwargs):
        self.gui_root = gui_root.resolve()
        super().__init__(*args, directory=str(self.gui_root), **kwargs)

    def translate_path(self, path):
        url = urllib.parse.urlsplit(path).path
        root = HERE if url.startswith('/ui-review/') else self.gui_root
        relative = url[len('/ui-review/'):] if url.startswith('/ui-review/') else url.lstrip('/')
        target = (root / urllib.parse.unquote(relative)).resolve()
        if not target.is_relative_to(root):
            return str(root / '__outside_root__')
        return str(target)

    def do_GET(self):
        if urllib.parse.urlsplit(self.path).path == '/ui-review/schema.json':
            data = json.dumps(bridge_schema(self.gui_root)).encode()
            self.send_response(200)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', str(len(data)))
            self.send_header('Cache-Control', 'no-store')
            self.end_headers()
            self.wfile.write(data)
            return
        super().do_GET()

    def end_headers(self):
        self.send_header('Cache-Control', 'no-store')
        super().end_headers()

    def log_message(self, *_args):
        pass


def serve(gui_root, port=0):
    server = http.server.ThreadingHTTPServer(('127.0.0.1', port),
        functools.partial(Handler, gui_root=pathlib.Path(gui_root)))
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return server


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--gui-root', type=pathlib.Path, required=True)
    parser.add_argument('--port', type=int, default=7310)
    args = parser.parse_args()
    bridge_schema(args.gui_root)
    server = serve(args.gui_root, args.port)
    print(f'http://127.0.0.1:{server.server_port}/ui-review/index.html?scenario=target', flush=True)
    try:
        threading.Event().wait()
    except KeyboardInterrupt:
        server.shutdown()
