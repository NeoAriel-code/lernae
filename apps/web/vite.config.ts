import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

const apiProxyTarget = process.env.LERNAE_DEV_API_PROXY_TARGET;

export default defineConfig(({ command }) => {
  if (command === "serve" && !apiProxyTarget) {
    throw new Error("Start the Web development server with `make dev` or `make web` to resolve the Server proxy target.");
  }
  return {
    plugins: [react()],
    server: {
      proxy: {
        ...(apiProxyTarget ? { "/api": apiProxyTarget } : {}),
      },
    },
  };
});
