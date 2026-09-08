// Static server for the XSS harness. Port 7251 (the security review's port
// allocation is 7250-7299).
// Serves the repository at / and this directory at /sec/. Read-only, loopback only.
import http from 'node:http';
import fs from 'node:fs';
import path from 'node:path';
import url from 'node:url';

const SEC = path.dirname(url.fileURLToPath(import.meta.url));

// internal/audit/xssharness -> the repository root, three levels up.
const REPO = path.resolve(SEC, '..', '..', '..');

// 7251 is this package allocation from the 7250-7299 range. Loopback only,
// read-only, and it exits with the harness.
const PORT = Number(process.env.DEBARK_XSS_PORT || 7251);

const TYPES = {
  '.html': 'text/html; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.mjs': 'text/javascript; charset=utf-8',
  '.css': 'text/css; charset=utf-8',
  '.json': 'application/json; charset=utf-8',
  '.svg': 'image/svg+xml',
};

http
  .createServer((req, res) => {
    let p = decodeURIComponent(req.url.split('?')[0]);
    let base = REPO;
    if (p.startsWith('/sec/')) {
      base = SEC;
      p = p.slice(4);
    }
    const file = path.join(base, p);
    if (!path.resolve(file).startsWith(base)) {
      res.writeHead(403).end('no');
      return;
    }
    fs.readFile(file, (err, buf) => {
      if (err) {
        res.writeHead(404).end('404 ' + p);
        return;
      }
      res.writeHead(200, { 'content-type': TYPES[path.extname(file)] || 'application/octet-stream' });
      res.end(buf);
    });
  })
  .listen(PORT, '127.0.0.1', () => console.log('listening on http://127.0.0.1:' + PORT));
