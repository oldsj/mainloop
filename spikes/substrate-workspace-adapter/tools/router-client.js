// Small control-plane probe for the Substrate CONNECT router. Request data, including any
// bearer token or credential body, is read from stdin so it never appears in argv or logs.
'use strict';

const http = require('node:http');

let input = '';
process.stdin.setEncoding('utf8');
process.stdin.on('data', (chunk) => {
  input += chunk;
  if (input.length > 1_200_000) process.stdin.destroy();
});
process.stdin.on('end', () => {
  let request;
  try {
    request = JSON.parse(input);
  } catch {
    process.stdout.write('{"error":"invalid request"}\n');
    process.exitCode = 2;
    return;
  }

  const targetPort = Number(request.targetPort || 8090);
  if (
    !/^[a-z0-9-]+$/.test(request.atespace || '') ||
    !/^[a-z0-9-]+$/.test(request.actor || '') ||
    !Number.isInteger(targetPort) ||
    targetPort < 1 ||
    targetPort > 65535 ||
    !['GET', 'POST'].includes(request.method) ||
    typeof request.path !== 'string' ||
    !request.path.startsWith('/')
  ) {
    process.stdout.write('{"error":"invalid request"}\n');
    process.exitCode = 2;
    return;
  }

  let completed = false;
  const finish = (result) => {
    if (completed) return;
    completed = true;
    process.stdout.write(`${JSON.stringify(result)}\n`);
  };

  const tunnel = http.request({
    hostname: 'atenet-router.ate-system.svc.cluster.local',
    port: 8081,
    method: 'CONNECT',
    path: `actor-upstream:${targetPort}`,
    headers: {
      host: `actor-upstream:${targetPort}`,
      'ate-target-actor': `${request.atespace}/${request.actor}`
    },
    timeout: 8000
  });
  tunnel.on('connect', (response, socket) => {
    if (response.statusCode !== 200) {
      socket.destroy();
      finish({ connectStatus: response.statusCode });
      return;
    }

    const headers = { connection: 'close', host: `actor-upstream:${targetPort}` };
    let body = Buffer.alloc(0);
    if (request.body !== undefined) {
      body = Buffer.from(JSON.stringify(request.body));
      headers['content-type'] = 'application/json';
      headers['content-length'] = String(body.length);
    } else if (typeof request.rawBody === 'string') {
      body = Buffer.from(request.rawBody);
      headers['content-type'] = 'application/octet-stream';
      headers['content-length'] = String(body.length);
    }
    if (typeof request.bearerToken === 'string') {
      headers.authorization = `Bearer ${request.bearerToken}`;
    }
    const headerLines = Object.entries(headers).map(([name, value]) => `${name}: ${value}`);
    const requestHeader = Buffer.from(
      `${request.method} ${request.path} HTTP/1.1\r\n${headerLines.join('\r\n')}\r\n\r\n`
    );
    const responseChunks = [];
    if (response.head && response.head.length) responseChunks.push(response.head);
    socket.setTimeout(15000, () => socket.destroy(new Error('response timeout')));
    socket.on('data', (chunk) => responseChunks.push(chunk));
    socket.on('end', () => {
      const responseBytes = Buffer.concat(responseChunks);
      const headerEnd = responseBytes.indexOf('\r\n\r\n');
      if (headerEnd < 0) {
        finish({ transportError: 'invalid-http-response', receivedBytes: responseBytes.length });
        return;
      }
      const statusLine = responseBytes.subarray(0, headerEnd).toString('latin1').split('\r\n')[0];
      const status = Number(statusLine.split(' ')[1]);
      const responseBody = responseBytes.subarray(headerEnd + 4).toString('utf8');
      finish(request.includeResponseBody ? { status, body: responseBody } : { status });
    });
    socket.on('error', (err) => finish({ transportError: err.code || 'request-failed' }));
    socket.write(Buffer.concat([requestHeader, body]));
  });
  tunnel.on('response', (response) => {
    response.resume();
    finish({ connectStatus: response.statusCode });
  });
  tunnel.on('timeout', () => {
    tunnel.destroy();
    finish({ transportError: 'connect-timeout' });
  });
  tunnel.on('error', (err) => finish({ transportError: err.code || 'connect-failed' }));
  tunnel.end();
});
