import path from "path"
import react from "@vitejs/plugin-react"
import tailwindcss from "@tailwindcss/vite"
import { defineConfig, type PluginOption } from "vite"

/**
 * Font source switch (private-by-default).
 *
 * Mora runs air-gapped by default, so fonts are served locally from
 * `public/fonts/fonts.css`. Set `VITE_FONT_SOURCE=cdn` to instead load
 * Inter / JetBrains Mono / Noto Sans SC from Google Fonts — the matching
 * `<link data-font-*>` tags are kept/stripped accordingly at build time.
 */
function fontSourcePlugin(): PluginOption {
  const useCdn = process.env.VITE_FONT_SOURCE === "cdn"
  const dropAttr = useCdn ? "data-font-local" : "data-font-cdn"
  return {
    name: "mora-font-source",
    transformIndexHtml(html) {
      return html.replace(
        new RegExp(`\\s*<link[^>]*${dropAttr}[^>]*/>\\s*`, "g"),
        "\n  ",
      )
    },
  }
}

export default defineConfig({
  plugins: [react(), tailwindcss(), fontSourcePlugin()],
  resolve: {
    alias: {
      "@": path.resolve(__dirname, "./src"),
    },
  },
  server: {
    proxy: {
      "/api": {
        target: "http://localhost:8080",
        changeOrigin: true,
      },
    },
  },
})
