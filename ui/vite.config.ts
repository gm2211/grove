import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

// go:embed cannot reach ../../ui/dist, so the build output lands directly in
// internal/server/dist where internal/server/ui.go embeds it.
export default defineConfig({
  plugins: [react(), tailwindcss()],
  build: {
    outDir: "../internal/server/dist",
    emptyOutDir: true,
  },
  server: {
    port: 5173,
  },
});
