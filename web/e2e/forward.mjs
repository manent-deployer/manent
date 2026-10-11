// Inoltro TCP usato dai test end-to-end: espone l'API del laboratorio (MANENT_API_TARGET,
// default api:8080) su localhost, l'unica origine HTTP che il browser tratta come contesto
// sicuro, necessario al login con PKCE (Web Crypto).
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
