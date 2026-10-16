// TCP forwarding used by the end-to-end tests: it exposes the lab API (MANENT_API_TARGET,
// default api:8080) on localhost, the only HTTP origin the browser treats as a secure
// context, needed for the PKCE login (Web Crypto).
import net from "node:net";

const [host, port] = (process.env.MANENT_API_TARGET ?? "api:8080").split(":");
const listen = Number(process.env.FORWARD_PORT ?? 8080);

net
  .createServer((client) => {
    const upstream = net.connect(Number(port), host);
    client.pipe(upstream).pipe(client);
    const close = () => {
      client.destroy();
      upstream.destroy();
    };
    client.on("error", close);
    upstream.on("error", close);
  })
  .listen(listen, "127.0.0.1", () => console.log(`inoltro 127.0.0.1:${listen} → ${host}:${port}`));
