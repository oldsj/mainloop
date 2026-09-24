import { createServer } from 'node:http';
import { Pool } from 'pg';
import { WebSocketServer } from 'ws';

const port = Number(process.env.PORT ?? 3000);
const pool = new Pool();
const server = createServer(async (request, response) => {
  if (request.method === 'GET' && request.url === '/') {
    response.writeHead(200, { 'content-type': 'text/html; charset=utf-8' });
    response.end(`<!doctype html>
<html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width">
<title>Mainloop dev workspace</title>
<main><h1>Branch workspace</h1><p>Postgres counter: <output id="count">…</output></p>
<button id="increment" type="button">Increment</button><p>WebSocket: <output id="echo">connecting…</output></p></main>
<script>
const count = document.querySelector('#count');
document.querySelector('#increment').onclick = async () => {
  const result = await fetch('/api/counter', { method: 'POST' }).then((r) => r.json());
  count.value = result.value;
};
fetch('/api/counter').then((r) => r.json()).then((v) => { count.value = v.value; });
const socket = new WebSocket((location.protocol === 'https:' ? 'wss://' : 'ws://') + location.host + '/ws');
socket.onopen = () => socket.send('echo ready');
socket.onmessage = (event) => { document.querySelector('#echo').value = event.data; };
</script></html>`);
    return;
  }

  if (request.url === '/api/counter' && request.method === 'GET') {
    const result = await pool.query('SELECT value FROM sample_counter WHERE id=1');
    response.writeHead(200, { 'content-type': 'application/json' });
    response.end(JSON.stringify({ value: result.rows[0]?.value ?? 0 }));
    return;
  }

  if (request.url === '/api/counter' && request.method === 'POST') {
    const result = await pool.query(
      'INSERT INTO sample_counter (id,value) VALUES (1,1) ON CONFLICT (id) DO UPDATE SET value=sample_counter.value+1 RETURNING value'
    );
    response.writeHead(200, { 'content-type': 'application/json' });
    response.end(JSON.stringify({ value: result.rows[0].value }));
    return;
  }

  response.writeHead(404);
  response.end('Not found');
});

const sockets = new WebSocketServer({ noServer: true });
server.on('upgrade', (request, socket, head) => {
  if (request.url !== '/ws') return socket.destroy();
  sockets.handleUpgrade(request, socket, head, (websocket) => {
    websocket.on('message', (message) => websocket.send(message.toString()));
  });
});

await pool.query(`CREATE TABLE IF NOT EXISTS sample_counter (
  id integer PRIMARY KEY,
  value integer NOT NULL DEFAULT 0
)`);
await pool.query('INSERT INTO sample_counter (id,value) VALUES (1,0) ON CONFLICT (id) DO NOTHING');
server.listen(port, '0.0.0.0', () => console.log(`sample app listening on ${port}`));
