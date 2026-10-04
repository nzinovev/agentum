import { defineConfig } from "vite";
export default defineConfig({
  base: "/",
  build: { outDir: "../internal/server/web", emptyOutDir: true },
});
