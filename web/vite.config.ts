import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

const backend = "http://127.0.0.1:18080";

export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      "/api": { target: backend, changeOrigin: false },
      "/git": { target: backend, changeOrigin: false },
    },
  },
  build: {
    outDir: "dist",
    // dist/.gitkeep is restored by the npm "build" script.
    emptyOutDir: true,
    sourcemap: false,
  },
});
